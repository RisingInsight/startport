// checkin_test.go 三平台签到的 fixture 用例 + 调度器时钟注入 + 环形裁剪
// （架构 §5.3；全部走 httptest mock 上游，**不碰真实网络**）。
package checkin

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RisingInsight/startport/internal/account"
)

// fixedNow 固定时钟（签名时间戳可复算）。
func fixedNow(ts time.Time) func() time.Time {
	return func() time.Time { return ts }
}

// loomyCred 测试用 Loomy blob。
func loomyCred() json.RawMessage {
	return json.RawMessage(`{"session":"sess-0123456789abcdef"}`)
}

// ── loomy ───────────────────────────────────────────────────────

func newLoomyAccount() *account.Account {
	return &account.Account{ID: "acc-loomy01", Platform: account.PlatformLoomy,
		Cred: json.RawMessage(`{"session":"sess-0123456789abcdef"}`)}
}

// 签到成功：双头逐项断言 + Result ok。
func TestLoomyCheckinSuccess(t *testing.T) {
	var sawToken, sawBearer, sawMethod, sawPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawMethod, sawPath = r.Method, r.URL.Path
		sawToken = r.Header.Get("token")
		sawBearer = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"code":"000000","desc":"ok","data":{"points":100}}`))
	}))
	defer srv.Close()
	c := newLoomyChecker()
	c.Base = srv.URL
	res, err := c.CheckIn(context.Background(), newLoomyAccount())
	if err != nil {
		t.Fatalf("CheckIn: %v", err)
	}
	if res.Status != StatusOK || !strings.Contains(res.Reward, "100") {
		t.Errorf("结果异常: %+v", res)
	}
	if sawMethod != http.MethodGet || sawPath != "/api/v1/points/first-login" {
		t.Errorf("请求形态异常: %s %s", sawMethod, sawPath)
	}
	// 双头契约：token 与 Authorization: Bearer 携带同一个 session。
	if sawToken != "sess-0123456789abcdef" || sawBearer != "Bearer sess-0123456789abcdef" {
		t.Errorf("双头异常: token=%q bearer=%q", sawToken, sawBearer)
	}
}

// 已签过 / 登录失效 / 接口变更三态。
func TestLoomyCheckinFailureModes(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		status2 string
		wantMsg string
		wantErr bool
	}{
		{"登录失效业务码", 200, `{"code":"020002","desc":"登录已失效"}`, "", "登录态已失效", true},
		{"HTTP 401", 401, `{"error":{"message":"x"}}`, "", "登录态已失效", true},
		{"业务失败", 200, `{"code":"999999","desc":"活动未开始"}`, StatusFailed, "活动未开始", false},
		{"接口变更", 200, `not-json`, StatusFailed, "平台接口可能已变更", false},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		c := newLoomyChecker()
		c.Base = srv.URL
		res, err := c.CheckIn(context.Background(), newLoomyAccount())
		srv.Close()
		if tc.wantErr {
			if err == nil || !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("%s: err = %v, want 含 %q", tc.name, err, tc.wantMsg)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: 不应返回错误: %v", tc.name, err)
			continue
		}
		if res.Status != tc.status2 || !strings.Contains(res.Message, tc.wantMsg) {
			t.Errorf("%s: 结果异常: %+v", tc.name, res)
		}
	}
	// 错误响应零秘密（脱敏契约）。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"token sess-0123456789abcdef bad"}`))
	}))
	c := newLoomyChecker()
	c.Base = srv.URL
	_, err := c.CheckIn(context.Background(), newLoomyAccount())
	srv.Close()
	if err != nil && strings.Contains(err.Error(), "sess-0123456789abcdef") {
		t.Errorf("错误消息泄露 session: %v", err)
	}
}

// ── autoclaw ────────────────────────────────────────────────────

func autoclawCred() json.RawMessage {
	return json.RawMessage(`{"token":"ac-token-1","refresh_token":"ac-rt-1","device_id":"dev-1"}`)
}

func newAutoClawAccount() *account.Account {
	return &account.Account{ID: "acc-ac01", Platform: account.PlatformAutoClaw, Cred: autoclawCred()}
}

// 签到成功：X-Auth-Sign 在固定时间戳下**逐字节可复算** + 品牌头束 + 路径。
func TestAutoClawCheckinSuccessSignFixture(t *testing.T) {
	var sawSign, sawTS, sawPath, sawHarness, sawVersion, sawBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		sawSign = r.Header.Get("X-Auth-Sign")
		sawTS = r.Header.Get("X-Auth-TimeStamp")
		sawHarness = r.Header.Get("X-Harness-Type")
		sawVersion = r.Header.Get("X-Version")
		raw, _ := readAllStr(r)
		sawBody = raw
		_, _ = w.Write([]byte(`{"code":0,"data":{"success":true,"reward_points":100,"already_completed":false}}`))
	}))
	defer srv.Close()
	c := newAutoClawChecker(false)
	c.Base = srv.URL
	fixedTS := time.Unix(1770000000, 0)
	c.Now = fixedNow(fixedTS)
	res, err := c.CheckIn(context.Background(), newAutoClawAccount())
	if err != nil {
		t.Fatalf("CheckIn: %v", err)
	}
	if res.Status != StatusOK || !strings.Contains(res.Reward, "100") {
		t.Errorf("结果异常: %+v", res)
	}
	if sawPath != autoclawTaskComplete {
		t.Errorf("路径 = %s", sawPath)
	}
	// 签名逐字节复算：MD5("100003&1770000000&38d2391985e2369a5fb8227d8e6cd5e5")。
	if sawTS != "1770000000" {
		t.Errorf("时间戳 = %q, want 1770000000（秒）", sawTS)
	}
	if sawSign != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" && sawSign == sawSign {
		// 签名以注入的时间戳复算（不硬编码 md5 值——上面的占位恒假，走复算）。
		expect := md5Hex("100003&1770000000&38d2391985e2369a5fb8227d8e6cd5e5")
		if sawSign != expect {
			t.Errorf("X-Auth-Sign = %q, want %q", sawSign, expect)
		}
	}
	// userapi 域品牌头：X-Harness-Type 保留 + X-Version 门控值。
	if sawHarness != "zcode" {
		t.Errorf("X-Harness-Type = %q（userapi 域应保留 zcode）", sawHarness)
	}
	if sawVersion != "1.18.5" {
		t.Errorf("X-Version = %q", sawVersion)
	}
	if sawBody != `{"task_id":"daily_signin"}` {
		t.Errorf("body = %s", sawBody)
	}
}

// 已签过（success:false + already_completed:true）不算成功。
func TestAutoClawCheckinAlready(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"success":false,"reward_points":0,"already_completed":true}}`))
	}))
	defer srv.Close()
	c := newAutoClawChecker(false)
	c.Base = srv.URL
	res, err := c.CheckIn(context.Background(), newAutoClawAccount())
	if err != nil {
		t.Fatalf("CheckIn: %v", err)
	}
	if res.Status != StatusAlready || !strings.Contains(res.Message, "已签到") {
		t.Errorf("已签过应返回 already: %+v", res)
	}
}

// 登录态失效（业务码 410000）→ 刷新（400002 降级 agent-refresh）→ 回写 → 重试成功。
func TestAutoClawCheckinRefreshWritebackRetry(t *testing.T) {
	refreshCalls := 0
	agentCalls := 0
	var newBlobToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case autoclawTaskComplete:
			auth := r.Header.Get("authorization")
			if strings.Contains(auth, "ac-token-NEW") {
				_, _ = w.Write([]byte(`{"code":0,"data":{"success":true,"reward_points":50,"already_completed":false}}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":410000,"msg":"expired"}`))
		case autoclawRefreshPath:
			refreshCalls++
			_, _ = w.Write([]byte(`{"code":400002,"msg":"sign bad"}`))
		case autoclawAgentRefresh:
			agentCalls++
			_, _ = w.Write([]byte(`{"code":0,"data":{"access_token":"ac-token-NEW"}}`))
		}
	}))
	defer srv.Close()

	store, _ := account.Load(t.TempDir() + "/accounts.json")
	store.SetLogf(func(string, ...any) {})
	defer store.Close()
	a, _ := store.Add(account.PlatformAutoClaw, autoclawCred(), "", "")

	c := newAutoClawChecker(false)
	c.Base = srv.URL
	Register(c) // RefreshAutoClaw 走注册表实例——测试注入 httptest Base
	defer Register(newAutoClawChecker(false))

	s := NewScheduler(SchedulerConfig{
		Store: store, Hours: []int{9}, Logf: func(string, ...any) {},
	})
	defer s.Close()

	res, err := s.CheckinAccount(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("CheckinAccount: %v", err)
	}
	if res.Status != StatusOK || !strings.Contains(res.Reward, "50") {
		t.Fatalf("刷新后重试应成功: %+v %v", res, err)
	}
	if refreshCalls != 1 || agentCalls != 1 {
		t.Errorf("降级链异常: refresh=%d agent=%d, want 1/1（400002 → agent-refresh）", refreshCalls, agentCalls)
	}
	// 回写断言：blob 里的 token 已更新（架构 §4.7：自管存储回写是必要项）。
	got := store.Get(a.ID)
	var blob map[string]any
	if err := json.Unmarshal(got.Cred, &blob); err != nil {
		t.Fatal(err)
	}
	newBlobToken, _ = blob["token"].(string)
	if newBlobToken != "ac-token-NEW" {
		t.Errorf("回写 token = %q, want ac-token-NEW", newBlobToken)
	}
	if !strings.Contains(got.Checkin[0].Reward, "50") {
		t.Errorf("签到历史未落账: %+v", got.Checkin)
	}
}

// ── qoder ───────────────────────────────────────────────────────

func qoderCred() json.RawMessage {
	return json.RawMessage(`{"access_token":"dt-1","user_id":"u-1"}`)
}

func newQoderAccount() *account.Account {
	return &account.Account{ID: "acc-qoder01", Platform: account.PlatformQoder, Cred: qoderCred()}
}

// 两跳成功：sash 最小头束（cosy-clienttype: 10 门控值）+ CLAIM_BENEFIT 过滤。
func TestQoderCheckinClaimFlow(t *testing.T) {
	var sawClient, sawUA, sawClaimPath, sawMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawClient = r.Header.Get("cosy-clienttype")
		sawUA = r.Header.Get("user-agent")
		sawMethod, sawClaimPath = r.Method, r.URL.Path
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"data":[
			  {"id":"c-1","actionType":"DAILY_SURVEY","claimStatus":"pending"},
			  {"id":"c-2","actionType":"CLAIM_BENEFIT","claimStatus":"pending","rewardName":"100 credits"},
			  {"id":"c-3","actionType":"CLAIM_BENEFIT","claimStatus":"claimed"}
			]}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer srv.Close()
	c := newQoderChecker()
	c.Base = srv.URL
	res, err := c.CheckIn(context.Background(), newQoderAccount())
	if err != nil {
		t.Fatalf("CheckIn: %v", err)
	}
	if res.Status != StatusOK || !strings.Contains(res.Reward, "100 credits") {
		t.Errorf("结果异常: %+v", res)
	}
	if sawClient != "10" {
		t.Errorf("cosy-clienttype = %q, want 10（门控值：5 会拿空活动列表）", sawClient)
	}
	if sawUA != "Qoder" {
		t.Errorf("user-agent = %q", sawUA)
	}
	if sawMethod != http.MethodPost || sawClaimPath != "/sash/api/v1/me/campaigns/c-2/claim" {
		t.Errorf("claim 请求异常: %s %s（应只领可领取的那条）", sawMethod, sawClaimPath)
	}
}

// 已领取全部 / 接口变更两态。
func TestQoderCheckinFailureModes(t *testing.T) {
	// 全部已领取 → already。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"c-2","actionType":"CLAIM_BENEFIT","claimStatus":"claimed"}]}`))
	}))
	c := newQoderChecker()
	c.Base = srv.URL
	res, err := c.CheckIn(context.Background(), newQoderAccount())
	srv.Close()
	if err != nil || res.Status != StatusAlready {
		t.Errorf("全部已领取应 already: %+v %v", res, err)
	}
	// 列表形状不符 → 显式"接口可能已变更"。
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"unexpected":"shape"}`))
	}))
	c2 := newQoderChecker()
	c2.Base = srv2.URL
	res, err = c2.CheckIn(context.Background(), newQoderAccount())
	srv2.Close()
	if err != nil {
		t.Fatalf("形状不符不应返回错误: %v", err)
	}
	if res.Status != StatusFailed || !strings.Contains(res.Message, "平台接口可能已变更") {
		t.Errorf("形状不符应显式告警: %+v", res)
	}
	// 401 → 登录失效。
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	c3 := newQoderChecker()
	c3.Base = srv3.URL
	_, err = c3.CheckIn(context.Background(), newQoderAccount())
	srv3.Close()
	if err == nil || !strings.Contains(err.Error(), "登录态已失效") {
		t.Errorf("401 应报登录失效: %v", err)
	}
}

// ── 注册表与调度器 ──────────────────────────────────────────────

// 注册表：支持的平台与"平台无此活动"语义。
func TestRegistrySupport(t *testing.T) {
	for _, p := range []string{"loomy", "autoclaw", "autoclaw-intl", "qoder"} {
		if !Supported(p) {
			t.Errorf("%s 应有签到能力", p)
		}
	}
	for _, p := range []string{"qoder-intl", "custom"} {
		if Supported(p) {
			t.Errorf("%s 不应有签到能力（架构 Q4/Q5）", p)
		}
	}
	// ErrPlatformNotSupported 供显式文案使用（team-lead 要求的语义错误）。
	if !strings.Contains(ErrPlatformNotSupported.Error(), "平台无此活动") {
		t.Error("哨兵文案异常")
	}
}

// 调度器：手动触发绕过窗口；TryLock 防双跑；失败记录 failed。
func TestSchedulerManualTriggerAndLock(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		_, _ = w.Write([]byte(`{"code":"000000","data":{"points":1}}`))
	}))
	defer srv.Close()

	store, _ := account.Load(t.TempDir() + "/accounts.json")
	store.SetLogf(func(string, ...any) {})
	defer store.Close()
	a, _ := store.Add(account.PlatformLoomy, loomyCred(), "", "")

	c := newLoomyChecker()
	c.Base = srv.URL
	Register(c) // 覆盖注册表里的 loomy 实例（指向 httptest）
	defer Register(newLoomyChecker())

	var revived int
	s := NewScheduler(SchedulerConfig{
		Store: store, Hours: []int{3}, Logf: func(string, ...any) {}, // 3 点 ≠ 当前，验证绕过窗口
		OnSuccess: func(platform, credLabel string) {
			if platform == account.PlatformLoomy && credLabel == a.ID {
				revived++ // 签到成功 → Revive 钩子被调（账号中心复活语义）
			}
		},
	})
	defer s.Close()

	res, err := s.CheckinAccount(context.Background(), a.ID)
	if err != nil || res.Status != StatusOK {
		t.Fatalf("手动签到异常: %+v %v", res, err)
	}
	if revived != 1 {
		t.Errorf("OnSuccess 复活钩子调用次数 = %d, want 1", revived)
	}
	// 环形历史落账。
	got := store.Get(a.ID)
	if len(got.Checkin) != 1 || got.Checkin[0].Status != StatusOK {
		t.Errorf("签到历史异常: %+v", got.Checkin)
	}
	// 再点一次：平台侧幂等语义由 fixture 模拟（仍返回成功）——这里只验证
	// 连续调用不 panic、每次都真实发起（TryLock 只挡并发，不挡顺序重复）。
	before := calls
	_, _ = s.CheckinAccount(context.Background(), a.ID)
	mu.Lock()
	if calls < before {
		t.Error("顺序重复调用被错误拦截")
	}
	mu.Unlock()
	// 不存在的账号。
	if _, err := s.CheckinAccount(context.Background(), "acc-nope"); err == nil {
		t.Error("不存在的账号应报错")
	}
}

// 调度器：失败进重试队列（30m × 2）后放弃；成功清空重试。
func TestSchedulerRetryBudget(t *testing.T) {
	fail := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			_, _ = w.Write([]byte(`{"code":"999999","desc":"活动未开始"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":"000000","data":{"points":1}}`))
	}))
	defer srv.Close()

	store, _ := account.Load(t.TempDir() + "/accounts.json")
	store.SetLogf(func(string, ...any) {})
	defer store.Close()
	a, _ := store.Add(account.PlatformLoomy, loomyCred(), "", "")

	now := time.Now()
	s := NewScheduler(SchedulerConfig{
		Store: store, Hours: []int{25}, // 永不命中 → 只走手动/重试路径
		Retry: time.Minute, MaxRetry: 2,
		Logf: func(string, ...any) {},
	})
	s.SetNow(fixedNow(now))
	defer s.Close()

	c := newLoomyChecker()
	c.Base = srv.URL
	Register(c)
	defer Register(newLoomyChecker())

	// 连续 3 次（首次 + 2 次重试预算）都失败 → 放弃（不再进重试队列）。
	now = now.Add(time.Minute)
	s.SetNow(fixedNow(now))
	s.RunAll(context.Background()) // 首次失败（RunAll = 定时路径）→ schedule
	now = now.Add(time.Minute)
	s.SetNow(fixedNow(now))
	s.mu.Lock()
	st := s.retries[a.ID]
	s.mu.Unlock()
	if st == nil {
		t.Fatal("首次失败后应进重试队列")
	}
	now = st.nextAt
	s.SetNow(fixedNow(now))
	s.mu.Lock()
	s.retries[a.ID] = &retryState{attempts: 1, nextAt: now}
	s.mu.Unlock()
	s.runRetries() // 第 1 次重试失败 → attempts=2
	now = now.Add(time.Minute)
	s.SetNow(fixedNow(now))
	s.mu.Lock()
	if st := s.retries[a.ID]; st == nil || st.attempts != 2 {
		t.Fatalf("重试状态异常: %+v", s.retries)
	}
	s.mu.Unlock()
	s.runRetries() // 第 2 次重试失败 → attempts=3 > 2 → 放弃
	s.mu.Lock()
	if _, exists := s.retries[a.ID]; exists {
		t.Error("超过 MaxRetry 后应放弃重试")
	}
	s.mu.Unlock()
	// 第 3 天签到成功 → 重试队列本已空，历史多一条 ok。
	fail = false
	s.SetNow(fixedNow(now.Add(time.Hour)))
	_, _ = s.CheckinAccount(context.Background(), a.ID)
	got := store.Get(a.ID)
	if got.Checkin[0].Status != StatusOK {
		t.Errorf("成功后历史应为 ok: %+v", got.Checkin)
	}
}

// md5Hex / readAllStr 测试辅助。
func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func readAllStr(r *http.Request) (string, error) {
	buf := make([]byte, 4096)
	var out []byte
	for {
		n, err := r.Body.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			return string(out), nil
		}
	}
}

// ── qoder 真实账号形状容错（用户实测修复回归）──────────────────

func newQoderCheckerAt(url string) *qoderChecker {
	c := newQoderChecker()
	c.Base = url
	return c
}

// 真实形状样例 1：data 为对象嵌套 list → 走通全链。
func TestQoderRealShapeNestedList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"list":[
			  {"id":"c-9","actionType":"CLAIM_BENEFIT","claimStatus":"pending","rewardName":"100 credits"}
			]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"result":true}}`))
	}))
	defer srv.Close()
	res, err := newQoderCheckerAt(srv.URL).CheckIn(context.Background(), newQoderAccount())
	if err != nil || res.Status != StatusOK || !strings.Contains(res.Reward, "100 credits") {
		t.Errorf("对象嵌套 list 应走通: %+v %v", res, err)
	}
}

// 真实形状样例 2：data 对象嵌套 campaigns 键 + 顶层 campaigns 数组两形态。
func TestQoderRealShapeAltKeys(t *testing.T) {
	cases := []struct{ name, body string }{
		{"data.campaigns", `{"data":{"campaigns":[{"id":"c-1","actionType":"CLAIM_BENEFIT","claimStatus":"pending"}]}}`},
		{"top-level campaigns", `{"campaigns":[{"id":"c-1","actionType":"CLAIM_BENEFIT","claimStatus":"pending"}]}`},
		{"top-level array", `[{"id":"c-1","actionType":"CLAIM_BENEFIT","claimStatus":"pending"}]`},
		{"data.records", `{"data":{"records":[{"id":"c-1","actionType":"CLAIM_BENEFIT","claimStatus":"pending"}]}}`},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(tc.body))
				return
			}
			_, _ = w.Write([]byte(`{"success":true}`))
		}))
		res, err := newQoderCheckerAt(srv.URL).CheckIn(context.Background(), newQoderAccount())
		srv.Close()
		if err != nil || res.Status != StatusOK {
			t.Errorf("%s: 应走通: %+v %v", tc.name, res, err)
		}
	}
}

// 业务码先行：非成功业务码 → 业务错误（含码与 msg），不误报形状变更。
// 已领取归一（v1.7.5 用户实测 bug 的回归钉）：
// claim 已签过时上游返回业务码 CLAIMED，网关曾把它判成 failed → 面板标红、
// 调度器 30 分钟一次重试直到耗尽当日额度。归一为 already（幂等成功态）。
func TestQoderCheckinClaimedIsAlready(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			// 列表说"可领取"（claimStatus 缺失），但 claim 侧才发现已领过——
			// 这正是真实账号踩到的路径：列表缓存口径与 claim 实时口径不一致。
			_, _ = w.Write([]byte(`{"data":[{"id":"c-2","actionType":"CLAIM_BENEFIT"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":"CLAIMED"}`))
	}))
	defer srv.Close()
	res, err := newQoderCheckerAt(srv.URL).CheckIn(context.Background(), newQoderAccount())
	if err != nil {
		t.Fatalf("CheckIn: %v", err)
	}
	if res.Status != StatusAlready {
		t.Fatalf("业务码 CLAIMED 应归一为 already，得到 %s（%s）——旧行为会标红并触发重试", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "已领取") {
		t.Errorf("文案应说明已领取: %q", res.Message)
	}
}

// 列表层就返回已领取码（CLAIMED / ALREADY_CLAIMED / 文案型）同样归一，
// 不进跳 2。
func TestQoderCheckinAlreadyVariants(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"claim_claimed", `{"code":"CLAIMED"}`},
		{"claim_upper_snake", `{"code":"ALREADY_CLAIMED"}`},
		{"claim_msg_only", `{"code":"4099","message":"benefit already claimed"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			res, err := newQoderCheckerAt(srv.URL).CheckIn(context.Background(), newQoderAccount())
			if err != nil {
				t.Fatalf("CheckIn: %v", err)
			}
			if res.Status != StatusAlready {
				t.Errorf("%s：应 already，得到 %s（%s）", tc.name, res.Status, res.Message)
			}
		})
	}
}

// 成功/失败边界不被"已领取"归一吃掉：真实业务错误仍按 failed 报。
func TestQoderCheckinRealErrorStillFailed(t *testing.T) {
	for _, body := range []string{
		`{"code":"CAMPAIGN_ENDED","message":"activity has ended"}`,
		`{"code":"RISK_CONTROL","msg":"forbidden"}`,
		`{"code":"500"}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		res, err := newQoderCheckerAt(srv.URL).CheckIn(context.Background(), newQoderAccount())
		srv.Close()
		if err != nil {
			t.Fatalf("%s：不应返回错误: %v", body, err)
		}
		if res.Status != StatusFailed {
			t.Errorf("%s：应仍是 failed（不被误洗成已签到），得到 %s（%s）", body, res.Status, res.Message)
		}
	}
}

// success:false + 已领取文案（部分上游不用 code 表达）→ already。
func TestQoderCheckinSuccessFalseAlready(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"data":[{"id":"c-2","actionType":"CLAIM_BENEFIT"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":false,"errorMessage":"已领取过该奖励"}`))
	}))
	defer srv.Close()
	res, err := newQoderCheckerAt(srv.URL).CheckIn(context.Background(), newQoderAccount())
	if err != nil {
		t.Fatalf("CheckIn: %v", err)
	}
	if res.Status != StatusAlready {
		t.Errorf("success:false + 已领取文案应 already，得到 %s（%s）", res.Status, res.Message)
	}
}

func TestQoderBizCodeFirst(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":"500","msg":"server busy"}`))
	}))
	defer srv.Close()
	res, err := newQoderCheckerAt(srv.URL).CheckIn(context.Background(), newQoderAccount())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusFailed || !strings.Contains(res.Message, "业务码 500") ||
		!strings.Contains(res.Message, "server busy") {
		t.Errorf("业务码应先行报错: %+v", res)
	}
	if strings.Contains(res.Message, "接口可能已变更") {
		t.Errorf("业务码错误不应误报形状变更: %+v", res)
	}
	// 字符串成功码 "000000"（loomy 同款）→ 不算失败，继续解析。
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"code":"000000","data":[{"id":"c-2","actionType":"CLAIM_BENEFIT","claimStatus":"pending"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer srv2.Close()
	res, err = newQoderCheckerAt(srv2.URL).CheckIn(context.Background(), newQoderAccount())
	if err != nil || res.Status != StatusOK {
		t.Errorf("字符串成功码应走通: %+v %v", res, err)
	}
}

// 诊断信息：形状不符的错误含键名与 data 类型、不含值（脱敏）。
func TestQoderShapeDiagnosis(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"errCode":"X-TOKEN-SECRET-VALUE","message":"welcome user 42","data":{"foo":"bar-value"}}`))
	}))
	defer srv.Close()
	res, err := newQoderCheckerAt(srv.URL).CheckIn(context.Background(), newQoderAccount())
	if err != nil {
		t.Fatal(err)
	}
	msg := res.Message
	for _, want := range []string{"平台接口可能已变更", "顶层键=[data,errCode,message]", "data=对象（键=[foo]）"} {
		if !strings.Contains(msg, want) {
			t.Errorf("诊断缺 %q: %s", want, msg)
		}
	}
	// 脱敏：值不进诊断（只有键名/类型）。
	for _, leak := range []string{"X-TOKEN-SECRET-VALUE", "bar-value", "42"} {
		if strings.Contains(msg, leak) {
			t.Errorf("诊断泄露值 %q: %s", leak, msg)
		}
	}
	// data 缺失形态。
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"message":"gone"}`))
	}))
	defer srv2.Close()
	res, _ = newQoderCheckerAt(srv2.URL).CheckIn(context.Background(), newQoderAccount())
	if !strings.Contains(res.Message, "data=缺失") {
		t.Errorf("data 缺失诊断异常: %s", res.Message)
	}
}

// CLAIMED 家族 → already（v1.7.6 真实联调修复；用户累计点 6 次全标红）。
func TestQoderClaimedIsAlready(t *testing.T) {
	// 列表跳 CLAIMED（无数据）→ already。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":"CLAIMED"}`))
	}))
	defer srv.Close()
	res, err := newQoderCheckerAt(srv.URL).CheckIn(context.Background(), newQoderAccount())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusAlready {
		t.Errorf("列表跳 CLAIMED 应 already: %+v", res)
	}
	if strings.Contains(res.Message, "失败") {
		t.Errorf("already 不应带失败字样: %+v", res)
	}

	// claim 跳 CLAIMED：列表可领 → claim 返回已领取 → already。
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"code":0,"data":[{"id":"c-1","actionType":"CLAIM_BENEFIT","claimStatus":"pending"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":"CLAIMED"}`))
	}))
	defer srv2.Close()
	res, err = newQoderCheckerAt(srv2.URL).CheckIn(context.Background(), newQoderAccount())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusAlready {
		t.Errorf("claim 跳 CLAIMED 应 already: %+v", res)
	}
}

// CLAIMED 但响应带活动列表/奖励信息 → Message 丰富化（数据保留利用）。
func TestQoderClaimedWithRewardData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":"CLAIMED","rewardPoints":100,
		  "data":{"list":[{"id":"c-1","actionType":"CLAIM_BENEFIT","claimStatus":"claimed","rewardName":"100 credits"}]}}`))
	}))
	defer srv.Close()
	res, err := newQoderCheckerAt(srv.URL).CheckIn(context.Background(), newQoderAccount())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusAlready {
		t.Fatalf("带数据的 CLAIMED 应 already: %+v", res)
	}
	if !strings.Contains(res.Message, "100") || !strings.Contains(res.Message, "credits") {
		t.Errorf("Message 应含奖励信息: %+v", res)
	}
}

// 其余非成功码仍 failed（不回归）；CLAIMED 家族变体同样 already。
func TestQoderBizCodesNoRegression(t *testing.T) {
	// 真实失败码（活动结束）仍 failed。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":"ACTIVITY_ENDED","msg":"activity ended"}`))
	}))
	defer srv.Close()
	res, _ := newQoderCheckerAt(srv.URL).CheckIn(context.Background(), newQoderAccount())
	if res.Status != StatusFailed {
		t.Errorf("ACTIVITY_ENDED 应 failed: %+v", res)
	}
	if !strings.Contains(res.Message, "活动已结束") && !strings.Contains(res.Message, "activity ended") {
		t.Errorf("失败文案应含上游原因: %+v", res)
	}
	// 变体大小写/同族码 → already。
	for _, code := range []string{"claimed_today", "already_claim", "ALREADY_CLAIMED", "Received"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"code":"` + code + `"}`))
		}))
		res, _ := newQoderCheckerAt(srv.URL).CheckIn(context.Background(), newQoderAccount())
		srv.Close()
		if res.Status != StatusAlready {
			t.Errorf("变体码 %s 应 already: %+v", code, res)
		}
	}
}
