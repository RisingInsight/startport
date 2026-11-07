// watch.go 社区信号监控（运维闭环的推广侧延伸）：周期性轮询仓库与文章的公开
// 统计——GitHub star/fork/issue、Discussions 回复、dev.to 反应/评论——发现变化
// 就经告警管道推送。用户不用记得看，数据来找他。
//
// 设计边界：
//   - 只调**公开匿名** API（GitHub REST / dev.to 公开文章接口），不带凭据、
//     每周期 3-4 个请求，远低于限流（匿名 60 次/小时）；
//   - 掘金统计需要登录 cookie，匿名接口拿不到——由掘金 APP 自带的通知兜底，
//     本循环不覆盖（明确不做）；
//   - 首轮观测只记基线不告警（进程重启后同样静默重建基线）；
//   - 变化摘要每个数据源每周期至多一条（同键走告警冷却天然合并）。
//
// 配置热生效：循环每个 tick 重读 config 的 community_watch 节（同 backupLoop）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/RisingInsight/startport/internal/logfmt"
)

// CommunityWatchCfg community_watch 配置节的运行时形态（与 config.Config.CommunityWatch 同构）。
type CommunityWatchCfg struct {
	Enabled         bool   `json:"enabled"`
	GitHubRepo      string `json:"github_repo"`       // "owner/name"；空 = 不监控 GitHub
	GitHubDiscussio int    `json:"github_discussion"` // Discussions 编号；0 = 不监控回复
	DevToArticle    string `json:"devto_article"`     // dev.to 文章路径 "username/slug"；空 = 不监控
	IntervalMinutes int    `json:"interval_minutes"`
}

// ghIssue 一个 issue/PR 的最小观测（新条目检测用）。
type ghIssue struct {
	Number float64
	Title  string
	IsPR   bool
}

// ghSnap GitHub 侧一轮观测。
type ghSnap struct {
	Stars    int
	Forks    int
	OpenIss  int
	Comments int // Discussions 回复数（未配置 discussion 时无意义）
	Issues   []ghIssue
}

// dtSnap dev.to 侧一轮观测。
type dtSnap struct {
	Reactions int
	Comments  int
}

// watchFetch 数据获取函数集合（测试注入点；生产走匿名 HTTP，见 alertFetch）。
type watchFetch struct {
	github func(repo string, discussion int) (ghSnap, error)
	devto  func(path string) (dtSnap, error)
}

// watchState 监控基线（运行态；重启后静默重建，不产生重复告警）。
type watchState struct {
	ghSeen        bool
	ghStars       int
	ghForks       int
	ghOpenIss     int
	issuesSeen    map[float64]bool
	discussSeen   bool
	discussComnts int
	devtoSeen     bool
	dtReactions   int
	dtComments    int
}

// communityWatchLoop 社区信号监控循环（serverMain 用 ctx 挂钩启停）。
func communityWatchLoop(ctx context.Context, cfgPath string, notify func(key, title, body string), logf func(format string, args ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	st := &watchState{issuesSeen: map[float64]bool{}}
	f := alertFetch
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cfg, err := Load(cfgPath)
		if err != nil {
			logf("[watch] 读取配置失败，本轮跳过: %v", err)
			continue
		}
		if !cfg.CommunityWatch.Enabled {
			continue
		}
		runWatchOnce(st, cfg.CommunityWatch, &f, notify, logf)
	}
}

// runWatchOnce 一轮监控：拉取全部已配置数据源 → 与基线对比 → 有变化经 notify 推送。
// 首轮只建基线（静默）。notify 为 nil 时仅更新状态（测试直接断言 notify 序列）。
func runWatchOnce(st *watchState, cfg CommunityWatchCfg, f *watchFetch, notify func(key, title, body string), logf func(format string, args ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if f == nil {
		f = &alertFetch
	}
	if cfg.GitHubRepo != "" {
		if snap, err := f.github(cfg.GitHubRepo, cfg.GitHubDiscussio); err != nil {
			logf("WARN: [watch] github %s: %v", cfg.GitHubRepo, err)
		} else if body := diffGitHub(st, snap, cfg.GitHubDiscussio); body != "" && notify != nil {
			notify("watch:github", "StarPort：仓库有动静", body)
		}
	}
	if cfg.DevToArticle != "" {
		if snap, err := f.devto(cfg.DevToArticle); err != nil {
			logf("WARN: [watch] dev.to %s: %v", cfg.DevToArticle, err)
		} else if body := diffDevTo(st, snap); body != "" && notify != nil {
			notify("watch:devto", "StarPort：dev.to 文章有反馈", body)
		}
	}
}

// diffGitHub 对比 GitHub 基线并生成变化摘要，同时推进基线。返回空串 = 无变化（或首轮建基线）。
func diffGitHub(st *watchState, snap ghSnap, discussion int) string {
	var lines []string
	if !st.ghSeen {
		// 首轮：登记全部基线，静默。
		st.ghStars, st.ghForks, st.ghOpenIss = snap.Stars, snap.Forks, snap.OpenIss
		for _, is := range snap.Issues {
			st.issuesSeen[is.Number] = true
		}
		st.discussSeen, st.discussComnts = discussion > 0, snap.Comments
		st.ghSeen = true
		return ""
	}
	if snap.Stars != st.ghStars {
		lines = append(lines, fmt.Sprintf("star %d→%d", st.ghStars, snap.Stars))
	}
	if snap.Forks != st.ghForks {
		lines = append(lines, fmt.Sprintf("fork %d→%d", st.ghForks, snap.Forks))
	}
	for _, is := range snap.Issues {
		if !st.issuesSeen[is.Number] {
			st.issuesSeen[is.Number] = true
			kind := "issue"
			if is.IsPR {
				kind = "PR"
			}
			lines = append(lines, fmt.Sprintf("新 %s #%d：%s", kind, int(is.Number), logfmt.Truncate(is.Title, 40)))
		}
	}
	if discussion > 0 && st.discussSeen && snap.Comments != st.discussComnts {
		lines = append(lines, fmt.Sprintf("Discussions #%d 新回复（评论 %d→%d）", discussion, st.discussComnts, snap.Comments))
	}
	st.ghStars, st.ghForks, st.ghOpenIss = snap.Stars, snap.Forks, snap.OpenIss
	if discussion > 0 {
		st.discussSeen, st.discussComnts = true, snap.Comments
	}
	return strings.Join(lines, "\n")
}

// diffDevTo 对比 dev.to 基线并生成变化摘要（含基线推进）。空串 = 无变化。
func diffDevTo(st *watchState, snap dtSnap) string {
	if !st.devtoSeen {
		st.dtReactions, st.dtComments = snap.Reactions, snap.Comments
		st.devtoSeen = true
		return ""
	}
	var lines []string
	if snap.Reactions != st.dtReactions {
		lines = append(lines, fmt.Sprintf("反应 %d→%d", st.dtReactions, snap.Reactions))
	}
	if snap.Comments != st.dtComments {
		lines = append(lines, fmt.Sprintf("评论 %d→%d", st.dtComments, snap.Comments))
	}
	st.dtReactions, st.dtComments = snap.Reactions, snap.Comments
	return strings.Join(lines, "\n")
}

// alertFetch 生产获取器：匿名 HTTP（公开仓库/公开文章无需凭据）。
var alertFetch = watchFetch{
	github: func(repo string, discussion int) (ghSnap, error) {
		var snap ghSnap
		var repoJSON struct {
			StargazersCount int `json:"stargazers_count"`
			ForksCount      int `json:"forks_count"`
			OpenIssuesCount int `json:"open_issues_count"`
		}
		if err := getJSON("https://api.github.com/repos/"+repo, &repoJSON); err != nil {
			return snap, err
		}
		snap.Stars, snap.Forks, snap.OpenIss = repoJSON.StargazersCount, repoJSON.ForksCount, repoJSON.OpenIssuesCount
		var issuesJSON []struct {
			Number      float64   `json:"number"`
			Title       string    `json:"title"`
			PullRequest *struct{} `json:"pull_request"`
		}
		if err := getJSON("https://api.github.com/repos/"+repo+"/issues?state=open&per_page=20&sort=created&direction=desc", &issuesJSON); err == nil {
			for _, it := range issuesJSON {
				snap.Issues = append(snap.Issues, ghIssue{Number: it.Number, Title: it.Title, IsPR: it.PullRequest != nil})
			}
		}
		if discussion > 0 {
			var discJSON struct {
				Comments int `json:"comments"`
			}
			if err := getJSON(fmt.Sprintf("https://api.github.com/repos/%s/discussions/%d", repo, discussion), &discJSON); err == nil {
				snap.Comments = discJSON.Comments
			}
		}
		return snap, nil
	},
	devto: func(path string) (dtSnap, error) {
		var artJSON struct {
			PublicReactionsCount int `json:"public_reactions_count"`
			CommentsCount        int `json:"comments_count"`
		}
		err := getJSON("https://dev.to/api/articles/"+path, &artJSON)
		return dtSnap{Reactions: artJSON.PublicReactionsCount, Comments: artJSON.CommentsCount}, err
	},
}

func getJSON(url string, dst any) error {
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}
