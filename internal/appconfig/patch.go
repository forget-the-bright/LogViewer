package appconfig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/tailscale/hujson"

	"logviewer/internal/config"
)

// fileMu 串行化对同一个配置文件的读-改-写，避免热加载（读）与
// Web 界面保存预设（写）并发时出现读到半写入内容或丢失更新。
var fileMu sync.Mutex

// writeFileAtomic 先写临时文件再 rename，保证读者永远不会看到截断/半写入的内容。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".logviewer-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		cleanup()
		return fmt.Errorf("设置文件权限失败: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("替换配置文件失败: %w", err)
	}
	return nil
}

// splice 表示对配置文件中某个 JSON Pointer 目标值的原位替换。
type splice struct {
	ptr      string
	newValue any
}

// spliceRawValues 在保留文件其余部分（注释、格式、其他字段）的前提下，把每个
// JSON Pointer 指向的值替换为 newValue（用 encoding/json 序列化）。通过 hujson
// AST 定位每个目标的字节区间，从后往前字节拼接，避免整体 Marshal 导致注释丢失。
//
// 某个指针不存在时跳过（不报错）；返回成功替换的数量。所有指针必须在解析时存在
// 语义；调用方负责只传应当存在的路径。
func spliceRawValues(raw []byte, edits []splice) ([]byte, int, error) {
	root, err := hujson.Parse(raw)
	if err != nil {
		return nil, 0, fmt.Errorf("解析配置失败: %w", err)
	}

	type span struct {
		start, end int
		val        []byte
	}
	spans := make([]span, 0, len(edits))
	for _, e := range edits {
		target := root.Find(e.ptr)
		if target == nil {
			continue
		}
		newVal, err := json.MarshalIndent(e.newValue, "", "  ")
		if err != nil {
			return nil, 0, fmt.Errorf("序列化 %s 失败: %w", e.ptr, err)
		}
		// 对齐目标行缩进，使拼入的多行 JSON 与上下文一致
		if indent := detectLineIndent(raw, target.StartOffset); indent != "" {
			newVal = indentJSONBlock(newVal, indent)
		}
		spans = append(spans, span{target.StartOffset, target.EndOffset, newVal})
	}
	if len(spans) == 0 {
		return raw, 0, nil
	}

	// 按 start 降序排列，从后往前替换，偏移量不受前面替换影响
	sort.Slice(spans, func(i, j int) bool { return spans[i].start > spans[j].start })

	var out bytes.Buffer
	out.Grow(len(raw))
	cursor := 0
	// 从前往后写：raw 按原始顺序，spans 降序所以倒着遍历
	for i := len(spans) - 1; i >= 0; i-- {
		sp := spans[i]
		out.Write(raw[cursor:sp.start])
		out.Write(sp.val)
		cursor = sp.end
	}
	out.Write(raw[cursor:])
	return out.Bytes(), len(spans), nil
}

// SpliceConfigValues 带锁地把若干 JSON Pointer→值 的替换落盘，保留注释与格式。
// 供加密/解密、迁移等需要改个别字段但不应剥光注释的全量操作使用。
func SpliceConfigValues(path string, replacements map[string]any) error {
	fileMu.Lock()
	defer fileMu.Unlock()

	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取配置失败: %w", err)
	}
	edits := make([]splice, 0, len(replacements))
	for ptr, v := range replacements {
		edits = append(edits, splice{ptr: ptr, newValue: v})
	}
	out, _, err := spliceRawValues(raw, edits)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, out, 0o600)
}

// PatchHostConfigs 在保留文件其余部分（注释、格式、其他字段）的前提下，
// 仅替换 hosts.<hostName>.configs 子树。通过 hujson AST 定位目标值的字节区间，
// 然后字节拼接，避免整体 Marshal 导致注释全部丢失。
//
// 若 host 节点存在但缺少 configs 键（例如用户把该键整段注释掉了——这是合法
// JSONC），则通过 AST 在该 host 对象内原位插入 configs 子树，依然保留注释。
// 只有 host 节点本身不存在（异常情况）才回退到全量 Save 创建该 host 条目。
func PatchHostConfigs(path string, hostName string, store config.ConfigStore) error {
	fileMu.Lock()
	defer fileMu.Unlock()

	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取配置失败: %w", err)
	}

	ptr := fmt.Sprintf("/hosts/%s/configs", escapeJSONPointer(hostName))
	out, n, err := spliceRawValues(raw, []splice{{ptr: ptr, newValue: store}})
	if err != nil {
		return err
	}
	if n > 0 {
		return writeFileAtomic(path, out, 0o600)
	}

	// configs 键不存在。区分两种情况：
	root, err := hujson.Parse(raw)
	if err != nil {
		return fmt.Errorf("解析配置失败: %w", err)
	}
	hostVal := root.Find("/hosts/" + escapeJSONPointer(hostName))
	if hostVal == nil {
		// host 本身不存在：只能全量重建（会创建该 host 条目）。
		return patchFallbackSaveLocked(path, raw, hostName, store)
	}
	// host 存在但缺 configs 键：AST 原位插入子树，不全量 Marshal，保留注释。
	inserted, err := insertHostConfigsMember(raw, hostVal, store)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, inserted, 0o600)
}

// insertHostConfigsMember 在已存在的 host 对象内插入 "configs" 成员，
// 返回完整的新文件字节。hostVal 必须是 *hujson.Object 且不含 configs 键。
// 通过字节拼接实现：在最后一个成员后补逗号、在闭合花括号前插入新成员，
// 缩进对齐上下文，不触碰文件其余任何字节（注释完整保留）。
func insertHostConfigsMember(raw []byte, hostVal *hujson.Value, store config.ConfigStore) ([]byte, error) {
	obj, ok := hostVal.Value.(*hujson.Object)
	if !ok {
		return nil, fmt.Errorf("目标 host 节点不是对象")
	}
	for i := range obj.Members {
		if lit, ok := obj.Members[i].Name.Value.(hujson.Literal); ok && lit.Kind() == '"' && lit.String() == "configs" {
			return nil, fmt.Errorf("configs 已存在，不应走插入路径")
		}
	}
	newVal, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("序列化 configs 失败: %w", err)
	}
	// outerIndent = host 条目的缩进；innerIndent = 其成员的缩进（再深一层）。
	outerIndent := detectLineIndent(raw, hostVal.StartOffset)
	innerIndent := outerIndent + "  "
	newVal = indentJSONBlock(newVal, innerIndent)

	var memberText bytes.Buffer
	memberText.WriteString("\n")
	memberText.WriteString(innerIndent)
	memberText.WriteString(`"configs": `)
	memberText.Write(newVal)
	memberText.WriteString("\n")
	memberText.WriteString(outerIndent)

	// hostVal.EndOffset 紧跟在闭合 '}' 之后；闭合花括号在其前一个字节。
	closeBrace := hostVal.EndOffset - 1
	if closeBrace < 0 || closeBrace >= len(raw) || raw[closeBrace] != '}' {
		return nil, fmt.Errorf("定位 host 闭合花括号失败")
	}

	// 需要两处拼接（从后往前，偏移互不影响）：
	//   1. 在 closeBrace 处插入 memberText；
	//   2. 若有已有成员，在最后一个成员值之后插入逗号（位于其 AfterExtra 之前，
	//      使注释仍归属于该成员）。
	type edit struct{ at int; b []byte }
	edits := []edit{{at: closeBrace, b: memberText.Bytes()}}
	if len(obj.Members) > 0 {
		edits = append(edits, edit{at: obj.Members[len(obj.Members)-1].Value.EndOffset, b: []byte(",")})
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].at > edits[j].at })

	var out bytes.Buffer
	out.Grow(len(raw) + 128)
	cursor := 0
	for i := len(edits) - 1; i >= 0; i-- {
		out.Write(raw[cursor:edits[i].at])
		out.Write(edits[i].b)
		cursor = edits[i].at
	}
	out.Write(raw[cursor:])
	return out.Bytes(), nil
}

// escapeJSONPointer 按 RFC 6901 转义 JSON Pointer 中的 ~ 和 / 字符。
func escapeJSONPointer(s string) string {
	s = strings.ReplaceAll(s, "~", "~0")
	s = strings.ReplaceAll(s, "/", "~1")
	return s
}

// detectLineIndent 返回 offset 所在行的前导空白（空格/tab）。
func detectLineIndent(raw []byte, offset int) string {
	lineStart := offset
	for lineStart > 0 && raw[lineStart-1] != '\n' {
		lineStart--
	}
	var indent []byte
	for i := lineStart; i < len(raw); i++ {
		if raw[i] == ' ' || raw[i] == '\t' {
			indent = append(indent, raw[i])
		} else {
			break
		}
	}
	return string(indent)
}

// indentJSONBlock 给多行 JSON 块的每一行（首行除外）加上 indent 前缀，
// 使其在原文件中的缩进与上下文对齐。
func indentJSONBlock(b []byte, indent string) []byte {
	if indent == "" {
		return b
	}
	lines := strings.SplitAfter(string(b), "\n")
	for i := 1; i < len(lines); i++ {
		lines[i] = indent + lines[i]
	}
	return []byte(strings.Join(lines, ""))
}

// patchFallbackSaveLocked 当 AST 定位失败时，解析全量配置后整体写回。
// 调用方必须已持有 fileMu。这种情况只应在配置结构损坏或首次初始化时发生。
func patchFallbackSaveLocked(path string, raw []byte, hostName string, store config.ConfigStore) error {
	standardized, err := hujson.Parse(raw)
	if err != nil {
		return fmt.Errorf("解析配置失败（无法定位 %s.configs）: %w", hostName, err)
	}
	standardized.Standardize()
	var cfg AppConfig
	if err := json.Unmarshal([]byte(standardized.String()), &cfg); err != nil {
		return fmt.Errorf("解析配置失败: %w", err)
	}
	if cfg.Hosts == nil {
		cfg.Hosts = map[string]HostConfig{}
	}
	h := cfg.Hosts[hostName]
	h.Configs = store
	cfg.Hosts[hostName] = h
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b, 0o600)
}
