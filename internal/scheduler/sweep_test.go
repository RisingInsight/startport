package scheduler

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/RisingInsight/startport/internal/auth"
	"github.com/RisingInsight/startport/internal/pool"
	"github.com/RisingInsight/startport/internal/upstream"
)

// checkinSweep：按日幂等 + 失败退避重试 + 重启补跑（2026-11-03 CodeBuddy 补签缺口）。
func newSweepPool(t *testing.T, uids ...string) *pool.Pool {
	t.Helper()
	p := pool.New("")
	for _, uid := range uids {
		p.Add(&auth.Auth{UID: uid, Nickname: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
		p.SetCredits(uid, 100, 0)
	}
	return p
}

// rtFunc 本包内的 roundTripper 打桩（server 包的 roundTripFunc 不可跨包复用）。
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// sweepUpstream 打桩 DailyCheckin 的上游：行为由 fn 决定（nil=成功，非 nil=按
// 上游 *Error 失败）。billingJSON 走 c.HTTP + ChatBaseCN，注入 roundTripFunc
// 恒返 200 code=0（成功）或指定形态（失败）。
func sweepUpstream(t *testing.T, fn func() error) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			if err := fn(); err != nil {
				// 失败形态：*Error 由调用方造好——直接把它从 transport 吐出去不现实
				//（HTTP 层），改为返回 500 文本（isTransientBillingErr → 重试）。
				return &http.Response{
					StatusCode: 500,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"code":10000,"msg":"boom"}`)),
				}, nil
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"code":0,"msg":"ok"}`)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

// 成功 → NoteCheckinDone → 收尾循环当天不再打上游（幂等闸）。
func TestSweepIdempotentAfterSuccess(t *testing.T) {
	calls := 0
	up := &upstream.Client{} // 不会被用到（打桩见下）
	_ = up
	s := New(Config{Pool: newSweepPool(t, "u1"), Upstream: sweepUpstream(t, func() error {
		calls++
		return nil
	})})
	s.sweepCheckin()
	s.sweepCheckin()
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1（成功后幂等闸短路）", calls)
	}
	if !s.cfg.Pool.CheckedInToday("u1") {
		t.Fatal("u1 should be marked checked-in")
	}
}

// 失败 → 不标记 → 退避内不重试、退避过期后重试（自愈闭环）。
// 口径说明：DailyCheckin 内部自带瞬时错误重试（500 会补打 2 次），所以上游
// 请求数不是"收尾循环发起轮数"——用 sweep.last 的写入次数（markSweepAttempt）
// 做发起轮数的权威计数。
func TestSweepRetriesAfterFailureWithBackoff(t *testing.T) {
	attempts := func(s *Scheduler, uid string) int {
		s.sweep.mu.Lock()
		defer s.sweep.mu.Unlock()
		_, ok := s.sweep.last[uid]
		if !ok {
			return 0
		}
		return 1 // last 只有一次写入痕迹；用"存在性+时间"判定，下方直接操纵时间
	}
	_ = attempts
	upErr := &upstream.Error{Kind: upstream.ErrServer, Msg: "boom"}
	s := New(Config{Pool: newSweepPool(t, "u1"), Upstream: sweepUpstream(t, func() error {
		return upErr
	})})
	// 第一轮：失败，last 记录本轮时刻。
	s.sweepCheckin()
	if s.cfg.Pool.CheckedInToday("u1") {
		t.Fatal("failed checkin must not be marked done")
	}
	first, ok := s.sweep.last["u1"]
	if !ok {
		t.Fatal("failed attempt must arm backoff")
	}
	// 退避内（last 刚写）：第二轮不打上游（last 不变）。
	s.sweepCheckin()
	if s.sweep.last["u1"] != first {
		t.Fatal("within backoff must not re-attempt")
	}
	// 拨 last 回 11 分钟前（越过首档 10 分钟退避）：重试 → last 推进。
	s.sweep.mu.Lock()
	s.sweep.last["u1"] = time.Now().Add(-11 * time.Minute)
	s.sweep.mu.Unlock()
	s.sweepCheckin()
	if s.sweep.last["u1"] == first {
		t.Fatal("after backoff must re-attempt")
	}
}

// 重启补跑：sweep.last 清零（新 Scheduler）+ 当天未成功 → 首轮立即补跑。
// 复现 2026-11-03 场景：09:00 槽失败，10:54 重启后必须马上补，而不是等 21:00。
func TestSweepCatchUpAfterRestart(t *testing.T) {
	calls := 0
	p := newSweepPool(t, "u1")
	// 模拟"09:00 失败、重启"：账号今天没有成功记录（CheckedInToday=false）。
	s := New(Config{Pool: p, Upstream: sweepUpstream(t, func() error {
		calls++
		return nil
	})})
	s.sweepCheckin() // 重启后的第一个 tick
	if calls != 1 {
		t.Fatalf("restart catch-up calls = %d, want 1", calls)
	}
	if !p.CheckedInToday("u1") {
		t.Fatal("catch-up should complete the day")
	}
}

// 门控：企业号/global 号零上游请求、直接闭环（无体系=完成）。
func TestSweepGatesEnterpriseAndGlobal(t *testing.T) {
	calls := 0
	p := pool.New("")
	g := &auth.Auth{UID: "g1", Nickname: "g1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"}
	e := &auth.Auth{UID: "e1", Nickname: "e1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999, EnterpriseID: "ent-1"}
	p.Add(g)
	p.Add(e)
	p.SetCredits("g1", 100, 0)
	p.SetCredits("e1", 100, 0)
	s := New(Config{Pool: p, Upstream: sweepUpstream(t, func() error {
		calls++
		return nil
	})})
	s.sweepCheckin()
	if calls != 0 {
		t.Fatalf("gated accounts must not hit upstream, calls = %d", calls)
	}
	if !p.CheckedInToday("g1") || !p.CheckedInToday("e1") {
		t.Fatal("gated accounts should be closed for the day")
	}
}
