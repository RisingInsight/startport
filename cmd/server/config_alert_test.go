package main

import (
	"os"
	"strings"
	"testing"
)

// validateAlert 静态校验红线。
func TestValidateAlert(t *testing.T) {
	base := func() *Config {
		c := &Config{}
		c.Alert.WebhookType = "feishu"
		return c
	}
	// 缺省（关闭 + 无 URL）：合法。
	if err := base().validateAlert(); err != nil {
		t.Fatalf("default alert should pass: %v", err)
	}
	// 开启但没 URL：拒绝。
	c := base()
	c.Alert.Enabled = true
	if err := c.validateAlert(); err == nil || !strings.Contains(err.Error(), "webhook_url") {
		t.Fatalf("enabled without url err = %v", err)
	}
	// URL + 非法类型：拒绝。
	c = base()
	c.Alert.WebhookURL = "https://hook.example/x"
	c.Alert.WebhookType = "slack"
	if err := c.validateAlert(); err == nil || !strings.Contains(err.Error(), "webhook_type") {
		t.Fatalf("bad type err = %v", err)
	}
	// URL 非 http(s)：拒绝。
	c = base()
	c.Alert.WebhookURL = "ftp://hook.example/x"
	if err := c.validateAlert(); err == nil || !strings.Contains(err.Error(), "http(s)") {
		t.Fatalf("non-http url err = %v", err)
	}
	// 全量合法。
	c = base()
	c.Alert.Enabled = true
	c.Alert.WebhookURL = "https://hook.example/x"
	c.Alert.CooldownMinutes = 15
	if err := c.validateAlert(); err != nil {
		t.Fatalf("valid alert rejected: %v", err)
	}
}

// 哨兵回填：GET 脱敏后的 webhook_url 哨兵提交回来，磁盘原值保留、enabled 照常更新。
func TestAlertWebhookSentinelRoundTrip(t *testing.T) {
	e := newAcctTestEnv(t)
	base := `{
	  "listen": "127.0.0.1:0",
	  "alert": {"enabled": false, "webhook_type": "feishu", "webhook_url": "https://qyapi.real/hook-token", "cooldown_minutes": 30}
	}`
	if err := os.WriteFile(e.path, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	// 面板保存：enabled 翻 true + webhook_url 只带回哨兵。
	sentinel := `{"alert":{"enabled":true,"webhook_url":"__STARPORT_SECRET_SET__"}}`
	if _, err := e.save([]byte(sentinel)); err != nil {
		t.Fatalf("save: %v", err)
	}
	cfg, err := Load(e.path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Alert.Enabled {
		t.Error("enabled should be updated to true")
	}
	if cfg.Alert.WebhookURL != "https://qyapi.real/hook-token" {
		t.Errorf("webhook_url = %q, want disk original preserved", cfg.Alert.WebhookURL)
	}
}
