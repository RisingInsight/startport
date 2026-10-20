// qoder_catalog.go Qoder 模型目录（带元数据，供面板「模型与档位」）。
//
// 远程目录：GET {gateway}/algo/api/v2/model/list?Encode=1（COSY 签名，GET 空体）。
// 返回 chat[]：key / display_name / enable / is_vl / is_reasoning /
// thinking_config.enabled.efforts / price_factor / max_input_tokens。
// 远程失败时回退静态表（价格快照来自参考实现 2026-09-20，非折后价）。
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// qoderStaticCredits 静态兜底倍率（参考实现 fallback 的 price_factor 快照）。
var qoderStaticCredits = map[string]string{
	"Qwen3.8-Flash":     "0.1",
	"Qwen3.8-Max":       "0.5",
	"Auto":              "1",
	"Ultimate":          "1.6",
	"Performance":       "1.1",
	"Efficient":         "0.3",
	"Sonus":             "3.2",
	"Cantus":            "3.2",
	"Qwen3.7-Max":       "0.5",
	"Qwen3.7-Plus":      "0.1",
	"Kimi-K3":           "0.8",
	"Kimi-K2.8-Preview": "0.3",
	"GLM-5.3":           "0.6",
	"GLM-5.3-Flash":     "0.1",
	"DeepSeek-V4-Pro":   "0.8",
	"DeepSeek-Flash":    "0.2",
	"MiniMax-M3":        "0.2",
}

const qoderCatalogPath = "/algo/api/v2/model/list?Encode=1"

// ModelCatalog 实现 CatalogProvider：远程目录优先，失败回退静态表。
func (a *qoderAdapter) ModelCatalog(ctx context.Context, t *Target) ([]ModelMeta, error) {
	creds, err := a.parseQoderSecret(t.Secret)
	if err != nil {
		return qoderStaticCatalog(a.intl), nil
	}
	url := strings.TrimRight(strings.TrimSpace(t.BaseURL), "/") + qoderCatalogPath
	headers, err := BuildAuthHeaders(nil, url, CosyIdentity{
		UserID: creds.UserID, AuthToken: creds.Token, Name: creds.Name,
		Email: creds.Email, MachineID: creds.MachineID,
	}, nil)
	if err != nil {
		return qoderStaticCatalog(a.intl), nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return qoderStaticCatalog(a.intl), nil
	}
	for _, h := range headers {
		req.Header.Set(h[0], h[1])
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return qoderStaticCatalog(a.intl), nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return qoderStaticCatalog(a.intl), nil
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return qoderStaticCatalog(a.intl), nil
	}
	if code, _ := payload["statusCodeValue"].(float64); code != 0 && code != 200 {
		return qoderStaticCatalog(a.intl), nil
	}
	models := parseQoderCatalog(payload)
	if len(models) == 0 {
		return qoderStaticCatalog(a.intl), nil
	}
	return models, nil
}

// parseQoderCatalog 解析远程目录（宽容：chat 在顶层或 data.chat）。
func parseQoderCatalog(payload map[string]any) []ModelMeta {
	chat, _ := payload["chat"].([]any)
	if chat == nil {
		if data, ok := payload["data"].(map[string]any); ok {
			chat, _ = data["chat"].([]any)
		}
	}
	out := make([]ModelMeta, 0, len(chat))
	seen := map[string]bool{}
	for _, raw := range chat {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		display := strings.TrimSpace(qoderText(item["display_name"]))
		key := strings.TrimSpace(qoderText(item["key"]))
		if display == "" || key == "" {
			continue
		}
		id := strings.Join(strings.Fields(display), "")
		if id == "" || seen[strings.ToLower(id)] {
			continue
		}
		seen[strings.ToLower(id)] = true
		efforts := qoderEfforts(item)
		reasoning := qoderTruthy(item["is_reasoning"]) || qoderTruthy(item["thinking_config"])
		meta := ModelMeta{
			ID:                id,
			Name:              display,
			Description:       qoderText(item["description"]),
			Credits:           qoderCredits(item["price_factor"]),
			ContextLength:     qoderContext(item),
			MaxOutputTokens:   qoderMaxOutputTokens,
			SupportedEfforts:  efforts,
			SupportsTools:     true,
			SupportsImages:    qoderTruthy(item["is_vl"]),
			SupportsReasoning: reasoning,
		}
		out = append(out, meta)
	}
	return out
}

// qoderStaticCatalog 静态兜底（与路由表同源；上下文/输出上限来自参考实现）。
func qoderStaticCatalog(intl bool) []ModelMeta {
	models := qoderFallbackModels(intl)
	out := make([]ModelMeta, 0, len(models))
	for _, m := range models {
		out = append(out, ModelMeta{
			ID:                m.ID,
			Name:              m.ID,
			Credits:           qoderCredits(qoderStaticCredits[m.ID]),
			ContextLength:     200_000,
			MaxOutputTokens:   qoderMaxOutputTokens,
			SupportedEfforts:  append([]string(nil), m.Efforts...),
			SupportsTools:     true,
			SupportsImages:    m.Vision,
			SupportsReasoning: m.Reasoning,
		})
	}
	return out
}

func qoderEfforts(item map[string]any) []string {
	tc, _ := item["thinking_config"].(map[string]any)
	enabled, _ := tc["enabled"].(map[string]any)
	switch efforts := enabled["efforts"].(type) {
	case map[string]any:
		out := make([]string, 0, len(efforts))
		for k := range efforts {
			out = append(out, k)
		}
		return out
	case []any:
		out := make([]string, 0, len(efforts))
		for _, v := range efforts {
			if s := qoderText(v); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func qoderContext(item map[string]any) int {
	if n := int(qoderNumber(item["max_input_tokens"])); n > 0 {
		return n
	}
	return 200_000
}

func qoderCredits(raw any) string {
	f := qoderNumber(raw)
	if f < 0 {
		return ""
	}
	text := formatQoderFactor(f)
	if text == "" {
		return ""
	}
	return "x" + text + " credits"
}

func formatQoderFactor(v float64) string {
	rounded := float64(int(v*100+0.5)) / 100
	if rounded == float64(int64(rounded)) {
		return fmt.Sprintf("%d", int64(rounded))
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", rounded), "0"), ".")
}

func qoderNumber(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case json.Number:
		f, _ := n.Float64()
		return f
	case string:
		var f float64
		_, _ = fmt.Sscanf(strings.TrimSpace(n), "%f", &f)
		return f
	}
	return -1
}

func qoderText(v any) string {
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func qoderTruthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return strings.TrimSpace(x) != ""
	case map[string]any:
		return len(x) > 0
	case []any:
		return len(x) > 0
	}
	return false
}
