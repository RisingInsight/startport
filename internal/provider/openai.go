// openai.go OpenAI Chat Completions 出站适配器（缺省协议；零回归基准）。
//
// 本适配器刻意保持「字节透传」：请求体原样发往上游、响应体原样回写、SSE 流
// 原样透传。理由是第三方 provider 的私有 SSE 字段（OpenRouter 的 provider
// 归因、硅基流动的细节字段）在重新序列化时会被吞掉，透传是唯一正确语义。
// URL 规则走 LegacyOpenAIRoot（逐字节复刻历史 endpoint()，老配置零回归）。
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// openAIAdapter OpenAI Chat Completions 适配器（无状态，可并发共享）。
type openAIAdapter struct{}

// NewOpenAIAdapter 构造 OpenAI 适配器。
func NewOpenAIAdapter() Adapter { return &openAIAdapter{} }

// Protocol 实现 Adapter。
func (a *openAIAdapter) Protocol() Protocol { return ProtocolOpenAI }

// BuildRequest 实现 Adapter：请求体原样透传。
func (a *openAIAdapter) BuildRequest(ctx context.Context, t *Target, openAIBody []byte, stream bool) (*http.Request, error) {
	model := ExtractModel(openAIBody)
	u := BuildChatURL(t, model, stream)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(openAIBody))
	if err != nil {
		return nil, fmt.Errorf("build openai request: %w", err)
	}
	ApplyHeaders(req, t, stream, nil)
	return req, nil
}

// ConvertResponse 实现 Adapter：body 原样返回（含错误体，保留上游 error.code）。
func (a *openAIAdapter) ConvertResponse(_ *Target, _ int, body []byte) []byte { return body }

// ConvertStream 实现 Adapter：SSE 流原样透传（不重建帧）。
func (a *openAIAdapter) ConvertStream(_ *Target, _ string, body io.ReadCloser) (io.ReadCloser, error) {
	return body, nil
}

// BuildModels 实现 Adapter。
func (a *openAIAdapter) BuildModels(ctx context.Context, t *Target) (*http.Request, error) {
	return buildModelsGET(ctx, t, BuildModelsURL(t))
}

// DecodeModels 实现 Adapter：解析 {"data":[{"id":...}]}。
func (a *openAIAdapter) DecodeModels(_ *Target, status int, body []byte) ([]string, error) {
	if status != http.StatusOK {
		return nil, NewHTTPError(status, "models_upstream_error", truncate(string(body), 300))
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parse models: %w", err)
	}
	out := make([]string, 0, len(parsed.Data))
	for _, d := range parsed.Data {
		if d.ID != "" {
			out = append(out, d.ID)
		}
	}
	return out, nil
}

// ExtractModel 从 OpenAI wire 请求体里取 model（URL 模板 {model} 占位用）。
// 解析失败返回空串（请求原样发上游，由上游给出更准确的错误）。
func ExtractModel(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	return probe.Model
}

// buildModelsGET 构造模型目录 GET 请求（三协议共用；鉴权/静态头同聊天请求）。
func buildModelsGET(ctx context.Context, t *Target, u string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build models request: %w", err)
	}
	ApplyHeaders(req, t, false, nil)
	return req, nil
}
