package config

import "testing"

func TestLogConfigValidate(t *testing.T) {
	good := DefaultConfig()
	if err := good.Validate(); err != nil {
		t.Fatalf("default config should be valid: %v", err)
	}

	// 0 = 不限读全量，合法
	full := good
	full.ReadLinesLimit = 0
	if err := full.Validate(); err != nil {
		t.Fatalf("ReadLinesLimit=0 (不限) should be valid: %v", err)
	}

	cases := []struct {
		name string
		mut  func(*LogConfig)
	}{
		{"negative limit", func(c *LogConfig) { c.ReadLinesLimit = -1 }},
		{"huge limit", func(c *LogConfig) { c.ReadLinesLimit = MaxReadLines + 1 }},
		{"negative context before", func(c *LogConfig) { c.ContextBefore = -1 }},
		{"negative context after", func(c *LogConfig) { c.ContextAfter = -1 }},
		{"huge context", func(c *LogConfig) { c.ContextBefore = MaxContext + 1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := good
			tc.mut(&c)
			if err := c.Validate(); err == nil {
				t.Errorf("expected error for %s, got nil", tc.name)
			}
		})
	}
}
