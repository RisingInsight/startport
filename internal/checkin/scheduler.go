// scheduler.go 平台签到调度器（账号中心 v1.6.0 · T3；架构 §5.1）。
//
//   - 每小时对表：本地小时数命中 Hours（默认 [9]）且当天未跑 → 全账号一轮；
//   - 失败重试（PRD §8 默认）：30 分钟后重试，最多 2 次；仍失败 → 该账号
//     CheckinRecord{failed} + 面板账号行标红（不引入通知通道）；
//   - 并发：逐账号串行（平台账号个位数；串行天然限频，满足"频率与官方客户端
//     一致"红线）；账号级 TryLock 防手动 + 定时双跑；
//   - 手动触发：panel API 立即执行，绕过小时窗口。
//
// 与 CodeBuddy 的 internal/scheduler 完全独立（两套语义命名强区分，架构 §11
// 风险 2）：本包日志前缀 `[checkin:<platform>]`。
package checkin

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/RisingInsight/startport/internal/account"
)

// SchedulerConfig 调度器装配参数（零值 = 全默认）。
type SchedulerConfig struct {
	Store *account.Store // 事实来源 + 签到历史落点（必填）

	Hours     []int         // 每日触发的本地小时数（默认 [9]）
	Retry     time.Duration // 失败重试间隔（默认 30 分钟）
	MaxRetry  int           // 失败最大重试次数（默认 2）
	OnSuccess func(platform, credLabel string)
	// OnSuccess 签到成功/已签过后的钩子（账号中心用它 Revive 被额度/auth 判死
	// 的引擎凭证——Revive 幂等且对 already 同样无害，已签过也说明登录态活着）。
	// OnRefresh 登录态刷新并回写 accounts.json 后的钩子（main 注入 = acctchan.Sync
	// 重投影）。必须在 UpdateCred 之后调用，否则 managed 渠道仍持旧 secret。
	OnRefresh func(platform, accountID string)

	Now  func() time.Time                 // 时钟注入（测试）
	Logf func(format string, args ...any) // 日志注入（测试静音）
}

// Scheduler 平台签到调度器。
type Scheduler struct {
	cfg        SchedulerConfig
	mu         sync.Mutex
	locks      map[string]*sync.Mutex // 账号级 TryLock（防手动+定时双跑）
	lastRunDay string                 // 当天已跑的小时窗口日期（YYYY-MM-DD）
	retries    map[string]*retryState // 账号 → 重试状态
	stop       chan struct{}
	stopped    bool
}

type retryState struct {
	attempts int
	nextAt   time.Time
}

// NewScheduler 构造并启动对表循环。
func NewScheduler(cfg SchedulerConfig) *Scheduler {
	if cfg.Store == nil {
		panic("checkin: SchedulerConfig.Store 必填")
	}
	if len(cfg.Hours) == 0 {
		cfg.Hours = []int{9}
	}
	if cfg.Retry <= 0 {
		cfg.Retry = 30 * time.Minute
	}
	if cfg.MaxRetry <= 0 {
		cfg.MaxRetry = 2
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	s := &Scheduler{
		cfg:     cfg,
		locks:   map[string]*sync.Mutex{},
		retries: map[string]*retryState{},
		stop:    make(chan struct{}),
	}
	go s.loop()
	return s
}

// SetNow 注入时钟（测试；必须在构造后立即调用）。
func (s *Scheduler) SetNow(f func() time.Time) {
	s.mu.Lock()
	s.cfg.Now = f
	s.mu.Unlock()
}

// Close 停止对表循环。
func (s *Scheduler) Close() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	close(s.stop)
	s.mu.Unlock()
}

// loop 对表循环（每分钟醒一次；对表逻辑幂等——同一天同一小时只跑一轮）。
func (s *Scheduler) loop() {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-tick.C:
			now := s.cfg.Now()
			s.runRetries()
			for _, hour := range s.cfg.Hours {
				if now.Hour() == hour {
					s.runDailyIfDue(now)
				}
			}
		}
	}
}

// dayKey 当天日期键。
func dayKey(now time.Time) string { return now.Format("2006-01-02") }

// runDailyIfDue 当天未跑过 → 全账号一轮（跨重启漏跑一天就错过了——与源实现
// 的"每天一次"口径一致，重试只覆盖当天内的失败）。
func (s *Scheduler) runDailyIfDue(now time.Time) {
	s.mu.Lock()
	today := dayKey(now)
	if s.lastRunDay == today {
		s.mu.Unlock()
		return
	}
	s.lastRunDay = today
	s.mu.Unlock()
	s.cfg.Logf("[checkin] 每日平台签到开始（%s）", today)
	s.RunAll(context.Background())
}

// runRetries 到期的失败重试（30m × 最多 MaxRetry 次）。
func (s *Scheduler) runRetries() {
	now := s.cfg.Now()
	s.mu.Lock()
	var due []string
	for id, st := range s.retries {
		if !now.Before(st.nextAt) {
			due = append(due, id)
		}
	}
	s.mu.Unlock()
	for _, id := range due {
		s.mu.Lock()
		st := s.retries[id]
		s.mu.Unlock()
		if st == nil {
			continue
		}
		a := s.cfg.Store.Get(id)
		if a == nil {
			s.mu.Lock()
			delete(s.retries, id)
			s.mu.Unlock()
			continue
		}
		s.cfg.Logf("[checkin:%s] 账号 %s 第 %d 次重试", a.Platform, a.Label, st.attempts)
		res, err := s.checkinAccount(context.Background(), a.ID, false)
		if err == nil && res.Status != StatusFailed {
			// 重试成功（ok / already）。
			s.mu.Lock()
			delete(s.retries, id)
			s.mu.Unlock()
			continue
		}
		// 仍失败：安排下一次（或放弃）。重试路径由这里统一记账
		// （checkinAccount(schedule=false) 不重复计）。
		s.scheduleRetry(id)
	}
}

// scheduleRetry 记一次待重试（超过 MaxRetry 放弃——面板台账里已是 failed 标红）。
func (s *Scheduler) scheduleRetry(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.retries[id]
	if st == nil {
		st = &retryState{attempts: 0}
		s.retries[id] = st
	}
	st.attempts++
	if st.attempts > s.cfg.MaxRetry {
		s.cfg.Logf("[checkin] 账号 %s 连续失败 %d 次，放弃重试（等待下一日）", id, st.attempts-1)
		delete(s.retries, id)
		return
	}
	st.nextAt = s.cfg.Now().Add(s.cfg.Retry)
}

// tryLock 账号级互斥（防手动 + 定时双跑）；返回释放函数。
func (s *Scheduler) tryLock(id string) (func(), bool) {
	s.mu.Lock()
	m := s.locks[id]
	if m == nil {
		m = &sync.Mutex{}
		s.locks[id] = m
	}
	s.mu.Unlock()
	if !m.TryLock() {
		return nil, false
	}
	return m.Unlock, true
}

// RunAll 全账号串行一轮（定时触发 / 手动全量）。失败进重试队列。
func (s *Scheduler) RunAll(ctx context.Context) {
	for _, a := range s.cfg.Store.List("") {
		if a.Disabled {
			continue // 停用账号跳过（与 CodeBuddy 口径一致）
		}
		if _, ok := For(a.Platform); !ok {
			continue // 平台无签到（qoder-intl / custom）
		}
		_, _ = s.checkinAccount(ctx, a.ID, true)
	}
}

// CheckinAccount 手动单账号签到（绕过小时窗口；不进重试队列——重试是定时
// 触发路径的语义，手动点过之后由用户自己决定是否再点）。
func (s *Scheduler) CheckinAccount(ctx context.Context, id string) (Result, error) {
	return s.checkinAccount(ctx, id, false)
}

// checkinAccount 单账号签到内部实现（TryLock 防双跑；结果经 RecordCheckin 落
// 环形历史；成功后触发 OnSuccess 复活钩子；schedule=true 时失败进重试队列）。
func (s *Scheduler) checkinAccount(ctx context.Context, id string, schedule bool) (Result, error) {
	a := s.cfg.Store.Get(id)
	if a == nil {
		return Result{}, fmt.Errorf("账号 %q 不存在", id)
	}
	checker, ok := For(a.Platform)
	if !ok {
		return Result{}, fmt.Errorf("%w（%s）", ErrPlatformNotSupported, a.Platform)
	}
	unlock, locked := s.tryLock(id)
	if !locked {
		return Result{}, fmt.Errorf("该账号的签到正在执行中")
	}
	defer unlock()

	// 账号可能在等锁期间被删：拿到锁后重读。
	a = s.cfg.Store.Get(id)
	if a == nil {
		return Result{}, fmt.Errorf("账号 %q 不存在", id)
	}

	res, err := checker.CheckIn(ctx, a)
	if err != nil && isAuthExpiredErr(err) && canRefresh(a) {
		// 登录态失效 + 有刷新凭证 → **强制刷新 + 回写 + 重试一次**
		// （架构 §4.7 最小闭环：401 换证前 force refresh，成功则重试同一凭证
		// 一次再进换证；自管存储回写已获授权——不回写则重启即丢）。
		if rerr := s.refreshAndWriteback(ctx, a); rerr != nil {
			s.cfg.Logf("[checkin:%s] 账号 %s 登录态刷新失败: %v", a.Platform, a.Label, rerr)
		} else {
			res, err = checker.CheckIn(ctx, a) // 用刷新后的 blob 重试一次
		}
	}
	if err != nil {
		s.cfg.Store.RecordCheckin(id, account.CheckinRecord{
			At: s.cfg.Now(), Status: StatusFailed, Error: err.Error(),
		})
		s.cfg.Logf("[checkin:%s] 账号 %s 签到失败: %v", a.Platform, a.Label, err)
		return res, err
	}
	rec := account.CheckinRecord{At: s.cfg.Now(), Status: res.Status, Reward: res.Reward}
	if res.Status == StatusFailed {
		rec.Error = res.Message
	} else {
		// already/ok 的文案是说明，不是错误——写进 Error 会让台账看起来像失败。
		rec.Message = res.Message
	}
	s.cfg.Store.RecordCheckin(id, rec)
	switch res.Status {
	case StatusOK, StatusAlready:
		// 成功/已签过都说明登录态活着：复活被额度或 auth 判死的引擎凭证
		// （Revive 幂等；对 healthy 凭证同样无害）。
		if s.cfg.OnSuccess != nil {
			s.cfg.OnSuccess(a.Platform, a.ID)
		}
		s.cfg.Logf("[checkin:%s] 账号 %s 签到结果: %s（%s）", a.Platform, a.Label, res.Status, res.Message)
	case StatusFailed:
		s.cfg.Logf("[checkin:%s] 账号 %s 签到失败: %s", a.Platform, a.Label, res.Message)
		if schedule {
			s.scheduleRetry(id)
		}
	}
	return res, nil
}

// canRefresh 报告账号是否具备刷新闭环的前提（平台注册表声明 + blob 带
// refresh_token；没有 → 账号页提示"无法自动刷新"）。
func canRefresh(a *account.Account) bool {
	return account.CanRefresh(a)
}

// refreshAndWriteback 强制刷新登录态并回写 accounts.json
// （架构 §4.7：网关自管存储的刷新结果**回写是安全且必要的**——否则重启即丢，
// 账号必死；ref 对桌面端文件"只读不回写"的顾虑在这里不成立）。
func (s *Scheduler) refreshAndWriteback(ctx context.Context, a *account.Account) error {
	updated, err := RefreshAccount(ctx, a)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(updated)
	if err != nil {
		return err
	}
	if uerr := s.cfg.Store.UpdateCred(a.ID, raw); uerr != nil {
		return uerr
	}
	s.cfg.Logf("[checkin:%s] 账号 %s 的登录态已刷新并回写", a.Platform, a.Label)
	if s.cfg.OnRefresh != nil {
		s.cfg.OnRefresh(a.Platform, a.ID)
	}
	return nil
}
