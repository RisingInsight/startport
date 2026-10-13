package pool

import (
	"testing"
	"time"

	"github.com/RisingInsight/startport/internal/auth"
)

// TestSweepExpiredCoolReason 冷却自然到期后：statusOf 不再透出 reason（接口层），
// sweepExpiredCoolReasonLocked 清掉内存/state.json 残留（数据层）。恢复即干净，
// 不留「可用 / 429 rate limit」的误导显示。禁用原因的 reason（until 为零）不在
// 扫除范围——它描述禁用而非冷却，由 DisabledReason 单独透出。
func TestSweepExpiredCoolReason(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolSoft, 40*time.Millisecond, "429 rate limit")

	if st, _ := p.Status("u1"); st.Reason != "429 rate limit" || !st.Cooling {
		t.Fatalf("cooling: reason=%q cooling=%v want reason 透出且 Cooling", st.Reason, st.Cooling)
	}
	time.Sleep(60 * time.Millisecond)

	// 到期后：接口层立即干净（不等扫除 tick）。
	if st, _ := p.Status("u1"); st.Reason != "" || st.Cooling {
		t.Fatalf("expired: reason=%q cooling=%v want 均空/false", st.Reason, st.Cooling)
	}

	// 数据层：扫除清掉 entry.reason 残留（重启恢复后也不会再透出），且不波及
	// softStreak 退避计数（提前清零会让连续限流的账号永不升档）。
	p.mu.Lock()
	before := p.byUID["u1"].softStreak
	p.sweepExpiredCoolReasonLocked(time.Now())
	swept := p.byUID["u1"].reason
	after := p.byUID["u1"].softStreak
	p.mu.Unlock()
	if swept != "" {
		t.Fatalf("sweep should clear entry.reason, got %q", swept)
	}
	if after != before {
		t.Fatalf("sweep must not touch softStreak: %d → %d", before, after)
	}

	// 禁用原因不受扫除影响（until 为零 → 条件排除）。
	p2 := New("")
	p2.Add(&auth.Auth{UID: "d1"})
	p2.Disable("d1", "12153 session dead")
	p2.mu.Lock()
	p2.sweepExpiredCoolReasonLocked(time.Now())
	p2.mu.Unlock()
	if st, _ := p2.Status("d1"); st.DisabledReason != "12153 session dead" {
		t.Fatalf("DisabledReason=%q want 保留（禁用原因不在扫除范围）", st.DisabledReason)
	}
}
