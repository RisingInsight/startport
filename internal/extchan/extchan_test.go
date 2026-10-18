package extchan

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestManager() *Manager {
	return New(Config{TimeoutSeconds: 5})
}

// HasPrefix 前缀协议解析：合法/非法/边界形态。
func TestHasPrefix(t *testing.T) {
	m := newTestManager()
	m.ReplaceAll([]ChannelDef{{Name: "sf", BaseURL: "https://x"}})

	for _, ok := range []string{"ext:sf/deepseek-v4", "ext:sf/gpt-4o"} {
		ch, bare, found := m.HasPrefix(ok)
		if !found || ch != "sf" || strings.Contains(bare, "/") {
			t.Errorf("%q 应解析为 sf 渠道, got ch=%q bare=%q found=%v", ok, ch, bare, found)
		}
	}
	for _, bad := range []string{"cn:glm-5.2", "global:abc", "ext:sf", "ext:/x", "ext:sf/", "extchan/m", "ext"} {
		if _, _, found := m.HasPrefix(bad); found {
			t.Errorf("%q 不应被识别为 ext 前缀", bad)
		}
	}
	// 未注册渠道：前缀成立但 Get 返回 nil → RouteFor 报错。
	if _, _, err := m.RouteFor("ext:ghost/m"); err == nil {
		t.Error("未注册渠道应报错")
	}
}

// 裸名唯一匹配路由 + 多渠道同名拒路由（要求前缀消歧）。
func TestRouteForBareName(t *testing.T) {
	m := newTestManager()
	m.ReplaceAll([]ChannelDef{
		{Name: "a", BaseURL: "https://a"},
		{Name: "b", BaseURL: "https://b"},
	})
	m.channels[0].mu.Lock()
	m.channels[0].modelsCache = []string{"m1", "shared"}
	m.channels[0].mu.Unlock()
	m.channels[1].mu.Lock()
	m.channels[1].modelsCache = []string{"m2", "shared"}
	m.channels[1].mu.Unlock()

	ch, bare, err := m.RouteFor("m1")
	if err != nil || ch.Name != "a" || bare != "m1" {
		t.Errorf("裸名唯一匹配应路由 a: ch=%v bare=%v err=%v", ch, bare, err)
	}
	if _, _, err := m.RouteFor("shared"); err == nil {
		t.Error("多渠道同名模型应拒绝路由（要求前缀消歧）")
	}
	if _, _, err := m.RouteFor("nope"); err != errNotExt {
		t.Errorf("未知裸名应返回 errNotExt, got %v", err)
	}
	// 前缀优先于裸名歧义。
	ch, bare, err = m.RouteFor("ext:b/shared")
	if err != nil || ch.Name != "b" || bare != "shared" {
		t.Errorf("前缀应消歧: ch=%v bare=%v err=%v", ch, bare, err)
	}
}

// 冷却状态机：429 指数退避 + 封顶；成功清零。
func TestCooldownState(t *testing.T) {
	c := &Channel{Name: "t"}
	c.NoteRateLimited(time.Second, 8*time.Second, "429")
	if !c.Cooldown() {
		t.Error("429 后应冷却")
	}
	for i := 0; i < 6; i++ {
		c.NoteRateLimited(time.Second, 8*time.Second, "429")
	}
	c.mu.Lock()
	d := time.Until(c.coolUntil)
	c.mu.Unlock()
	if d > 9*time.Second {
		t.Errorf("冷却应封顶 8s, got %v", d)
	}
	c.NoteSuccess()
	if c.Cooldown() {
		t.Error("成功后冷却应清零")
	}
}

// 熔断：连败达阈值触发长冷却。
func TestBreaker(t *testing.T) {
	c := &Channel{Name: "t"}
	for i := 0; i < 4; i++ {
		c.NoteFailure(5, time.Minute, "transport fail")
	}
	if c.Cooldown() {
		t.Error("未达阈值不应熔断")
	}
	c.NoteFailure(5, time.Minute, "transport fail")
	if !c.Cooldown() {
		t.Error("达阈值应熔断")
	}
	c.NoteSuccess()
	if c.Cooldown() {
		t.Error("成功应解除熔断")
	}
}

// 端到端：假上游 chat（流式与非流式）+ 模型发现 + 429 治理。
func TestChatAndModelsEndToEnd(t *testing.T) {
	var seenAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
			var body struct {
				Model  string `json:"model"`
				Stream bool   `json:"stream"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Model != "inner-model" {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":{"message":"bad model"}}`))
				return
			}
			if body.Stream {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
					"data: {\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n\ndata: [DONE]\n\n"))
				return
			}
			_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`))
		case strings.HasSuffix(r.URL.Path, "/v1/models"):
			_, _ = w.Write([]byte(`{"data":[{"id":"inner-model"},{"id":"m2"}]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	m := newTestManager()
	m.ReplaceAll([]ChannelDef{{Name: "sf", BaseURL: srv.URL, APIKey: "sk-test"}})
	ch := m.Get("sf")

	// 非流式。
	_, status, body, err := m.Chat(context.Background(), ch,
		[]byte(`{"model":"inner-model","stream":false}`), false)
	if err != nil || status != 200 {
		t.Fatalf("non-stream chat: status=%d err=%v", status, err)
	}
	if !strings.Contains(string(body), `"content":"ok"`) {
		t.Errorf("非流式 body 不含回复: %s", body)
	}
	if seenAuth != "Bearer sk-test" {
		t.Errorf("Authorization 头缺失/错误: %q", seenAuth)
	}

	// 流式（首帧前超时路径覆盖：200 开流 → rc 可读）。
	rc, status, _, err := m.Chat(context.Background(), ch,
		[]byte(`{"model":"inner-model","stream":true}`), true)
	if err != nil || status != 200 {
		t.Fatalf("stream chat: status=%d err=%v", status, err)
	}
	buf := make([]byte, 4096)
	n, _ := rc.Read(buf)
	if !strings.Contains(string(buf[:n]), "data:") {
		t.Errorf("流首帧应为 SSE data: %q", string(buf[:n]))
	}
	rc.Close()

	// 模型自动发现。
	ids := m.EnsureModels(context.Background(), ch)
	if len(ids) != 2 || ids[0] != "inner-model" {
		t.Errorf("模型发现结果异常: %v", ids)
	}

	// 429 治理：第二次进入短冷却。
	m.noteFailureForTest(ch, http.StatusTooManyRequests)
	if !ch.Cooldown() {
		t.Error("429 后应冷却")
	}
}

func (m *Manager) noteFailureForTest(c *Channel, status int) {
	// 复用 relay 侧同一分类入口的语义（429 → NoteRateLimited）。
	c.NoteRateLimited(m.base, m.max, "429 (test)")
}

// endpoint URL 规范化。
func TestEndpoint(t *testing.T) {
	cases := map[string]string{
		"https://api.x.cn":         "https://api.x.cn/v1/chat/completions",
		"https://api.x.cn/":        "https://api.x.cn/v1/chat/completions",
		"https://api.x.cn/v1":      "https://api.x.cn/v1/chat/completions",
		"https://api.x.cn/v1/":     "https://api.x.cn/v1/chat/completions",
		"https://api.x.cn/openai/v1": "https://api.x.cn/openai/v1/chat/completions",
	}
	for in, want := range cases {
		if got := endpoint(in, "/chat/completions"); got != want {
			t.Errorf("endpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

// ReplaceAll 运行态保留：同名渠道冷却状态在热重载后仍在；改 base_url 丢目录缓存。
func TestReplaceAllPreservesRuntime(t *testing.T) {
	m := newTestManager()
	m.ReplaceAll([]ChannelDef{{Name: "a", BaseURL: "https://old"}})
	ch := m.Get("a")
	ch.NoteRateLimited(time.Minute, time.Minute, "429")
	ch.mu.Lock()
	ch.modelsCache = []string{"m"}
	ch.mu.Unlock()

	m.ReplaceAll([]ChannelDef{{Name: "a", BaseURL: "https://new", APIKey: "k"}})
	ch2 := m.Get("a")
	if !ch2.Cooldown() {
		t.Error("热重载应保留冷却状态（同渠道）")
	}
	ch2.mu.Lock()
	hasCache := len(ch2.modelsCache) > 0
	ch2.mu.Unlock()
	if hasCache {
		t.Error("改 base_url 应丢弃模型目录缓存")
	}
	// 删除渠道。
	m.ReplaceAll([]ChannelDef{})
	if m.Get("a") != nil {
		t.Error("删除的渠道应被移除")
	}
}

// 情报解析：宽松字段 + modality 双形态 + detail_slug 拼 URL。
func TestFetchIntel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
		  "updated_at": "2026-10-18",
		  "items": [
		    {"name":"硅基流动","free_type":"长期免费","quota":"2000万 tokens",
		     "gates":["注册"],"entry_url":"https://x","detail_slug":"siliconflow",
		     "modality":["glm-5.2","deepseek-v4"]},
		    {"name":"单串modality","modality":"glm-5.2"}
		  ]
		}`))
	}))
	defer srv.Close()
	data, err := fetchIntel(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Items) != 2 || data.Items[0].Name != "硅基流动" {
		t.Fatalf("items 解析异常: %+v", data.Items)
	}
	if data.Items[0].Modality != "glm-5.2 / deepseek-v4" {
		t.Errorf("modality 数组应拼接: %q", data.Items[0].Modality)
	}
	if data.Items[0].DetailURL != "https://token-fbi.com/intel/siliconflow" {
		t.Errorf("detail_slug 应拼 URL: %q", data.Items[0].DetailURL)
	}
	if data.Items[1].Modality != "glm-5.2" {
		t.Errorf("modality 字符串形态: %q", data.Items[1].Modality)
	}
}

// EnsureModels 负缓存：失败后 5 分钟内不再打上游。
func TestEnsureModelsNegativeCache(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(500)
	}))
	defer srv.Close()
	m := newTestManager()
	m.ReplaceAll([]ChannelDef{{Name: "a", BaseURL: srv.URL}})
	ch := m.Get("a")
	m.EnsureModels(context.Background(), ch)
	m.EnsureModels(context.Background(), ch)
	m.EnsureModels(context.Background(), ch)
	if calls != 1 {
		t.Errorf("负缓存应只打一次上游, got %d", calls)
	}
	// 显式 Models 配置：完全不发现。
	m2 := newTestManager()
	m2.ReplaceAll([]ChannelDef{{Name: "b", BaseURL: srv.URL, Models: []string{"m1"}}})
	if ids := m2.EnsureModels(context.Background(), m2.Get("b")); len(ids) != 1 || ids[0] != "m1" {
		t.Errorf("显式 Models 应直通: %v", ids)
	}
	if calls != 1 {
		t.Errorf("显式配置不应触发发现, calls=%d", calls)
	}
}
