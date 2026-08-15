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
	// 对不存在的 host 打补丁：应回退到全量 Save（不会丢失字段）
	if err := PatchHostConfigs(path, "new-host", newStore); err != nil {
		t.Fatalf("PatchHostConfigs failed: %v", err)
	}

	cfg, _, err := Load(path, nil)
	if err != nil {
		t.Fatalf("重载配置失败: %v", err)
	}
	if _, ok := cfg.Hosts["new-host"]; !ok {
		t.Error("new-host 未被创建")
	}
	if _, ok := cfg.Hosts["local"]; !ok {
		t.Error("local 主机丢失")
	}
}
