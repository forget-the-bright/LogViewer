package host

import (
	"testing"

	"logviewer/internal/config"
)

func newLocalForTest(t *testing.T, name string, dirs []string) *LocalHost {
	t.Helper()
	h, err := NewLocalHost(name, dirs, nil, config.NewConfigStore(), nil)
	if err != nil {
		t.Fatalf("NewLocalHost(%s): %v", name, err)
	}
	return h
}

func sameHost(a, b Host) bool { return a == b }

// TestRebuild_LocalDirsChange 固化热加载语义：
// 改名或目录集合变更的本机实例必须被替换；名称+dirs 均不变则保留原实例（不中断读取）。
func TestRebuild_LocalDirsChange(t *testing.T) {
	h1 := newLocalForTest(t, "local", []string{"/tmp/a"})
	m, err := NewManager(h1)
	if err != nil {
		t.Fatal(err)
	}

	// 相同 dirs -> 保留同一实例
	sameDirs := newLocalForTest(t, "local", []string{"/tmp/a"})
	m.Rebuild([]Host{sameDirs})
	if got, _ := m.Get("local"); !sameHost(got, h1) {
		t.Fatalf("dirs 未变时应保留旧实例")
	}

	// 变更 dirs -> 必须替换为新实例
	newDirs := newLocalForTest(t, "local", []string{"/tmp/a", "/tmp/b"})
	m.Rebuild([]Host{newDirs})
	if got, _ := m.Get("local"); !sameHost(got, newDirs) {
		t.Fatalf("dirs 变更后应替换为新实例")
	}

	// 新增主机 -> 出现在集合中
	extra := newLocalForTest(t, "local2", []string{"/tmp/x"})
	m.Rebuild([]Host{newDirs, extra})
	if _, err := m.Get("local2"); err != nil {
		t.Fatalf("新增主机应可见: %v", err)
	}

	// 删除主机 -> 从集合移除
	changed := m.Rebuild([]Host{newDirs})
	if _, err := m.Get("local2"); err == nil {
		t.Fatalf("删除的主机应不可见")
	}
	// local2 被移除，应出现在 changed 列表中（供上层通知其活跃 WS 重连）。
	if len(changed) != 1 || changed[0] != "local2" {
		t.Fatalf("changed = %v, want [local2]", changed)
	}
}

// TestRebuild_ReturnsChangedHosts 验证 Rebuild 返回被替换/移除的主机别名，
// 且配置未变的主机不出现在列表中（不应触发无谓重连）。
func TestRebuild_ReturnsChangedHosts(t *testing.T) {
	h1 := newLocalForTest(t, "local", []string{"/tmp/a"})
	h2 := newLocalForTest(t, "remote", []string{"/tmp/r"})
	m, err := NewManager(h1, h2)
	if err != nil {
		t.Fatal(err)
	}

	// 两者都不变：changed 应为空。
	same1 := newLocalForTest(t, "local", []string{"/tmp/a"})
	same2 := newLocalForTest(t, "remote", []string{"/tmp/r"})
	if changed := m.Rebuild([]Host{same1, same2}); len(changed) != 0 {
		t.Fatalf("未变更时 changed 应为空，got %v", changed)
	}

	// 只替换 local：changed 只含 local。
	newLocal := newLocalForTest(t, "local", []string{"/tmp/b"})
	changed := m.Rebuild([]Host{newLocal, same2})
	if len(changed) != 1 || changed[0] != "local" {
		t.Fatalf("changed = %v, want [local]", changed)
	}
}

// TestLocalHost_Fingerprint 确保本机指纹随 dirs 变化而变化（Rebuild 判定的基础）。
func TestLocalHost_Fingerprint(t *testing.T) {
	a := newLocalForTest(t, "local", []string{"/tmp/a"})
	b := newLocalForTest(t, "local", []string{"/tmp/a"})
	c := newLocalForTest(t, "local", []string{"/tmp/c"})
	if a.Fingerprint() != b.Fingerprint() {
		t.Fatalf("相同 dirs 指纹应一致")
	}
	if a.Fingerprint() == c.Fingerprint() {
		t.Fatalf("不同 dirs 指纹应不同")
	}
}
