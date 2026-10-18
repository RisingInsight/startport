// config_adapter_test.go 原生适配器（adapter 字段）的配置校验与映射。
package main

import (
	"strings"
	"testing"

	"github.com/RisingInsight/startport/internal/provider"
)

// 合法配置：adapter=loomy + protocol=openai + 无 auth → 通过，且映射到运行时定义。
func TestExtChannelAdapterLoomyValid(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{
	  "ext_channels": [{
	    "name": "loomy",
	    "base_url": "https://loomyad.xunfei.cn/api/v1",
	    "protocol": "openai",
	    "adapter": "loomy",
	    "credentials": [{"label": "primary", "secret": "sess-abc"}]
	  }]
	}`))
	if err != nil {
		t.Fatalf("合法配置应解析成功: %v", err)
	}
	if got := cfg.ExtChannels[0].Adapter; got != provider.LoomyAdapterName {
		t.Errorf("Adapter = %q", got)
	}
	defs := extChannelDefs(cfg.ExtChannels)
	if len(defs) != 1 || defs[0].Adapter != provider.LoomyAdapterName {
		t.Fatalf("ChannelDef.Adapter 未映射: %+v", defs)
	}
}

// protocol 留空也是合法的（Loomy 是 openai 形态，空 = openai）。
func TestExtChannelAdapterLoomyEmptyProtocol(t *testing.T) {
	if _, err := ParseConfig([]byte(`{
	  "ext_channels": [{
	    "name": "loomy", "base_url": "https://loomyad.xunfei.cn/api/v1",
	    "adapter": "loomy",
	    "credentials": [{"label": "primary", "secret": "sess-abc"}]
	  }]
	}`)); err != nil {
		t.Fatalf("protocol 留空应合法: %v", err)
	}
}

// adapter 名归一：写 "Loomy" / " LOOMY " 都能用。
func TestExtChannelAdapterNameNormalized(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{
	  "ext_channels": [{
	    "name": "loomy", "base_url": "https://loomyad.xunfei.cn/api/v1",
	    "adapter": "  LOOMY ",
	    "credentials": [{"label": "primary", "secret": "sess-abc"}]
	  }]
	}`))
	if err != nil {
		t.Fatalf("大小写应不敏感: %v", err)
	}
	if got := cfg.ExtChannels[0].Adapter; got != "loomy" {
		t.Errorf("Adapter 应归一为 loomy, got %q", got)
	}
}

// 未注册的 adapter：明确报错并点名（不能静默当成协议适配器）。
func TestExtChannelAdapterUnknown(t *testing.T) {
	_, err := ParseConfig([]byte(`{
	  "ext_channels": [{
	    "name": "x", "base_url": "https://example.com", "adapter": "nope",
	    "credentials": [{"label": "primary", "secret": "sk-abcdef"}]
	  }]
	}`))
	if err == nil {
		t.Fatal("未知 adapter 应报错")
	}
	if !strings.Contains(err.Error(), "adapter") || !strings.Contains(err.Error(), "nope") {
		t.Errorf("错误应点名 adapter 字段与取值: %v", err)
	}
	if !strings.Contains(err.Error(), provider.LoomyAdapterName) {
		t.Errorf("错误应给出可用清单: %v", err)
	}
}

// adapter 与 protocol 冲突（loomy 只能是 openai）：报错。
func TestExtChannelAdapterProtocolMismatch(t *testing.T) {
	_, err := ParseConfig([]byte(`{
	  "ext_channels": [{
	    "name": "x", "base_url": "https://loomyad.xunfei.cn/api/v1",
	    "protocol": "anthropic", "adapter": "loomy",
	    "credentials": [{"label": "primary", "secret": "sess-abc"}]
	  }]
	}`))
	if err == nil {
		t.Fatal("协议冲突应报错")
	}
	if !strings.Contains(err.Error(), "openai") {
		t.Errorf("错误应说明支持的协议: %v", err)
	}
}

// adapter + 渠道级 auth 并存：报错（适配器独占鉴权）。
func TestExtChannelAdapterRejectsAuth(t *testing.T) {
	_, err := ParseConfig([]byte(`{
	  "ext_channels": [{
	    "name": "x", "base_url": "https://loomyad.xunfei.cn/api/v1",
	    "adapter": "loomy", "auth": {"mode": "bearer"},
	    "credentials": [{"label": "primary", "secret": "sess-abc"}]
	  }]
	}`))
	if err == nil {
		t.Fatal("adapter + auth 应报错")
	}
	if !strings.Contains(err.Error(), "auth") {
		t.Errorf("错误应点名 auth 字段: %v", err)
	}
}

// adapter + 凭证级 auth 并存：同样报错（凭证级会覆盖，冲突更隐蔽）。
func TestExtChannelAdapterRejectsCredentialAuth(t *testing.T) {
	_, err := ParseConfig([]byte(`{
	  "ext_channels": [{
	    "name": "x", "base_url": "https://loomyad.xunfei.cn/api/v1",
	    "adapter": "loomy",
	    "credentials": [{"label": "primary", "secret": "sess-abc",
	                     "auth": {"mode": "x-api-key"}}]
	  }]
	}`))
	if err == nil {
		t.Fatal("adapter + 凭证级 auth 应报错")
	}
	if !strings.Contains(err.Error(), "credentials[0].auth") {
		t.Errorf("错误应点名凭证级 auth: %v", err)
	}
}

// AutoClaw 两地区：合法配置 + 与 loomy 同一套校验规则（协议 openai、禁 auth）。
func TestExtChannelAdapterAutoClaw(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{
	  "ext_channels": [{
	    "name": "autoclaw",
	    "base_url": "https://autoglm-acceleration-api.zhipuai.cn/autoclaw-proxy/proxy/autoclaw",
	    "protocol": "openai",
	    "adapter": "autoclaw",
	    "models": ["glm-5.3", "glm-5.3-flash"],
	    "credentials": [{"label": "primary", "secret": "ac-token"}]
	  }]
	}`))
	if err != nil {
		t.Fatalf("autoclaw 配置应合法: %v", err)
	}
	defs := extChannelDefs(cfg.ExtChannels)
	if len(defs) != 1 || defs[0].Adapter != provider.AutoClawAdapterName {
		t.Fatalf("ChannelDef.Adapter 未映射: %+v", defs)
	}
	// 国际版。
	cfg, err = ParseConfig([]byte(`{
	  "ext_channels": [{
	    "name": "autoclaw-intl",
	    "base_url": "https://autoglm-api.autoglm.ai/autoclaw-proxy/proxy/autoclaw",
	    "adapter": "autoclaw-intl",
	    "credentials": [{"label": "primary", "secret": "ac-token"}]
	  }]
	}`))
	if err != nil {
		t.Fatalf("autoclaw-intl 配置应合法: %v", err)
	}
	if got := cfg.ExtChannels[0].Adapter; got != provider.AutoClawIntlAdapterName {
		t.Errorf("Adapter = %q", got)
	}
	// 与 loomy 同规则：adapter + auth 硬错误。
	if _, err := ParseConfig([]byte(`{
	  "ext_channels": [{
	    "name": "x", "base_url": "https://example.com", "adapter": "autoclaw",
	    "auth": {"mode": "custom", "header": "X-Authorization"},
	    "credentials": [{"label": "p", "secret": "ac-token"}]
	  }]
	}`)); err == nil {
		t.Fatal("autoclaw + auth 应报错（认证头由适配器独占）")
	}
}

// 未声明 adapter 的普通渠道不受影响（零回归）。
func TestExtChannelNoAdapterStillValid(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{
	  "ext_channels": [{
	    "name": "sf", "base_url": "https://api.siliconflow.cn",
	    "auth": {"mode": "bearer"}, "api_key": "sk-old"
	  }]
	}`))
	if err != nil {
		t.Fatalf("普通渠道应仍然合法: %v", err)
	}
	if got := cfg.ExtChannels[0].Adapter; got != "" {
		t.Errorf("Adapter 应为空, got %q", got)
	}
}
