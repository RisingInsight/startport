package usage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// 成功/失败尝试计数、total 的 pt+ct 兜底口径、按域/账号聚合。
func TestAddAndTotals(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "uid1", "glm-5.2", Delta{PromptTokens: 100, HasPromptTokens: true, CompletionTokens: 50, HasCompletion: true, Credit: 1.5, HasCredit: true, ModelRate: "0.05", LatencyMs: 200, HasLatency: true}, true)
	// 失败尝试：无 usage → 只计请求数与失败数，token 不加。
	r.Add(now, "global", "uid1", "claude-4.6", Delta{}, false)
	// 上游没给 total 时用 pt+ct 兜底，保证总量口径连续。
	r.Add(now, "cn", "uid1", "glm-5.2", Delta{PromptTokens: 10, HasPromptTokens: true, CompletionTokens: 5, HasCompletion: true}, true)

	s := r.Snapshot(24, nil)
	if s.Totals.Requests != 3 || s.Totals.Errors != 1 {
		t.Fatalf("requests/errors = %d/%d, want 3/1", s.Totals.Requests, s.Totals.Errors)
	}
	if s.Totals.PromptTokens != 110 || s.Totals.CompletionTok != 55 {
		t.Fatalf("pt/ct = %d/%d, want 110/55", s.Totals.PromptTokens, s.Totals.CompletionTok)
	}
	if s.Totals.TotalTokens != 165 {
		t.Fatalf("tt = %d, want 165（无 total 时按 pt+ct 兜底）", s.Totals.TotalTokens)
	}
	if s.Totals.Credits != 1.5 || s.Totals.CreditSamples != 1 || s.Totals.CreditTokens != 150 || s.Totals.CreditsPer1MTokens != 10000 {
		t.Fatalf("credit totals = %+v, want credits=1.5 samples=1 tokens=150 ratio=10000", s.Totals)
	}
	if s.Totals.AvgLatencyMs != 200 {
		t.Fatalf("avg latency = %v, want 200", s.Totals.AvgLatencyMs)
	}
	if len(s.ByRealm) != 2 {
		t.Fatalf("by_realm = %d 项, want 2", len(s.ByRealm))
	}
	if s.ByAccount[0].Realm == "" {
		t.Fatal("by_account 行缺 realm 标注")
	}
	if len(s.CreditByAccount) != 1 || s.CreditByAccount[0].Key != "uid1" ||
		s.CreditByAccount[0].CreditSamples != 1 || s.CreditByAccount[0].CreditsPer1MTokens != 10000 {
		t.Fatalf("credit_by_account = %+v, want one uid1 row", s.CreditByAccount)
	}
	if len(s.CreditByModel) != 1 || s.CreditByModel[0].Key != "glm-5.2" ||
		s.CreditByModel[0].Rate != "0.05" || s.CreditByModel[0].CreditsPer1MTokens != 10000 {
		t.Fatalf("credit_by_model = %+v, want one glm-5.2 rate=0.05 row", s.CreditByModel)
	}
}

// Rollup 把超出 hourlyKeep 的小时桶折叠为日桶，且幂等：重复折叠不重复计数。
// 窗口口径：24h 窗口不含 100 天前的日桶；hours=0（全部历史）才含日点。
func TestRollupIdempotent(t *testing.T) {
	r := New("")
	old := time.Now().AddDate(0, 0, -100) // 100 天前，超出 90 天小时保留
	r.Add(old, "cn", "u", "m", Delta{PromptTokens: 7, HasPromptTokens: true}, true)
	r.Add(old, "cn", "u", "m", Delta{PromptTokens: 7, HasPromptTokens: true}, true)
	r.Add(time.Now(), "cn", "u", "m", Delta{PromptTokens: 1, HasPromptTokens: true}, true)

	r.Rollup(time.Now())
	after := r.Snapshot(24, nil)
	if after.Totals.Requests != 1 || after.Totals.PromptTokens != 1 {
		t.Fatalf("24h 窗口 totals = %d/%d, want 1/1（窗口外日桶不进聚合）", after.Totals.Requests, after.Totals.PromptTokens)
	}
	if len(after.Series) != 1 || after.Series[0].Scope != "hour" {
		t.Fatalf("series = %+v, want 仅当前小时 1 个点", after.Series)
	}

	all := r.Snapshot(0, nil)
	if all.Totals.Requests != 3 || all.Totals.PromptTokens != 15 {
		t.Fatalf("全部历史 totals = %d/%d, want 3/15", all.Totals.Requests, all.Totals.PromptTokens)
	}
	if len(all.Series) != 2 || all.Series[0].Scope != "day" || all.Series[1].Scope != "hour" {
		t.Fatalf("series = %+v, want 日点在前 + 小时点在后", all.Series)
	}

	r.Rollup(time.Now())
	again := r.Snapshot(0, nil)
	if again.Totals.Requests != 3 || again.Totals.PromptTokens != 15 {
		t.Fatalf("二次折叠后 totals = %d/%d, want 3/15（幂等被破坏）", again.Totals.Requests, again.Totals.PromptTokens)
	}
}

// 落盘→新实例恢复，数据不丢；落盘结构带版本号。
func TestFlushLoadRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	r1 := New(path)
	r1.Add(time.Now(), "cn", "u1", "glm-5.2", Delta{PromptTokens: 42, HasPromptTokens: true, TotalTokens: 42, HasTotal: true}, true)
	r1.Save()

	r2 := New(path)
	s := r2.Snapshot(24, nil)
	if s.Totals.Requests != 1 || s.Totals.TotalTokens != 42 {
		t.Fatalf("恢复后 totals = %d/%d, want 1/42", s.Totals.Requests, s.Totals.TotalTokens)
	}
	raw, _ := os.ReadFile(path)
	var f file
	if err := json.Unmarshal(raw, &f); err != nil || f.Version != fileVersion || len(f.Buckets) != 1 {
		t.Fatalf("落盘文件异常: err=%v buckets=%d", err, len(f.Buckets))
	}
}

// 并发落盘回归：后台 flush 与面板 Save() 若并发写同一个 <path>.tmp 并互相 rename，
// 会把半截内容换成正式文件（usage.json 静默损坏）。修复后由 writeMu 串行化整段
// 落盘，任意并发下产物都必须是完整可解析的 JSON。
func TestConcurrentFlushKeepsFileIntact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	r := New(path)
	var wg sync.WaitGroup
	// 多个 goroutine 反复 Save，模拟后台 ticker 与面板「刷新」并发触发。
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				r.Save()
			}
		}()
	}
	// 一个写者持续 Add（与落盘并发），每次用不同 model 撑大落盘体积，
	// 让并发写 tmp 更容易暴露撕裂（mu 与 writeMu 分离后仍须无竞争）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 500; j++ {
			r.Add(time.Now(), "cn", "u1", fmt.Sprintf("m-%d", j),
				Delta{PromptTokens: 1, HasPromptTokens: true, TotalTokens: 1, HasTotal: true}, true)
		}
	}()
	wg.Wait()
	r.Save()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回落盘文件失败: %v", err)
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("并发落盘后文件损坏（非法 JSON，长度 %d）: %v", len(raw), err)
	}
	if f.Version != fileVersion {
		t.Fatalf("落盘版本 = %d, want %d", f.Version, fileVersion)
	}
}

// 落盘失败必须回滚 dirty：否则「已清标记但没写成功」会让快照被永久丢弃
// （下次 flush 因 !dirty 直接跳过，直到再有新写入）。此处确定性注入失败路径。
func TestFlushFailureRestoresDirty(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "usage.json")
	r := New(good)
	r.Add(time.Now(), "cn", "u1", "m", Delta{PromptTokens: 7, HasPromptTokens: true, TotalTokens: 7, HasTotal: true}, true)

	// 把 path 指向「父级是普通文件」的位置，使 MkdirAll 必失败。
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.path = filepath.Join(blocker, "sub", "usage.json")
	r.Save() // 走失败分支

	r.mu.Lock()
	dirtyAfterFail := r.dirty
	r.mu.Unlock()
	if !dirtyAfterFail {
		t.Fatalf("落盘失败后 dirty 未回滚，快照将被永久丢弃")
	}

	// 恢复可写路径后，同一份数据必须能成功落盘。
	r.path = good
	r.Save()
	raw, err := os.ReadFile(good)
	if err != nil {
		t.Fatalf("恢复路径后仍未落盘: %v", err)
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("落盘内容非法 JSON: %v", err)
	}
	if len(f.Buckets) != 1 || f.Buckets[0].TT != 7 {
		t.Fatalf("落盘数据 = %+v, want 1 bucket tt=7", f.Buckets)
	}
}

// TTFB 必须按桶累计、出现在汇总与时序点里（缓存是否生效的直接指标）。
// 与延迟分开记：TTFB 只反映上游 prefill，总延迟会被生成阶段稀释。
func TestTTFBTrackedInTotalsAndSeries(t *testing.T) {
	r := New("")
	now := time.Now()
	base := Delta{PromptTokens: 100, HasPromptTokens: true, TotalTokens: 100, HasTotal: true}
	d1 := base
	d1.TTFBMs, d1.HasTTFB = 4000, true
	d2 := base
	d2.TTFBMs, d2.HasTTFB = 6000, true
	r.Add(now, "cn", "u1", "m", d1, true)
	r.Add(now, "cn", "u1", "m", d2, true)

	s := r.Snapshot(24, nil)
	if s.Totals.AvgTTFBMs != 5000 {
		t.Fatalf("totals avg_ttfb_ms = %v, want 5000", s.Totals.AvgTTFBMs)
	}
	if len(s.Series) == 0 {
		t.Fatal("series 为空")
	}
	if s.Series[0].AvgTTFBMs != 5000 {
		t.Fatalf("series avg_ttfb_ms = %v, want 5000", s.Series[0].AvgTTFBMs)
	}
}

// 未带 TTFB 观测（HasTTFB=false）时不得计入均值——否则失败尝试会把均值拉低。
func TestTTFBAbsentNotCounted(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "u1", "m", Delta{PromptTokens: 10, HasPromptTokens: true}, true)
	d := Delta{PromptTokens: 10, HasPromptTokens: true, TTFBMs: 3000, HasTTFB: true}
	r.Add(now, "cn", "u1", "m", d, true)

	s := r.Snapshot(24, nil)
	if s.Totals.AvgTTFBMs != 3000 {
		t.Fatalf("avg_ttfb_ms = %v, want 3000（无观测的那笔不应计入分母）", s.Totals.AvgTTFBMs)
	}
}

// 版本 1 文件没有积分字段：按零值恢复，旧 Token 数据保持可见且不产生伪比例。
func TestLoadLegacyWithoutCredit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	legacy := `{"version":1,"saved":"2026-09-28T00:00:00+08:00","buckets":[{"s":"h:2026-09-28T10","r":"cn","u":"u1","m":"glm-5.2","q":1,"p":42,"t":42}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	r := New(path)
	s := r.Snapshot(0, nil)
	if s.Totals.TotalTokens != 42 || s.Totals.CreditSamples != 0 || s.Totals.CreditsPer1MTokens != 0 {
		t.Fatalf("legacy totals = %+v, want token-only history", s.Totals)
	}
	if len(s.CreditByAccount) != 0 || len(s.CreditByModel) != 0 {
		t.Fatalf("legacy credit dimensions = %+v / %+v, want none", s.CreditByAccount, s.CreditByModel)
	}
}

// 小时桶折叠为日桶时必须保留积分、样本数和匹配 Token，比例不能因 Rollup 漂移。
func TestCreditSurvivesRollup(t *testing.T) {
	r := New("")
	old := time.Now().AddDate(0, 0, -100)
	r.Add(old, "cn", "u", "m", Delta{PromptTokens: 100, HasPromptTokens: true, TotalTokens: 100, HasTotal: true, Credit: 1.25, HasCredit: true, ModelRate: "0.5"}, true)
	r.Add(old.Add(2*time.Hour), "cn", "u", "m", Delta{PromptTokens: 300, HasPromptTokens: true, TotalTokens: 300, HasTotal: true, Credit: 3.75, HasCredit: true, ModelRate: "0.5"}, true)
	r.Rollup(time.Now())
	s := r.Snapshot(0, nil)
	if s.Totals.Credits != 5 || s.Totals.CreditSamples != 2 || s.Totals.CreditTokens != 400 || s.Totals.CreditsPer1MTokens != 12500 {
		t.Fatalf("rolled credit totals = %+v, want credits=5 tokens=400 ratio=12500", s.Totals)
	}
	if len(s.CreditByModel) != 1 || s.CreditByModel[0].Rate != "0.5" || s.CreditByModel[0].CreditsPer1MTokens != 12500 {
		t.Fatalf("rolled credit_by_model = %+v, want one rate-preserving row", s.CreditByModel)
	}
}

// 模型维度按“裸模型名 + 生效倍率”合并；同倍率跨账号/时间合并，不同倍率拆行。
func TestCreditDimensionsRateGrouping(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "u1", "cn:glm-5.2", Delta{TotalTokens: 100, HasTotal: true, Credit: 1, HasCredit: true, ModelRate: "0.5"}, true)
	r.Add(now.Add(time.Hour), "cn", "u2", "glm-5.2", Delta{TotalTokens: 200, HasTotal: true, Credit: 2, HasCredit: true, ModelRate: "0.5"}, true)
	r.Add(now.Add(2*time.Hour), "cn", "u2", "glm-5.2", Delta{TotalTokens: 300, HasTotal: true, Credit: 6, HasCredit: true, ModelRate: "0.8"}, true)

	s := r.Snapshot(24, nil)
	if len(s.CreditByAccount) != 2 || len(s.CreditByModel) != 2 {
		t.Fatalf("dimensions accounts=%+v models=%+v, want 2 accounts and 2 model-rate rows", s.CreditByAccount, s.CreditByModel)
	}
	if s.CreditByModel[0].Key != "glm-5.2" || s.CreditByModel[0].Rate != "0.8" ||
		s.CreditByModel[0].Credits != 6 || s.CreditByModel[0].CreditTokens != 300 {
		t.Fatalf("first model row = %+v, want rate=0.8 credits=6 tokens=300", s.CreditByModel[0])
	}
	if s.CreditByModel[1].Rate != "0.5" || s.CreditByModel[1].Credits != 3 || s.CreditByModel[1].CreditTokens != 300 {
		t.Fatalf("merged model row = %+v, want rate=0.5 credits=3 tokens=300", s.CreditByModel[1])
	}
}

// 旧桶缺倍率时由当前目录倍率回填，并与新桶同倍率记录合并；目录缺失时保留未知行。
func TestCreditLegacyRateFallback(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "u1", "glm-5.2", Delta{TotalTokens: 100, HasTotal: true, Credit: 1, HasCredit: true}, true)
	r.Add(now.Add(time.Hour), "cn", "u1", "glm-5.2", Delta{TotalTokens: 200, HasTotal: true, Credit: 2, HasCredit: true, ModelRate: "0.79"}, true)

	s := r.SnapshotWithRates(24, nil, func(realm, model string) string {
		if realm == "cn" && model == "glm-5.2" {
			return "0.79"
		}
		return ""
	})
	if len(s.CreditByModel) != 1 || s.CreditByModel[0].Rate != "0.79" ||
		s.CreditByModel[0].Credits != 3 || s.CreditByModel[0].CreditTokens != 300 {
		t.Fatalf("fallback model rows = %+v, want legacy merged into rate=0.79", s.CreditByModel)
	}
}

// Snapshot 全口径窗口过滤：窗口外的数据不进**任何**聚合（卡片/表格/时序），
// 切窗口数字随之变化；hours=0 全部历史。Buckets 为窗口内命中的桶数。
func TestSnapshotWindowFilter(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now.Add(-48*time.Hour), "cn", "u", "m", Delta{PromptTokens: 5, HasPromptTokens: true}, true) // 窗口(24h)外
	r.Add(now, "cn", "u", "m", Delta{PromptTokens: 3, HasPromptTokens: true}, true)                    // 窗口内
	s := r.Snapshot(24, nil)
	if s.Totals.Requests != 1 || s.Totals.PromptTokens != 3 {
		t.Fatalf("24h 窗口 totals = %d/%d, want 1/3（48h 前的数据应被过滤）", s.Totals.Requests, s.Totals.PromptTokens)
	}
	if len(s.Series) != 1 || s.Series[0].Scope != "hour" || s.Series[0].PromptTokens != 3 {
		t.Fatalf("series = %+v, want 仅窗口内 1 个小时点", s.Series)
	}
	if s.Buckets != 1 {
		t.Fatalf("buckets = %d, want 1（窗口内命中桶数）", s.Buckets)
	}

	all := r.Snapshot(0, nil)
	if all.Totals.Requests != 2 || all.Totals.PromptTokens != 8 {
		t.Fatalf("全部历史 totals = %d/%d, want 2/8", all.Totals.Requests, all.Totals.PromptTokens)
	}
	// since 是全库数据起点，不受窗口影响。
	if all.Since == "" || s.Since != all.Since {
		t.Fatalf("since 应为全库起点且不随窗口变化: all=%q windowed=%q", all.Since, s.Since)
	}
}

// 显式区间（「今天」/「自定义」）与滚动窗口走同一套全口径过滤；区间是闭区间
// （桶起点落在 [From, To] 内即命中），且 From/To 会回显给面板确认口径。
func TestSnapshotExplicitWindow(t *testing.T) {
	r := New("")
	base := time.Now().Truncate(time.Hour).Add(-5 * time.Hour)
	for i := 0; i < 6; i++ {
		r.Add(base.Add(time.Duration(i)*time.Hour), "cn", "u", "m",
			Delta{PromptTokens: 10, HasPromptTokens: true}, true)
	}
	// 只取中间两小时（base+2h、base+3h）。
	s := r.SnapshotWindow(Window{
		From: base.Add(2 * time.Hour),
		To:   base.Add(3 * time.Hour),
	}, nil, nil)
	if s.Totals.Requests != 2 || s.Totals.PromptTokens != 20 {
		t.Fatalf("显式区间 totals = %d/%d, want 2/20", s.Totals.Requests, s.Totals.PromptTokens)
	}
	if len(s.Series) != 2 || s.Buckets != 2 {
		t.Fatalf("显式区间 series/buckets = %d/%d, want 2/2", len(s.Series), s.Buckets)
	}
	if s.WindowFrom == "" || s.WindowTo == "" {
		t.Fatalf("显式区间应回显 window_from/window_to: %+v", s)
	}
	if _, err := time.Parse(time.RFC3339, s.WindowFrom); err != nil {
		t.Fatalf("window_from 不是 RFC3339: %q", s.WindowFrom)
	}

	// 只有 From（「今天」的形态）：从该点起到最新，全量命中。
	only := r.SnapshotWindow(Window{From: base.Add(4 * time.Hour)}, nil, nil)
	if only.Totals.Requests != 2 {
		t.Fatalf("仅 From 的 totals = %d, want 2", only.Totals.Requests)
	}
	if only.WindowFrom == "" || only.WindowTo != "" {
		t.Fatalf("仅 From 时 window_to 应为空: %+v", only)
	}

	// 空窗口（From/To 全零且 Hours<=0）= 全部历史，与 Snapshot(0) 等价。
	all := r.SnapshotWindow(Window{}, nil, nil)
	if all.Totals.Requests != 6 {
		t.Fatalf("全零窗口 totals = %d, want 6（全部历史）", all.Totals.Requests)
	}
	if all.WindowFrom != "" || all.WindowTo != "" {
		t.Fatalf("全部历史不应回显区间: %+v", all)
	}
}

// 滚动窗口的上限仍是 60 天，且与显式区间互不干扰（From/To 优先）。
func TestWindowBounds(t *testing.T) {
	// From/To 优先于 Hours。
	from := time.Now().Add(-2 * time.Hour)
	gotFrom, gotTo := Window{Hours: 720, From: from}.bounds()
	if !gotFrom.Equal(from) || !gotTo.IsZero() {
		t.Fatalf("From 应优先于 Hours: from=%v to=%v", gotFrom, gotTo)
	}
	// 只有 Hours：起点 = 当前整点往回 Hours-1 小时。
	f, to := Window{Hours: 24}.bounds()
	want := time.Now().Truncate(time.Hour).Add(-23 * time.Hour)
	if !f.Equal(want) || !to.IsZero() {
		t.Fatalf("24h bounds = %v/%v, want %v/零值", f, to, want)
	}
	// Hours<=0 且无 From/To = 全部历史。
	if f, to := (Window{}).bounds(); !f.IsZero() || !to.IsZero() {
		t.Fatalf("空窗口 bounds = %v/%v, want 零值/零值", f, to)
	}
	// 上限 60 天。
	f60, _ := Window{Hours: 100000}.bounds()
	want60 := time.Now().Truncate(time.Hour).Add(-(24*60 - 1) * time.Hour)
	if !f60.Equal(want60) {
		t.Fatalf("超限 Hours 未被夹到 60 天: %v want %v", f60, want60)
	}
}

// 脏 scope（解析失败）不进任何口径，也不会让整次快照失败。
func TestBucketTimeRejectsGarbage(t *testing.T) {
	if _, ok := bucketTime("h:not-a-time"); ok {
		t.Fatal("脏小时 scope 应判定失败")
	}
	if _, ok := bucketTime("d:2026-13-45"); ok {
		t.Fatal("脏日 scope 应判定失败")
	}
	if ts, ok := bucketTime("h:2026-09-30T13"); !ok || ts.Hour() != 13 {
		t.Fatalf("合法小时 scope 解析失败: %v %v", ts, ok)
	}
	if ts, ok := bucketTime("d:2026-09-30"); !ok || ts.Day() != 30 {
		t.Fatalf("合法日 scope 解析失败: %v %v", ts, ok)
	}
}

// Stop 触发最终落盘（Start 后未到防抖间隔也要落）。
func TestLifecycleFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	r := New(path)
	r.Start()
	r.Add(time.Now(), "cn", "u", "m", Delta{PromptTokens: 9, HasPromptTokens: true}, true)
	r.Stop()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Stop 后应有落盘文件: %v", err)
	}
}

// TestCacheHitRateAccumulation（issue #92）：桶累计 hit/miss，credit_by_model
// 与 totals 输出命中率；无缓存观测的桶不参与（rate 零值省略）。
func TestCacheHitRateAccumulation(t *testing.T) {
	r := New(t.TempDir() + "/usage.json")
	base := time.Date(2026, 10, 1, 10, 0, 0, 0, time.Local)
	r.Add(base, "cn", "u1", "glm-5.3", Delta{
		PromptTokens: 1000, HasPromptTokens: true,
		Credit: 1, HasCredit: true, TotalTokens: 1005, HasTotal: true,
		HasCacheTokens: true, CacheHitTokens: 900, CacheMissTokens: 100,
	}, true)
	r.Add(base.Add(time.Minute), "cn", "u1", "glm-5.3", Delta{
		PromptTokens: 1000, HasPromptTokens: true,
		Credit: 2, HasCredit: true, TotalTokens: 1004, HasTotal: true,
		HasCacheTokens: true, CacheHitTokens: 100, CacheMissTokens: 900,
	}, true)
	snap := r.Snapshot(0, nil)
	if got := snap.CreditByModel[0].CacheHitRate; got != 50 {
		t.Fatalf("cache_hit_rate=%v want 50", got)
	}
	if snap.Totals.CacheHitRate != 50 {
		t.Fatalf("totals cache_hit_rate=%v want 50", snap.Totals.CacheHitRate)
	}
	if snap.CreditByModel[0].CacheHitTokens != 1000 || snap.CreditByModel[0].CacheMissTokens != 1000 {
		t.Fatalf("hit/miss 累计错误: %+v", snap.CreditByModel[0])
	}

	// 无缓存观测的桶：rate 零值。
	r2 := New(t.TempDir() + "/usage2.json")
	r2.Add(base, "cn", "u1", "m", Delta{PromptTokens: 10, HasPromptTokens: true, Credit: 1, HasCredit: true, TotalTokens: 12, HasTotal: true}, true)
	if got := r2.Snapshot(0, nil).CreditByModel[0].CacheHitRate; got != 0 {
		t.Fatalf("无缓存观测应 rate=0, got %v", got)
	}
}

// TestCacheTokensPersistRoundtrip：ch/cm 落盘 v4 并无损恢复。
func TestCacheTokensPersistRoundtrip(t *testing.T) {
	dir := t.TempDir()
	r := New(dir + "/usage.json")
	base := time.Date(2026, 10, 1, 10, 0, 0, 0, time.Local)
	r.Add(base, "cn", "u1", "m", Delta{
		Credit: 1, HasCredit: true, TotalTokens: 100, HasTotal: true,
		HasCacheTokens: true, CacheHitTokens: 75, CacheMissTokens: 25,
	}, true)
	r.Save()
	r2 := New(dir + "/usage.json")
	snap := r2.Snapshot(0, nil)
	if snap.CreditByModel[0].CacheHitRate != 75 {
		t.Fatalf("恢复后 cache_hit_rate=%v want 75", snap.CreditByModel[0].CacheHitRate)
	}
}

// TestSeriesGroupsModelAndAccountByDay：「模型×天」「账号×天」分组时序。
// 要点：①小时桶按归属**天**归并（同一天两个小时的桶合成一个点）；
// ②组按 credits 降序；③账号组带昵称与域；④显式窗口过滤生效（窗口外的
// 高积分桶不得撑进小节——它正是 credits 排序里最该被截掉的那个）。
func TestSeriesGroupsModelAndAccountByDay(t *testing.T) {
	r := New("")
	day1AM := time.Date(2026, 9, 30, 9, 0, 0, 0, time.Local)
	day1PM := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	day2 := time.Date(2026, 10, 1, 23, 0, 0, 0, time.Local)
	old := time.Now().AddDate(0, 0, -100)
	// glm-5.2 两天都有（day1 两个小时的桶）；deepseek-v4 仅 day1。
	r.Add(day1AM, "cn", "u1", "glm-5.2", Delta{PromptTokens: 100, HasPromptTokens: true, Credit: 2.0, HasCredit: true}, true)
	r.Add(day1PM, "cn", "u1", "glm-5.2", Delta{PromptTokens: 10, HasPromptTokens: true, Credit: 0.5, HasCredit: true}, true)
	r.Add(day2, "cn", "u1", "glm-5.2", Delta{PromptTokens: 20, HasPromptTokens: true}, true)
	r.Add(day1AM, "cn", "u2", "deepseek-v4", Delta{PromptTokens: 30, HasPromptTokens: true, Credit: 1.0, HasCredit: true}, true)
	// 窗口外的老桶：积分最高，若窗口过滤失效它会冲到模型/账号小节第一位。
	r.Add(old, "cn", "u2", "old-model", Delta{PromptTokens: 999, HasPromptTokens: true, Credit: 99, HasCredit: true}, true)

	from := time.Date(2026, 9, 30, 0, 0, 0, 0, time.Local)
	to := time.Date(2026, 10, 2, 0, 0, 0, 0, time.Local)
	s := r.SnapshotWindow(Window{From: from, To: to}, map[string]string{"u1": "甲号", "u2": "乙号"}, nil)

	if len(s.ModelSeries) != 2 {
		t.Fatalf("model_series = %d 组, want 2（窗口外 old-model 不入组）", len(s.ModelSeries))
	}
	if s.ModelSeries[0].Key != "glm-5.2" || s.ModelSeries[1].Key != "deepseek-v4" {
		t.Fatalf("model_series 顺序 = %s,%s, want glm-5.2,deepseek-v4（credits 降序）",
			s.ModelSeries[0].Key, s.ModelSeries[1].Key)
	}
	g := s.ModelSeries[0]
	if len(g.Points) != 2 || g.Points[0].T != "2026-09-30" || g.Points[1].T != "2026-10-01" {
		t.Fatalf("glm 组点 = %+v, want 2026-09-30 + 2026-10-01 按天升序", g.Points)
	}
	if g.Points[0].Scope != "day" {
		t.Fatalf("分组点 scope = %s, want day", g.Points[0].Scope)
	}
	// 同一天两个小时的桶归并为一个天点：requests=2、credits=2.5。
	if g.Points[0].Requests != 2 || g.Points[0].Credits != 2.5 {
		t.Fatalf("day1 点 = req %d / credits %v, want 2 / 2.5", g.Points[0].Requests, g.Points[0].Credits)
	}
	if g.Points[1].Requests != 1 || g.Points[1].Credits != 0 {
		t.Fatalf("day2 点 = req %d / credits %v, want 1 / 0", g.Points[1].Requests, g.Points[1].Credits)
	}

	if len(s.AccountSeries) != 2 {
		t.Fatalf("account_series = %d 组, want 2", len(s.AccountSeries))
	}
	if s.AccountSeries[0].Key != "u1" || s.AccountSeries[0].Nickname != "甲号" || s.AccountSeries[0].Realm != "cn" {
		t.Fatalf("account_series[0] = %+v, want u1/甲号/cn", s.AccountSeries[0])
	}
	if s.AccountSeries[1].Key != "u2" {
		t.Fatalf("account_series[1].Key = %s, want u2", s.AccountSeries[1].Key)
	}
	// 窗口外 99 分不计：u2 组内只剩 deepseek-v4 的 day1 一天、credits=1.0。
	if len(s.AccountSeries[1].Points) != 1 || s.AccountSeries[1].Points[0].Credits != 1.0 {
		t.Fatalf("u2 组点 = %+v, want 单点 credits=1.0（窗口外老桶不入组）", s.AccountSeries[1].Points)
	}
}

// TestKeySeriesGrouping：「密钥×天」分组时序。
// 要点:①Delta.KeyName 进桶键,同一天两个小时的桶按密钥归并;②经管理密钥
// (空 KeyName)的请求独立成组(前端显示「管理密钥」);③组按 credits 降序。
func TestKeySeriesGrouping(t *testing.T) {
	r := New("")
	day1AM := time.Date(2026, 10, 9, 9, 0, 0, 0, time.Local)
	day1PM := time.Date(2026, 10, 9, 14, 0, 0, 0, time.Local)
	day2 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.Local)
	r.Add(day1AM, "cn", "u1", "glm-5.3", Delta{PromptTokens: 100, HasPromptTokens: true, Credit: 2.0, HasCredit: true, KeyName: "claude-code"}, true)
	r.Add(day1PM, "cn", "u1", "glm-5.3", Delta{PromptTokens: 10, HasPromptTokens: true, Credit: 0.5, HasCredit: true, KeyName: "claude-code"}, true)
	r.Add(day2, "cn", "u1", "glm-5.3", Delta{PromptTokens: 20, HasPromptTokens: true, Credit: 1.0, HasCredit: true, KeyName: "my-agent"}, true)
	// 经管理密钥(api_key)来的请求:KeyName 空,独立成「管理密钥」组。
	r.Add(day1AM, "cn", "u2", "glm-5.3", Delta{PromptTokens: 5, HasPromptTokens: true, Credit: 0.1, HasCredit: true}, true)

	from := time.Date(2026, 10, 9, 0, 0, 0, 0, time.Local)
	to := time.Date(2026, 10, 11, 0, 0, 0, 0, time.Local)
	snap := r.SnapshotWindow(Window{From: from, To: to}, nil, nil)

	if len(snap.KeySeries) != 3 {
		t.Fatalf("key_series = %d 组, want 3 (claude-code/my-agent/管理密钥)", len(snap.KeySeries))
	}
	if snap.KeySeries[0].Key != "claude-code" {
		t.Fatalf("第一组 = %q, want claude-code (credits 降序)", snap.KeySeries[0].Key)
	}
	if len(snap.KeySeries[0].Points) != 1 || snap.KeySeries[0].Points[0].Credits != 2.5 {
		t.Fatalf("claude-code 组点 = %+v, want 单点 credits=2.5(同日两桶归并)", snap.KeySeries[0].Points)
	}
	var mgmtName, agentName string
	for _, g := range snap.KeySeries {
		switch g.Key {
		case "":
			mgmtName = "found"
			if len(g.Points) != 1 || g.Points[0].Credits != 0.1 {
				t.Fatalf("管理密钥组点 = %+v, want 单点 credits=0.1", g.Points)
			}
		case "my-agent":
			agentName = "found"
		}
	}
	if mgmtName == "" || agentName == "" {
		t.Fatalf("缺少 管理密钥(空名)/my-agent 组: %+v", snap.KeySeries)
	}
}
