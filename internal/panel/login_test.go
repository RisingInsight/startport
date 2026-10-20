// login_test.go 登录式添加的面板 API + 投影冒烟（v1.7.0；httptest mock，
// 零真实网络）。入池 → 投影 → 引擎可见一条龙。
package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RisingInsight/startport/internal/acclogin"
	"github.com/RisingInsight/startport/internal/account"
	"github.com/RisingInsight/startport/internal/acctchan"
	"github.com/RisingInsight/startport/internal/extchan"
)

// loginEnv 登录 API 的测试环境（store + 投影 + mock 端点 + Manager 全真链）。
type loginEnv struct {
	*acctEnv
	mgr           *acclogin.Manager
	mock          *httptest.Server
	pollCalls     atomic.Int64 // qoder poll 请求计数（取消停轮询断言用）
	pollAlways202 atomic.Bool  // true = poll 恒 202（保持 pending）
}

func newLoginEnv(t *testing.T) *loginEnv {
	e := &loginEnv{acctEnv: newAcctEnv(t)}
	// mock 上游：qoder poll（持续 202；测试用 Manager 内部直连不改状态）+
	// autoclaw send/login。
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/deviceToken/poll", func(w http.ResponseWriter, r *http.Request) {
		n := e.pollCalls.Add(1)
		if e.pollAlways202.Load() || n == 1 {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"token":"dt-LGN","refresh_token":"rt-LGN","user_id":"u-777"}`))
	})
	mux.HandleFunc("GET /api/v1/userinfo", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"u-777","name":"LGN 用户"}`))
	})
	mux.HandleFunc("POST /userapi/v1/agent-send-code", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"result":true}}`))
	})
	mux.HandleFunc("POST /userapi/v1/agent-login/", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Code any `json:"code"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if n, ok := body.Code.(float64); ok && n == 123456 {
			_, _ = w.Write([]byte(`{"code":0,"data":{"access_token":"ac-LGN","refresh_token":"rt-LGN"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":630202,"msg":"验证码错误"}`))
	})
	e.mock = httptest.NewServer(mux)

	var defs *[]extchan.ChannelDef
	defs = &e.defs
	mgr, err := acclogin.NewManager(acclogin.Config{
		Store:           e.store,
		TTL:             2 * time.Second,
		PollEvery:       30 * time.Millisecond,
		MachineIDFile:   t.TempDir() + "/qoder-machine-id",
		QoderOpenAPI:    map[bool]string{false: e.mock.URL, true: e.mock.URL},
		QoderWeb:        map[bool]string{false: e.mock.URL, true: e.mock.URL},
		AutoClawUserAPI: e.mock.URL,
		Logf:            func(string, ...any) {},
		AfterPool: func(platform, accountID string) {
			// 与 main 的装配一致：入池 → acctchan.Sync 投影。
			_, _ = acctchan.Sync(e.store,
				func() []extchan.ChannelDef { return *defs },
				func(next []extchan.ChannelDef) error { *defs = next; e.saves++; e.extMgr.ReplaceAll(next); return nil },
				func(string, ...any) {})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.mgr = mgr
	e.pan.cfg.Login = mgr
	t.Cleanup(mgr.Close)
	return e
}

// 登录 → 入池 → 投影冒烟：autoclaw 短信登录全链（start→verify→success），
// managed 渠道出现且凭证 label = account.ID；响应零 secret。
func TestLoginAutoClawSMSProjected(t *testing.T) {
	e := newLoginEnv(t)
	rec := e.do("POST", "/startport/api/accounts/login/start",
		`{"platform":"autoclaw","phone":"+86 138-0000-8000"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("start status = %d: %s", rec.Code, rec.Body.String())
	}
	var started struct {
		SessionID string `json:"session_id"`
		Mode      string `json:"mode"`
		PhoneSent bool   `json:"phone_sent"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &started)
	if started.Mode != "sms" || !started.PhoneSent || started.SessionID == "" {
		t.Fatalf("start 结果异常: %+v", started)
	}
	// 验证成功 → 入池 + 投影。
	rec = e.do("POST", "/startport/api/accounts/login/sms/verify",
		`{"session_id":"`+started.SessionID+`","code":"123456"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("verify status = %d: %s", rec.Code, rec.Body.String())
	}
	var done struct {
		Status    string `json:"status"`
		AccountID string `json:"account_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &done)
	if done.Status != "success" || done.AccountID == "" {
		t.Fatalf("verify 结果异常: %+v", done)
	}
	// 投影：managed autoclaw 渠道 + credentials[label=accountID]。
	found := false
	for _, def := range e.defs {
		if def.Name == account.PlatformAutoClaw {
			found = true
			if !def.Managed || len(def.Credentials) != 1 || def.Credentials[0].Label != done.AccountID {
				t.Errorf("投影异常: %+v", def)
			}
		}
	}
	if !found {
		t.Error("managed 渠道未创建")
	}
	// 引擎可见（热重载链通）。
	seen := false
	for _, st := range e.extMgr.Status() {
		if st.Name == account.PlatformAutoClaw && len(st.Credentials) == 1 {
			seen = true
		}
	}
	if !seen {
		t.Error("引擎热重载断链")
	}
	// 响应零 secret。
	if strings.Contains(rec.Body.String(), "ac-LGN") {
		t.Error("verify 响应泄露 token")
	}
}

// qoder start 的授权 URL 形态（mode=auth_url；零 verifier 泄露）。
func TestLoginQoderStartShape(t *testing.T) {
	e := newLoginEnv(t)
	rec := e.do("POST", "/startport/api/accounts/login/start", `{"platform":"qoder"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("start status = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		SessionID string `json:"session_id"`
		Mode      string `json:"mode"`
		LoginURL  string `json:"login_url"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Mode != "auth_url" || !strings.Contains(out.LoginURL, "/device/selectAccounts") {
		t.Errorf("start 结果异常: %+v", out)
	}
	// region=intl 归一到 qoder-intl（两条同构）。
	rec = e.do("POST", "/startport/api/accounts/login/start", `{"platform":"qoder","region":"intl"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("intl start status = %d", rec.Code)
	}
}

// 不支持平台 / 未启用 / 错误验证码的边界。
func TestLoginBoundaries(t *testing.T) {
	e := newLoginEnv(t)
	// 不支持平台（trae v1.7.6 起走浏览器登录，用无登录流程的 loomy 验边界）。
	if rec := e.do("POST", "/startport/api/accounts/login/start", `{"platform":"loomy"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("loomy 应 400: %d", rec.Code)
	}
	// 错误验证码 → 400 + 码表文案。
	rec := e.do("POST", "/startport/api/accounts/login/start", `{"platform":"autoclaw","phone":"13800008000"}`)
	var started struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &started)
	rec = e.do("POST", "/startport/api/accounts/login/sms/verify",
		`{"session_id":"`+started.SessionID+`","code":"000000"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "验证码不正确") {
		t.Errorf("错码应 400 + 码表文案: %d %s", rec.Code, rec.Body.String())
	}
	// 未知会话 → 404。
	if rec := e.do("POST", "/startport/api/accounts/login/wait", `{"session_id":"lgn-nope"}`); rec.Code != http.StatusNotFound {
		t.Errorf("未知会话应 404: %d", rec.Code)
	}
	// Login 未启用 → 501。
	bare := New(Config{Version: "test"})
	lrec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/startport/api/accounts/login/start", strings.NewReader(`{"platform":"qoder"}`))
	bare.ServeHTTP(lrec, req)
	if lrec.Code != http.StatusNotImplemented {
		t.Errorf("未启用应 501: %d", lrec.Code)
	}
}

// 路径参数形态（acct.js 契约）：GET wait / POST {sid}/sms_verify / {sid}/cancel
// 与 body 形态等价；capabilities 下发支持平台。
func TestLoginPathAliasContract(t *testing.T) {
	e := newLoginEnv(t)
	// capabilities。
	rec := e.do("GET", "/startport/api/accounts/login/capabilities", "")
	var caps struct {
		Platforms []string `json:"platforms"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &caps)
	want := map[string]bool{"qoder": true, "qoder-intl": true, "autoclaw": true, "autoclaw-intl": true, "trae": true}
	if len(caps.Platforms) != 5 {
		t.Fatalf("capabilities = %v", caps.Platforms)
	}
	for _, p := range caps.Platforms {
		if !want[p] {
			t.Errorf("未知平台 %q", p)
		}
	}
	// autoclaw 短信链：start → 路径形态 verify → 路径形态 wait 拿 account_id。
	rec = e.do("POST", "/startport/api/accounts/login/start",
		`{"platform":"autoclaw","phone":"13800008000"}`)
	var started struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &started)
	rec = e.do("POST", "/startport/api/accounts/login/"+started.SessionID+"/sms_verify",
		`{"code":"123456"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("路径形态 verify status = %d: %s", rec.Code, rec.Body.String())
	}
	rec = e.do("GET", "/startport/api/accounts/login/"+started.SessionID+"/wait", "")
	var st struct {
		Status    string `json:"status"`
		AccountID string `json:"account_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	if st.Status != "success" || st.AccountID == "" {
		t.Errorf("路径形态 wait 异常: %+v", st)
	}
	// 路径形态 cancel：对不存在会话 404。
	if rec := e.do("POST", "/startport/api/accounts/login/lgn-nope/cancel", ""); rec.Code != http.StatusNotFound {
		t.Errorf("未知会话 cancel 应 404: %d", rec.Code)
	}
	// qoder：GET 路径形态 wait 在 pending 阶段可用。
	rec = e.do("POST", "/startport/api/accounts/login/start", `{"platform":"qoder"}`)
	_ = json.Unmarshal(rec.Body.Bytes(), &started)
	rec = e.do("GET", "/startport/api/accounts/login/"+started.SessionID+"/wait", "")
	if rec.Code != http.StatusOK {
		t.Errorf("qoder 路径形态 wait status = %d", rec.Code)
	}
}

// 冒烟修复回归：verify 的 code 字段**数字/字符串双兼容**（冻结契约=前端传
// 数字；字符串形态同样走通）；非法形态 → 400 格式错误。两态（body/路径）
// 都覆盖。
func TestLoginSMSVerifyCodeDualForm(t *testing.T) {
	// 数字形态（冻结契约）：全链走通。
	e := newLoginEnv(t)
	rec := e.do("POST", "/startport/api/accounts/login/start", `{"platform":"autoclaw","phone":"13800008000"}`)
	var started struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &started)
	rec = e.do("POST", "/startport/api/accounts/login/sms/verify",
		`{"session_id":"`+started.SessionID+`","code":123456}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("数字 code 应走通: %d %s", rec.Code, rec.Body.String())
	}
	var st struct {
		Status    string `json:"status"`
		AccountID string `json:"account_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	if st.Status != "success" || st.AccountID == "" {
		t.Fatalf("数字形态结果异常: %+v", st)
	}

	// 字符串形态：同样走通（手工调用兼容）。
	rec = e.do("POST", "/startport/api/accounts/login/start", `{"platform":"autoclaw","phone":"13900009000"}`)
	_ = json.Unmarshal(rec.Body.Bytes(), &started)
	rec = e.do("POST", "/startport/api/accounts/login/"+started.SessionID+"/sms_verify",
		`{"code":"123456"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("字符串 code 应走通: %d %s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	if st.Status != "success" || st.AccountID == "" {
		t.Fatalf("字符串形态结果异常: %+v", st)
	}

	// 非法形态（12a456 / 浮点 / 负数）→ 400 格式错误，会话保留可重输。
	for i, bad := range []string{`"12a456"`, `1.5`, `-123456`, `""`, `null`} {
		// 每轮独立手机号：label 兜底=脱敏手机号（同平台唯一），避免相互污染。
		phone := fmt.Sprintf("1370000700%d", i)
		rec = e.do("POST", "/startport/api/accounts/login/start", `{"platform":"autoclaw","phone":"`+phone+`"}`)
		_ = json.Unmarshal(rec.Body.Bytes(), &started)
		rec = e.do("POST", "/startport/api/accounts/login/"+started.SessionID+"/sms_verify",
			`{"code":`+bad+`}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "验证码格式错误") {
			t.Errorf("非法 code %s 应 400 格式错误: %d %s", bad, rec.Code, rec.Body.String())
			continue
		}
		// 会话保留：重输合法码仍可成功（错误形态不销毁会话）。
		rec = e.do("POST", "/startport/api/accounts/login/"+started.SessionID+"/sms_verify",
			`{"code":"123456"}`)
		if rec.Code != http.StatusOK {
			t.Errorf("非法形态后重输合法码应走通: %d %s", rec.Code, rec.Body.String())
		}
	}
}
