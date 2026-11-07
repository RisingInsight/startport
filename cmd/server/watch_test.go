package main

import (
	"strings"
	"testing"
)

// 社区监控核心逻辑：首轮建基线静默、变化告警、新 issue 报标题、无变化零通知。
// fetch 打桩（不联网），notify 捕获（断言告警内容）。
func TestCommunityWatchDiffs(t *testing.T) {
	st := &watchState{issuesSeen: map[float64]bool{}}
	var notes []string
	notify := func(key, title, body string) { notes = append(notes, key+" :: "+body) }
	logf := func(string, ...any) {}
	cfg := CommunityWatchCfg{Enabled: true, GitHubRepo: "o/n", GitHubDiscussio: 1, DevToArticle: "u/slug"}

	// 三轮状态序列：首轮基线（静默）→ star+反应变化 → 新 issue + 评论变化
	series := []struct {
		gh ghSnap
		dt dtSnap
	}{
		{ghSnap{Stars: 0, Forks: 0, OpenIss: 0, Comments: 0, Issues: []ghIssue{{Number: 1, Title: "初次报告"}}}, dtSnap{}},
		{ghSnap{Stars: 3, Forks: 0, OpenIss: 1, Comments: 0, Issues: []ghIssue{{Number: 1, Title: "初次报告"}}}, dtSnap{Reactions: 2}},
		{ghSnap{Stars: 3, Forks: 0, OpenIss: 2, Comments: 2, Issues: []ghIssue{
			{Number: 1, Title: "初次报告"},
			{Number: 2, Title: "登录失败 bug"},
		}}, dtSnap{Reactions: 2, Comments: 1}},
	}
	for _, row := range series {
		sGh, sDt := row.gh, row.dt
		f := &watchFetch{
			github: func(repo string, discussion int) (ghSnap, error) { return sGh, nil },
			devto:  func(path string) (dtSnap, error) { return sDt, nil },
		}
		runWatchOnce(st, cfg, f, notify, logf)
	}

	// 两个数据源各自独立通知（不同冷却键）：共 4 条，顺序 gh→dt→gh→dt。
	if len(notes) != 4 {
		t.Fatalf("notify 次数 = %d, want 4（首轮静默，后两轮各两条）\n%v", len(notes), notes)
	}
	if !strings.Contains(notes[0], "watch:github") || !strings.Contains(notes[0], "star 0→3") {
		t.Errorf("第一条应报 star 变化: %q", notes[0])
	}
	if !strings.Contains(notes[1], "watch:devto") || !strings.Contains(notes[1], "反应 0→2") {
		t.Errorf("第二条应报 dev.to 反应变化: %q", notes[1])
	}
	if !strings.Contains(notes[2], "新 issue #2") || !strings.Contains(notes[2], "登录失败 bug") {
		t.Errorf("第三条应报新 issue 标题: %q", notes[2])
	}
	if !strings.Contains(notes[2], "Discussions #1 新回复") {
		t.Errorf("第三条应报 Discussions 回复: %q", notes[2])
	}
	if !strings.Contains(notes[3], "watch:devto") || !strings.Contains(notes[3], "评论 0→1") {
		t.Errorf("第四条应报 dev.to 评论变化: %q", notes[3])
	}
}

// 无变化轮：连续相同状态 → 零通知。
func TestCommunityWatchQuietWhenNoChange(t *testing.T) {
	st := &watchState{issuesSeen: map[float64]bool{}}
	logf := func(string, ...any) {}
	var notes []string
	notify := func(key, title, body string) { notes = append(notes, key) }
	f := &watchFetch{
		github: func(repo string, discussion int) (ghSnap, error) {
			return ghSnap{Stars: 5, Forks: 1, Comments: 1}, nil
		},
		devto: func(path string) (dtSnap, error) { return dtSnap{Reactions: 3, Comments: 1}, nil },
	}
	cfg := CommunityWatchCfg{Enabled: true, GitHubRepo: "o/n", GitHubDiscussio: 1, DevToArticle: "u/slug"}
	runWatchOnce(st, cfg, f, notify, logf) // 基线
	runWatchOnce(st, cfg, f, notify, logf) // 无变化
	runWatchOnce(st, cfg, f, notify, logf) // 仍无变化
	if len(notes) != 0 {
		t.Fatalf("无变化轮不应告警: %v", notes)
	}
}

// 单源：GitHub 未配置时 dev.to 照常工作，github fetcher 不被调用。
func TestCommunityWatchDevToOnly(t *testing.T) {
	st := &watchState{issuesSeen: map[float64]bool{}}
	var notes []string
	notify := func(key, title, body string) { notes = append(notes, key+" :: "+body) }
	logf := func(string, ...any) {}
	githubCalled := false
	f := &watchFetch{
		github: func(repo string, discussion int) (ghSnap, error) {
			githubCalled = true
			return ghSnap{}, nil
		},
		devto: func(path string) (dtSnap, error) { return dtSnap{Reactions: 5, Comments: 2}, nil },
	}
	cfg := CommunityWatchCfg{Enabled: true, DevToArticle: "u/slug"}
	runWatchOnce(st, cfg, f, notify, logf) // 基线（静默）
	// 第二轮：反应 5→7（有变化 → 1 条告警）
	f2 := &watchFetch{
		github: func(repo string, discussion int) (ghSnap, error) {
			githubCalled = true
			return ghSnap{}, nil
		},
		devto: func(path string) (dtSnap, error) { return dtSnap{Reactions: 7, Comments: 2}, nil },
	}
	runWatchOnce(st, cfg, f2, notify, logf)
	if githubCalled {
		t.Fatal("GitHub 未配置时不应调用 github fetcher")
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "watch:devto") {
		t.Fatalf("notes = %v, want 单条 dev.to 变化", notes)
	}
}

// notify 为 nil：仅推进基线不 panic（生产中 notify 恒非 nil，防御路径）。
func TestCommunityWatchNilNotify(t *testing.T) {
	st := &watchState{issuesSeen: map[float64]bool{}}
	f := &watchFetch{
		github: func(repo string, discussion int) (ghSnap, error) {
			return ghSnap{Stars: 1, Issues: []ghIssue{{Number: 9, Title: "x"}}}, nil
		},
	}
	cfg := CommunityWatchCfg{Enabled: true, GitHubRepo: "o/n"}
	runWatchOnce(st, cfg, f, nil, func(string, ...any) {})
	if len(st.issuesSeen) != 1 {
		t.Fatalf("issuesSeen = %v, want 1 entry", st.issuesSeen)
	}
}
