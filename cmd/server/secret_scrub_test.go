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
	scrubSecretSentinels(incoming)
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
	scrubSecretSentinels(incoming)
	if incoming["api_key"] != "sk-real-key-1234567890" {
		t.Errorf("真实密钥被误删/改写: %v", incoming["api_key"])
	}
}
