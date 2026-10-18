package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RisingInsight/startport/internal/extchan"
)

// newProbePanel 构造挂真实 extchan.Manager 的面板（探针不落盘/不改状态的断言更强）。
func newProbePanel(t *testing.T, upURL string) (*Panel, *extchan.Manager) {
	t.Helper()
	mgr := extchan.New(extchan.Config{TimeoutSeconds: 5})
	mgr.ReplaceAll([]extchan.ChannelDef{{
		Name: "sf", BaseURL: upURL,
		Credentials: []extchan.CredentialDef{
			{Label: "primary", Secret: "sk-panel-secret"},
			{Label: "backup", Secret: "sk-backup-secret"},
		},
	}})
	return New(Config{Version: "test", ExtChan: mgr}), mgr
}

// 探测：只读、不污染渠道状态、不回显 secret。
func TestExtProbeNoStatePollution(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/models") {
			t.Errorf("探测应只打模型目录, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid key sk-panel-secret"}}`))
	}))
	defer up.Close()
	p, mgr := newProbePanel(t, up.URL)
	before := mgr.Status()

	body := `{"channel":{"name":"draft","base_url":"` + up.URL + `","protocol":"openai","allow_private":true,
	  "credentials":[{"label":"p","secret":"sk-panel-secret"}]}}`
	req := httptest.NewRequest("POST", "/startport/api/ext_channels/probe", strings.NewReader(body))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "sk-panel-secret") {
		t.Errorf("探测响应泄露 secret: %s", rec.Body.String())
	}
	var resp struct {
		OK     bool                `json:"ok"`
		Result extchan.ProbeResult `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK || resp.Result.OK {
		t.Error("401 上游应返回 ok:false")
	}
	if resp.Result.ErrorCode != "upstream_401" {
		t.Errorf("error_code = %q", resp.Result.ErrorCode)
	}
	// 状态未被污染（草稿探测根本不进管理器）。
	after := mgr.Status()
	if len(after) != 1 || after[0].State != before[0].State ||
		after[0].CredentialsUsable != before[0].CredentialsUsable ||
		after[0].LastErr != before[0].LastErr {
		t.Errorf("探测污染了渠道状态: before=%+v after=%+v", before[0], after[0])
	}
	for _, c := range after[0].Credentials {
		if c.Requests != 0 {
			t.Errorf("探测不应计入用量: %+v", c)
		}
	}

	// 已保存渠道探测：同样不改状态，且响应不含磁盘 secret。
	req = httptest.NewRequest("POST", "/startport/api/ext_channels/probe",
		strings.NewReader(`{"name":"sf","credential_label":"primary"}`))
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "sk-panel-secret") {
		t.Error("已保存渠道探测泄露 secret")
	}
	after2 := mgr.Status()
	if after2[0].State != before[0].State || after2[0].LastErr != before[0].LastErr {
		t.Error("已保存渠道探测污染了状态")
	}
}

// 探测 SSRF 红线：内网 base_url 默认拒绝，且不发起任何上游请求。
func TestExtProbeRejectsPrivateBaseURL(t *testing.T) {
	var hits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer up.Close()
	p, _ := newProbePanel(t, up.URL)
	// 127.0.0.1 属于内网/回环，未声明 allow_private 时必须拒绝。
	body := `{"channel":{"name":"d","base_url":"http://127.0.0.1:9","credentials":[]}}`
	req := httptest.NewRequest("POST", "/startport/api/ext_channels/probe", strings.NewReader(body))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("内网 base_url 应 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if hits != 0 {
		t.Error("SSRF 拒绝后不应发起上游请求")
	}
	if !strings.Contains(rec.Body.String(), "base_url") {
		t.Errorf("错误信息应指出字段: %s", rec.Body.String())
	}
}

// 探测缺参：既没有 channel/draft 也没有 name → 400（明确语义）。
func TestExtProbeRequiresTarget(t *testing.T) {
	p, _ := newProbePanel(t, "https://api.example.com")
	req := httptest.NewRequest("POST", "/startport/api/ext_channels/probe", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rec.Code)
	}
}

// 预设端点：≥21 条、key 唯一、base_url 恒空。
func TestExtPresetsEndpoint(t *testing.T) {
	p, _ := newProbePanel(t, "https://api.example.com")
	req := httptest.NewRequest("GET", "/startport/api/ext_presets", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var body struct {
		Presets []struct {
			Key            string `json:"key"`
			Protocol       string `json:"protocol"`
			BaseURL        string `json:"base_url"`
			NativeRequired bool   `json:"native_required"`
			Note           string `json:"note"`
		} `json:"presets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Presets) < 21 {
		t.Fatalf("预设数 = %d, want ≥21", len(body.Presets))
	}
	seen := map[string]bool{}
	native := 0
	for _, p := range body.Presets {
		if p.Key == "" || seen[p.Key] {
			t.Errorf("预设 key 空或重复: %q", p.Key)
		}
		seen[p.Key] = true
		if p.BaseURL != "" {
			t.Errorf("预设 %q base_url 必须为空: %q", p.Key, p.BaseURL)
		}
		if p.Protocol != "openai" && p.Protocol != "anthropic" && p.Protocol != "gemini" {
			t.Errorf("预设 %q 协议非法: %q", p.Key, p.Protocol)
		}
		if p.NativeRequired {
			native++
		}
	}
	if native == 0 {
		t.Error("应有 native_required 平台（Qoder/TRAE 等）")
	}
}

// 渠道状态端点：summary 汇总 + 凭证维度（不回显 secret）。
func TestExtChannelsStatusSummary(t *testing.T) {
	p, mgr := newProbePanel(t, "https://api.example.com")
	mgr.Get("sf").CredentialByLabel("backup").NoteInvalid("401")
	req := httptest.NewRequest("GET", "/startport/api/ext_channels", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "sk-panel-secret") {
		t.Error("渠道状态泄露 secret")
	}
	var body struct {
		Channels []extchan.Status `json:"channels"`
		Summary  map[string]int   `json:"summary"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Channels) != 1 {
		t.Fatalf("channels = %d", len(body.Channels))
	}
	if body.Summary["channels"] != 1 || body.Summary["credentials"] != 2 ||
		body.Summary["usable"] != 1 || body.Summary["abnormal"] != 0 {
		t.Errorf("summary 异常: %+v", body.Summary)
	}
}
