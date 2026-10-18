// intel.go 额度情报拉取：token-fbi.com 的开放数据 data.json（免费额度情报聚合，
// 每日人工复核）。StarPort 只把它作为**信息展示**（面板「额度情报」视图）：
// 每条情报都引导用户去各平台官方注册领取，网关不解析、不依赖其内容做任何
// 路由/出站决策，也绝不自动注册任何平台。
//
// 数据结构宽松解析：只提取面板展示所需字段，未知字段忽略——上游加字段不破。
package extchan

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// IntelItem 单条情报。
type IntelItem struct {
	Name         string   `json:"name"`
	Vendor       string   `json:"vendor,omitempty"`
	Category     string   `json:"category,omitempty"`  // tool / model / event
	FreeType     string   `json:"free_type,omitempty"` // 长期免费 / 免费层 / 注册赠送 / 每日赠送 / 限时活动
	Quota        string   `json:"quota,omitempty"`     // 额度说明
	Validity     string   `json:"validity,omitempty"`  // 有效期
	Region       string   `json:"region,omitempty"`
	Gates        []string `json:"gates,omitempty"` // 门槛（需客户端/实名等）
	Note         string   `json:"note,omitempty"`
	EntryURL     string   `json:"entry_url,omitempty"`
	DetailURL    string   `json:"detail_url,omitempty"`
	Modality     string   `json:"modality,omitempty"` // 可用模型
	LastVerified string   `json:"last_verified,omitempty"`
}

// IntelData data.json 的展示投影。
type IntelData struct {
	Items     []IntelItem `json:"items"`
	UpdatedAt string      `json:"updated_at,omitempty"`
}

// fetchIntel 拉取并解析 data.json。仅 https；响应体上限 8MiB（防御异常数据源）。
func fetchIntel(ctx context.Context, url string) (*IntelData, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	hc := &http.Client{Timeout: 25 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("intel source %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Items []struct {
			Name         *string         `json:"name"`
			Vendor       *string         `json:"vendor"`
			Category     *string         `json:"category"`
			FreeType     *string         `json:"free_type"`
			Quota        *string         `json:"quota"`
			Validity     *string         `json:"validity"`
			Region       *string         `json:"region"`
			Gates        []any           `json:"gates"`
			Note         *string         `json:"note"`
			EntryURL     *string         `json:"entry_url"`
			DetailSlug   *string         `json:"detail_slug"`
			Modality     json.RawMessage `json:"modality"` // string 或 array 都可能
			LastVerified *string         `json:"last_verified"`
		} `json:"items"`
		UpdatedAt *string `json:"updated_at"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("parse intel: %w", err)
	}
	out := &IntelData{}
	if parsed.UpdatedAt != nil {
		out.UpdatedAt = *parsed.UpdatedAt
	}
	for _, it := range parsed.Items {
		if it.Name == nil || *it.Name == "" {
			continue
		}
		item := IntelItem{
			Name:         *it.Name,
			Category:     deref(it.Category),
			FreeType:     deref(it.FreeType),
			Quota:        deref(it.Quota),
			Validity:     deref(it.Validity),
			Region:       deref(it.Region),
			Note:         deref(it.Note),
			EntryURL:     deref(it.EntryURL),
			LastVerified: deref(it.LastVerified),
		}
		if it.Vendor != nil {
			item.Vendor = *it.Vendor
		}
		for _, g := range it.Gates {
			if s, ok := g.(string); ok {
				item.Gates = append(item.Gates, s)
			}
		}
		// detail_slug → 详情页 URL（站点路由 /intel/<slug>；仅做展示链接）。
		if it.DetailSlug != nil && *it.DetailSlug != "" {
			item.DetailURL = "https://token-fbi.com/intel/" + *it.DetailSlug
		}
		// modality 兼容 string / []string 两种形态。
		if len(it.Modality) > 0 {
			var one string
			if json.Unmarshal(it.Modality, &one) == nil {
				item.Modality = one
			} else {
				var many []string
				if json.Unmarshal(it.Modality, &many) == nil && len(many) > 0 {
					item.Modality = joinStrings(many, " / ")
				}
			}
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func joinStrings(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}
