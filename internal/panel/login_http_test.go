// login_http_test.go 登录式添加的 **HTTP 栈回归**（v1.7.0 发布修复）：
// Start 的请求 ctx 取消后轮询协程必须存活——旧实现的 ctx 生命周期 bug 只有
// 走真实 panel mux 才能暴露（直接调 Start(ctx=Background) 会掩盖）。
package panel

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/RisingInsight/startport/internal/account"
)

// 回归 1（根因）：HTTP 栈全链——start 请求正常返回（请求 ctx 已死）→ mock
// 第 1 次 202、第 2 次成功 → wait 拿到 success + account_id。
func TestLoginQoderHTTPStackSuccess(t *testing.T) {
	e := newLoginEnv(t)
	rec := e.do("POST", "/startport/api/accounts/login/start", `{"platform":"qoder"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("start status = %d: %s", rec.Code, rec.Body.String())
	}
	var started struct {
		SessionID string `json:"session_id"`
		LoginURL  string `json:"login_url"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &started)
	if started.SessionID == "" || started.LoginURL == "" {
		t.Fatalf("start 结果异常: %+v", started)
	}
	// wait（长轮询）：修复前这里永远 pending（轮询协程从未发请求）。
	rec = e.do("POST", "/startport/api/accounts/login/wait",
		`{"session_id":"`+started.SessionID+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("wait status = %d: %s", rec.Code, rec.Body.String())
	}
	var st struct {
		Status    string `json:"status"`
		AccountID string `json:"account_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	if st.Status != "success" || st.AccountID == "" {
		t.Fatalf("wait 结果异常: %+v（ctx 生命周期 bug 未修？）", st)
	}
	acc := e.store.Get(st.AccountID)
	if acc == nil || acc.Platform != account.PlatformQoder {
		t.Fatalf("账号未入池: %+v", acc)
	}
	var blob map[string]any
	_ = json.Unmarshal(acc.Cred, &blob)
	if blob["access_token"] != "dt-LGN" {
		t.Errorf("blob 异常: %s", acc.Cred)
	}
}

// 回归 2：Cancel 后轮询协程停止（mock 计数不再增长——不留泄漏）。
func TestLoginQoderCancelStopsPoller(t *testing.T) {
	e := newLoginEnv(t)
	e.pollAlways202.Store(true) // 保持 pending，便于观察轮询计数
	rec := e.do("POST", "/startport/api/accounts/login/start", `{"platform":"qoder"}`)
	var started struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &started)
	// 等几个轮询节拍（PollEvery=30ms）。
	time.Sleep(300 * time.Millisecond)
	before := e.pollCalls.Load()
	// 用户取消（路径形态——acct.js 同款）。
	if rec := e.do("POST", "/startport/api/accounts/login/"+started.SessionID+"/cancel", ""); rec.Code != http.StatusOK {
		t.Fatalf("cancel status = %d", rec.Code)
	}
	time.Sleep(300 * time.Millisecond)
	after := e.pollCalls.Load()
	if after != before {
		t.Errorf("取消后轮询未停止: before=%d after=%d", before, after)
	}
	// Wait → 404（规格 §3.2 取消语义）。
	if rec := e.do("POST", "/startport/api/accounts/login/wait",
		`{"session_id":"`+started.SessionID+`"}`); rec.Code != http.StatusNotFound {
		t.Errorf("取消后 wait 应 404: %d", rec.Code)
	}
}
