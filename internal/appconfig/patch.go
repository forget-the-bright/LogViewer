package appconfig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

// PatchHostConfigs 在保留文件其余部分（注释、格式、其他字段）的前提下，
// 仅替换 hosts.<hostName>.configs 子树。通过 hujson AST 定位目标值的字节区间，
// 然后做字节拼接，避免整体 Marshal 导致注释全部丢失。
//
// 若 host 节点不存在（异常情况），回退到全量 Save（会创建该 host 条目）。
func PatchHostConfigs(path string, hostName string, store config.ConfigStore) error {
	fileMu.Lock()
	defer fileMu.Unlock()

	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取配置失败: %w", err)
	}

	root, err := hujson.Parse(raw)
	if err != nil {
		return fmt.Errorf("解析配置失败: %w", err)
	}

	// 用 JSON Pointer 定位 hosts.<hostName>.configs
	ptr := fmt.Sprintf("/hosts/%s/configs", escapeJSONPointer(hostName))
	target := root.Find(ptr)
	if target == nil {
		return patchFallbackSaveLocked(path, raw, hostName, store)
	}

	start, end := target.StartOffset, target.EndOffset

	// 序列化新 configs
	newVal, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 configs 失败: %w", err)
	}

	// 检测目标行的缩进，对齐新写入的 JSON
	indent := detectLineIndent(raw, start)
	if indent != "" {
		newVal = indentJSONBlock(newVal, indent)
	}

	// 字节拼接：[0, start) + newVal + [end, len)
	var buf bytes.Buffer
	buf.Grow(len(raw) - (end - start) + len(newVal))
	buf.Write(raw[:start])
	buf.Write(newVal)
	buf.Write(raw[end:])

	return writeFileAtomic(path, buf.Bytes(), 0o600)
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
