// qa_config_test.go QA 独立验收（配置/面板保存链路）：
//   - P0-11 哨兵回填：credentials[].secret 原样 POST 回存后仍是磁盘原值；
//   - 新凭证误带哨兵 → 删除该字段，绝不把哨兵写盘；
//   - P0-12 互斥报错必须同时点名 api_key 与 credentials。
package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/RisingInsight/startport/internal/panel"
)

// qaCurConfig 磁盘上的现状（两条渠道：新形态凭证 + 老形态 api_key）。
const qaCurConfig = `{
  "ext_channels": [
    {"name": "pool", "base_url": "https://api.example.com", "protocol": "openai",
     "models": ["m1"],
     "credentials": [
       {"label": "primary", "secret": "sk-real-primary", "expires_at": "2027-01-01"},
       {"label": "backup", "secret": "sk-real-backup"}
     ]},
    {"name": "legacy", "base_url": "https://api.example.com", "api_key": "sk-real-legacy",
     "models": ["m1"]}
  ]
}`

// P0-11：GET config 的哨兵原样 POST 回去，secret 必须保持不变。
func TestQACredentialSentinelRoundTripKeepsSecret(t *testing.T) {
	cur := map[string]any{}
	if err := json.Unmarshal([]byte(qaCurConfig), &cur); err != nil {
		t.Fatal(err)
	}
	// 向导提交体：已保存凭证只带哨兵（ewCollect 行为），label 不变。
	incoming := map[string]any{}
	if err := json.Unmarshal([]byte(`{
	  "ext_channels": [
	    {"name": "pool", "base_url": "https://api.example.com", "protocol": "openai",
	     "models": ["m1"],
	     "credentials": [
	       {"label": "primary", "secret": "`+panel.SecretSentinel+`", "expires_at": "2027-01-01"},
	       {"label": "backup", "secret": "`+panel.SecretSentinel+`"},
	       {"label": "third", "secret": "sk-brand-new"}
	     ]},
	    {"name": "legacy", "base_url": "https://api.example.com",
	     "models": ["m1"], "api_key": "`+panel.SecretSentinel+`"}
	  ]
	}`), &incoming); err != nil {
		t.Fatal(err)
	}
	scrubSecretSentinels(incoming, cur)
	cfg, err := ParseConfig(mergedJSON(mergeConfigMaps(cur, incoming)))
	if err != nil {
		t.Fatalf("保存后配置应可解析: %v", err)
	}
	creds := cfg.ExtChannels[0].Credentials
	if len(creds) != 3 {
		t.Fatalf("凭证数 = %d", len(creds))
	}
	want := map[string]string{
		"primary": "sk-real-primary",
		"backup":  "sk-real-backup",
		"third":   "sk-brand-new",
	}
	for _, c := range creds {
		if c.Secret != want[c.Label] {
			t.Errorf("凭证 %q secret = %q, want %q", c.Label, c.Secret, want[c.Label])
		}
		if c.Secret == panel.SecretSentinel {
			t.Errorf("凭证 %q 把哨兵写盘了", c.Label)
		}
	}
	// 老配置渠道：api_key 同样按渠道名回填。
	if got := cfg.ExtChannels[1].APIKey; got != "sk-real-legacy" {
		t.Errorf("api_key = %q, want sk-real-legacy", got)
	}
}

// 新渠道的新凭证误带哨兵：删除该字段，保存即报错（不能把哨兵当密钥写盘）。
func TestQANewCredentialSentinelDropped(t *testing.T) {
	cur := map[string]any{}
	if err := json.Unmarshal([]byte(qaCurConfig), &cur); err != nil {
		t.Fatal(err)
	}
	incoming := map[string]any{}
	if err := json.Unmarshal([]byte(`{
	  "ext_channels": [
	    {"name": "fresh", "base_url": "https://api.example.com",
	     "credentials": [{"label": "primary", "secret": "`+panel.SecretSentinel+`"}]}
	  ]
	}`), &incoming); err != nil {
		t.Fatal(err)
	}
	scrubSecretSentinels(incoming, cur)
	ecs := incoming["ext_channels"].([]any)
	cm := ecs[0].(map[string]any)["credentials"].([]any)[0].(map[string]any)
	if _, still := cm["secret"]; still {
		t.Errorf("新凭证的哨兵未被删除: %+v", cm)
	}
	_, err := ParseConfig(mergedJSON(mergeConfigMaps(cur, incoming)))
	if err == nil || !strings.Contains(err.Error(), "secret") {
		t.Errorf("新凭证缺 secret 应保存失败并指出字段, got %v", err)
	}
}

// P0-12：api_key 与 credentials 互斥 → 报错必须同时点名两个字段。
func TestQAMutexErrorNamesBothFields(t *testing.T) {
	_, err := ParseConfig([]byte(`{"ext_channels":[{"name":"a","base_url":"https://a",
	  "api_key":"k","credentials":[{"label":"p","secret":"s"}]}]}`))
	if err == nil {
		t.Fatal("同时配置 api_key 与 credentials 应报错")
	}
	msg := err.Error()
	for _, field := range []string{"api_key", "credentials"} {
		if !strings.Contains(msg, field) {
			t.Errorf("错误信息应点名 %q: %v", field, err)
		}
	}
	// 错误信息还要给出可操作建议（PRD：fail fast + 修复建议）。
	if !strings.Contains(msg, "互斥") {
		t.Errorf("错误信息应说明互斥关系: %v", err)
	}
}
