// qa_v120_test.go QA 独立验收（v1.2.0 面板侧）：adapter 透出、向导接线、探测。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/RisingInsight/startport/internal/extchan"
)

const qaPanelLoomySession = "qa-loomy-panel-session-555666"

// qaLoomyPanel 构造挂 Loomy 渠道的面板。
func qaLoomyPanel(t *testing.T, upURL string) (*Panel, *extchan.Manager) {
	t.Helper()
	mgr := extchan.New(extchan.Config{TimeoutSeconds: 5})
	mgr.ReplaceAll([]extchan.ChannelDef{{
		Name: "loomy", BaseURL: upURL + "/api/v1", Protocol: "openai", Adapter: "loomy",
		Credentials: []extchan.CredentialDef{{Label: "primary", Secret: qaPanelLoomySession}},
	}})
	return New(Config{Version: "test", ExtChan: mgr}), mgr
}

// 必测 9：/api/ext_channels 透出 adapter，且 0 命中 session。
func TestQAPanelChannelsExposeAdapterNoSecret(t *testing.T) {
	p, _ := qaLoomyPanel(t, "https://loomyad.xunfei.cn")
	req := httptest.NewRequest("GET", "/startport/api/ext_channels", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), qaPanelLoomySession) {
		t.Error("/api/ext_channels 泄露 session")
	}
	var body struct {
		Channels []struct {
			Name     string `json:"name"`
			Protocol string `json:"protocol"`
			Adapter  string `json:"adapter"`
			Creds    []struct {
				Label  string `json:"label"`
				Masked bool   `json:"masked"`
			} `json:"credentials"`
		} `json:"channels"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Channels) != 1 {
		t.Fatalf("channels = %d", len(body.Channels))
	}
	ch := body.Channels[0]
	if ch.Adapter != "loomy" || ch.Protocol != "openai" {
		t.Errorf("adapter/protocol = %q/%q", ch.Adapter, ch.Protocol)
	}
	if len(ch.Creds) != 1 || !ch.Creds[0].Masked {
		t.Errorf("凭证应 masked: %+v", ch.Creds)
	}
}

// 必测 9：/api/ext_presets 的 loomy 条目带 adapter，且不再 native_required。
func TestQAPanelPresetsExposeLoomyAdapter(t *testing.T) {
	p, _ := qaLoomyPanel(t, "https://loomyad.xunfei.cn")
	req := httptest.NewRequest("GET", "/startport/api/ext_presets", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var body struct {
		Presets []struct {
			Key            string `json:"key"`
			Adapter        string `json:"adapter"`
			NativeRequired bool   `json:"native_required"`
			BaseURL        string `json:"base_url"`
			Auth           struct {
				Mode string `json:"mode"`
			} `json:"auth"`
			Note string `json:"note"`
		} `json:"presets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, pr := range body.Presets {
		if pr.Key != "loomy" {
			continue
		}
		found = true
		if pr.Adapter != "loomy" {
			t.Errorf("loomy 预设 adapter = %q", pr.Adapter)
		}
		if pr.NativeRequired {
			t.Error("loomy 已由原生适配器覆盖，不应 native_required")
		}
		if pr.BaseURL != "" {
			t.Errorf("预设 base_url 必须为空: %q", pr.BaseURL)
		}
		if pr.Auth.Mode != "" {
			t.Errorf("loomy 预设不应声明 auth（适配器独占）: %q", pr.Auth.Mode)
		}
		if !strings.Contains(pr.Note, "14") {
			t.Errorf("预设备注应提示 14 天有效期: %s", pr.Note)
		}
	}
	if !found {
		t.Fatal("预设目录缺 loomy")
	}
}

// 必测 7/9：向导草稿探测（adapter=loomy）走 {base}/models + 双头。
func TestQAPanelProbeLoomyDraft(t *testing.T) {
	var sawPath, sawToken, sawAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		sawToken = r.Header.Get("token")
		sawAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"spark-x"}]}`))
	}))
	defer up.Close()
	p, _ := qaLoomyPanel(t, "https://loomyad.xunfei.cn")
	body := `{"channel":{"name":"loomy","base_url":"` + up.URL + `/api/v1","protocol":"openai",
	  "adapter":"loomy","allow_private":true,
	  "credentials":[{"label":"primary","secret":"` + qaPanelLoomySession + `"}]}}`
	req := httptest.NewRequest("POST", "/startport/api/ext_channels/probe", strings.NewReader(body))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), qaPanelLoomySession) {
		t.Error("探测响应泄露 session")
	}
	var resp struct {
		OK     bool                `json:"ok"`
		Result extchan.ProbeResult `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Result.ModelsCount != 1 {
		t.Fatalf("Loomy 草稿探测应成功: %+v", resp.Result)
	}
	if resp.Result.Adapter != "loomy" {
		t.Errorf("ProbeResult.Adapter = %q", resp.Result.Adapter)
	}
	if sawPath != "/api/v1/models" || sawToken != qaPanelLoomySession ||
		sawAuth != "Bearer "+qaPanelLoomySession {
		t.Errorf("探测形态异常: path=%q token=%q auth=%q", sawPath, sawToken, sawAuth)
	}
}

// 必测 9：向导/卡片接线（静态断言）。
func TestQAWizardAdapterWiring(t *testing.T) {
	htmlBytes, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(htmlBytes)
	// 下拉存在且有「默认 + loomy」两项。
	idx := strings.Index(html, `id="ewAdapter"`)
	if idx < 0 {
		t.Fatal("index.html 缺 ewAdapter 下拉")
	}
	sel := html[idx:]
	sel = sel[:strings.Index(sel, "</select>")]
	for _, want := range []string{`value=""`, `value="loomy"`} {
		if !strings.Contains(sel, want) {
			t.Errorf("ewAdapter 缺选项 %q", want)
		}
	}
	// 置灰提示元素存在。
	if !strings.Contains(html, `id="ewAuthHint"`) {
		t.Error("缺 ewAuthHint（置灰说明）")
	}

	wizJS, err := os.ReadFile("extwiz.js")
	if err != nil {
		t.Fatal(err)
	}
	wiz := string(wizJS)
	// ewSyncAdapter 必须真的把三个 auth 控件置灰，并在选中时清空 mode。
	syncIdx := strings.Index(wiz, "function ewSyncAdapter()")
	if syncIdx < 0 {
		t.Fatal("extwiz.js 缺 ewSyncAdapter")
	}
	syncBody := wiz[syncIdx:]
	syncBody = syncBody[:strings.Index(syncBody, "\n}")]
	for _, want := range []string{"ewAuthMode", "ewAuthParam", "ewAuthHeader", "disabled = on", "el.value = ''"} {
		if !strings.Contains(syncBody, want) {
			t.Errorf("ewSyncAdapter 缺关键逻辑 %q", want)
		}
	}
	// 收集表单时 adapter 非空则不发 auth（服务端会拒绝两者并存）。
	collectIdx := strings.Index(wiz, "function ewCollect(")
	collect := wiz[collectIdx:]
	collect = collect[:strings.Index(collect, "\n}")]
	if !strings.Contains(collect, "ch.adapter") {
		t.Error("ewCollect 未提交 adapter")
	}
	if !strings.Contains(collect, "adapter ? ''") {
		t.Error("ewCollect 在 adapter 非空时未禁用 auth")
	}

	extJS, err := os.ReadFile("ext.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(extJS), "c.adapter") {
		t.Error("ext.js 未渲染 adapter 徽标")
	}
	// 前端任何文件都不得出现 session 明文渲染路径。
	for _, bad := range []string{".secret", "credentials[].secret"} {
		if strings.Contains(string(extJS), bad) {
			t.Errorf("ext.js 出现敏感字段引用 %q", bad)
		}
	}
}
