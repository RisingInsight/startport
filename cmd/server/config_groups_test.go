package main

import "testing"

// validateModelGroups 静态校验：fail fast 的每条红线各一例，加一条全绿的合法形态。
func TestValidateModelGroups(t *testing.T) {
	ok := func(c *Config) bool {
		return c.validateModelGroups() == nil
	}
	base := func() *Config {
		c := &Config{}
		c.ExtChannels = []ExtChannel{{Name: "qoder"}}
		c.ModelGroups = []ModelGroupDef{{
			Name:    "deepseek-any",
			Members: []string{"cn:deepseek-v4.1-flash", "ext:qoder/DeepSeek-V4-Pro"},
			Routing: "order",
		}}
		return c
	}

	if !ok(base()) {
		t.Fatal("valid groups rejected")
	}
	if !ok(func() *Config { c := base(); c.ModelGroups[0].Routing = ""; return c }()) {
		t.Fatal("empty routing should default to order")
	}

	cases := []struct {
		name string
		mut  func(c *Config)
	}{
		{"组名大写", func(c *Config) { c.ModelGroups[0].Name = "Bad" }},
		{"组名含斜杠", func(c *Config) { c.ModelGroups[0].Name = "a/b" }},
		{"组名重复", func(c *Config) {
			c.ModelGroups = append(c.ModelGroups, c.ModelGroups[0])
		}},
		{"routing 脏值", func(c *Config) { c.ModelGroups[0].Routing = "smart" }},
		{"无成员", func(c *Config) { c.ModelGroups[0].Members = nil }},
		{"成员重复", func(c *Config) {
			c.ModelGroups[0].Members = []string{"cn:a", "cn:a"}
		}},
		{"裸名成员", func(c *Config) { c.ModelGroups[0].Members = []string{"bare-model"} }},
		{"嵌套组", func(c *Config) { c.ModelGroups[0].Members = []string{"group/other"} }},
		{"realm 成员空模型", func(c *Config) { c.ModelGroups[0].Members = []string{"cn:"} }},
		{"ext 成员缺渠道", func(c *Config) { c.ModelGroups[0].Members = []string{"ext:ghost/m"} }},
		{"ext 成员缺模型", func(c *Config) { c.ModelGroups[0].Members = []string{"ext:qoder/"} }},
		{"ext 成员缺斜杠", func(c *Config) { c.ModelGroups[0].Members = []string{"ext:qoder"} }},
	}
	for _, tc := range cases {
		c := base()
		tc.mut(c)
		if err := c.validateModelGroups(); err == nil {
			t.Errorf("%s: expected error, got nil", tc.name)
		}
	}
}
