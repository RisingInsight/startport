package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

const traeSecret = `{"accessToken":"jwt-1","uid":"u-1","machineId":"m-1","deviceId":"d-1","variant":"solo"}`

func traeTarget(base, secret string) *Target {
	return &Target{
		Name: "trae", Protocol: ProtocolOpenAI, BaseURL: base,
		Secret: secret, ClientModel: "ext:trae/glm-5.2",
	}
}

func TestTraeRequestHeadersAndPayload(t *testing.T) {
	body := []byte(`{
		"model":"glm-5.2",
		"messages":[
			{"role":"developer","content":"sys"},
			{"role":"user","content":"hi"},
			{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"read","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"c1","content":"ok"},
			{"role":"tool","tool_call_id":"orphan","content":"drop"}
		],
		"tools":[{"type":"function","function":{"name":"read","description":"r","parameters":{"type":"object"}}}],
		"tool_choice":{"type":"function","function":{"name":"read"}},
		"temperature":"0.3",
		"top_p":0.9,
		"max_tokens":4096,
		"stream_options":{"include_usage":true},
		"unknown":"drop"
	}`)
	req, err := NewTraeAdapter().BuildRequest(context.Background(), traeTarget("https://trae.example", traeSecret), body, true)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if req.Method != http.MethodPost || req.URL.Path != traeChatPath {
		t.Errorf("请求形态 = %s %s", req.Method, req.URL.Path)
	}
	for name, want := range map[string]string{
		"Authorization":      "Cloud-IDE-JWT jwt-1",
		"X-Cloudide-Token":   "jwt-1",
		"X-Ide-Token":        "jwt-1",
		"X-Uid":              "u-1",
		"X-Machine-Id":       "m-1",
		"X-Device-Id":        "d-1",
		"X-App-Id":           traeAppID,
		"X-Ide-Version-Code": traeIDEVersionCode,
		"User-Agent":         traeUserAgent,
		"Accept":             "text/event-stream",
	} {
		if got := req.Header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out["function"] != "solo_work_lite" || out["stream"] != true {
		t.Errorf("函数/流式字段异常: %+v", out)
	}
	if out["config_name"] != "glm-5.2" || out["model"] != "glm-5.2" {
		t.Errorf("模型字段异常: %+v", out)
	}
	if out["max_tokens"].(float64) != 4096 {
		t.Errorf("max_tokens = %v", out["max_tokens"])
	}
	if _, ok := out["temperature"]; ok {
		t.Error("字符串 temperature 应被丢弃（上游类型零容忍）")
	}
	if out["top_p"].(float64) != 0.9 {
		t.Errorf("top_p = %v", out["top_p"])
	}
	for _, dropped := range []string{"stream_options", "unknown"} {
		if _, ok := out[dropped]; ok {
			t.Errorf("白名单外字段 %q 不应出现", dropped)
		}
	}
	msgs := out["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages 数量 = %d, want 4（孤儿 tool 结果应丢弃）: %v", len(msgs), msgs)
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Errorf("developer 应降成 system: %v", first)
	}
	if parts, ok := first["content"].([]any); !ok || len(parts) != 1 {
		t.Errorf("字符串 content 应转 text 块: %v", first["content"])
	}
	assistant := msgs[2].(map[string]any)
	call := assistant["tool_calls"].([]any)[0].(map[string]any)
	if _, ok := call["function_call"]; !ok {
		t.Errorf("assistant tool_calls.function 应改名 function_call: %v", call)
	}
	tools := out["tools"].([]any)
	params := tools[0].(map[string]any)["function"].(map[string]any)["parameters"]
	if _, ok := params.(string); !ok {
		t.Errorf("tools parameters 应为 JSON 字符串: %T", params)
	}
	if out["tool_choice"] != "read" {
		t.Errorf("tool_choice = %v, want read", out["tool_choice"])
	}
}

func TestTraeEmptyIdentityHeadersOmitted(t *testing.T) {
	req, err := NewTraeAdapter().BuildRequest(context.Background(),
		traeTarget("https://trae.example", `{"accessToken":"jwt-1"}`),
		[]byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`), true)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"X-Uid", "X-Machine-Id", "X-Device-Id"} {
		if got := req.Header.Get(name); got != "" {
			t.Errorf("%s 为空时不应发送，实际 %q", name, got)
		}
	}
}

const traeSSE = "event: output\ndata: {\"response\":\"Hel\",\"reasoning_content\":\"think\"}\n\n" +
	"event: output\ndata: {\"response\":\"lo\"}\n\n" +
	"event: token_usage\ndata: {\"prompt_tokens\":1,\"completion_tokens\":2,\"total_tokens\":3}\n\n" +
	"event: done\ndata: {\"finish_reason\":\"stop\"}\n\n"

func TestTraeStreamConversion(t *testing.T) {
	r, err := newTraeStream(traeTarget("https://trae.example", traeSecret),
		io.NopCloser(strings.NewReader(traeSSE)))
	if err != nil {
		t.Fatalf("newTraeStream: %v", err)
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	text := string(raw)
	for _, want := range []string{
		`"content":"Hel"`, `"reasoning_content":"think"`, `"content":"lo"`,
		`"finish_reason":"stop"`, `"total_tokens":3`, `"model":"ext:trae/glm-5.2"`,
		"data: [DONE]",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("流输出缺少 %q:\n%s", want, text)
		}
	}
}

func TestTraeStreamFirstFrameError(t *testing.T) {
	cases := []struct {
		sse    string
		status int
		code   string
	}{
		{`event: error` + "\ndata: {\"code\":4008,\"message\":\"quota\"}\n\n", http.StatusTooManyRequests, "trae_plan_limit"},
		{`event: error` + "\ndata: {\"code\":4001,\"message\":\"model unavailable\"}\n\n", http.StatusNotFound, "trae_model_unavailable"},
	}
	for _, tc := range cases {
		_, err := newTraeStream(traeTarget("https://trae.example", traeSecret),
			io.NopCloser(strings.NewReader(tc.sse)))
		var he *HTTPError
		if !errors.As(err, &he) {
			t.Fatalf("首帧错误应返回 *HTTPError，实际 %v", err)
		}
		if he.Status != tc.status || he.Code != tc.code {
			t.Errorf("错误 = %d/%s, want %d/%s", he.Status, he.Code, tc.status, tc.code)
		}
	}
}

func TestTraeAggregate(t *testing.T) {
	raw, err := traeAggregate([]byte(traeSSE), "ext:trae/glm-5.2")
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	choice := out["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["content"] != "Hello" || msg["reasoning_content"] != "think" {
		t.Errorf("聚合内容异常: %+v", msg)
	}
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason = %v", choice["finish_reason"])
	}
	usage := out["usage"].(map[string]any)
	if usage["total_tokens"].(float64) != 3 {
		t.Errorf("usage = %+v", usage)
	}
	if out["model"] != "ext:trae/glm-5.2" {
		t.Errorf("model = %v", out["model"])
	}
}

func TestTraeModels(t *testing.T) {
	req, err := NewTraeAdapter().BuildModels(context.Background(),
		traeTarget("https://trae.example", traeSecret))
	if err != nil {
		t.Fatalf("BuildModels: %v", err)
	}
	if req.Method != http.MethodPost || req.URL.Path != traeModelsPath {
		t.Errorf("目录请求 = %s %s", req.Method, req.URL.Path)
	}
	raw, _ := io.ReadAll(req.Body)
	if !strings.Contains(string(raw), `"poly_prompt":true`) ||
		!strings.Contains(string(raw), `"function":"solo_work_lite"`) {
		t.Errorf("目录请求体异常: %s", raw)
	}
	payload := `{"config_info_list":[
		{"config_name":"glm-5.2","display_config":{"display_name":"GLM-5.2"},"config_switch":true},
		{"config_name":"hidden","display_config":{"display_name":"Hidden"},"is_invisible_to_user":true},
		{"config_name":"empty","display_config":{}},
		{"config_name":"off","display_config":{"display_name":"Off"},"config_switch":false},
		{"config_name":"deepseek-v4-pro","display_config":{"display_name":"Dead"}},
		{"config_name":"glm-5.2","display_config":{"display_name":"Dup"}}
	]}`
	got, err := NewTraeAdapter().DecodeModels(nil, http.StatusOK, []byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "glm-5.2" {
		t.Errorf("目录过滤 = %v, want [glm-5.2]", got)
	}
}

func TestTraeNormalizeStatus(t *testing.T) {
	a := NewTraeAdapter().(*traeAdapter)
	cases := []struct{ in, want int }{
		{http.StatusUnauthorized, http.StatusUnauthorized},
		{http.StatusTooManyRequests, http.StatusTooManyRequests},
		{http.StatusNotFound, http.StatusNotFound},
		{http.StatusRequestEntityTooLarge, http.StatusRequestEntityTooLarge},
		{http.StatusForbidden, http.StatusBadGateway}, // 不能当凭证失效
		{http.StatusInternalServerError, http.StatusBadGateway},
		{http.StatusBadRequest, http.StatusBadRequest},
	}
	for _, tc := range cases {
		got, _ := a.NormalizeStatus(tc.in, []byte("upstream detail"))
		if got != tc.want {
			t.Errorf("NormalizeStatus(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestTraeNonSSERejected(t *testing.T) {
	_, err := NewTraeAdapter().ConvertStream(
		traeTarget("https://trae.example", traeSecret),
		"application/json",
		io.NopCloser(strings.NewReader(`{"error":"not sse"}`)),
	)
	var he *HTTPError
	if !errors.As(err, &he) || he.Code != "trae_bad_stream" {
		t.Fatalf("非 SSE 200 应显式报错: %v", err)
	}
}

func TestTraeNestedCredential(t *testing.T) {
	creds, err := parseTraeSecret(`{"auth":{"accessToken":"jwt","machineId":"m"},"account":{"uid":"u"}}`)
	if err != nil {
		t.Fatalf("嵌套凭据应可解析: %v", err)
	}
	if creds.AccessToken != "jwt" || creds.UID != "u" || creds.MachineID != "m" {
		t.Errorf("嵌套字段解析异常: %+v", creds)
	}
}

func TestTraeIntlRejected(t *testing.T) {
	_, err := parseTraeSecret(`{"accessToken":"jwt","variant":"intl"}`)
	var he *HTTPError
	if !errors.As(err, &he) || he.Code != "trae_intl_unsupported" {
		t.Fatalf("intl 应被拒绝: %v", err)
	}
}
