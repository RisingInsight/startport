// qoder_test.go Qoder 协议转换 / 信封解包 / 思考拆解 / 错误分类 / 静态目录。
package provider

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// 错误分类：判定链顺序（排队 → 业务码 → 额度关键词 → 状态码）。
func TestQoderClassifyUpstreamError(t *testing.T) {
	cases := []struct {
		name   string
		status int
		text   string
		kind   qoderUpstreamKind
	}{
		// ① 排队：业务码 10605 / isQueued（前置于额度泛词）。
		{"10605 排队", 403, `{"code":"10605","message":"暂不可服务","retryAfterSeconds":9}`, qoderKindQueued},
		{"isQueued 排队", 403, `{"message":"{\"isQueued\":true}"}`, qoderKindQueued},
		// 排队正文里的 plan/credit 泛词不能截胡。
		{"排队含额度泛词", 403, `{"code":"10605","message":"plan credit 暂不可服务"}`, qoderKindQueued},
		// ② 业务码 105 → 鉴权（即使带额度泛词）。
		{"105 鉴权", 403, `{"code":"105","message":"expired"}`, qoderKindAuth},
		{"105 裸文本", 200, `data: {"statusCodeValue":403,"body":"{\"code\":\"105\"}"}`, qoderKindAuth},
		// ③ 额度业务码 / 关键词 / 定价链接。
		{"112 额度", 403, `{"code":"112"}`, qoderKindQuota},
		{"pricingUrl", 403, `{"message":"see https://qoder.com/pricing for plans"}`, qoderKindQuota},
		{"insufficient", 402, `{"message":"insufficient quota"}`, qoderKindQuota},
		// ④ 状态码兜底。
		{"429", 429, `{"message":"too many"}`, qoderKindRate},
		{"401", 401, `{}`, qoderKindAuth},
		{"裸 403", 403, `{"message":"forbidden"}`, qoderKindForbidden},
		{"500", 500, `{}`, qoderKindServer},
		{"未知", 400, `{"message":"bad request"}`, qoderKindUnknown},
	}
	for _, c := range cases {
		got := qoderClassifyUpstreamError(c.status, c.text)
		if got.kind != c.kind {
			t.Errorf("%s: kind = %v, want %v（分类=%q）", c.name, got.kind, c.kind, got.message)
		}
	}
}

// 额度错误的定价页链接透出；ASCII 大小写不敏感。
func TestQoderClassifyPricingURL(t *testing.T) {
	got := qoderClassifyUpstreamError(403, `{"message":"upgrade at HTTP://Qoder.com/Pricing now"}`)
	if got.pricingURL != "HTTP://Qoder.com/Pricing" {
		t.Errorf("pricingURL = %q", got.pricingURL)
	}
	// 链接被引号截断（源正则的 [^"\\] 停止符）。
	got = qoderClassifyUpstreamError(403, `{"message":"see \"https://x.com/pricing?a=1\" end"}`)
	if got.pricingURL != "https://x.com/pricing?a=1" {
		t.Errorf("pricingURL = %q", got.pricingURL)
	}
}

// 排队文案必须说明"不是登录态或额度问题"（防止用户去重新登录正常账号）。
func TestQoderQueuedMessage(t *testing.T) {
	got := qoderClassifyUpstreamError(403, `{"code":"10605","retryAfterSeconds":9}`)
	if got.kind != qoderKindQueued {
		t.Fatalf("kind = %v", got.kind)
	}
	if !strings.Contains(got.message, "不是登录态或额度问题") {
		t.Errorf("排队文案缺关键说明: %q", got.message)
	}
	if got.queue.retryAfterSecs == nil || *got.queue.retryAfterSecs != 9 {
		t.Errorf("retryAfterSecs 未解析: %+v", got.queue)
	}
}

// 信封解包：200 信封 → 内层 chunk；statusCodeValue != 200 → 业务错误；[DONE]。
func TestQoderParseSSELine(t *testing.T) {
	// 正常 chunk：内层是 JSON 字符串。
	ev := qoderParseSSELine(`{"statusCodeValue":200,"body":"{\"choices\":[{\"delta\":{\"content\":\"你\"}}]}"}`)
	if ev.kind != qoderEventChunk {
		t.Fatalf("kind = %v", ev.kind)
	}
	choices, _ := ev.chunk["choices"].([]any)
	if len(choices) != 1 {
		t.Errorf("chunk 解析异常: %+v", ev.chunk)
	}
	// 业务错误。
	ev = qoderParseSSELine(`{"statusCodeValue":401,"body":"{\"code\":\"105\",\"message\":\"expired\"}"}`)
	if ev.kind != qoderEventError || ev.status != 401 || ev.classified.kind != qoderKindAuth {
		t.Errorf("业务错误解包异常: %+v", ev)
	}
	// [DONE]（外层与内层两种形态）。
	if ev := qoderParseSSELine(`[DONE]`); ev.kind != qoderEventDone {
		t.Error("[DONE] 外层未识别")
	}
	if ev := qoderParseSSELine(`{"statusCodeValue":200,"body":"[DONE]"}`); ev.kind != qoderEventDone {
		t.Error("[DONE] 内层未识别")
	}
	// 非 JSON / 空：skip。
	if ev := qoderParseSSELine(`not-json`); ev.kind != qoderEventSkip {
		t.Error("非 JSON 应 skip")
	}
}

// 思考标签拆解：跨分片（`<thi` + `nking>` 分两片）不漏正文、不漏思考。
func TestQoderThinkingParserCrossChunk(t *testing.T) {
	p := newQoderThinkingParser()
	p.push("前言<thi")
	p.push("nking>思考内容</thi")
	p.push("nking>正文")
	p.finish()
	var thinking, content string
	for _, piece := range p.take() {
		if piece.isThinking {
			thinking += piece.text
		} else {
			content += piece.text
		}
	}
	if thinking != "思考内容" {
		t.Errorf("思考 = %q", thinking)
	}
	if content != "前言正文" {
		t.Errorf("正文 = %q（跨分片碎片不应漏出）", content)
	}
}

// 思考拆解：正文以 `<think` 结尾时 finish 必须冲刷缓冲（否则少几个字符）。
func TestQoderThinkingParserFlushTail(t *testing.T) {
	p := newQoderThinkingParser()
	p.push("回答以 <think 结尾")
	p.finish()
	var content string
	for _, piece := range p.take() {
		content += piece.text
	}
	if content != "回答以 <think 结尾" {
		t.Errorf("尾部被吞: %q", content)
	}
}

// 闭标签后的空行吃掉（上游习惯 `</thinking>\n\n正文`）。
func TestQoderThinkingParserStripNewline(t *testing.T) {
	p := newQoderThinkingParser()
	p.push("<thinking>想</thinking>\n\n正文")
	p.finish()
	var content string
	for _, piece := range p.take() {
		if !piece.isThinking {
			content += piece.text
		}
	}
	if content != "正文" {
		t.Errorf("闭标签后空行未吃掉: %q", content)
	}
}

// 协议转换：失败 assistant 轮连带 tool 结果丢弃；纯工具调用补非空 content。
func TestQoderNormalizeMessages(t *testing.T) {
	raw := []any{
		map[string]any{"role": "system", "content": "sys"},
		map[string]any{"role": "assistant", "__failed": true,
			"tool_calls": []any{map[string]any{"id": "call_1"}}},
		map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "orphan"},
		map[string]any{"role": "assistant", "content": "",
			"tool_calls": []any{map[string]any{"id": "call_2", "type": "function",
				"function": map[string]any{"name": "f", "arguments": "{\"x\":1}"}}}},
		map[string]any{"role": "user", "content": "hi"},
	}
	out := qoderNormalizeMessages(raw)
	if len(out) != 3 {
		t.Fatalf("消息数 = %d, want 3（失败轮与孤儿 tool 结果被丢）", len(out))
	}
	second, _ := out[1].(map[string]any)
	if second["content"] != " " {
		t.Errorf("纯工具调用的 assistant content = %v, want 非空空格", second["content"])
	}
}

// system 置顶：system 提示必须在消息序列最前（上游顶层 system 无效）。
func TestQoderFinalMessagesSystemFirst(t *testing.T) {
	raw := []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "system", "content": "be nice"},
	}
	out := qoderFinalMessages(raw)
	first, _ := out[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "be nice" {
		t.Errorf("system 未置顶: %+v", out)
	}
	if len(out) != 2 {
		t.Fatalf("消息数 = %d", len(out))
	}
}

// "关闭思考"降级为不指定（上游发 false 会断连）；档位别名归一。
func TestQoderResolveThinking(t *testing.T) {
	model := qoderModel{ID: "Qwen3.8-Flash", Key: "qfmodel", Reasoning: true,
		Efforts: []string{"low", "medium", "xhigh"}}
	// 关闭 → 不指定。
	got := qoderResolveThinking(map[string]any{"reasoning_effort": false}, model)
	if got.Enable != nil {
		t.Errorf("关闭思考应降级为不指定: %+v", got)
	}
	// 未指定 → enable=true 无档位。
	got = qoderResolveThinking(map[string]any{}, model)
	if got.Enable == nil || !*got.Enable || got.Effort != "" {
		t.Errorf("未指定思考: %+v", got)
	}
	// 别名归一：minimal→low、high/max→xhigh。
	for in, want := range map[string]string{"minimal": "low", "high": "xhigh", "max": "xhigh", "medium": "medium"} {
		got = qoderResolveThinking(map[string]any{"reasoning_effort": in}, model)
		if got.Effort != want {
			t.Errorf("effort(%q) = %q, want %q", in, got.Effort, want)
		}
	}
	// 不支持的档位回落 medium。
	got = qoderResolveThinking(map[string]any{"reasoning_effort": "ultra"}, model)
	if got.Effort != "medium" {
		t.Errorf("不支持档位应回落 medium, got %q", got.Effort)
	}
	// 非推理模型 → 不指定。
	got = qoderResolveThinking(map[string]any{"reasoning_effort": "high"},
		qoderModel{ID: "x", Key: "x", Reasoning: false})
	if got.Enable != nil {
		t.Errorf("非推理模型不应指定思考: %+v", got)
	}
}

// build_upstream_body：固定信封关键字段 + model_config 剥 thinking_config。
func TestQoderBuildUpstreamBody(t *testing.T) {
	model := qoderModel{ID: "Qwen3.8-Flash", Key: "qfmodel", Reasoning: true,
		Efforts: []string{"low", "medium", "xhigh"}}
	finalMessages := []any{map[string]any{"role": "system", "content": "sys"},
		map[string]any{"role": "user", "content": "hi"}}
	enable := true
	body := qoderBuildUpstreamBody(model, finalMessages, nil, 4096,
		qoderThinkingChoice{Enable: &enable, Effort: "low"}, "u-1", "")
	if body["stream"] != true {
		t.Error("上游恒流式（stream:true）")
	}
	if body["session_type"] != "qodercli" || body["agent_id"] != "agent_common" || body["chat_task"] != "FREE_INPUT" {
		t.Errorf("会话常量异常: %+v", body)
	}
	params, _ := body["parameters"].(map[string]any)
	if params["max_tokens"] != json.Number("4096") {
		t.Errorf("max_tokens = %v", params["max_tokens"])
	}
	if params["enable_thinking"] != true || params["reasoning_effort"] != "low" {
		t.Errorf("thinking 参数异常: %+v", params)
	}
	mc, _ := body["model_config"].(map[string]any)
	if mc["key"] != "qfmodel" || mc["is_reasoning"] != true {
		t.Errorf("model_config 异常: %+v", mc)
	}
	if _, hasThinkingConfig := mc["thinking_config"]; hasThinkingConfig {
		t.Error("model_config 不得携带 thinking_config（会覆盖 enable_thinking）")
	}
	ctx, _ := body["chat_context"].(map[string]any)
	if ctx["text"] != "hi" {
		t.Errorf("chat_context.text = %v, want 最后一条 user 文本", ctx["text"])
	}
	// max_tokens 缺省钳到 32768；超上界同钳。
	body = qoderBuildUpstreamBody(model, finalMessages, nil, 0,
		qoderThinkingChoice{}, "u-1", "")
	params = body["parameters"].(map[string]any)
	if params["max_tokens"] != json.Number("32768") {
		t.Errorf("缺省 max_tokens = %v, want 32768", params["max_tokens"])
	}
	body = qoderBuildUpstreamBody(model, finalMessages, nil, 999999,
		qoderThinkingChoice{}, "u-1", "")
	params = body["parameters"].(map[string]any)
	if params["max_tokens"] != json.Number("32768") {
		t.Errorf("超上界 max_tokens = %v, want 32768", params["max_tokens"])
	}
}

// 静态目录：两地区各自的条目；解析先本地区后另一地区，未命中硬失败。
func TestQoderStaticModelsAndResolve(t *testing.T) {
	cn := qoderStaticModelIDs(false)
	intl := qoderStaticModelIDs(true)
	if len(cn) == 0 || len(intl) == 0 {
		t.Fatal("静态目录为空")
	}
	// Qwen3.7-Plus 两地区都有；DeepSeek-V4-Pro 中国版 reasoning=true（表内差异保留）。
	if !containsStr(cn, "Qwen3.7-Plus") || !containsStr(intl, "Kimi-K3") {
		t.Errorf("目录条目缺失: cn=%v intl=%v", cn, intl)
	}
	if m, ok := resolveQoderModel(false, "QWEN3.8-FLASH"); !ok || m.Key != "qfmodel" {
		t.Errorf("大小写不敏感解析失败: %+v", m)
	}
	// 内部 key 也认（客户端写 qfmodel 不该报模型不存在）。
	if m, ok := resolveQoderModel(true, "qfmodel"); !ok || m.ID != "Qwen3.8-Flash" {
		t.Errorf("upstreamKey 容错失败: %+v", m)
	}
	// 未知名字硬失败。
	if _, ok := resolveQoderModel(false, "claude-sonnet-4"); ok {
		t.Error("未知名应硬失败（不静默回落）")
	}
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// 非流式聚合：SSE → chat.completion（含 reasoning_content 与 usage）。
func TestQoderAggregateSSE(t *testing.T) {
	sse := "data: {\"statusCodeValue\":200,\"body\":\"{\\\"choices\\\":[{\\\"delta\\\":{\\\"role\\\":\\\"assistant\\\",\\\"content\\\":\\\"你\\\"}}]}\"}\n" +
		"data: {\"statusCodeValue\":200,\"body\":\"{\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"<thinking>内部\\\"}}]}\"}\n" +
		"data: {\"statusCodeValue\":200,\"body\":\"{\\\"choices\\\":[{\\\"delta\\\":{},\\\"finish_reason\\\":\\\"stop\\\"}]}\"}\n" +
		"data: {\"statusCodeValue\":200,\"body\":\"{\\\"choices\\\":[],\\\"usage\\\":{\\\"total_tokens\\\":7}}\"}\n" +
		"data: [DONE]\n"
	a := NewQoderAdapter().(*qoderAdapter)
	tgt := &Target{Name: "qoder", Protocol: ProtocolOpenAI,
		BaseURL: "https://gateway.qoder.com.cn", ClientModel: "ext:qoder/Qwen3.8-Flash"}
	out := a.ConvertResponse(tgt, http.StatusOK, []byte(sse))
	var resp struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content   string  `json:"content"`
				Reasoning *string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("聚合结果不是 JSON: %s", out)
	}
	if resp.Model != "Qwen3.8-Flash" {
		t.Errorf("model = %q", resp.Model)
	}
	if !strings.Contains(resp.Choices[0].Message.Content, "你") {
		t.Errorf("正文缺失: %+v", resp.Choices)
	}
	// 混入正文的思考被拆进 reasoning_content（聚合器按模型能力启用拆解）。
	if resp.Choices[0].Message.Reasoning == nil ||
		!strings.Contains(*resp.Choices[0].Message.Reasoning, "内部") {
		t.Errorf("混流思考未拆进 reasoning_content: %+v", resp.Choices)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q", resp.Choices[0].FinishReason)
	}
	if resp.Usage.TotalTokens != 7 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

// ConvertStream：逐帧解包 + 首帧预读（业务错误在返回前转 typed error）。
func TestQoderConvertStreamBusinessError(t *testing.T) {
	a := NewQoderAdapter()
	sse := "data: {\"statusCodeValue\":403,\"body\":\"{\\\"code\\\":\\\"10605\\\",\\\"message\\\":\\\"排队\\\",\\\"retryAfterSeconds\\\":10}\"}\n\n"
	rc, err := a.ConvertStream(newQoderTarget(t), "text/event-stream",
		newNopCloser(strings.NewReader(sse)))
	if err == nil {
		t.Fatal("首帧预读应把排队信封转成 typed error")
	}
	if rc != nil {
		t.Error("错误路径不应返回 reader")
	}
	var he *HTTPError
	if !asHTTPError(err, &he) {
		t.Fatalf("错误类型 = %T, want *HTTPError", err)
	}
	if he.Code != "qoder_model_busy" || he.Status != http.StatusServiceUnavailable {
		t.Errorf("错误语义异常: %+v", he)
	}
}

// ConvertStream 正常路径：role 首帧 + content 帧 + finish 帧 + usage 帧 + [DONE]。
func TestQoderConvertStreamFrames(t *testing.T) {
	a := NewQoderAdapter()
	sse := "data: {\"statusCodeValue\":200,\"body\":\"{\\\"choices\\\":[{\\\"delta\\\":{\\\"role\\\":\\\"assistant\\\",\\\"content\\\":\\\"he\\\"}}]}\"}\n\n" +
		"data: {\"statusCodeValue\":200,\"body\":\"{\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"llo\\\"}}]}\"}\n\n" +
		"data: {\"statusCodeValue\":200,\"body\":\"{\\\"choices\\\":[{\\\"delta\\\":{},\\\"finish_reason\\\":\\\"stop\\\"}]}\"}\n\n" +
		"data: {\"statusCodeValue\":200,\"body\":\"{\\\"choices\\\":[],\\\"usage\\\":{\\\"total_tokens\\\":2}}\"}\n\n" +
		"data: [DONE]\n\n"
	rc, err := a.ConvertStream(newQoderTarget(t), "text/event-stream", newNopCloser(strings.NewReader(sse)))
	if err != nil {
		t.Fatalf("ConvertStream: %v", err)
	}
	out, _ := io_ReadAll(rc)
	s := string(out)
	if strings.Contains(s, "statusCodeValue") {
		t.Errorf("信封未解包: %s", s)
	}
	if strings.Count(s, `"role":"assistant"`) != 1 {
		t.Errorf("role 首帧异常: %s", s)
	}
	if !strings.Contains(s, `"content":"he"`) || !strings.Contains(s, `"content":"llo"`) {
		t.Errorf("content 增量缺失: %s", s)
	}
	if !strings.Contains(s, `"finish_reason":"stop"`) || !strings.Contains(s, `"total_tokens":2`) {
		t.Errorf("收尾帧缺失: %s", s)
	}
	if !strings.Contains(s, "data: [DONE]") {
		t.Error("终止帧缺失")
	}
}

// newQoderTarget / newNopCloser / asHTTPError 测试辅助。
func newQoderTarget(t *testing.T) *Target {
	t.Helper()
	return &Target{Name: "qoder", Protocol: ProtocolOpenAI,
		BaseURL:     "https://gateway.qoder.com.cn",
		Secret:      `{"mode":"cn","accessToken":"jt-test-token","userId":"u-1","machineId":"m-1","nickname":"N","email":"e@x"}`,
		ClientModel: "ext:qoder/Qwen3.8-Flash"}
}

func newNopCloser(r *strings.Reader) ioNopCloser { return ioNopCloser{r} }

type ioNopCloser struct{ r *strings.Reader }

func (c ioNopCloser) Read(p []byte) (int, error) { return c.r.Read(p) }
func (c ioNopCloser) Close() error               { return nil }

func asHTTPError(err error, target **HTTPError) bool {
	if he, ok := err.(*HTTPError); ok {
		*target = he
		return true
	}
	return false
}

func io_ReadAll(r interface{ Read([]byte) (int, error) }) ([]byte, error) {
	var out []byte
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			return out, err
		}
	}
}

// 凭证解析：JSON 形态、地区不匹配、缺字段报错、machineId 派生稳定。
func TestQoderParseSecret(t *testing.T) {
	a := NewQoderAdapter().(*qoderAdapter)
	creds, err := a.parseQoderSecret(
		`{"mode":"cn","accessToken":"jt-abc","userId":"u-1","nickname":"N","email":"e@x"}`)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if creds.Token != "jt-abc" || creds.UserID != "u-1" || creds.MachineID == "" {
		t.Errorf("解析结果异常: %+v", creds)
	}
	// machineId 派生稳定（同 secret 同派生）。
	creds2, _ := a.parseQoderSecret(
		`{"mode":"cn","accessToken":"jt-abc","userId":"u-1","nickname":"N","email":"e@x"}`)
	if creds.MachineID != creds2.MachineID {
		t.Error("machineId 派生不稳定")
	}
	// 地区不匹配。
	if _, err := a.parseQoderSecret(`{"mode":"global","accessToken":"dt-1","userId":"u"}`); err == nil {
		t.Error("地区不匹配应报错")
	}
	// 缺 userId。
	if _, err := a.parseQoderSecret(`{"mode":"cn","accessToken":"jt-abc"}`); err == nil {
		t.Error("缺 userId 应报错")
	}
	// 裸 token 拒收。
	if _, err := a.parseQoderSecret("jt-abc"); err == nil {
		t.Error("裸 token 应报错（COSY 需要用户与设备标识）")
	}
	// Bearer 前缀剥离。
	creds, err = a.parseQoderSecret(`{"mode":"cn","accessToken":"Bearer jt-abc","userId":"u-1"}`)
	if err != nil {
		t.Fatalf("Bearer 前缀应剥离: %v", err)
	}
	if creds.Token != "jt-abc" {
		t.Errorf("token = %q", creds.Token)
	}
}
