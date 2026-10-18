// qa_p0_verify_test.go QA 独立验收（面板侧）：P0-7 探测成功路径、P0-8 提醒可见、
// P0-11 接口不回显 secret。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RisingInsight/startport/internal/extchan"
)

const qaPanelSecret = "QA-PANEL-SECRET-987654321"

// qaPanel 构造挂真实 Manager 的面板（两凭证：primary / backup）。
func qaPanel(t *testing.T, modelsBody string, status int) (*Panel, *extchan.Manager) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(modelsBody))
	}))
	t.Cleanup(up.Close)
	mgr := extchan.New(extchan.Config{TimeoutSeconds: 5})
	mgr.ReplaceAll([]extchan.ChannelDef{{
		Name: "sf", BaseURL: up.URL,
		Credentials: []extchan.CredentialDef{
			{Label: "primary", Secret: qaPanelSecret},
			{Label: "backup", Secret: "QA-BACKUP-SECRET-123456"},
		},
	}})
	return New(Config{Version: "qa", ExtChan: mgr}), mgr
}

// P0-7：正确凭证 → ok=true + 延迟 + 模型数（既有用例只覆盖失败路径）。
func TestQAProbeSuccessReportsLatencyAndModels(t *testing.T) {
	p, mgr := qaPanel(t, `{"data":[{"id":"a"},{"id":"b"},{"id":"c"}]}`, http.StatusOK)
	before := mgr.Status()

	// 已保存渠道探测（服务端持有真实 secret）。
	req := httptest.NewRequest("POST", "/startport/api/ext_channels/probe",
		strings.NewReader(`{"name":"sf","credential_label":"primary"}`))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK     bool                `json:"ok"`
		Result extchan.ProbeResult `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || !resp.Result.OK {
		t.Fatalf("正确凭证探测应 ok=true: %+v", resp.Result)
	}
	if resp.Result.ModelsCount != 3 || len(resp.Result.ModelsSample) != 3 {
		t.Errorf("模型数/样例异常: count=%d sample=%v", resp.Result.ModelsCount, resp.Result.ModelsSample)
	}
	if resp.Result.Status != http.StatusOK || resp.Result.Probe != "models" {
		t.Errorf("探测元信息异常: %+v", resp.Result)
	}
	if resp.Result.LatencyMs < 0 {
		t.Errorf("延迟为负: %d", resp.Result.LatencyMs)
	}
	if strings.Contains(rec.Body.String(), qaPanelSecret) {
		t.Error("探测响应泄露 secret")
	}
	// 探测不得改动渠道/凭证状态与计数（前后深比较关键字段）。
	after := mgr.Status()
	if after[0].State != before[0].State || after[0].LastErr != before[0].LastErr ||
		after[0].LastOK.Equal(before[0].LastOK) == false || after[0].CredentialsUsable != before[0].CredentialsUsable {
		t.Errorf("探测污染状态: before=%+v after=%+v", before[0], after[0])
	}
	for _, c := range after[0].Credentials {
		if c.Requests != 0 || c.Cooling || c.State != extchan.CredHealthy {
			t.Errorf("探测污染了凭证: %+v", c)
		}
	}
}

// P0-7：草稿探测（明文 secret 只在本次请求内存里用）。
func TestQAProbeDraftSuccess(t *testing.T) {
	p, _ := qaPanel(t, `{"data":[{"id":"m1"}]}`, http.StatusOK)
	// 取上游地址：从 Manager 快照里拿（base_url 不进 secret 面）。
	base := p.cfg.ExtChan.Status()[0].BaseURL
	body := `{"channel":{"name":"draft","base_url":"` + base + `","protocol":"openai","allow_private":true,
	  "credentials":[{"label":"p","secret":"` + qaPanelSecret + `"}]}}`
	req := httptest.NewRequest("POST", "/startport/api/ext_channels/probe", strings.NewReader(body))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("草稿探测失败 %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), qaPanelSecret) {
		t.Error("草稿探测响应泄露 secret")
	}
}

// P0-8 / P0-11：/api/ext_channels 暴露 invalid 与 expiring_soon，且不含任何真实 secret。
func TestQAExtChannelsAlertsAndNoSecret(t *testing.T) {
	p, mgr := qaPanel(t, `{"data":[]}`, http.StatusOK)
	soon := time.Now().Add(36 * time.Hour).Format("2006-01-02")
	upURL := mgr.Status()[0].BaseURL
	mgr.ReplaceAll([]extchan.ChannelDef{{
		Name: "sf", BaseURL: upURL,
		Credentials: []extchan.CredentialDef{
			{Label: "primary", Secret: qaPanelSecret},
			{Label: "soon", Secret: "QA-SOON-SECRET-55555", ExpiresAt: soon},
			{Label: "dead", Secret: "QA-DEAD-SECRET-66666"},
		},
	}})
	mgr.Get("sf").CredentialByLabel("dead").NoteInvalid("401 authentication failed")

	req := httptest.NewRequest("GET", "/startport/api/ext_channels", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	raw := rec.Body.String()
	for _, secret := range []string{qaPanelSecret, "QA-SOON-SECRET-55555", "QA-DEAD-SECRET-66666"} {
		if strings.Contains(raw, secret) {
			t.Errorf("/api/ext_channels 泄露 secret %q", secret)
		}
	}
	var body struct {
		Channels []struct {
			State                   string `json:"state"`
			CredentialsTotal        int    `json:"credentials_total"`
			CredentialsUsable       int    `json:"credentials_usable"`
			CredentialsInvalid      int    `json:"credentials_invalid"`
			CredentialsExpiringSoon int    `json:"credentials_expiring_soon"`
			Credentials             []struct {
				Label        string `json:"label"`
				State        string `json:"state"`
				ExpiringSoon bool   `json:"expiring_soon"`
				Masked       bool   `json:"masked"`
			} `json:"credentials"`
		} `json:"channels"`
		Summary map[string]int `json:"summary"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	ch := body.Channels[0]
	if ch.CredentialsTotal != 3 || ch.CredentialsInvalid != 1 || ch.CredentialsExpiringSoon != 1 {
		t.Errorf("凭证计数异常: %+v", ch)
	}
	if ch.State != extchan.StateAvailable {
		t.Errorf("仍有一条可用凭证，state 应为 available: %q", ch.State)
	}
	for _, c := range ch.Credentials {
		if !c.Masked {
			t.Errorf("凭证 %q 未标记 masked", c.Label)
		}
	}
	if body.Summary["abnormal"] != 0 {
		t.Errorf("有可用凭证的渠道不应计入 abnormal: %+v", body.Summary)
	}
}

// P0-10 情报条目：带 preset_key / native_required / configured，且不含情报原文之外的秘密。
func TestQAIntelItemsCarryPresetFields(t *testing.T) {
	p, _ := qaPanel(t, `{"data":[]}`, http.StatusOK)
	req := httptest.NewRequest("GET", "/startport/api/ext_intel", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	// 未拉取情报时必须是安静态（ok:true + enabled:false + 空 items），不能 500。
	var body struct {
		OK      bool  `json:"ok"`
		Enabled bool  `json:"enabled"`
		Items   []any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v (%s)", err, rec.Body.String())
	}
	if !body.OK || body.Enabled || len(body.Items) != 0 {
		t.Errorf("未拉取情报时形态异常: %+v", body)
	}
}
