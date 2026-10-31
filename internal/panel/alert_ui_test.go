package panel

import (
	"os"
	"strings"
	"testing"
)

// 告警 UI 契约：配置表单四字段进 CFG_MAP（load/collect 自动读写 alert 节）、
// 「发送测试」按钮走 alert/test 端点、index.html 有输入件。
func TestAlertUIContract(t *testing.T) {
	appJS, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"alert_enabled: ['alert', 'enabled']",
		"alert_webhook_type: ['alert', 'webhook_type']",
		"alert_webhook_url: ['alert', 'webhook_url']",
		"alert_cooldown_minutes: ['alert', 'cooldown_minutes']",
		"alert/test",
	} {
		if !strings.Contains(string(appJS), want) {
			t.Errorf("app.js 缺告警接线 %q", want)
		}
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`name="alert_enabled"`, `name="alert_webhook_type"`,
		`name="alert_webhook_url"`, `name="alert_cooldown_minutes"`,
		`id="btnAlertTest"`,
	} {
		if !strings.Contains(string(html), want) {
			t.Errorf("index.html 缺告警输入件 %q", want)
		}
	}
}
