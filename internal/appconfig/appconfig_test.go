package appconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestGenerateAndLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "logviewer.json")

	cfg, path, err := Load(p, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if path != p {
		t.Errorf("path = %q want %q", path, p)
	}
	if cfg.Addr != ":8080" {
		t.Errorf("addr = %q", cfg.Addr)
	}
	local, ok := cfg.Hosts["local"]
	if !ok {
		t.Fatal("local host missing")
	}
	if len(local.Configs.Configs) == 0 {
		t.Error("local configs should have default")
	}
	if local.Configs.DefaultName == "" {
		t.Error("default name empty")
	}
	if fi, err := os.Stat(p); err != nil {
		t.Fatalf("template not written: %v", err)
	} else if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		// Windows 上 Unix 权限位不生效，只在非 Windows 校验 0600
		t.Errorf("file perm = %v want 0600", fi.Mode().Perm())
	}
}

func TestJSONCComments(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "logviewer.json")
	content := `{
		// 行注释
		"addr": ":9090",
		"hosts": {
			"local": { "dirs": [], "configs": {} }
		}
	}`
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Load(p, nil)
	if err != nil {
		t.Fatalf("Load with comments: %v", err)
	}
	if cfg.Addr != ":9090" {
		t.Errorf("addr = %q", cfg.Addr)
	}
}

func TestMergeLocalDirsDedup(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "logviewer.json")
	if err := os.WriteFile(p, []byte(`{
		"hosts": { "local": { "dirs": ["/a"], "configs": {} } }
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Load(p, []string{"/a", "/b", "/b", "/c"})
	if err != nil {
		t.Fatal(err)
	}
	dirs := cfg.Hosts["local"].Dirs
	// /a 已存在；/b 去重；/c 加入
	set := map[string]bool{}
	for _, d := range dirs {
		if set[d] {
			t.Errorf("duplicate dir %q in %v", d, dirs)
		}
		set[d] = true
	}
	for _, want := range []string{"/a", "/b", "/c"} {
		abs, _ := filepath.Abs(want)
		if !set[abs] {
			t.Errorf("missing dir %q (abs %q) in %v", want, abs, dirs)
		}
	}
}

func TestMigrateOldConfig(t *testing.T) {
	dir := t.TempDir()
	oldDir := filepath.Join(dir, "config")
	if err := os.MkdirAll(oldDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldStore := map[string]any{
		"default_name": "默认配置",
		"configs": map[string]any{
			"默认配置": map[string]any{
				"ConfigName": "默认配置", "FollowTail": false, "ReadLinesLimit": 50,
			},
			"自定义": map[string]any{"ConfigName": "自定义", "Encoding": "gbk"},
		},
	}
	b, _ := json.MarshalIndent(oldStore, "", "  ")
	if err := os.WriteFile(filepath.Join(oldDir, "configs.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}

	p := filepath.Join(dir, "logviewer.json")
	cfg, _, err := Load(p, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	confs := cfg.Hosts["local"].Configs
	if _, ok := confs.Configs["自定义"]; !ok {
		t.Errorf("migrated config missing: %+v", confs.Configs)
	}
	if confs.Configs["默认配置"].ReadLinesLimit != 50 {
		t.Errorf("default config not migrated correctly: %+v", confs.Configs["默认配置"])
	}
	// 旧文件应被改名备份
	if _, err := os.Stat(filepath.Join(oldDir, "configs.json")); !os.IsNotExist(err) {
		t.Error("old configs.json should be renamed")
	}
	if _, err := os.Stat(filepath.Join(oldDir, "configs.json.bak")); err != nil {
		t.Errorf("backup not created: %v", err)
	}
}

func TestValidate(t *testing.T) {
	bad := &AppConfig{Hosts: map[string]HostConfig{
		"local": {SSH: &SSHConfig{Host: "x"}},
	}}
	if err := bad.Validate(); err == nil {
		t.Error("local with ssh should fail")
	}

	badNoSSH := &AppConfig{Hosts: map[string]HostConfig{
		"myhost": {Dirs: []string{"/var/log"}},
	}}
	if err := badNoSSH.Validate(); err == nil {
		t.Error("non-local host without ssh should fail")
	}

	bad2 := &AppConfig{Hosts: map[string]HostConfig{
		"remote": {SSH: &SSHConfig{Host: "1.2.3.4", Username: "", Password: "x"}},
	}}
	if err := bad2.Validate(); err == nil {
		t.Error("missing username should fail")
	}

	good := &AppConfig{
		LogLevel:            "info",
		SessionGraceSeconds: 45,
		Hosts: map[string]HostConfig{
			"remote": {SSH: &SSHConfig{Host: "1.2.3.4", Username: "u", Password: "p"}},
		},
	}
	if err := good.Validate(); err != nil {
		t.Errorf("good config failed: %v", err)
	}

	badLevel := *good
	badLevel.LogLevel = "verbose"
	if err := badLevel.Validate(); err == nil {
		t.Error("invalid log_level should fail")
	}

	badGrace := *good
	badGrace.SessionGraceSeconds = 2
	if err := badGrace.Validate(); err == nil {
		t.Error("session_grace_seconds below 5 should fail")
	}
}

func TestDisplayNameAndFileExtensions(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "logviewer.json")
	// Use forward slashes to avoid JSON escape issues on Windows
	dirJSON := filepath.ToSlash(dir)
	content := `{
		"hosts": {
			"local": {
				"dirs": ["` + dirJSON + `"],
				"display_name": "我的本机",
				"file_extensions": [".txt", ".json"],
				"configs": {}
			}
		}
	}`
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Load(p, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	local := cfg.Hosts["local"]
	if local.DisplayName != "我的本机" {
		t.Errorf("DisplayName = %q, want %q", local.DisplayName, "我的本机")
	}
	if len(local.FileExtensions) != 2 || local.FileExtensions[0] != ".txt" {
		t.Errorf("FileExtensions = %v, want [.txt .json]", local.FileExtensions)
	}
}
