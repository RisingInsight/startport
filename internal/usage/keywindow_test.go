package usage

import (
	"testing"
	"time"
)

// KeyWindow 密钥窗口汇总：只算该密钥名的桶、from 之前的桶不入窗、
// 竖线转义口径与桶键一致。
func TestKeyWindowFiltersByKeyAndTime(t *testing.T) {
	r := New("")
	now := time.Now()
	name := "受限 agent"
	r.Add(now, "cn", "u1", "m", Delta{KeyName: name, TotalTokens: 300, HasTotal: true}, true)
	r.Add(now, "cn", "u1", "m", Delta{KeyName: name, TotalTokens: 200, HasTotal: true}, true)
	r.Add(now, "cn", "u1", "m", Delta{KeyName: "别的密钥", TotalTokens: 999, HasTotal: true}, true)
	r.Add(now, "cn", "u1", "m", Delta{KeyName: name}, false) // 失败尝试：进请求数不进 token

	tokens, reqs := r.KeyWindow(name, time.Now().Add(-time.Hour))
	if tokens != 500 {
		t.Errorf("tokens = %d, want 500", tokens)
	}
	if reqs != 3 {
		t.Errorf("reqs = %d, want 3（含失败尝试）", reqs)
	}

	// from 在未来：空窗。
	tokens, reqs = r.KeyWindow(name, now.Add(time.Hour))
	if tokens != 0 || reqs != 0 {
		t.Errorf("future window = %d/%d, want 0/0", tokens, reqs)
	}

	// 名字里的竖线按桶键口径转义后仍可命中。
	r.Add(now, "cn", "u1", "m", Delta{KeyName: "a|b", TotalTokens: 7, HasTotal: true}, true)
	tokens, _ = r.KeyWindow("a|b", now.Add(-time.Hour))
	if tokens != 7 {
		t.Errorf("piped name tokens = %d, want 7", tokens)
	}
}

// 周期零点窗口与 Rollup 折叠出的日桶边界对齐：把一小时桶折成日桶后，
// KeyWindow(当日零点) 仍能从日桶取到数。
func TestKeyWindowCountsRolledDayBuckets(t *testing.T) {
	r := New("")
	now := time.Now()
	name := "滚动密钥"
	r.Add(now, "cn", "u1", "m", Delta{KeyName: name, TotalTokens: 1234, HasTotal: true}, true)

	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	before, _ := r.KeyWindow(name, midnight)
	if before != 1234 {
		t.Fatalf("pre-roll tokens = %d, want 1234", before)
	}

	// 折叠：把 hourlyKeep 之前的小时桶压成日桶（当前小时不在折叠范围，
	// 手动把桶改写为昨日小时再 Rollup 才会触发；这里直接验证日桶口径——
	// 构造一个昨日零点之后的日桶形状不可行，改为把当前桶 scope 伪装成
	// 日桶起点，验证 bucketTime(d:) 路径参与汇总）。
	r.mu.Lock()
	for _, b := range r.buckets {
		if b.KN == name {
			b.Scope = "d:" + now.Format("2006-01-02")
		}
	}
	r.mu.Unlock()
	after, _ := r.KeyWindow(name, midnight)
	if after != 1234 {
		t.Errorf("day-bucket tokens = %d, want 1234", after)
	}
}
