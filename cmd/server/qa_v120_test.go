// qa_v120_test.go QA 独立验收（v1.2.0 配置层）：原生适配器的互斥校验与映射。
package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/RisingInsight/startport/internal/extchan"
	"github.com/RisingInsight/startport/internal/panel"
	"github.com/RisingInsight/startport/internal/provider"
)

// 必测 2：adapter + 渠道级 auth → 400，错误点名 `.auth`（字段路径可照抄）。
func TestQAAdapterRejectsChannelAuthWithFieldPath(t *testing.T) {
	_, err := ParseConfig([]byte(`{
	  "ext_channels": [{
	    "name": "loomy", "base_url": "https://loomyad.xunfei.cn/api/v1",
	    "adapter": "loomy", "auth": {"mode": "bearer"},
	    "credentials": [{"label": "primary", "secret": "sess-abc"}]
	  }]
	}`))
	if err == nil {
		t.Fatal("adapter + auth 必须失败（fail fast）")
	}
	if !strings.Contains(err.Error(), "ext_channels[0].auth") {
		t.Errorf("错误应点名字段路径 ext_channels[0].auth: %v", err)
	}
}

// 必测 2：adapter + 凭证级 auth → 400，错误点名 credentials[j].auth。
func TestQAAdapterRejectsCredentialAuthWithFieldPath(t *testing.T) {
	_, err := ParseConfig([]byte(`{
	  "ext_channels": [{
	    "name": "loomy", "base_url": "https://loomyad.xunfei.cn/api/v1",
	    "adapter": "loomy",
	    "credentials": [{"label": "primary", "secret": "sess-abc",
	                     "auth": {"mode": "query", "param": "key"}}]
	  }]
	}`))
	if err == nil {
		t.Fatal("adapter + 凭证级 auth 必须失败")
	}
	if !strings.Contains(err.Error(), "ext_channels[0].credentials[0].auth") {
		t.Errorf("错误应点名字段路径: %v", err)
	}
}

// 必测 2：adapter=loomy + protocol=anthropic → 400（两套报文规则不能混）。
func TestQAAdapterProtocolMismatchRejected(t *testing.T) {
	_, err := ParseConfig([]byte(`{
	  "ext_channels": [{
	    "name": "loomy", "base_url": "https://loomyad.xunfei.cn/api/v1",
	    "adapter": "loomy", "protocol": "anthropic",
	    "credentials": [{"label": "primary", "secret": "sess-abc"}]
	  }]
	}`))
	if err == nil {
		t.Fatal("协议冲突必须失败")
	}
	msg := err.Error()
	if !strings.Contains(msg, "adapter") || !strings.Contains(msg, "openai") {
		t.Errorf("错误应说明 adapter 只支持 openai: %v", err)
	}
}

// 必测 2：拼错 adapter（"lomy"）→ 错误列出可用值 loomy。
func TestQAAdapterTypoListsAvailable(t *testing.T) {
	_, err := ParseConfig([]byte(`{
	  "ext_channels": [{
	    "name": "x", "base_url": "https://example.com", "adapter": "lomy",
	    "credentials": [{"label": "p", "secret": "s3cret"}]
	  }]
	}`))
	if err == nil {
		t.Fatal("未知 adapter 必须失败")
	}
	msg := err.Error()
	if !strings.Contains(msg, "lomy") || !strings.Contains(msg, provider.LoomyAdapterName) {
		t.Errorf("错误应点名错误值并列出可用清单: %v", err)
	}
}

// 必测 2：未声明 adapter 的老配置零回归，且运行期定义 Adapter 为空。
func TestQAAdapterEmptyKeepsLegacy(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{
	  "ext_channels": [{
	    "name": "sf", "base_url": "https://api.siliconflow.cn", "api_key": "sk-old",
	    "models": ["m1"]
	  }]
	}`))
	if err != nil {
		t.Fatalf("老配置应合法: %v", err)
	}
	if cfg.ExtChannels[0].Adapter != "" {
		t.Errorf("Adapter 应为空, got %q", cfg.ExtChannels[0].Adapter)
	}
	defs := extChannelDefs(cfg.ExtChannels)
	if defs[0].Adapter != "" || defs[0].Protocol != "" {
		t.Errorf("运行期定义不应被注入 adapter/protocol: %+v", defs[0])
	}
	// 运行态：正常渠道仍然用协议适配器。
	m := extchan.New(extchan.Config{})
	m.ReplaceAll(defs)
	if st := m.Status()[0]; st.Adapter != "" || st.Protocol != "openai" {
		t.Errorf("Status 异常: adapter=%q protocol=%q", st.Adapter, st.Protocol)
	}
}

// 归一写回：配置里写 "  LOOMY " 保存后 stdout/运行期都是 "loomy"。
func TestQAAdapterNameNormalizedOnParse(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{
	  "ext_channels": [{
	    "name": "loomy", "base_url": "https://loomyad.xunfei.cn/api/v1",
	    "adapter": "  LOOMY ",
	    "credentials": [{"label": "primary", "secret": "sess-abc"}]
	  }]
	}`))
	if err != nil {
		t.Fatalf("大小写/空白应不敏感: %v", err)
	}
	if cfg.ExtChannels[0].Adapter != provider.LoomyAdapterName {
		t.Errorf("Adapter 未归一: %q", cfg.ExtChannels[0].Adapter)
	}
	defs := extChannelDefs(cfg.ExtChannels)
	m := extchan.New(extchan.Config{})
	m.ReplaceAll(defs)
	if st := m.Status()[0]; st.Adapter != provider.LoomyAdapterName || st.Protocol != "openai" {
		t.Errorf("Status 异常: %+v", st)
	}
}

// 面板保存链路：哨兵回填不能丢 adapter 字段。
func TestQAAdapterSurvivesPanelSentinelRoundTrip(t *testing.T) {
	cur := map[string]any{}
	if err := json.Unmarshal([]byte(`{
	  "ext_channels": [{
	    "name": "loomy", "base_url": "https://loomyad.xunfei.cn/api/v1",
	    "protocol": "openai", "adapter": "loomy",
	    "credentials": [{"label": "primary", "secret": "sess-real"}]
	  }]
	}`), &cur); err != nil {
		t.Fatal(err)
	}
	incoming := map[string]any{}
	if err := json.Unmarshal([]byte(`{
	  "ext_channels": [{
	    "name": "loomy", "base_url": "https://loomyad.xunfei.cn/api/v1",
	    "protocol": "openai", "adapter": "loomy",
	    "credentials": [{"label": "primary", "secret": "`+panel.SecretSentinel+`"}]
	  }]
	}`), &incoming); err != nil {
		t.Fatal(err)
	}
	scrubSecretSentinels(incoming, cur)
	cfg, err := ParseConfig(mergedJSON(mergeConfigMaps(cur, incoming)))
	if err != nil {
		t.Fatalf("保存后配置应可解析: %v", err)
	}
	if cfg.ExtChannels[0].Adapter != provider.LoomyAdapterName {
		t.Errorf("adapter 在面板往返中丢失: %q", cfg.ExtChannels[0].Adapter)
	}
	if got := cfg.ExtChannels[0].Credentials[0].Secret; got != "sess-real" {
		t.Errorf("secret 回填异常: %q", got)
	}
}
