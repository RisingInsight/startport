package checkin

import (
	"context"
	"strings"
	"testing"

	"github.com/RisingInsight/startport/internal/account"
)

// OnDailySweep：定时轮结束的汇总钩子（成功/已签过/失败计数 + 失败明细）。
// 全部成功时钩子仍触发，注入方按 Failed==0 自行决定是否推送。
func TestRunAllDailySweepHook(t *testing.T) {
	st := batchStore(t,
		&account.Account{ID: "acc-ok", Platform: "sweep-a", Label: "正常号"},
		&account.Account{ID: "acc-already", Platform: "sweep-a", Label: "已签号"},
		&account.Account{ID: "acc-bad", Platform: "sweep-a", Label: "坏号"},
	)
	Register(&fakeChecker{platform: "sweep-a", res: map[string]Result{
		"acc-ok":      {Status: StatusOK, Reward: "100 credits"},
		"acc-already": {Status: StatusAlready},
		"acc-bad":     {Status: StatusFailed, Message: "登录态已失效"},
	}})

	var sumGot BatchSummary
	var failsGot []string
	hooked := false
	s := NewScheduler(SchedulerConfig{
		Store: st,
		OnDailySweep: func(sum BatchSummary, failures []string) {
			hooked = true
			sumGot = sum
			failsGot = failures
		},
	})
	s.Close() // 停对表循环：直接驱动 RunAll（与 batch 测试同口径）
	s.RunAll(context.Background())
	if !hooked {
		t.Fatal("OnDailySweep not invoked")
	}
	if sumGot.Total != 3 || sumGot.OK != 1 || sumGot.Already != 1 || sumGot.Failed != 1 || sumGot.Skipped != 0 {
		t.Fatalf("summary = %+v, want {3,1,1,1,0}", sumGot)
	}
	if len(failsGot) != 1 || !strings.Contains(failsGot[0], "坏号") || !strings.Contains(failsGot[0], "登录态已失效") {
		t.Fatalf("failures = %v, want 坏号 with reason", failsGot)
	}
}
