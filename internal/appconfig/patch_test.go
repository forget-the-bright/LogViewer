package appconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"logviewer/internal/config"
)

func TestPatchHostConfigsPreservesComments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logviewer.json")

	original := `{
  // 顶部注释，必须保留
  "addr": ":8080",
  "auth": {
    "enabled": false,
    "username": "",
    "password": "",
    "session_ttl_minutes": 720
  },
  "hosts": {
    "local": {
      // 本机目录注释
      "dirs": ["/var/log"],
      "configs": {
        "default_name": "默认配置",
        "configs": {
          "旧配置": {
            "ConfigName": "旧配置"
          }
        }
      }
    }
  }
}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	newStore := config.NewConfigStore()
	newStore.DefaultName = "新默认"
	newStore.Configs["新预设"] = config.LogConfig{ConfigName: "新预设"}

	if err := PatchHostConfigs(path, "local", newStore); err != nil {
		t.Fatalf("PatchHostConfigs failed: %v", err)
	}

	result, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	resultStr := string(result)

	// 注释必须保留
	if !strings.Contains(resultStr, "顶部注释，必须保留") {
		t.Error("顶部注释丢失")
	}
	if !strings.Contains(resultStr, "本机目录注释") {
		t.Error("host 注释丢失")
	}
	// addr 字段保留
	if !strings.Contains(resultStr, `"addr": ":8080"`) {
		t.Error("addr 字段丢失")
	}
	// 新配置写入
	if !strings.Contains(resultStr, "新默认") {
		t.Error("新默认配置未写入")
	}
	if !strings.Contains(resultStr, "新预设") {
		t.Error("新预设未写入")
	}
	// 旧配置不在
	if strings.Contains(resultStr, "旧配置") {
		t.Error("旧配置未被替换")
	}

	// 验证写回的文件仍可解析
	cfg, _, err := Load(path, nil)
	if err != nil {
		t.Fatalf("重载配置失败: %v", err)
	}
	localCfg := cfg.Hosts["local"]
	if localCfg.Configs.DefaultName != "新默认" {
		t.Errorf("DefaultName = %q, want 新默认", localCfg.Configs.DefaultName)
	}
}

// TestPatchHostConfigsMissingConfigsKeyPreservesComments 验证当 host 存在、
// 但其 configs 键被整段注释掉（合法 JSONC）时，保存预设通过 AST 原位插入子树，
// 不会全量 Marshal 剥光注释。
func TestPatchHostConfigsMissingConfigsKeyPreservesComments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logviewer.json")

	original := `{
  // 顶部注释必须保留
  "addr": ":8080",
  "hosts": {
    "local": {
      // 本机目录注释
      "dirs": ["/var/log"]
      // configs 暂时被注释掉：
      // "configs": { "default_name": "旧", "configs": {} }
    }
  }
}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	store := config.NewConfigStore()
	store.DefaultName = "新默认"
	store.Configs["预设A"] = config.LogConfig{ConfigName: "预设A"}
	if err := PatchHostConfigs(path, "local", store); err != nil {
		t.Fatalf("PatchHostConfigs failed: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)

	// 注释必须完整保留（这是本测试的核心：不允许全量重写剥光注释）
	for _, want := range []string{
		"顶部注释必须保留",
		"本机目录注释",
		"configs 暂时被注释掉",
		`"addr": ":8080"`,
		`"dirs": ["/var/log"]`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("注释/字段丢失: %q\n--- 文件 ---\n%s", want, s)
		}
	}
	// 新配置已插入
	if !strings.Contains(s, "新默认") || !strings.Contains(s, "预设A") {
		t.Errorf("新 configs 未写入:\n%s", s)
	}
	// 文件仍可解析且值正确
	cfg, _, err := Load(path, nil)
	if err != nil {
		t.Fatalf("回写后重载失败: %v\n文件:\n%s", err, s)
	}
	if cfg.Hosts["local"].Configs.DefaultName != "新默认" {
		t.Errorf("DefaultName=%q", cfg.Hosts["local"].Configs.DefaultName)
	}
}

func TestPatchHostConfigsNonExistentHostFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logviewer.json")

	original := `{
  "addr": ":8080",
  "hosts": {
    "local": {
      "dirs": [],
      "configs": {"default_name":"d","configs":{}}
    }
  }
}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	newStore := config.NewConfigStore()
	// 对不存在的 host 打补丁：应回退到全量 Save（不会丢失字段）。
	// 回退创建的条目只有 configs（Validate 会拒绝非 local 无 ssh 的主机），
	// 因此直接检查文件内容，不走 Load+Validate。
	if err := PatchHostConfigs(path, "new-host", newStore); err != nil {
		t.Fatalf("PatchHostConfigs failed: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, `"new-host"`) {
		t.Error("new-host 未被创建")
	}
	if !strings.Contains(s, `"local"`) {
		t.Error("local 主机丢失")
	}
	if !strings.Contains(s, `":8080"`) {
		t.Error("addr 字段丢失")
	}
}

// TestSplicePasswordsPreservesComments 验证加解密密码时通过 AST 原位替换标量，
// 注释（尤其注释掉的远程主机示例块）完整保留，而不是被全量 Marshal 剥光。
func TestSplicePasswordsPreservesComments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logviewer.json")

	original := `{
  // 顶部说明
  "addr": ":8080",
  "auth": {
    "enabled": true,
    "username": "admin",
    "password": "plain-admin-pw",
    "session_ttl_minutes": 720
  },
  // 添加远程机器示例（取消注释并修改）：
  // "prod-web-01": {
  //   "ssh": { "host": "10.0.0.11", "username": "root", "password": "changeme" }
  // }
  "hosts": {
    "local": { "dirs": [], "configs": { "default_name": "默认", "configs": {} } },
    "remote1": {
      "ssh": {
        "host": "10.0.0.1",
        "port": 22,
        "username": "root",
        "password": "plain-ssh-pw"
      },
      "dirs": ["/var/log"],
      "configs": { "default_name": "默认", "configs": {} }
    }
  }
}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	// 模拟加密：加载 -> 改两个密码 -> 通过 SpliceConfigValues 原位写回
	cfg, _, err := Load(path, nil)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	cfg.Auth.Password = "enc:v1:AUTH-ENC"
	cfg.Hosts["remote1"] = func() HostConfig {
		h := cfg.Hosts["remote1"]
		h.SSH.Password = "enc:v1:SSH-ENC"
		return h
	}()

	if err := SpliceConfigValues(path, cfg.PasswordFieldPointers()); err != nil {
		t.Fatalf("SpliceConfigValues failed: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)

	// 注释必须完整保留
	for _, want := range []string{
		"顶部说明",
		"添加远程机器示例",
		`"prod-web-01"`,
		`"10.0.0.11"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("注释/示例丢失: %q\n--- 文件内容 ---\n%s", want, s)
		}
	}
	// 新密码写入、旧明文消失
	if !strings.Contains(s, "enc:v1:AUTH-ENC") {
		t.Error("auth 加密密码未写入")
	}
	if !strings.Contains(s, "enc:v1:SSH-ENC") {
		t.Error("ssh 加密密码未写入")
	}
	if strings.Contains(s, "plain-admin-pw") || strings.Contains(s, "plain-ssh-pw") {
		t.Error("明文密码未被替换")
	}
	// 其他未改字段保持原样
	if !strings.Contains(s, `"username": "admin"`) {
		t.Error("未改字段被改动")
	}

	// 写回后的文件仍可解析且密码值正确
	reloaded, _, err := Load(path, nil)
	if err != nil {
		t.Fatalf("回写后重载失败: %v", err)
	}
	if reloaded.Auth.Password != "enc:v1:AUTH-ENC" {
		t.Errorf("auth.password = %q", reloaded.Auth.Password)
	}
	if reloaded.Hosts["remote1"].SSH.Password != "enc:v1:SSH-ENC" {
		t.Errorf("ssh.password = %q", reloaded.Hosts["remote1"].SSH.Password)
	}
}

// TestGenerateTemplateMigratePreservesComments 验证迁移旧配置时，
// 模板的注释与注释掉的远程示例不被全量序列化剥光。
func TestGenerateTemplateMigratePreservesComments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logviewer.json")
	store := config.NewConfigStore()
	store.DefaultName = "迁移默认"
	store.Configs["迁移预设"] = config.LogConfig{ConfigName: "迁移预设"}

	if err := GenerateTemplate(path, &store); err != nil {
		t.Fatalf("GenerateTemplate failed: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.Contains(s, "prod-web-01") {
		t.Error("迁移后注释掉的远程示例丢失")
	}
	if !strings.Contains(s, "LogViewer 配置文件") {
		t.Error("顶部标题注释丢失")
	}
	if !strings.Contains(s, "迁移默认") || !strings.Contains(s, "迁移预设") {
		t.Error("迁移数据未写入")
	}
	// 文件仍可解析
	cfg, _, err := Load(path, nil)
	if err != nil {
		t.Fatalf("回写后重载失败: %v", err)
	}
	if cfg.Hosts["local"].Configs.DefaultName != "迁移默认" {
		t.Errorf("DefaultName = %q", cfg.Hosts["local"].Configs.DefaultName)
	}
}
