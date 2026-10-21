// codex.go Codex（ChatGPT 订阅编码后端）原生适配器。
//
// 上游 = chatgpt.com/backend-api/codex 的 Responses 面（SSE），凭证来自
// `codex login` 产出的 auth.json。契约来源：hotchpotch/openai-api-server-via-codex
// （Apache-2.0，源码级提取）+ AgentHub 对照；⚠ StarPort 侧未真实账号验收。
//
// 凭证（blob.secret 二选一）：
//   - auth.json 全文（auth_mode 必须 = "chatgpt"；取 tokens 三元组 + account_id，
//     account_id 缺失时从 id_token 的 chatgpt_account_id 提取）；
//   - 或裸 access_token 字符串（快捷形态，无刷新能力）。
//
// 鉴权：Bearer access_token + chatgpt-account-id 头；originator/UA 伪装官方
// codex_cli_rs。上游恒为 SSE（stream=true + store=false 强制），非流式客户端
// 在适配器聚合。token 刷新机在账号层（checkin 包，后续迭代）。
package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// CodexAdapterName Codex 原生适配器的注册名（config.ext_channels[].adapter）。
const CodexAdapterName = "codex"

// codexDefaultBackend ChatGPT Codex 后端缺省基址（参考实现同款）。
const codexDefaultBackend = "https://chatgpt.com/backend-api/codex"

type codexAdapter struct{}

func newCodexAdapter() *codexAdapter { return &codexAdapter{} }

// init 注册原生适配器（platform_test 一致性测试会钉住四处同步）。
func init() { RegisterNative(newCodexAdapter()) }

// Name 实现 NativeAdapter。
func (c *codexAdapter) Name() string { return CodexAdapterName }

// Protocol 实现 Adapter：承载 Responses 协议。
func (c *codexAdapter) Protocol() Protocol { return ProtocolResponses }

// StaticModels 实现 StaticModels：官方客户端白名单（参考实现 defaultModels 同款）。
func (c *codexAdapter) StaticModels() []string {
	return []string{
		"gpt-5.1", "gpt-5.1-codex-max", "gpt-5.1-codex-mini",
		"gpt-5.2", "gpt-5.2-codex",
		"gpt-5.3-codex", "gpt-5.3-codex-spark",
		"gpt-5.4", "gpt-5.4-mini",
		"gpt-5.5", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna",
	}
}

// codexCredentials 从 blob.secret 解析出的凭证（Refresh 字段给账号层刷新机
// 用，转发适配器本阶段只消费 Access）。
type codexCredentials struct {
	Access  string
	Refresh string
	Account string
	ID      string
}

// parseCodexSecret 解析 blob.secret：auth.json 全文或裸 access_token。
func parseCodexSecret(secret string) (codexCredentials, error) {
	s := strings.TrimSpace(secret)
	if s == "" {
		return codexCredentials{}, fmt.Errorf("缺少凭证（粘贴 auth.json 全文或其中的 access_token）")
	}
	if !strings.HasPrefix(s, "{") {
		return codexCredentials{Access: s}, nil
	}
	var doc struct {
		AuthMode string `json:"auth_mode"`
		Tokens   struct {
			Access  string `json:"access_token"`
			Refresh string `json:"refresh_token"`
			ID      string `json:"id_token"`
			Account string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal([]byte(s), &doc); err != nil {
		return codexCredentials{}, fmt.Errorf("auth.json 解析失败: %w", err)
	}
	if doc.AuthMode != "" && doc.AuthMode != "chatgpt" {
		return codexCredentials{}, fmt.Errorf("auth_mode %q 不受支持（需要 ChatGPT 模式，请重新 codex login）", doc.AuthMode)
	}
	cred := codexCredentials{
		Access:  doc.Tokens.Access,
		Refresh: doc.Tokens.Refresh,
		Account: doc.Tokens.Account,
		ID:      doc.Tokens.ID,
	}
	if cred.Account == "" && cred.ID != "" {
		cred.Account = codexAccountIDFromIDToken(cred.ID)
	}
	if cred.Access == "" {
		return codexCredentials{}, fmt.Errorf("auth.json 缺少 access_token（请重新 codex login）")
	}
	return cred, nil
}

// codexAccountIDFromIDToken 从 id_token 的 payload 提取 chatgpt_account_id
// （顶层或 https://api.openai.com/auth 嵌套，参考实现同款双位置）。
func codexAccountIDFromIDToken(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		ChatgptAccountID string `json:"chatgpt_account_id"`
		Auth             struct {
			ChatgptAccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	if claims.ChatgptAccountID != "" {
		return claims.ChatgptAccountID
	}
	return claims.Auth.ChatgptAccountID
}

// BuildRequest 实现 Adapter：chat wire → Responses payload + Codex 头组。
func (c *codexAdapter) BuildRequest(ctx context.Context, t *Target, openAIBody []byte, stream bool) (*http.Request, error) {
	cred, err := parseCodexSecret(t.Secret)
	if err != nil {
		return nil, err
	}
	payload, err := chatToResponsesRequest(openAIBody, true)
	if err != nil {
		return nil, err
	}
	// Codex 后端恒为 SSE（非流式客户端由 ConvertResponse 聚合），不落库。
	payload["stream"] = true
	payload["store"] = false
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal codex request: %w", err)
	}
	base := strings.TrimRight(t.BaseURL, "/")
	if base == "" {
		base = codexDefaultBackend
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/responses", strings.NewReader(string(payloadBytes)))
	if err != nil {
		return nil, fmt.Errorf("build codex request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+cred.Access)
	req.Header.Set("originator", "codex_cli_rs")
	req.Header.Set("User-Agent", "codex_cli_rs/1.0.0 (Windows; x86_64) starport")
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("session_id", newAutoClawRequestID())
	if cred.Account != "" {
		req.Header.Set("chatgpt-account-id", cred.Account)
	}
	return req, nil
}

// ConvertResponse 实现 Adapter：非流式聚合路径（上游恒 SSE → 取最终 response
// → 折 chat envelope）。
func (c *codexAdapter) ConvertResponse(t *Target, status int, body []byte) []byte {
	if status >= 400 {
		return codexErrorBody(status, body)
	}
	if looksLikeSSE(body) {
		final, err := aggregateResponsesSSE(body)
		if err != nil {
			return ErrorEnvelope(http.StatusBadGateway, "upstream_error", err.Error())
		}
		body = final
	}
	return responsesResponseToChat(body, t.ClientModel)
}

// ConvertStream 实现 Adapter：Responses SSE → OpenAI chunks（增量）。
func (c *codexAdapter) ConvertStream(t *Target, _ string, body io.ReadCloser) (io.ReadCloser, error) {
	st := newResponsesOutStream(t.ClientModel)
	return newTransformReader(body, st.handle, st.finishChunks), nil
}

// BuildModels 实现 Adapter：模型目录走上游 models 端点（需鉴权；失败时调用方
// 回落 StaticModels）。
func (c *codexAdapter) BuildModels(ctx context.Context, t *Target) (*http.Request, error) {
	cred, err := parseCodexSecret(t.Secret)
	if err != nil {
		return nil, err
	}
	base := strings.TrimRight(t.BaseURL, "/")
	if base == "" {
		base = codexDefaultBackend
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models?client_version=1.0.0", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+cred.Access)
	req.Header.Set("originator", "codex_cli_rs")
	return req, nil
}

// DecodeModels 实现 Adapter：{data:[…]} 形态取 id 列表。
func (c *codexAdapter) DecodeModels(t *Target, status int, body []byte) ([]string, error) {
	if status >= 400 {
		return nil, fmt.Errorf("codex 模型目录查询失败（HTTP %d）", status)
	}
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("codex 模型目录解析失败")
	}
	ids := make([]string, 0, len(resp.Data))
	for _, m := range resp.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

// codexErrorBody 上游错误 → OpenAI 错误信封（消息尽量取上游原文）。
func codexErrorBody(status int, body []byte) []byte {
	var structured struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	msg := ""
	if json.Unmarshal(body, &structured) == nil {
		msg = firstNonEmpty(structured.Error.Message, structured.Message)
	}
	if msg == "" {
		msg = "codex 上游错误: " + truncate(string(body), 300)
	}
	return ErrorEnvelope(status, "upstream_error", scrubText(msg, nil))
}
