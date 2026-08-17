package host

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"logviewer/internal/config"
)

func newTestLocal(t *testing.T, dirs ...string) *LocalHost {
	t.Helper()
	h, err := NewLocalHost("local", dirs, nil, config.NewConfigStore(), nil)
	if err != nil {
		t.Fatalf("NewLocalHost: %v", err)
	}
	return h
}

func TestResolvePathBasic(t *testing.T) {
	root := t.TempDir()
	h := newTestLocal(t, root)

	// 合法：root 内文件
	good := filepath.Join(root, "app.log")
	if got, err := h.ResolvePath(good); err != nil || got != good {
		t.Errorf("合法路径被拒: got=%q err=%v", got, err)
	}

	// 非法：跳出 root
	bad := filepath.Join(root, "..", "etc", "passwd")
	if _, err := h.ResolvePath(bad); err == nil {
		t.Errorf("路径穿越未被拦截: %s", bad)
	}
}

// 软链指向 root 外部必须被拒（这是本次修复的核心）。
func TestResolvePathSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建软链需要管理员权限，跳过")
	}
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.log")
	if err := os.WriteFile(secret, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// root/link -> outside
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("无法创建软链: %v", err)
	}
	h := newTestLocal(t, root)

	// 访问 root/link/secret.log 应被拒，因为真实路径在 root 外
	target := filepath.Join(link, "secret.log")
	if _, err := h.ResolvePath(target); err == nil {
		t.Errorf("软链逃逸未被拦截: %s -> %s", target, secret)
	}
}

// 软链指向 root 内部应放行。
func TestResolvePathSymlinkInside(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建软链需要管理员权限，跳过")
	}
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "ln")
	if err := os.Symlink(sub, link); err != nil {
		t.Skipf("无法创建软链: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, "a.log"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newTestLocal(t, root)
	if _, err := h.ResolvePath(filepath.Join(link, "a.log")); err != nil {
		t.Errorf("root 内软链被误拒: %v", err)
	}
}

// 多 root 去重
func TestRootsDedup(t *testing.T) {
	root := t.TempDir()
	h := newTestLocal(t, root, root, root+string(filepath.Separator)+".")
	if len(h.Dirs()) != 1 {
		t.Errorf("去重失败: %v", h.Dirs())
	}
}

func TestLsFiltersExtensions(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.log"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.out"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "c.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := newTestLocal(t, root)
	nodes, err := h.Ls(root)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, n := range nodes {
		names[n.Name] = true
	}
	if !names["a.log"] || !names["b.out"] {
		t.Errorf("log/out 文件应显示: %v", names)
	}
	if names["c.txt"] {
		t.Errorf("非 log/out 文件应隐藏: %v", names)
	}
	if !names["sub"] {
		t.Errorf("子目录应显示: %v", names)
	}
}

func TestHostManager(t *testing.T) {
	root := t.TempDir()
	local, _ := NewLocalHost("local", []string{root}, nil, config.NewConfigStore(), nil)
	m, err := NewManager(local)
	if err != nil {
		t.Fatal(err)
	}
	if h, err := m.Get("local"); err != nil || h.Name() != "local" {
		t.Errorf("Get local: %v %v", h, err)
	}
	if _, err := m.Get("nope"); err == nil {
		t.Error("不存在的 host 应报错")
	}
	if len(m.List()) != 1 || m.List()[0].Name != "local" {
		t.Errorf("List: %+v", m.List())
	}
}

func TestLsCustomExtensions(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"a.log", "b.txt", "c.json", "noext"} {
		if err := os.WriteFile(filepath.Join(root, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// 自定义后缀：只显示 .txt
	h, err := NewLocalHost("local", []string{root}, []string{"txt"}, config.NewConfigStore(), nil)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := h.Ls(root)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, n := range nodes {
		names[n.Name] = true
	}
	if !names["b.txt"] {
		t.Errorf(".txt 应显示: %v", names)
	}
	if names["a.log"] || names["c.json"] || names["noext"] {
		t.Errorf("非 .txt 文件应隐藏: %v", names)
	}

	// "*" 展示所有文件（含无后缀）
	hall, err := NewLocalHost("local", []string{root}, []string{"*"}, config.NewConfigStore(), nil)
	if err != nil {
		t.Fatal(err)
	}
	nodes2, _ := hall.Ls(root)
	all := map[string]bool{}
	for _, n := range nodes2 {
		all[n.Name] = true
	}
	for _, want := range []string{"a.log", "b.txt", "c.json", "noext"} {
		if !all[want] {
			t.Errorf("showAll 下 %s 应显示: %v", want, all)
		}
	}

	// 后缀自动补点且大小写不敏感
	h2, err := NewLocalHost("local", []string{root}, []string{"JSON"}, config.NewConfigStore(), nil)
	if err != nil {
		t.Fatal(err)
	}
	nodes3, _ := h2.Ls(root)
	found := false
	for _, n := range nodes3 {
		if n.Name == "c.json" {
			found = true
		}
	}
	if !found {
		t.Errorf("'JSON' 应匹配 c.json")
	}
}

func TestNormalizeExts(t *testing.T) {
	set, all := normalizeExts(nil)
	if all {
		t.Error("nil 不应是 showAll")
	}
	if !set[".log"] || !set[".out"] {
		t.Errorf("nil 应回退默认 .log/.out: %v", set)
	}

	set, all = normalizeExts([]string{"*"})
	if !all {
		t.Error("含 * 应 showAll")
	}
	if set != nil {
		t.Errorf("showAll 时 set 应为 nil")
	}

	set, _ = normalizeExts([]string{" TXT ", "json"})
	if !set[".txt"] || !set[".json"] {
		t.Errorf("应补点并小写去空白: %v", set)
	}
}

func TestLocalHostDisplayName(t *testing.T) {
	h := newTestLocal(t, t.TempDir())
	// 默认无 displayName 时 Info().DisplayName 为空
	if info := h.Info(); info.DisplayName != "" {
		t.Errorf("默认 DisplayName 应为空，got %q", info.DisplayName)
	}
	h.SetDisplayName("我的机器")
	if info := h.Info(); info.DisplayName != "我的机器" {
		t.Errorf("DisplayName = %q, want %q", info.DisplayName, "我的机器")
	}
}
