package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/RisingInsight/startport/internal/extchan"
	"github.com/RisingInsight/startport/internal/panel"
)

// 老配置（只有 api_key）零修改可用：解析成功且映射出 label=default 凭证。
func TestExtChannelLegacyAPIKeyCompatible(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{
	  "ext_channels": [
	    {"name": "sf", "base_url": "https://api.siliconflow.cn", "api_key": "sk-old",
	     "models": ["Qwen/Qwen3-235B-A22B"], "disabled": false}
	  ]
	}`))
	if err != nil {
		t.Fatalf("老配置应解析成功: %v", err)
	}
	if len(cfg.ExtChannels) != 1 {
		t.Fatalf("渠道数 = %d", len(cfg.ExtChannels))
	}
	defs := extChannelDefs(cfg.ExtChannels)
	if len(defs) != 1 {
		t.Fatalf("ChannelDef 数 = %d", len(defs))
	}
	creds := extchan.NormalizeCredentialDefs(defs[0])
	if len(creds) != 1 || creds[0].Label != "default" || creds[0].Secret != "sk-old" {
		t.Fatalf("api_key 应合成 default 凭证: %+v", creds)
	}
	if defs[0].Protocol != "" {
		t.Errorf("老配置 protocol 应留空（由运行态归一为 openai）: %q", defs[0].Protocol)
	}

	// 运行态：credentials_total=1 / state=available。
	m := extchan.New(extchan.Config{})
	m.ReplaceAll(defs)
	st := m.Status()
	if len(st) != 1 || st[0].CredentialsTotal != 1 || st[0].State != extchan.StateAvailable {
		t.Errorf("Status 异常: %+v", st)
	}
}

// 新 schema 全字段可解析并正确映射（含凭证级 auth、别名、path、headers）。
func TestExtChannelFullSchema(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{
	  "ext_channels": [{
	    "name": "gem", "base_url": "https://generativelanguage.googleapis.com",
	    "protocol": "gemini", "path": "/v1beta/models/{model}:generateContent",
	    "auth": {"mode": "custom", "header": "x-goog-api-key"},
	    "headers": {"X-Title": "StarPort"},
	    "models": ["gemini-2.0-flash"],
	    "aliases": {"fast": "gemini-2.0-flash-8b"},
	    "note": "来自情报：每月免费额度",
	    "timeout_seconds": 90,
	    "credentials": [
	      {"label": "primary", "secret": "AIza-1", "expires_at": "2027-01-01"},
	      {"label": "backup", "secret": "AIza-2", "disabled": true,
	       "auth": {"mode": "query", "param": "key"}}
	    ]
	  }]
	}`))
	if err != nil {
		t.Fatalf("全字段配置应解析成功: %v", err)
	}
	defs := extChannelDefs(cfg.ExtChannels)
	d := defs[0]
	if d.Protocol != "gemini" || d.Path == "" || d.Auth.Mode != "custom" || d.Auth.Header != "x-goog-api-key" {
		t.Errorf("协议四要素映射异常: %+v", d)
	}
	if d.Aliases["fast"] != "gemini-2.0-flash-8b" || d.Note == "" || d.TimeoutSeconds != 90 {
		t.Errorf("别名/备注/超时映射异常: %+v", d)
	}
	if len(d.Credentials) != 2 || d.Credentials[0].Label != "primary" ||
		d.Credentials[1].Auth.Mode != "query" {
		t.Errorf("凭证映射异常: %+v", d.Credentials)
	}
}

// 静态校验：每条失败都必须带字段路径（用户照抄即可定位）。
func TestExtChannelValidation(t *testing.T) {
	cases := []struct {
		name string
		json string
		want string
	}{
		{
			"api_key 与 credentials 互斥",
			`{"ext_channels":[{"name":"a","base_url":"https://a","api_key":"k",
			  "credentials":[{"label":"p","secret":"s"}]}]}`,
			"api_key",
		},
		{
			"渠道名非法",
			`{"ext_channels":[{"name":"Bad Name","base_url":"https://a"}]}`,
			"ext_channels[0].name",
		},
		{
			"渠道名重复",
			`{"ext_channels":[{"name":"a","base_url":"https://a"},{"name":"a","base_url":"https://b"}]}`,
			"渠道名重复",
		},
		{
			"protocol 非法",
			`{"ext_channels":[{"name":"a","base_url":"https://a","protocol":"grpc"}]}`,
			"ext_channels[0].protocol",
		},
		{
			"path 必须 / 开头",
			`{"ext_channels":[{"name":"a","base_url":"https://a","path":"chat"}]}`,
			"ext_channels[0].path",
		},
		{
			"path 不得是完整 URL",
			`{"ext_channels":[{"name":"a","base_url":"https://a","path":"https://evil/x"}]}`,
			"ext_channels[0].path",
		},
		{
			"path 不得含 ..",
			`{"ext_channels":[{"name":"a","base_url":"https://a","path":"/../../etc"}]}`,
			"ext_channels[0].path",
		},
		{
			"Host 头禁止覆盖",
			`{"ext_channels":[{"name":"a","base_url":"https://a","headers":{"Host":"evil.com"}}]}`,
			"ext_channels[0].headers.Host",
		},
		{
			"Authorization 静态头拒绝",
			`{"ext_channels":[{"name":"a","base_url":"https://a","headers":{"Authorization":"raw"}}]}`,
			"Authorization",
		},
		{
			"auth.mode 非法",
			`{"ext_channels":[{"name":"a","base_url":"https://a","auth":{"mode":"oauth"}}]}`,
			"ext_channels[0].auth",
		},
		{
			"custom 缺 header",
			`{"ext_channels":[{"name":"a","base_url":"https://a","auth":{"mode":"custom"}}]}`,
			"ext_channels[0].auth",
		},
		{
			"凭证 label 重复",
			`{"ext_channels":[{"name":"a","base_url":"https://a","credentials":[
			  {"label":"p","secret":"1"},{"label":"p","secret":"2"}]}]}`,
			"label",
		},
		{
			"凭证 secret 不能为空",
			`{"ext_channels":[{"name":"a","base_url":"https://a","credentials":[{"label":"p","secret":""}]}]}`,
			"secret",
		},
		{
			"expires_at 可解析",
			`{"ext_channels":[{"name":"a","base_url":"https://a","credentials":[
			  {"label":"p","secret":"s","expires_at":"不是日期"}]}]}`,
			"expires_at",
		},
		{
			"别名不能含 /",
			`{"ext_channels":[{"name":"a","base_url":"https://a","aliases":{"a/b":"m"}}]}`,
			"aliases",
		},
		{
			"别名不得与 models 重名",
			`{"ext_channels":[{"name":"a","base_url":"https://a","models":["m"],"aliases":{"m":"x"}}]}`,
			"aliases",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(tc.json))
			if err == nil {
				t.Fatalf("应校验失败: %s", tc.json)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息应含 %q, got: %v", tc.want, err)
			}
		})
	}
}

// 上限与边界：凭证数、渠道数、别名数、备注长度。
func TestExtChannelLimits(t *testing.T) {
	manyCreds := `{"ext_channels":[{"name":"a","base_url":"https://a","credentials":[`
	for i := 0; i < 21; i++ {
		if i > 0 {
			manyCreds += ","
		}
		manyCreds += `{"label":"c` + string(rune('a'+i)) + `","secret":"s"}`
	}
	manyCreds += `]}]}`
	if _, err := ParseConfig([]byte(manyCreds)); err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Errorf("凭证数超限应报错, got %v", err)
	}

	if _, err := ParseConfig([]byte(`{"ext_channels":[{"name":"a","base_url":"https://a","note":"` +
		strings.Repeat("x", 501) + `"}]}`)); err == nil {
		t.Error("备注过长应报错")
	}

	// 空 ext_channels 合法。
	if _, err := ParseConfig([]byte(`{"ext_channels":[]}`)); err != nil {
		t.Errorf("空渠道列表应合法: %v", err)
	}
}

// 无凭证渠道合法（自建内网 one-api 场景）。
func TestExtChannelNoCredentialsAllowed(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{"ext_channels":[{"name":"local","base_url":"http://127.0.0.1:3000"}]}`))
	if err != nil {
		t.Fatalf("无凭证渠道应合法: %v", err)
	}
	defs := extChannelDefs(cfg.ExtChannels)
	if got := extchan.NormalizeCredentialDefs(defs[0]); len(got) != 0 {
		t.Errorf("不应合成凭证: %+v", got)
	}
}

// 面板保存链路的联调断言（P0-12 回归重点）：老配置（只有 api_key）在向导里
// 不做任何凭证改动直接保存——前端只提交 api_key 哨兵，经 scrub 按渠道名回填后
// 必须保留真实 key，并仍合成 label=default 凭证。
func TestLegacyChannelPanelRoundTripKeepsAPIKey(t *testing.T) {
	cur := map[string]any{}
	if err := json.Unmarshal([]byte(`{
	  "ext_channels": [
	    {"name": "legacy", "base_url": "https://api.siliconflow.cn", "api_key": "sk-real-legacy",
	     "models": ["m1"], "disabled": false}
	  ]
	}`), &cur); err != nil {
		t.Fatal(err)
	}
	// 向导提交体：无 credentials，api_key 为哨兵（EXTWIZ 的 ewSave 行为）。
	incoming := map[string]any{}
	if err := json.Unmarshal([]byte(`{
	  "ext_channels": [
	    {"name": "legacy", "base_url": "https://api.siliconflow.cn", "protocol": "openai",
	     "models": ["m1"], "timeout_seconds": 0, "api_key": "`+panel.SecretSentinel+`"}
	  ]
	}`), &incoming); err != nil {
		t.Fatal(err)
	}
	scrubSecretSentinels(incoming, cur)
	cfg, err := ParseConfig(mergedJSON(mergeConfigMaps(cur, incoming)))
	if err != nil {
		t.Fatalf("保存后配置应可解析: %v", err)
	}
	if got := cfg.ExtChannels[0].APIKey; got != "sk-real-legacy" {
		t.Errorf("老配置保存后 api_key 丢失/被改写: %q", got)
	}
	defs := extChannelDefs(cfg.ExtChannels)
	creds := extchan.NormalizeCredentialDefs(defs[0])
	if len(creds) != 1 || creds[0].Label != "default" || creds[0].Secret != "sk-real-legacy" {
		t.Errorf("老配置应仍合成 default 凭证: %+v", creds)
	}
	if cfg.ExtChannels[0].Credentials != nil {
		t.Error("老配置回传不应凭空产生 credentials（否则会与 api_key 互斥）")
	}
}
