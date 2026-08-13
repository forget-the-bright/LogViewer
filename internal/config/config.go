package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// FilterRule 过滤规则构建器：所有条件最终拼装为一条正则交给命令（grep/Select-String）处理。
// 拼接顺序：时间范围 -> 日志级别 -> 内容，用 .* 做 AND 连接。
// 定义了 CustomRegex 时直接使用（优先级最高），忽略其余条件。
type FilterRule struct {
	TimeStart     string   `json:"TimeStart"`     // 起，格式 2006-01-02 15:04:05（精确到秒）
	TimeEnd       string   `json:"TimeEnd"`       // 止
	TimePrecision string   `json:"TimePrecision"` // 时间粒度: day/hour/minute/second，空=second
	Levels        []string `json:"Levels"`        // 日志级别，如 ["ERROR","WARN"]，以 | 做 OR
	Content       string   `json:"Content"`       // 内容关键词（非正则时按字面量转义）
	Exclude       string   `json:"Exclude"`       // 排除关键词，独立一轮反转过滤
	CustomRegex   string   `json:"CustomRegex"`   // 自定义正则，优先于拼装
}

// LogConfig 一次日志查看的完整过滤参数。所有字段都映射为系统原生命令参数。
type LogConfig struct {
	ConfigName string `json:"ConfigName"`
	// 读取参数
	FollowTail     bool   `json:"FollowTail"`     // true=实时跟踪(tail -F / Get-Content -Wait)；false=一次性读取
	ReadLinesLimit int    `json:"ReadLinesLimit"` // 读取末 N 行，0=全部
	Encoding       string `json:"Encoding"`       // utf-8 / gbk
	// 过滤参数（映射 grep/Select-String）
	CaseSensitive  bool       `json:"CaseSensitive"`  // 大小写敏感
	InvertMatch    bool       `json:"InvertMatch"`    // 反转匹配（不匹配主模式的行）
	ContextBefore  int        `json:"ContextBefore"`  // 匹配行前 N 行（grep -B / sls -Context）
	ContextAfter   int        `json:"ContextAfter"`   // 匹配行后 N 行（grep -A / sls -Context）
	UseRegex       bool       `json:"UseRegex"`       // 内容/排除是否按正则（时间与级别恒为正则）
	FilterRule     FilterRule `json:"FilterRule"`     // 过滤规则构建器
	HighlightRules []string   `json:"HighlightRules"` // 高亮关键词规则
}

// DefaultConfigName 内置默认配置名
const DefaultConfigName = "默认配置"

// DefaultConfig 返回内置默认配置（默认跟踪模式，过滤规则留空=不过滤）
func DefaultConfig() LogConfig {
	return LogConfig{
		ConfigName:      DefaultConfigName,
		FollowTail:      true,
		ReadLinesLimit:  200,
		Encoding:        "utf-8",
		CaseSensitive:   false,
		InvertMatch:     false,
		ContextBefore:   0,
		ContextAfter:    0,
		UseRegex:        true,
		FilterRule:      FilterRule{},
		HighlightRules:  []string{"ERROR", "WARN"},
	}
}

// ConfigStore 落在磁盘上的配置仓库
type ConfigStore struct {
	DefaultName string               `json:"default_name"`
	Configs     map[string]LogConfig `json:"configs"`
}

// Manager 负责配置的持久化与 CRUD
type Manager struct {
	mu   sync.Mutex
	path string
	data *ConfigStore
}

// NewManager 加载或初始化配置仓库。没有配置文件时写入内置默认配置。
func NewManager(dir string) (*Manager, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "configs.json")
	m := &Manager{path: path, data: &ConfigStore{Configs: map[string]LogConfig{}}}

	if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
		if err := json.Unmarshal(b, m.data); err != nil {
			return nil, fmt.Errorf("解析配置文件失败: %w", err)
		}
		if m.data.Configs == nil {
			m.data.Configs = map[string]LogConfig{}
		}
	} else {
		// 首次运行：写入默认配置
		def := DefaultConfig()
		m.data.DefaultName = DefaultConfigName
		m.data.Configs[DefaultConfigName] = def
		if err := m.persist(); err != nil {
			return nil, err
		}
	}

	// 确保默认配置存在
	if _, ok := m.data.Configs[m.data.DefaultName]; !ok {
		if m.data.DefaultName == "" {
			m.data.DefaultName = DefaultConfigName
		}
		if _, ok := m.data.Configs[m.data.DefaultName]; !ok {
			m.data.Configs[m.data.DefaultName] = DefaultConfig()
		}
	}
	return m, nil
}

func (m *Manager) persist() error {
	b, err := json.MarshalIndent(m.data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.path, b, 0o644)
}

// List 返回所有配置名
func (m *Manager) List() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.data.Configs))
	for name := range m.data.Configs {
		names = append(names, name)
	}
	return names
}

// Get 获取指定配置
func (m *Manager) Get(name string) (LogConfig, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.data.Configs[name]
	return c, ok
}

// GetDefault 获取当前默认配置
func (m *Manager) GetDefault() LogConfig {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.data.Configs[m.data.DefaultName]; ok {
		return c
	}
	return DefaultConfig()
}

// Save 新增或覆盖保存配置
func (m *Manager) Save(c LogConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c.ConfigName == "" {
		return fmt.Errorf("配置名不能为空")
	}
	m.data.Configs[c.ConfigName] = c
	return m.persist()
}

// Delete 删除配置（默认配置不可删除）
func (m *Manager) Delete(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if name == m.data.DefaultName {
		return fmt.Errorf("默认配置不可删除")
	}
	if _, ok := m.data.Configs[name]; !ok {
		return fmt.Errorf("配置 %q 不存在", name)
	}
	delete(m.data.Configs, name)
	return m.persist()
}

// SetDefault 设为默认配置
func (m *Manager) SetDefault(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.data.Configs[name]; !ok {
		return fmt.Errorf("配置 %q 不存在", name)
	}
	m.data.DefaultName = name
	return m.persist()
}

// Rename 重命名配置
func (m *Manager) Rename(oldName, newName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.data.Configs[oldName]
	if !ok {
		return fmt.Errorf("配置 %q 不存在", oldName)
	}
	if _, exists := m.data.Configs[newName]; exists {
		return fmt.Errorf("配置 %q 已存在", newName)
	}
	c.ConfigName = newName
	delete(m.data.Configs, oldName)
	m.data.Configs[newName] = c
	if m.data.DefaultName == oldName {
		m.data.DefaultName = newName
	}
	return m.persist()
}