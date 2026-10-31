// 选号理由（PickExcludingForRealmReason）：层标签 / 权重后缀 / LRU 兜底 / 全冷却兜底
// 各路径的理由短语契约。理由供「运行日志」展示，前缀稳定是面板与排障的依赖面。
package pool

import (
	"strings"
	"testing"
	"time"

	"github.com/RisingInsight/startport/internal/auth"
)

// withLongPickGap 把防撞号窗口临时放大到 1h：连续 pick 视为「刚用过」，
// 用于确定性触发 LRU 兜底路径。
func withLongPickGap(t *testing.T) {
	t.Helper()
	old := minPickGap
	minPickGap = time.Hour
	t.Cleanup(func() { minPickGap = old })
}

func TestPickReasonWeightedTierLabels(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100, 0)

	// 无模型上下文：不带层标签，只有策略 + 权重。
	_, reason := p.PickExcludingForRealmReason(nil, "", "")
	if !strings.HasPrefix(reason, "加权随机·权重") {
		t.Fatalf("model-less pick reason = %q, want 加权随机·权重N prefix", reason)
	}

	// 带模型、无成本观测：无观测层。
	_, reason = p.PickExcludingForRealmReason(nil, "m", "")
	if !strings.HasPrefix(reason, "无观测层·加权随机") {
		t.Fatalf("unobserved pick reason = %q, want 无观测层·加权随机 prefix", reason)
	}

	// 实测免费：免费层。
	p.NoteModelCost("u1", "m", 0, 100)
	_, reason = p.PickExcludingForRealmReason(nil, "m", "")
	if !strings.HasPrefix(reason, "免费层·加权随机") {
		t.Fatalf("free-tier pick reason = %q, want 免费层·加权随机 prefix", reason)
	}

	// 实测收费：收费层。
	p.NoteModelCost("u1", "m", 0.4, 100)
	_, reason = p.PickExcludingForRealmReason(nil, "m", "")
	if !strings.HasPrefix(reason, "收费层·加权随机") {
		t.Fatalf("paid-tier pick reason = %q, want 收费层·加权随机 prefix", reason)
	}
}

func TestPickReasonFallbackEarliestExpiry(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "n1"})
	p.SetCredits("u1", 100, 0)
	p.Cooldown("u1", CoolSoft, 30*time.Minute, "test")
	acct, reason := p.PickExcludingForRealmReason(nil, "", "")
	if acct == nil || acct.UID != "u1" {
		t.Fatalf("fallback pick = %+v, want u1", acct)
	}
	if !strings.HasPrefix(reason, "全冷却兜底(") || !strings.HasSuffix(reason, "解冻") {
		t.Fatalf("fallback reason = %q, want 全冷却兜底(kind)·HH:MM解冻", reason)
	}
}

func TestPickReasonLRUBackstop(t *testing.T) {
	withLongPickGap(t)
	p := New("")
	for _, uid := range []string{"u1", "u2", "u3", "u4", "u5"} {
		p.Add(&auth.Auth{UID: uid})
		p.SetCredits(uid, 100, 0)
	}
	// 5 个号各选中一次：top5 全员 lastUsed 落在 1h 防撞窗口内，下一次 pick
	// 走 LRU 兜底（eligible 为空 → 全候选里取 usedSeq 最旧者）。
	for i := 0; i < 5; i++ {
		if p.Pick() == nil {
			t.Fatal("pick nil during warmup")
		}
	}
	_, reason := p.PickExcludingForRealmReason(nil, "", "")
	if reason != "LRU兜底·top5刚用" {
		t.Fatalf("LRU reason = %q", reason)
	}
}

func TestPickReasonEmptyWhenNoCandidate(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "test")
	acct, reason := p.PickExcludingForRealmReason(nil, "", "")
	if acct != nil || reason != "" {
		t.Fatalf("nil pick should carry empty reason, got acct=%+v reason=%q", acct, reason)
	}
}
