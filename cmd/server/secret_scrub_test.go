package main

import (
	"testing"

	"github.com/RisingInsight/startport/internal/panel"
)

// 面板提交里的脱敏哨兵不得覆盖真实密钥（F-004 保存侧）。
func TestScrubSecretSentinels(t *testing.T) {
	incoming := map[string]any{
		"api_key":  panel.SecretSentinel,
		"listen":   "0.0.0.0:7863",
		"upstream": map[string]any{"device_token": panel.SecretSentinel, "user_agent": "UA"},
		"upstash":  map[string]any{"token": panel.SecretSentinel, "url": "https://x"},
	}
	scrubSecretSentinels(incoming, map[string]any{})
	if _, ok := incoming["api_key"]; ok {
		t.Error("api_key 哨兵应被删除（深合并即保留原值）")
	}
	if incoming["listen"] != "0.0.0.0:7863" {
		t.Errorf("非敏感键应保留, got %v", incoming["listen"])
	}
	up := incoming["upstream"].(map[string]any)
	if _, ok := up["device_token"]; ok {
		t.Error("device_token 哨兵应被删除")
	}
	if up["user_agent"] != "UA" {
		t.Errorf("非敏感子键应保留, got %v", up["user_agent"])
	}
	us := incoming["upstash"].(map[string]any)
	if _, ok := us["token"]; ok {
		t.Error("upstash.token 哨兵应被删除")
	}
	if us["url"] != "https://x" {
		t.Errorf("非敏感子键应保留, got %v", us["url"])
	}
}

// 真实（非哨兵）密钥必须原样保留，不被误删。
func TestScrubKeepsRealSecrets(t *testing.T) {
	incoming := map[string]any{"api_key": "sk-real-key-1234567890"}
	scrubSecretSentinels(incoming, map[string]any{})
	if incoming["api_key"] != "sk-real-key-1234567890" {
		t.Errorf("真实密钥被误删/改写: %v", incoming["api_key"])
	}
}

// 外部渠道哨兵回填：同名渠道从磁盘配置找回原 api_key（数组整体替换，
// 不回填会把哨兵字面量写进 config.json）。
func TestScrubExtChannelSentinelRefill(t *testing.T) {
	incoming := map[string]any{
		"ext_channels": []any{
			map[string]any{"name": "siliconflow", "base_url": "https://api.siliconflow.cn", "api_key": panel.SecretSentinel},
			map[string]any{"name": "newchan", "base_url": "https://api.example.com", "api_key": panel.SecretSentinel},
		},
	}
	cur := map[string]any{
		"ext_channels": []any{
			map[string]any{"name": "siliconflow", "api_key": "sk-real-sf-key-0001"},
		},
	}
	scrubSecretSentinels(incoming, cur)
	ecs := incoming["ext_channels"].([]any)
	sf := ecs[0].(map[string]any)
	if sf["api_key"] != "sk-real-sf-key-0001" {
		t.Errorf("同名渠道哨兵应回填磁盘原值, got %v", sf["api_key"])
	}
	nc := ecs[1].(map[string]any)
	if _, ok := nc["api_key"]; ok {
		t.Error("无同名原渠道（新渠道）的哨兵应被删除而非写成字面量")
	}
}

// 真实（非哨兵）外部渠道密钥必须原样保留。
func TestScrubExtChannelKeepsRealKeys(t *testing.T) {
	incoming := map[string]any{
		"ext_channels": []any{
			map[string]any{"name": "a", "api_key": "sk-keep-me-12345"},
		},
	}
	scrubSecretSentinels(incoming, map[string]any{})
	ec := incoming["ext_channels"].([]any)[0].(map[string]any)
	if ec["api_key"] != "sk-keep-me-12345" {
		t.Errorf("真实渠道密钥被误删/改写: %v", ec["api_key"])
	}
}

// 凭证级哨兵回填：按 (渠道名, label) 从磁盘回填；新凭证的哨兵必须删除。
func TestScrubExtCredentialSentinelRefill(t *testing.T) {
	incoming := map[string]any{
		"ext_channels": []any{
			map[string]any{
				"name": "a", "base_url": "https://a",
				"credentials": []any{
					map[string]any{"label": "primary", "secret": panel.SecretSentinel},
					map[string]any{"label": "brand-new", "secret": panel.SecretSentinel},
				},
			},
		},
	}
	cur := map[string]any{
		"ext_channels": []any{
			map[string]any{
				"name": "a",
				"credentials": []any{
					map[string]any{"label": "primary", "secret": "sk-real-primary"},
				},
			},
		},
	}
	scrubSecretSentinels(incoming, cur)
	creds := incoming["ext_channels"].([]any)[0].(map[string]any)["credentials"].([]any)
	primary := creds[0].(map[string]any)
	if primary["secret"] != "sk-real-primary" {
		t.Errorf("已保存凭证的哨兵应回填磁盘原值, got %v", primary["secret"])
	}
	if _, ok := creds[1].(map[string]any)["secret"]; ok {
		t.Error("新凭证误带哨兵时应删除 secret（绝不把哨兵写盘）")
	}
}

// 老配置兜底：磁盘上只有 api_key 时，提交的 credentials[{label:"default"}] 从
// api_key 回填（面板把老渠道改写成凭证形态的场景）。
func TestScrubExtCredentialLegacyAPIKeyFallback(t *testing.T) {
	incoming := map[string]any{
		"ext_channels": []any{
			map[string]any{
				"name": "legacy", "base_url": "https://legacy",
				"credentials": []any{
					map[string]any{"label": "default", "secret": panel.SecretSentinel},
				},
			},
		},
	}
	cur := map[string]any{
		"ext_channels": []any{
			map[string]any{"name": "legacy", "api_key": "sk-legacy-key"},
		},
	}
	scrubSecretSentinels(incoming, cur)
	creds := incoming["ext_channels"].([]any)[0].(map[string]any)["credentials"].([]any)
	if got := creds[0].(map[string]any)["secret"]; got != "sk-legacy-key" {
		t.Errorf("老 api_key 应兜底回填 default 凭证, got %v", got)
	}
}

// 老配置渠道原样回传（只有 api_key 哨兵）：按名回填，不丢鉴权。
func TestScrubExtChannelLegacyRoundTrip(t *testing.T) {
	incoming := map[string]any{
		"ext_channels": []any{
			map[string]any{"name": "legacy", "base_url": "https://legacy", "api_key": panel.SecretSentinel},
		},
	}
	cur := map[string]any{
		"ext_channels": []any{
			map[string]any{"name": "legacy", "api_key": "sk-legacy-key"},
		},
	}
	scrubSecretSentinels(incoming, cur)
	ec := incoming["ext_channels"].([]any)[0].(map[string]any)
	if ec["api_key"] != "sk-legacy-key" {
		t.Errorf("老配置渠道回传应保留 api_key, got %v", ec["api_key"])
	}
}
