// codex_refresh.go Codex 凭证刷新（ChatGPT OAuth 刷新流）。
//
// auth.json 的 access_token 有效期以小时计，刷新机用 refresh_token 换新令牌组
// 并回写 auth_json（调度器负责落 accounts.json + 重投影）。契约来源：
// openai-api-server-via-codex（Apache-2.0，auth.go 刷新函数同款）；
// ⚠ StarPort 侧未真实账号验收。
package checkin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/RisingInsight/startport/internal/account"
)

const (
	codexRefreshURL = "https://auth.openai.com/oauth/token"
	codexClientID   = "app_EMoamEEZ73f0CkXaXp7hrann"
)

// RefreshCodex 刷新 Codex 账号凭证，返回更新后的 blob map（auth_json 换新）。
func RefreshCodex(ctx context.Context, a *account.Account) (map[string]any, error) {
	blob, err := decodeBlob(a)
	if err != nil {
		return nil, err
	}
	authJSON := blobString(blob, "auth_json")
	if authJSON == "" {
		return nil, fmt.Errorf("账号 blob 缺少 auth_json")
	}
	var doc struct {
		AuthMode    string `json:"auth_mode"`
		LastRefresh string `json:"last_refresh"`
		Tokens      struct {
			Access  string `json:"access_token"`
			Refresh string `json:"refresh_token"`
			ID      string `json:"id_token"`
			Account string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal([]byte(authJSON), &doc); err != nil {
		return nil, fmt.Errorf("auth.json 解析失败: %w", err)
	}
	if doc.AuthMode != "" && doc.AuthMode != "chatgpt" {
		return nil, fmt.Errorf("auth_mode %q 不受支持（需要 ChatGPT 模式，请重新 codex login）", doc.AuthMode)
	}
	if doc.Tokens.Refresh == "" {
		return nil, authExpiredf("auth.json 无刷新令牌（请重新 codex login）")
	}
	// 标准 OAuth2 刷新：client_id + 刷新令牌 → 新令牌组（JSON body，参考实现同款）。
	bodyMap := map[string]string{"client_id": codexClientID}
	bodyMap["grant_type"] = "refresh_token"
	bodyMap["refresh_token"] = doc.Tokens.Refresh
	body, err := json.Marshal(bodyMap)
	if err != nil {
		return nil, fmt.Errorf("序列化刷新请求失败")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexRefreshURL, strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("构造刷新请求失败")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("刷新请求失败（网络）")
	}
	defer resp.Body.Close()
	var result struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		ID      string `json:"id_token"`
		Error   string `json:"error"`
		Desc    string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("刷新响应解析失败（HTTP %d）", resp.StatusCode)
	}
	if resp.StatusCode/100 != 2 || result.Access == "" {
		detail := strings.TrimSpace(strings.Join([]string{result.Error, result.Desc}, " "))
		if result.Error == "invalid_grant" {
			return nil, authExpiredf("刷新令牌已被吊销或过期（%s）；请重新 codex login", strings.TrimSpace(detail))
		}
		return nil, fmt.Errorf("刷新失败（HTTP %d）%s", resp.StatusCode, detail)
	}
	// 回写：三令牌更新（缺失字段保留旧值）+ last_refresh 时间戳。
	doc.Tokens.Access = result.Access
	if result.Refresh != "" {
		doc.Tokens.Refresh = result.Refresh
	}
	if result.ID != "" {
		doc.Tokens.ID = result.ID
	}
	doc.LastRefresh = time.Now().UTC().Format(time.RFC3339)
	newJSON, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("序列化新 auth.json 失败: %w", err)
	}
	return map[string]any{"auth_json": string(newJSON)}, nil
}
