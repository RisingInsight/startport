package pool

import (
	"testing"
	"time"

	"github.com/RisingInsight/startport/internal/auth"
)

// 单笔 <0.5 的扣费必须累计生效，不能被逐笔四舍五入抹掉
// （实测 glm-5.3-flash 单笔 ~0.4，旧实现 int64(0.4+0.5)=0 → 永远扣不掉）。
func TestNoteModelCostAccumulatesFractionalCredits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100, 0)

	p.NoteModelCost("u1", "m", 0.4, 1000)
	st, _ := p.Status("u1")
	if st.CreditsObserved < 0.39 || st.CreditsObserved > 0.41 {
		t.Fatalf("observed=%v want ~0.4", st.CreditsObserved)
	}
	if st.Credits != 100 {
		t.Fatalf("credits=%d want 100（0.4 未满 1，不整扣）", st.Credits)
	}

	// 累计 1.2 → 实扣 1（旧实现三笔全部丢弃）
	p.NoteModelCost("u1", "m", 0.4, 1000)
	p.NoteModelCost("u1", "m", 0.4, 1000)
	st, _ = p.Status("u1")
	if st.Credits != 99 {
		t.Fatalf("credits=%d want 99（累计 1.2 → 实扣 1）", st.Credits)
	}
	if st.CreditsObserved < 1.19 || st.CreditsObserved > 1.21 {
		t.Fatalf("observed=%v want ~1.2", st.CreditsObserved)
	}
}

// 上游权威余额变化时本机观测消耗归零（已被权威值涵盖）；未变化时保留
// ——上游对账延迟期间，余额不动但消耗仍可见。
func TestObservedResetsOnlyOnAuthoritativeChange(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsDetailed("u1", 100, 100, 0, time.Time{}, 0)

	p.NoteModelCost("u1", "m", 3, 1000)
	st, _ := p.Status("u1")
	if st.CreditsObserved < 2.99 || st.CreditsObserved > 3.01 {
		t.Fatalf("observed=%v want ~3", st.CreditsObserved)
	}

	// 上游值未变（尚未结算）→ 观测消耗保留；credits 仍以权威值覆盖
	p.SetCreditsDetailed("u1", 100, 100, 0, time.Time{}, 0)
	st, _ = p.Status("u1")
	if st.Credits != 100 {
		t.Fatalf("credits=%d want 100（权威值）", st.Credits)
	}
	if st.CreditsObserved < 2.99 {
		t.Fatalf("observed=%v 应保留（上游未结算）", st.CreditsObserved)
	}

	// 上游结算（权威值变化）→ 观测消耗归零
	p.SetCreditsDetailed("u1", 97, 100, 0, time.Time{}, 0)
	st, _ = p.Status("u1")
	if st.Credits != 97 || st.CreditsObserved != 0 {
		t.Fatalf("结算后 credits=%d observed=%v，want 97/0", st.Credits, st.CreditsObserved)
	}
}
