// checkin_api_test.go 一键签到 API（v1.7.4）：platforms 过滤 + 汇总/逐账号
// 结果契约 + 平台键显性校验 + 501 降级。假 Checker 注册进全局注册表（panel
// 测试二进制不依赖真实平台签到实现，也不会触网）。
package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/RisingInsight/startport/internal/account"
	"github.com/RisingInsight/startport/internal/checkin"
)

// stubChecker 可编程假 Checker（按账号 ID 返回预设结果/错误，记录调用序列）。
type stubChecker struct {
	platform string
	mu       sync.Mutex
	calls    []string
	res      map[string]checkin.Result
	err      map[string]error
}

func (s *stubChecker) Platform() string { return s.platform }

func (s *stubChecker) CheckIn(_ context.Context, a *account.Account) (checkin.Result, error) {
	s.mu.Lock()
	s.calls = append(s.calls, a.ID)
	res, hasRes := s.res[a.ID]
	err, hasErr := s.err[a.ID]
	s.mu.Unlock()
	if hasErr {
		return checkin.Result{}, err
	}
	if hasRes {
		return res, nil
	}
	return checkin.Result{Status: checkin.StatusOK, Reward: "10 测试积分", Message: "签到成功"}, nil
}

func (s *stubChecker) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// newCheckinEnv 带签到调度器的面板环境：真 Scheduler（即停对表循环）+ 真
// Store；假 Checker 由用例按平台注册。
func newCheckinEnv(t *testing.T) *acctEnv {
	t.Helper()
	e := newAcctEnv(t)
	sched := checkin.NewScheduler(checkin.SchedulerConfig{Store: e.store})
	sched.Close() // 测试只打手动端点；对表循环不得与断言竞态
	e.pan.cfg.Checkin = sched
	return e
}

type checkinBatchOut struct {
	OK      bool                 `json:"ok"`
	Message string               `json:"message"`
	Summary checkin.BatchSummary `json:"summary"`
	Results []checkin.BatchResult `json:"results"`
}

// TestPlatformCheckinAllBatchSummary 全量一键签到：汇总逐项对得上 + 台账落账。
func TestPlatformCheckinAllBatchSummary(t *testing.T) {
	loomy := &stubChecker{platform: account.PlatformLoomy}
	checkin.Register(loomy)
	e := newCheckinEnv(t)
	id1, rec := e.add("loomy", `{"session":"sess-0123456789abcdef"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add#1 = %d: %s", rec.Code, rec.Body.String())
	}
	id2, rec := e.add("loomy", `{"session":"sess-fedcba9876543210"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add#2 = %d: %s", rec.Code, rec.Body.String())
	}
	loomy.res = map[string]checkin.Result{
		id1: {Status: checkin.StatusOK, Reward: "100 credits", Message: "签到成功"},
		id2: {Status: checkin.StatusAlready, Message: "今天已签到"},
	}

	rec = e.do("POST", "/startport/api/accounts/platform_checkin_all", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out checkinBatchOut
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || out.Summary.Total != 2 || out.Summary.OK != 1 || out.Summary.Already != 1 ||
		out.Summary.Failed != 0 || out.Summary.Skipped != 0 {
		t.Fatalf("汇总异常: %+v", out)
	}
	if !strings.Contains(out.Message, "成功 1 · 已签过 1") {
		t.Errorf("message 缺汇总: %q", out.Message)
	}
	if len(out.Results) != 2 {
		t.Fatalf("results 应含逐账号结果: %d", len(out.Results))
	}
	for _, r := range out.Results {
		if r.ID == "" || r.Label == "" || r.Platform != account.PlatformLoomy || r.Status == "" {
			t.Errorf("结果缺字段: %+v", r)
		}
	}
	// 台账：两个账号各落一条。
	for _, id := range []string{id1, id2} {
		if a := e.store.Get(id); a == nil || len(a.Checkin) != 1 {
			t.Errorf("账号 %s 应落一条签到台账", id)
		}
	}
}

// TestPlatformCheckinAllPlatformFilter {"platforms":["autoclaw"]} 只签该平台。
func TestPlatformCheckinAllPlatformFilter(t *testing.T) {
	loomy := &stubChecker{platform: account.PlatformLoomy}
	autoclaw := &stubChecker{platform: account.PlatformAutoClaw}
	checkin.Register(loomy)
	checkin.Register(autoclaw)
	e := newCheckinEnv(t)
	if _, rec := e.add("loomy", `{"session":"sess-0123456789abcdef"}`); rec.Code != http.StatusCreated {
		t.Fatalf("add loomy = %d: %s", rec.Code, rec.Body.String())
	}
	idA, rec := e.add("autoclaw", `{"token":"tok-0123456789"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add autoclaw = %d: %s", rec.Code, rec.Body.String())
	}
	autoclaw.res = map[string]checkin.Result{
		idA: {Status: checkin.StatusOK, Reward: "200 积分"},
	}

	rec = e.do("POST", "/startport/api/accounts/platform_checkin_all", `{"platforms":["autoclaw"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out checkinBatchOut
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Summary.Total != 1 || out.Summary.OK != 1 {
		t.Fatalf("应只签 autoclaw 一个账号: %+v", out.Summary)
	}
	if len(out.Results) != 1 || out.Results[0].Platform != account.PlatformAutoClaw {
		t.Fatalf("results 应只含 autoclaw: %+v", out.Results)
	}
	if n := loomy.callCount(); n != 0 {
		t.Errorf("未选平台不应被调用，实际 %d", n)
	}
}

// TestPlatformCheckinAllUnknownPlatform 未知平台键 400（显性失败，不悄悄全量）。
func TestPlatformCheckinAllUnknownPlatform(t *testing.T) {
	e := newCheckinEnv(t)
	rec := e.do("POST", "/startport/api/accounts/platform_checkin_all", `{"platforms":["nope"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "未知平台") {
		t.Errorf("应带平台键错误说明: %s", rec.Body.String())
	}
}

// TestPlatformCheckinAllDisabled 调度器未注入（Checkin nil）→ 501 优雅降级。
func TestPlatformCheckinAllDisabled(t *testing.T) {
	e := newAcctEnv(t)
	rec := e.do("POST", "/startport/api/accounts/platform_checkin_all", "")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
}

// TestPlatformCheckinAllEmptyScope 范围内只有无签到平台账号：全部 skipped、
// message 说明原因，不算失败。
func TestPlatformCheckinAllEmptyScope(t *testing.T) {
	e := newCheckinEnv(t)
	if _, rec := e.add("qoder-intl", `{"access_token":"dt-abcdef","user_id":"u1"}`); rec.Code != http.StatusCreated {
		t.Fatalf("add = %d: %s", rec.Code, rec.Body.String())
	}
	rec := e.do("POST", "/startport/api/accounts/platform_checkin_all", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out checkinBatchOut
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Summary.Total != 1 || out.Summary.Skipped != 1 || out.Summary.Failed != 0 {
		t.Fatalf("无签到平台应 skipped: %+v", out.Summary)
	}
	if !strings.Contains(out.Message, "没有可签到的账号") {
		t.Errorf("message 应说明无可签账号: %q", out.Message)
	}
}
