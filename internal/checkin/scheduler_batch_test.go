// scheduler_batch_test.go 一键签到（RunBatch）+ 对表窗口语义（runDailyIfDue
// "错过窗口补签"）的调度器层用例：假 Checker 注入注册表（虚构平台键，不覆盖
// 真实平台注册项）、假时钟注入调度器，全部不碰真实网络。
package checkin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RisingInsight/startport/internal/account"
)

// fakeChecker 可编程假 Checker：按账号 ID 返回预设结果/错误，记录调用序列。
type fakeChecker struct {
	platform string
	mu       sync.Mutex
	calls    []string
	res      map[string]Result
	err      map[string]error
	delay    time.Duration
}

func (f *fakeChecker) Platform() string { return f.platform }

func (f *fakeChecker) CheckIn(ctx context.Context, a *account.Account) (Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, a.ID)
	res, hasRes := f.res[a.ID]
	err, hasErr := f.err[a.ID]
	delay := f.delay
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-time.After(delay):
		}
	}
	if hasErr {
		return Result{}, err
	}
	if hasRes {
		return res, nil
	}
	return Result{Status: StatusOK, Reward: "1 测试积分", Message: "签到成功"}, nil
}

func (f *fakeChecker) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// batchStore 落临时 accounts.json 并 Load。调度器层用虚构平台键（Store 不校验
// 平台枚举），假 Checker 注册进全局注册表时不碰真实平台的注册项。
func batchStore(t *testing.T, accts ...*account.Account) *account.Store {
	t.Helper()
	raw, err := json.Marshal(accts)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "accounts.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := account.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	s.SetLogf(func(string, ...any) {})
	t.Cleanup(s.Close)
	return s
}

// newBatchScheduler 构造即停对表循环的调度器（测试直接驱动 runDailyIfDue /
// RunBatch，避免 1 分钟对表 tick 与断言竞态）。
func newBatchScheduler(t *testing.T, store *account.Store) *Scheduler {
	t.Helper()
	s := NewScheduler(SchedulerConfig{Store: store})
	s.Close()
	return s
}

// TestRunBatchMixed 一键签到的四类账号：成功 / 已签 / 停用跳过 / 平台无签到。
// skipped 不发起上游调用、不落签到台账（没执行就没有记录）。
func TestRunBatchMixed(t *testing.T) {
	ca := &fakeChecker{platform: "batch-a", res: map[string]Result{
		"acc-b01": {Status: StatusOK, Reward: "10 积分", Message: "签到成功"},
	}}
	cb := &fakeChecker{platform: "batch-b", res: map[string]Result{
		"acc-b03": {Status: StatusAlready, Message: "今天已签到"},
	}}
	Register(ca)
	Register(cb)
	store := batchStore(t,
		&account.Account{ID: "acc-b01", Platform: "batch-a", Label: "A1"},
		&account.Account{ID: "acc-b02", Platform: "batch-a", Label: "A2", Disabled: true},
		&account.Account{ID: "acc-b03", Platform: "batch-b", Label: "B1"},
		&account.Account{ID: "acc-b04", Platform: "batch-none", Label: "N1"},
	)
	s := newBatchScheduler(t, store)

	results, sum := s.RunBatch(context.Background())
	if sum.Total != 4 || sum.OK != 1 || sum.Already != 1 || sum.Failed != 0 || sum.Skipped != 2 {
		t.Fatalf("汇总异常: %+v", sum)
	}
	byID := map[string]BatchResult{}
	for _, r := range results {
		byID[r.ID] = r
	}
	if byID["acc-b01"].Status != StatusOK || byID["acc-b01"].Reward != "10 积分" {
		t.Errorf("成功账号结果异常: %+v", byID["acc-b01"])
	}
	if byID["acc-b02"].Status != StatusSkipped || byID["acc-b02"].Message != "账号已停用" {
		t.Errorf("停用账号应 skipped: %+v", byID["acc-b02"])
	}
	if byID["acc-b03"].Status != StatusAlready {
		t.Errorf("已签账号结果异常: %+v", byID["acc-b03"])
	}
	if byID["acc-b04"].Status != StatusSkipped {
		t.Errorf("无签到平台应 skipped: %+v", byID["acc-b04"])
	}
	if n := ca.callCount(); n != 1 {
		t.Errorf("batch-a 应只对未停用账号发起 1 次签到，实际 %d", n)
	}
	// 台账：执行过的有记录，skipped 的没有。
	if a := store.Get("acc-b01"); len(a.Checkin) != 1 || a.Checkin[0].Status != StatusOK {
		t.Errorf("成功账号应落台账: %+v", a.Checkin)
	}
	if a := store.Get("acc-b02"); len(a.Checkin) != 0 {
		t.Errorf("停用账号不应落台账: %+v", a.Checkin)
	}
	if a := store.Get("acc-b04"); len(a.Checkin) != 0 {
		t.Errorf("无签到平台不应落台账: %+v", a.Checkin)
	}
}

// TestRunBatchPlatformFilter platforms 过滤只签指定平台；重复键去重。
func TestRunBatchPlatformFilter(t *testing.T) {
	ca := &fakeChecker{platform: "batch-fa"}
	cb := &fakeChecker{platform: "batch-fb"}
	Register(ca)
	Register(cb)
	store := batchStore(t,
		&account.Account{ID: "acc-fa1", Platform: "batch-fa", Label: "FA1"},
		&account.Account{ID: "acc-fb1", Platform: "batch-fb", Label: "FB1"},
	)
	s := newBatchScheduler(t, store)

	_, sum := s.RunBatch(context.Background(), "batch-fb")
	if sum.Total != 1 || sum.OK != 1 {
		t.Fatalf("过滤后应只含 batch-fb: %+v", sum)
	}
	if n := ca.callCount(); n != 0 {
		t.Errorf("未选平台不应被调用，实际 %d", n)
	}
	// 重复平台键 = 同一个集合，不重复签到。
	_, sum = s.RunBatch(context.Background(), "batch-fa", "batch-fa")
	if sum.Total != 1 {
		t.Fatalf("重复键应去重: %+v", sum)
	}
}

// TestRunBatchFailure 业务失败计入 failed（不中断其余账号）。
func TestRunBatchFailure(t *testing.T) {
	cf := &fakeChecker{platform: "batch-f", err: map[string]error{
		"acc-f01": errors.New("签到请求失败（网络）"),
	}}
	Register(cf)
	store := batchStore(t,
		&account.Account{ID: "acc-f01", Platform: "batch-f", Label: "F1"},
		&account.Account{ID: "acc-f02", Platform: "batch-f", Label: "F2"},
	)
	s := newBatchScheduler(t, store)

	results, sum := s.RunBatch(context.Background())
	if sum.Failed != 1 || sum.OK != 1 || sum.Total != 2 {
		t.Fatalf("汇总异常: %+v", sum)
	}
	if results[0].Status != StatusFailed || results[0].Message == "" {
		t.Errorf("失败结果应带原因: %+v", results[0])
	}
	// 失败落台账（failed 态），面板行标红依据。
	if a := store.Get("acc-f01"); len(a.Checkin) != 1 || a.Checkin[0].Status != StatusFailed {
		t.Errorf("失败应落台账: %+v", a.Checkin)
	}
}

// TestRunBatchTimeoutSkipsRest ctx 超时后剩余账号记 skipped 且不发起调用。
func TestRunBatchTimeoutSkipsRest(t *testing.T) {
	ct := &fakeChecker{platform: "batch-t", delay: 50 * time.Millisecond}
	Register(ct)
	store := batchStore(t,
		&account.Account{ID: "acc-t01", Platform: "batch-t", Label: "T1"},
		&account.Account{ID: "acc-t02", Platform: "batch-t", Label: "T2"},
		&account.Account{ID: "acc-t03", Platform: "batch-t", Label: "T3"},
	)
	s := newBatchScheduler(t, store)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	results, sum := s.RunBatch(ctx)
	if sum.Skipped != 2 {
		t.Fatalf("超时后剩余账号应 skipped: %+v", sum)
	}
	if n := ct.callCount(); n > 1 {
		t.Errorf("超时后不应继续发起调用，实际 %d 次", n)
	}
	for _, r := range results[1:] {
		if r.Status != StatusSkipped {
			t.Errorf("剩余账号状态 = %s，应 skipped", r.Status)
		}
	}
}

// TestRunBatchTryLockConflict 账号签到进行中（定时/手动并发）→ skipped 不报失败。
func TestRunBatchTryLockConflict(t *testing.T) {
	cl := &fakeChecker{platform: "batch-l"}
	Register(cl)
	store := batchStore(t, &account.Account{ID: "acc-l01", Platform: "batch-l", Label: "L1"})
	s := newBatchScheduler(t, store)

	unlock, ok := s.tryLock("acc-l01")
	if !ok {
		t.Fatal("预置锁失败")
	}
	defer unlock()
	results, sum := s.RunBatch(context.Background())
	if sum.Skipped != 1 || sum.Failed != 0 {
		t.Fatalf("并发中的账号应 skipped: %+v", sum)
	}
	if results[0].Status != StatusSkipped || !strings.Contains(results[0].Message, "正在执行中") {
		t.Errorf("跳过说明异常: %+v", results[0])
	}
}

// TestRunDailyIfDueWindowAndCatchUp 对表窗口语义：窗口前不抢跑；进程在窗口
// 小时内没在运行（重启晚于窗口）也能在之后的第一个对表 tick 补跑当天一轮。
func TestRunDailyIfDueWindowAndCatchUp(t *testing.T) {
	ck := &fakeChecker{platform: "batch-d"}
	Register(ck)
	store := batchStore(t, &account.Account{ID: "acc-d01", Platform: "batch-d", Label: "D1"})
	s := newBatchScheduler(t, store)

	cur := time.Date(2026, 10, 31, 8, 0, 0, 0, time.Local)
	s.runDailyIfDue(cur)
	if n := ck.callCount(); n != 0 {
		t.Fatalf("窗口（9 点）前不应触发，实际 %d 次", n)
	}
	// 窗口后任意 tick（模拟 17 点重启的进程）：当天补跑。
	cur = time.Date(2026, 10, 31, 17, 0, 0, 0, time.Local)
	s.runDailyIfDue(cur)
	if n := ck.callCount(); n != 1 {
		t.Fatalf("窗口后应补跑当天一轮，实际 %d 次", n)
	}
	// 同一天再对表：幂等不重跑。
	s.runDailyIfDue(cur)
	if n := ck.callCount(); n != 1 {
		t.Fatalf("同一天应只跑一轮，实际 %d 次", n)
	}
	// 次日窗口前：不抢跑。
	cur = time.Date(2026, 11, 1, 8, 30, 0, 0, time.Local)
	s.runDailyIfDue(cur)
	if n := ck.callCount(); n != 1 {
		t.Fatalf("次日窗口前不应触发，实际 %d 次", n)
	}
	// 次日到点：新的一天再跑一轮。
	cur = time.Date(2026, 11, 1, 9, 0, 0, 0, time.Local)
	s.runDailyIfDue(cur)
	if n := ck.callCount(); n != 2 {
		t.Fatalf("次日应再跑一轮，实际 %d 次", n)
	}
}
