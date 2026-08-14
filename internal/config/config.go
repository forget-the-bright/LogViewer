package config

import (
	"encoding/json"
	"fmt"
	"sync"
)

// FilterRule 过滤规则构建器：所有条件最终拼装为一条正则交给命令（grep/Select-String）处理。
// 拼接顺序：时间范围 -> 日志级别 -> 内容，用 .* 做 AND 连接。
// 定义了 CustomRegex 时直接使用（优先级最高），忽略其余条件。
type FilterRule struct {
	TimeStart     string   `json:"TimeStart"`
	TimeEnd       string   `json:"TimeEnd"`
	TimePrecision string   `json:"TimePrecision"` // day/hour/minute/second，空=second
	Levels        []string `json:"Levels"`
	Content       string   `json:"Content"`
	Exclude       string   `json:"Exclude"`
	CustomRegex   string   `json:"CustomRegex"`
}

// LogConfig 一次日志查看的完整过滤参数。所有字段都映射为系统原生命令参数。
type LogConfig struct {
	ConfigName string `json:"ConfigName"`
	// 读取参数
	FollowTail     bool       `json:"FollowTail"`
	ReadLinesLimit int        `json:"ReadLinesLimit"`
	Encoding       string     `json:"Encoding"`
	// 过滤参数
	CaseSensitive  bool       `json:"CaseSensitive"`
	InvertMatch    bool       `json:"InvertMatch"`
	ContextBefore  int        `json:"ContextBefore"`
	ContextAfter   int        `json:"ContextAfter"`
	UseRegex       bool       `json:"UseRegex"`
	FilterRule     FilterRule `json:"FilterRule"`
	HighlightRules []string   `json:"HighlightRules"`
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

// ConfigStore 可序列化的配置仓库（落在 logviewer.json 的 hosts.<alias>.configs 下，
// 历史上也存于独立的 configs.json）。
type ConfigStore struct {
	DefaultName string               `json:"default_name"`
	Configs     map[string]LogConfig `json:"configs"`
}

// NewConfigStore 返回一个含默认配置的空仓库。
func NewConfigStore() ConfigStore {
	return ConfigStore{
		DefaultName: DefaultConfigName,
		Configs:     map[string]LogConfig{DefaultConfigName: DefaultConfig()},
	}
}

// SaveFunc 由 Manager 在配置变更时回调，把仓库持久化到外部存储
// （appconfig 写回 logviewer.json）。可为 nil，表示不持久化（纯内存）。
type SaveFunc func(ConfigStore) error

// Manager 负责配置的 CRUD 与持久化。它本身不关心存储位置，所有落盘动作通过 SaveFunc 完成。
type Manager struct {
	mu   sync.Mutex
	data *ConfigStore
	save SaveFunc
}

// NewManager 用给定的初始仓库构造 Manager。save 为 nil 时变更只留在内存。
// 返回的 Manager 会保证 data.Configs 非 nil 且默认配置存在。
func NewManager(initial ConfigStore, save SaveFunc) *Manager {
	data := initial
	if data.Configs == nil {
		data.Configs = map[string]LogConfig{}
	}
	if data.DefaultName == "" {
		data.DefaultName = DefaultConfigName
	}
	if _, ok := data.Configs[data.DefaultName]; !ok {
		data.Configs[data.DefaultName] = DefaultConfig()
	}
	return &Manager{data: &data, save: save}
}

// Snapshot 返回当前仓库的深拷贝（供 appconfig 持久化整个 AppConfig 时使用）。
func (m *Manager) Snapshot() ConfigStore {
	m.mu.Lock()
	defer m.mu.Unlock()
	return cloneStore(m.data)
}

func cloneStore(s *ConfigStore) ConfigStore {
	out := ConfigStore{
		DefaultName: s.DefaultName,
		Configs:     make(map[string]LogConfig, len(s.Configs)),
	}
	for k, v := range s.Configs {
		if v.FilterRule.Levels != nil {
			v.FilterRule.Levels = append([]string(nil), v.FilterRule.Levels...)
		}
		if v.HighlightRules != nil {
			v.HighlightRules = append([]string(nil), v.HighlightRules...)
		}
		out.Configs[k] = v
	}
	return out
}

// persist 在锁内调用 save 钩子。
func (m *Manager) persist() error {
	if m.save == nil {
		return nil
	}
	return m.save(cloneStore(m.data))
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
	if c.ConfigName == "" {
		m.mu.Unlock()
		return fmt.Errorf("配置名不能为空")
	}
	m.data.Configs[c.ConfigName] = c
	err := m.persist()
	m.mu.Unlock()
	return err
}

// Delete 删除配置（默认配置不可删除）
func (m *Manager) Delete(name string) error {
	m.mu.Lock()
	if name == m.data.DefaultName {
		m.mu.Unlock()
		return fmt.Errorf("默认配置不可删除")
	}
	if _, ok := m.data.Configs[name]; !ok {
		m.mu.Unlock()
		return fmt.Errorf("配置 %q 不存在", name)
	}
	delete(m.data.Configs, name)
	err := m.persist()
	m.mu.Unlock()
	return err
}

// SetDefault 设为默认配置
func (m *Manager) SetDefault(name string) error {
	m.mu.Lock()
	if _, ok := m.data.Configs[name]; !ok {
		m.mu.Unlock()
		return fmt.Errorf("配置 %q 不存在", name)
	}
	m.data.DefaultName = name
	err := m.persist()
	m.mu.Unlock()
	return err
}

// Rename 重命名配置
func (m *Manager) Rename(oldName, newName string) error {
	m.mu.Lock()
	c, ok := m.data.Configs[oldName]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("配置 %q 不存在", oldName)
	}
	if _, exists := m.data.Configs[newName]; exists {
		m.mu.Unlock()
		return fmt.Errorf("配置 %q 已存在", newName)
	}
	c.ConfigName = newName
	delete(m.data.Configs, oldName)
	m.data.Configs[newName] = c
	if m.data.DefaultName == oldName {
		m.data.DefaultName = newName
	}
	err := m.persist()
	m.mu.Unlock()
	return err
}

// MarshalJSON 保证序列化时 Configs 非 nil。
func (s ConfigStore) MarshalJSON() ([]byte, error) {
	type alias ConfigStore
	if s.Configs == nil {
		s.Configs = map[string]LogConfig{}
	}
	return json.Marshal(alias(s))
}
