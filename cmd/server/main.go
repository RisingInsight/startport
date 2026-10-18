// main.go starport 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/RisingInsight/startport/internal/apikeys"
	"github.com/RisingInsight/startport/internal/auth"
	"github.com/RisingInsight/startport/internal/extchan"
	"github.com/RisingInsight/startport/internal/livecfg"
	"github.com/RisingInsight/startport/internal/panel"
	"github.com/RisingInsight/startport/internal/pool"
	"github.com/RisingInsight/startport/internal/redisstore"
	"github.com/RisingInsight/startport/internal/reqlog"
	"github.com/RisingInsight/startport/internal/scheduler"
	"github.com/RisingInsight/startport/internal/server"
	"github.com/RisingInsight/startport/internal/session"
	"github.com/RisingInsight/startport/internal/update"
	"github.com/RisingInsight/startport/internal/upstream"
	"github.com/RisingInsight/startport/internal/usage"
)

// appVersion 网关版本（StarPort 独立版本线起点），透出到 /startport/api/overview。
//
// 用 var 而非 const：const 无法被 `go build -ldflags -X` 注入，CI 也就无法把
// git tag 打成二进制版本号（自动更新的「版本源」必须可注入）。默认值保留源码
// 版本，本地裸构建（不带 -X）行为与改动前完全一致。
var appVersion = "1.5.0"

// appCommit / appBuildTime 构建期元信息，由 CI 经 -ldflags -X 注入（本地构建为空）。
// 仅用于启动日志与 /startport/api/overview 观测，不参与任何路由或出站决策。
var (
	appCommit    = ""
	appBuildTime = ""
)

// versionString 组合版本号与构建元信息，供启动日志透出（无 commit 时只回版本号）。
func versionString() string {
	s := appVersion
	if appCommit != "" {
		s += " (" + appCommit
		if appBuildTime != "" {
			s += ", " + appBuildTime
		}
		s += ")"
	}
	return s
}

// maskKeyForLog 对密钥做日志掩码：保留前 6 位便于识别，其余打星（F-007：避免
// 明文密钥写入 stdout/容器日志）。空串返回空串。
func maskKeyForLog(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 6 {
		return "****"
	}
	return s[:6] + "****"
}

// checkAPIKeySafety 启动期安全默认校验：
//   - 占位符 / 已知弱口令 → 拒绝启动（fail fast，避免"照抄示例就上线"被白嫖）；
//   - 密钥过短 → 告警（不阻断，兼容既有短密钥部署）；
//   - 空密钥 + 非回环监听 → 醒目告警（此时任何能连到该端口的人都能调用与改配置）；
//   - 非回环监听 + 无请求体上限 → 醒目告警（chatCompletions 的 io.ReadAll 无上限
//     读入，单个超大请求即可耗尽内存；只告警不改默认——agent 大上下文/多图 base64
//     可达几十上百 MB，改默认上限会制造 413 新故障源）。
func checkAPIKeySafety(cfg *Config) {
	placeholder, tooShort := cfg.APIKeyWeakness()
	if placeholder {
		log.Fatalf("api_key %q 是占位符/弱口令，拒绝启动。"+
			"请改成长随机串（例如 `openssl rand -base64 24`），或留空并确保仅本机访问。", cfg.APIKey)
	}
	if tooShort {
		log.Printf("WARN: api_key 长度不足 %d（当前 %d 字符），建议换成长随机串。",
			minAPIKeyLen, len(strings.TrimSpace(cfg.APIKey)))
	}
	if strings.TrimSpace(cfg.APIKey) == "" && !cfg.ListenIsLoopback() {
		log.Printf("WARN: 未设置 api_key 且监听 %s 非本机回环——网关对同网段/公网完全开放"+
			"（任何人可调用接口并改配置）。请设置 api_key，或把 listen 改为 127.0.0.1:7863。", cfg.Listen)
	}
	if !cfg.ListenIsLoopback() && cfg.Server.MaxBodyMB <= 0 {
		log.Printf("[WARN] 监听地址 %s 非本机回环且未设置请求体上限（server.max_body_mb）："+
			"单个超大请求即可耗尽内存；公网/局域网部署建议在 config.json 设置 server.max_body_mb（如 256）", cfg.Listen)
	}
}

// usagePathFor 由 state 文件路径推出用量文件路径：同目录、文件名 usage.json。
// 这样 config 里改 state_file 时用量数据跟着走，不需要额外配置项。
func usagePathFor(stateFile string) string { return stateSibling(stateFile, "usage.json") }

// stateSibling 返回与 state 文件同目录的指定文件名路径（相对路径场景回落当前目录）。
// usage.json（用量记录）与 output_probes.json（模型上限探测）共用本规则。
func stateSibling(stateFile, name string) string {
	dir := filepath.Dir(stateFile)
	if dir == "" || dir == "." {
		return name
	}
	return filepath.Join(dir, name)
}

func main() {
	cfgPath := flag.String("config", "config.json", "配置文件路径（默认当前目录 config.json；不存在时自动生成推荐配置）")
	openBrowserFlag := flag.Bool("open", true,
		"启动后自动打开默认浏览器到管理面板（Docker 容器内自动跳过；-open=false 关闭）")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		// errors.Is 才能看穿 Load 里 fmt.Errorf("%w") 的包装；os.IsNotExist 不行。
		if errors.Is(err, fs.ErrNotExist) {
			// 首次运行：目录下没有配置 → 自动落一份推荐配置（含随机 api_key）再加载。
			// 双击 exe / 裸跑 docker 即开，无需先手工复制样例。
			if key, werr := WriteDefault(*cfgPath); werr == nil {
				log.Printf("config %s 不存在，已生成推荐配置（api_key=%s，完整值记录在该文件里，可自行修改）", *cfgPath, maskKeyForLog(key))
				cfg, err = Load(*cfgPath)
			}
			if err != nil {
				// 生成失败（目录只读等）：退回纯默认 + env（旧行为兜底），不阻塞启动。
				log.Printf("config %s not found (auto-generate failed), using defaults+env: %v", *cfgPath, err)
				cfg, err = Load("")
			}
		}
		if err != nil {
			fatalExit("load config: %v", err)
		}
	}

	// 安全默认校验：占位符/弱口令直接拒绝启动（防"照抄示例就上线"被白嫖）；
	// 空密钥 + 非回环监听给出醒目告警（此时网关对同网段/公网完全开放）。
	checkAPIKeySafety(cfg)

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		fatalExit("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	// redisstore：未配置/连接失败 → Noop（纯内存模式，一切功能照常）。
	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)

	p := pool.New(cfg.StateFile)
	// 停机序：先 pool.Close()（最后一次 Flush → SaveState 已提交到 store），
	// 再 store.Close() 排空在途异步写（最后一笔 Redis 镜像必须写完才关连接）。
	defer func() {
		p.Close()
		_ = store.Close()
	}()

	// Agent 接入密钥：与 state 文件同目录（data/apikeys.json），只存 SHA-256 摘要。
	// 面板创建/管理；/v1/* 鉴权接受（chatAuth），面板与 /status 不接受。
	keyStore := apikeys.New(stateSibling(cfg.StateFile, "apikeys.json"))
	keyStore.Start()
	defer keyStore.Stop()
	p.SetStore(store)
	p.RestoreFromSnapshot() // 择新恢复：Redis 快照比本地新才采用，否则本地优先
	p.SyncToDir(auths)      // 与 auths 目录对齐：新账号加入、已删除文件账号剔除（状态保留）

	// 熔断器 + 在途上限（含 global 分档）+ 连败降权 + 闲置补偿调优（从 config 注入，
	// 非正值回退默认）。
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetMaxInFlightGlobal(cfg.Pool.MaxInFlightGlobal) // global 域 WAF 风控分档（P1-1）
	p.SetDegrade(cfg.Pool.DegradeThreshold, cfg.DegradeCooldownDur, cfg.DegradeCooldownMaxD)
	p.SetSoftRateMax(cfg.SoftRateMaxDur)                 // 软冷却指数退避封顶（soft_rate_max，默认 2h）
	p.SetCostExploreInterval(cfg.CostExploreIntervalDur) // costTier 探索窗口（issue #136，默认 30m；0 关停）
	p.SetCreditFloor(cfg.Pool.CreditFloor)               // 积分保底（默认 0 = 关闭）
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)
	p.SetPreferExpiring(cfg.Pool.PreferExpiring)

	// 会话粘性路由（可配关闭）。
	var sessRouter *session.Router
	redisMode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		redisMode = "upstash"
	}
	if cfg.SessionSticky.Enabled {
		sessRouter = session.New(session.Config{
			TTL:        cfg.SessionTTL,
			GCInterval: cfg.SessionGCInterval,
			Store:      store,
			Available:  p.AvailableUIDs,
			// realm 感知闭包：带前缀模型名按 realm 过滤可用账号（跨 realm 不泄漏）；
			// 裸名走 cn（现状零回归）。闭包内部 resolveModel 剥前缀，再按 realm 过滤。
			AvailableForModel: realmAwareAvailableForModel(p),
		})
		sessRouter.LoadFromStore() // 启动时从 Redis 恢复粘性（读操作仅此处）
		sessRouter.StartGC()
		defer sessRouter.StopGC()
	}
	sessCount := func() int {
		if sessRouter != nil {
			return sessRouter.Count()
		}
		return 0
	}

	up := upstream.New()

	// 积分保底的「收费」兜底判据：接上游模型目录的积分倍率表。本地实测台账无观测
	// 时用它判收费——否则「没学过」恒等于「放行」，高价新模型会把触底号一笔打穿
	// （kimi-k3-1 实案：全池无观测 → 保底全放行 → 两笔打穿并硬冷却到次日 04:00）。
	// 位于 up 装配之后：倍率表由探测下发，闭包每次调用读实时快照。
	p.SetModelRateOf(func(realm, model string) string { return up.ModelRate(realm, model) })

	// 短 RPC 总时长上限（refresh/checkin/balance/FetchModels），语义不变。
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// 聊天 SSE 首字节前（响应头）上限：cfg 已 normalize（缺省回落 timeout_seconds）。
	up.HeaderTimeout = time.Duration(cfg.Upstream.HeaderTimeoutSeconds) * time.Second
	if tr, ok := up.ChatHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = up.HeaderTimeout
	}
	// 聊天 SSE 流中空闲上限（S3 空闲监控读取）。
	up.IdleTimeout = time.Duration(cfg.Upstream.IdleTimeoutSeconds) * time.Second
	up.SanitizeFingerprints.Store(cfg.Features.SanitizeBlacklistFingerprints)
	// 出站 UA 与归属头（issue #42 + 上游同步）：
	// UserAgent 非空则完全覆盖；ClientVersion/CliVersion 缺省对齐官方形态；
	// ClientName 非空时 chat 路径注入 X-IDE-* 四头（用量归因对齐官方桌面端）。
	up.UserAgent = cfg.Upstream.UserAgent
	up.ClientVersion = cfg.Upstream.ClientVersion
	up.CliVersion = cfg.Upstream.CliVersion
	up.ClientName = cfg.Upstream.ClientName
	up.DeviceToken = cfg.Upstream.DeviceToken
	up.DeviceTokenFile = cfg.Upstream.DeviceTokenFile
	up.PassthroughIP = cfg.Upstream.PassthroughIP
	// global realm 路由（config global 段）：上游侧开关（第一道闸）+ base 覆盖；
	// auth 侧开关（auth.SetGlobalEnabled）是第二道闸，两者同 config global.enabled。
	up.GlobalEnabled = cfg.Global.Enabled
	// SSRF 防护（安全官 F-009）：这两个 base 是用户可配且出站带账号 Bearer，
	// 必须校验——否则可配成攻击者主机（token 外发）或 169.254.169.254（打内网）。
	// 空串 = 用内置默认，合法。校验失败直接启动失败，绝不 fail-open。
	if err := upstream.ValidateOutboundBase(cfg.Global.ChatBase, "global.chat_base"); err != nil {
		log.Fatalf("config: %v", err)
	}
	if err := upstream.ValidateOutboundBase(cfg.Global.BillingBase, "global.billing_base"); err != nil {
		log.Fatalf("config: %v", err)
	}
	up.ChatBaseGlobal = cfg.Global.ChatBase
	up.BillingBaseGlobal = cfg.Global.BillingBase
	auth.SetGlobalEnabled(cfg.Global.Enabled)
	// model.json 本地缓存接线（context_length/max_output_tokens 四级查找链第 3 级）：
	// 数据目录与 state.json 同风格（Docker volume 持久化路径）。首次缺失/损坏自动
	// 回落仓库内嵌种子；models.dev 按需拉取成功后原子写回。
	upstream.SetModelCatalogPath(stateSibling(cfg.StateFile, "model.json"))

	// 外部渠道（多平台阶段一）：openai-compatible 上游与 CodeBuddy 池并存。
	// 出站 base 的 SSRF 校验在这里做（config 包不依赖 upstream；校验失败拒启，
	// 绝不 fail-open——与 global.chat_base 同一安全红线）。
	extMgr := extchan.New(extchan.Config{
		TimeoutSeconds: cfg.Upstream.TimeoutSeconds,
	})
	for i, ec := range cfg.ExtChannels {
		if err := upstream.ValidateOutboundBaseAllowPrivate(ec.BaseURL, fmt.Sprintf("ext_channels[%d].base_url", i), ec.AllowPrivate); err != nil {
			log.Fatalf("config: %v", err)
		}
	}
	extMgr.ReplaceAll(extChannelDefs(cfg.ExtChannels))
	extIntelStop := extMgr.StartIntel(cfg.ExtIntel.URL,
		time.Duration(cfg.ExtIntel.Hours)*time.Hour, cfg.ExtIntel.Enabled)
	defer extIntelStop()
	if len(cfg.ExtChannels) > 0 {
		log.Printf("[extchan] 已启用 %d 个外部渠道（ext:<name>/<model> 路由）", len(cfg.ExtChannels))
	}
	if cfg.ExtIntel.Enabled && cfg.ExtIntel.URL != "" {
		log.Printf("[extchan] 额度情报：%s 每 %d 小时拉取一次（仅面板展示）", cfg.ExtIntel.URL, cfg.ExtIntel.Hours)
	}

	sch := scheduler.New(scheduler.Config{
		Pool:           p,
		Upstream:       up,
		CheckinHours:   cfg.Schedule.CheckinHours,
		TravelHours:    cfg.Schedule.TravelHours,
		ActivityHours:  cfg.Schedule.ActivityHours,
		KeepaliveHours: cfg.Schedule.KeepaliveHours,
		BlackcatHours:  cfg.Schedule.BlackcatHours,
		GrowthHours:    cfg.Schedule.GrowthHours,
		// 快过期积分优先消耗：签到/余额刷新按此窗口分桶（issue:积分过期）。
		ExpiringSoonWindow: cfg.ExpiringSoonDur,
		CheckinDisabled:    !cfg.Schedule.CheckinEnabled,
		TravelDisabled:     !cfg.Schedule.TravelEnabled,
		ActivityDisabled:   !cfg.Schedule.ActivityEnabled,
		KeepaliveDisabled:  !cfg.Schedule.KeepaliveEnabled,
		BlackcatDisabled:   !cfg.Schedule.BlackcatEnabled,
		GrowthDisabled:     !cfg.Schedule.GrowthEnabled,
		// 保号类四任务是否覆盖禁用账号（缺省 false = 禁用即跳过，保持既有行为）。
		IncludeDisabledInTasks: cfg.Schedule.IncludeDisabledInTasks,
	})
	switch {
	case !cfg.Schedule.CheckinEnabled:
		log.Printf("签到已禁用（schedule.checkin_enabled=false）")
	default:
		log.Printf("签到已启用：%v 点（签到 + 余额查询解冻）", cfg.Schedule.CheckinHours)
	}
	switch {
	case !cfg.Schedule.TravelEnabled:
		log.Printf("猫猫旅行已禁用（schedule.travel_enabled=false）")
	default:
		log.Printf("猫猫旅行已启用：%v 点（独立排程：领养 / 派出 / 领奖）", cfg.Schedule.TravelHours)
	}
	switch {
	case !cfg.Schedule.ActivityEnabled:
		log.Printf("活跃上报已禁用（schedule.activity_enabled=false）")
	default:
		log.Printf("活跃上报已启用：%v 点（每日 1 次，点亮连登 + 解锁 first_buddy）", cfg.Schedule.ActivityHours)
	}
	if !cfg.Schedule.KeepaliveEnabled {
		log.Printf("token 保活已禁用（schedule.keepalive_enabled=false）")
	} else {
		log.Printf("token 保活已启用：%v 点", cfg.Schedule.KeepaliveHours)
	}
	switch {
	case !cfg.Schedule.BlackcatEnabled:
		log.Printf("夜猫子已禁用（schedule.blackcat_enabled=false）")
	default:
		log.Printf("夜猫子已启用：%v 点（23:00–08:00 窗口 glm-5.2 对话补足）", cfg.Schedule.BlackcatHours)
	}
	switch {
	case !cfg.Schedule.BalanceRefreshEnabled:
		log.Printf("余额后台刷新已禁用（schedule.balance_refresh_enabled=false）")
	case cfg.BalanceRefreshInterval > 0:
		log.Printf("余额后台刷新：每 %s（签到时点照常额外刷新）", cfg.BalanceRefreshInterval)
	}
	if cfg.Schedule.IncludeDisabledInTasks {
		log.Printf("保号任务覆盖禁用账号（schedule.include_disabled_in_tasks=true）：禁用号仍签到 / 活跃 / 保活 / 刷新余额，但不参与选号")
	}

	// 管理面板日志镜像：标准 log（stderr）与 chat 表格日志（stdout）双路复制进
	// 面板环形缓冲，供 /startport/api/logs 读取；控制台输出行为完全不变。
	// live 承载可热改字段（api_key/soft_rate/脱敏开关），面板保存配置时在线替换。
	live := livecfg.New(livecfg.Snapshot{
		APIKey:               cfg.APIKey,
		SoftCooldown:         cfg.SoftRateDur,
		SanitizeFingerprints: cfg.Features.SanitizeBlacklistFingerprints,
		RecordClientInfo:     cfg.Logging.RequestClientInfo,
	})
	// 用量记录器：与 state 文件同目录，随 state_file 配置一起搬移。
	// datapath 由 state 文件路径推出，避免再加一个配置项。
	usagePath := usagePathFor(cfg.StateFile)
	rec := usage.New(usagePath)
	rec.Start()
	defer rec.Stop()
	log.Printf("[usage] 逐请求用量记录已启用: %s (%s)", usagePath, rec.Describe())

	// 请求指标始终启用；JSONL 归档只写脱敏元数据，写盘失败不影响聊天请求。
	requestLog := reqlog.New(reqlog.Config{
		Dir:           stateSibling(cfg.StateFile, "request-logs"),
		Enabled:       cfg.Logging.RequestArchiveEnabled,
		RetentionDays: cfg.Logging.RequestRetentionDays,
		MaxBytes:      int64(cfg.Logging.RequestArchiveMaxMB) << 20,
	})
	defer requestLog.Close()
	rs := requestLog.Snapshot().Archive
	if rs.Enabled {
		log.Printf("[reqlog] 请求指标已启用；JSONL 归档 %s（保留 %d 天，上限 %d MiB）",
			rs.Dir, cfg.Logging.RequestRetentionDays, cfg.Logging.RequestArchiveMaxMB)
	} else {
		log.Printf("[reqlog] 请求指标已启用；JSONL 归档已关闭")
	}

	// 更新检查器（安全官 F-003）：仅「检查 + 提示」，固定可信源、版本单调、失败静默；
	// **绝不自动下载或执行**产物。单调性状态与 state 文件同目录（跨重启防回放）。
	upd := update.New(update.Config{
		Current:   appVersion,
		Interval:  cfg.UpdateCheckInterval,
		Enabled:   cfg.Update.CheckEnabled,
		StatePath: stateSibling(cfg.StateFile, "update-state.json"),
	})
	if cfg.Update.CheckEnabled {
		log.Printf("[update] 版本检查已启用：每 %s 检查一次（仅提示，不自动下载）", cfg.UpdateCheckInterval)
	} else {
		log.Printf("[update] 版本检查已关闭（update.check_enabled=false）")
	}

	// 可信代理白名单（安全官 F-008）：解析失败直接启动失败——安全配置不能
	// fail-open（静默退回"无条件采信转发头"正是本项要修的缺陷）。
	trustedProxies, err := server.ParseTrustedProxies(cfg.Server.TrustedProxies)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	server.SetTrustedProxies(trustedProxies)
	if len(trustedProxies) > 0 {
		log.Printf("[server] 可信代理白名单已生效（%d 条）：仅这些来源的 X-Forwarded-For/X-Real-IP 会被采信", len(trustedProxies))
	}

	pn := panel.New(panel.Config{
		Pool:        p,
		Usage:       rec,
		RequestLog:  requestLog,
		Upstream:    up,
		Scheduler:   sch,
		AuthDir:     cfg.AuthDir,
		APIKey:      cfg.APIKey,
		RedisMode:   redisMode,
		StickyCount: sessCount,
		Version:     appVersion,
		Live:        live,
		Update:      upd,
		APIKeys:     keyStore,
		// 外部渠道只读观测（多平台阶段一）：渠道快照 + 额度情报端点。
		ExtChan: extMgr,
		// 模型上限探测数据（scripts/probe_max_tokens.py --panel-out 写入）：
		// 与 state 文件同目录，缺省 data/output_probes.json。
		ProbeFile:  stateSibling(cfg.StateFile, "output_probes.json"),
		ConfigPath: *cfgPath,
		LoadConfig: func() (any, error) {
			return Load(*cfgPath)
		},
		SaveConfig: func(raw []byte) ([]string, error) {
			return saveConfig(raw, *cfgPath, live, p, up, sch, extMgr)
		},
	})
	// 成长任务队列每日自动执行（与「执行全部待办」同管线）：Sequential 族零点解锁后
	// 无需手动扫描；hook 返回即启动（异步执行），已在跑时内部跳过。
	sch.SetGrowthHook(pn.RunGrowthQueueOnce)
	log.SetOutput(io.MultiWriter(os.Stderr, pn.Logs()))
	server.SetChatLogOutput(io.MultiWriter(os.Stdout, pn.Logs()))

	h := server.NewHandler(server.Config{
		Pool:         p,
		Upstream:     up,
		APIKey:       cfg.APIKey,
		Session:      sessRouter,
		StickyCount:  sessCount,
		RedisMode:    redisMode,
		SoftCooldown: cfg.SoftRateDur,
		MaxBodyBytes: cfg.ServerMaxBodyBytes,
		Panel:        pn,
		Live:         live,
		Usage:        rec,
		RequestLog:   requestLog,
		APIKeys:      keyStore,
		PromptMode:   cfg.Prompt.Mode,
		PromptText:   cfg.PromptText,
		// 来源记录开关经 livecfg 热生效；此处同时填静态字段，供 Live 为 nil 的
		// 裸用/测试路径拿到同一缺省值。
		RecordClientInfo: cfg.Logging.RequestClientInfo,
		// handler 侧第三道闸（global realm）：false（显式逃生门）时不列 global: 模型名。
		GlobalEnabled: cfg.Global.Enabled,
		// 外部渠道（多平台阶段一）：nil 形态（extMgr 内部无渠道）对外等效关闭。
		Ext: extMgr,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)
	sch.StartBalanceRefresh(ctx, cfg.BalanceRefreshInterval)
	upd.Start(ctx)

	// 启动即预热模型积分倍率表：倍率只在 FetchModels/FetchGlobalModelInfos 成功时
	// 填充（两者均懒触发），重启后到首次 /v1/models 或面板模型页被访问之前，
	// ModelRate 恒返回空串——积分保底的目录兜底在这段空窗期内形同虚设，触底号
	// 会被当成「收费未知」放行并打穿（实测：重启后 2 分钟，97 分的账号打收费
	// 模型归零；倍率表当时尚未建立）。
	// 异步执行：不阻塞监听启动；失败仅记日志（下一轮懒触发或本轮重试仍可补上）。
	go warmModelRates(ctx, up, p)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		// ReadTimeout 覆盖整个请求读取（含 body 上传）：防慢速 body 拖死连接。
		// 请求体已无网关侧上限（max_body_mb 移除）。缺省 300s（issue #100：旧固定
		// 60s 会掐掉大上下文/文件块经反代链的慢速上传，客户端收到
		// 400 "read body: ... i/o timeout"）；server.read_timeout="0" 显式关闭。
		// 改动需重启进程。
		ReadTimeout: cfg.ServerReadTimeoutDur,
		// IdleTimeout keep-alive 空闲连接回收：配合 chat 出站 ctx 传播防连接泄漏堆积。
		// 注意：SSE 流式响应期间连接非空闲，不受此项掐断；不设全局 WriteTimeout
		// （长流式生成合法时长可达数分钟，全局 WriteTimeout 会误杀在途 SSE）。
		IdleTimeout: 120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		p.Flush() // 信号触发：先落盘再做优雅停机
		// 30s 而非更短：活跃 SSE 流合法时长可达数分钟（本文件上方 IdleTimeout 注释），
		// Shutdown 期间在途流仍在排空——30s 平衡"重启速度"与"长流不硬掐"：
		// 正常排空通常秒级返回；真有挂死连接时最多多等 30s 后由 Close 确定性退出。
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			// Shutdown 超时（context deadline）= 仍有残留连接未排空：Close 立即
			// 掐断它们并关闭监听器，确保进程确定性退出（不悬挂、不泄漏资源）。
			_ = srv.Close()
			log.Printf("graceful shutdown timed out (%v), forced close; in-flight connections dropped", err)
		}
	}()

	// 先显式绑定端口再对外宣告"已就绪"：端口被占用能 fail fast（而非 Serve 时才报）。
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		// 端口被占用且探测到就是本网关 → 已有一个实例在跑（"重复双击"是常见操作）。
		// 此时不报错，直接打开浏览器指向已在运行的实例——"再双击一次 = 再看一眼面板"。
		if isAddrInUse(err) && ourServiceRunning(cfg.Listen) {
			url := panelURL(cfg.Listen)
			log.Printf("检测到网关已在运行（%s），直接打开管理面板", url)
			if shouldAutoOpen(*openBrowserFlag) {
				autoOpenPanel(url) // 同步：确保浏览器已唤起再退出
			}
			return
		}
		if isAddrInUse(err) {
			fatalExit("端口 %s 已被占用——可能已有一个 starport 实例在运行，或该端口被其它程序占用。\n"+
				"    排查：任务栏/窗口列表里找 starport 的控制台窗口；或改 config.json 的 listen 换端口。\n"+
				"    原始错误：%v", cfg.Listen, err)
		}
		fatalExit("listen %s: %v", cfg.Listen, err)
	}
	log.Printf("starport %s listening on %s (api_key=%v)，管理面板 %s",
		versionString(), cfg.Listen, cfg.APIKey != "", panelURL(cfg.Listen))
	if shouldAutoOpen(*openBrowserFlag) {
		go autoOpenPanel(panelURL(cfg.Listen))
	}
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		fatalExit("http: %v", err)
	}
	log.Printf("bye")
}

// probeHostPort 把 listen 地址转成可直连的 host:port（通配 host 回落 127.0.0.1）。
func probeHostPort(listen string) string {
	port := panelListenPath(listen) // ":7863"
	host := strings.TrimSuffix(listen, port)
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return host + port
}

// panelURL 由 listen 地址推出可访问的面板 URL。
func panelURL(listen string) string { return "http://" + probeHostPort(listen) + "/startport/" }

// isAddrInUse 判断监听失败是否因端口被占用（Windows 与 Unix 措辞不同，双通道识别）。
func isAddrInUse(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.EADDRINUSE) {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "only one usage of each socket address") || // Windows
		strings.Contains(s, "address already in use") // Unix
}

// ourServiceRunning 探测该地址上是否已有**本网关**在跑：GET /healthz 校验 service 标识。
// 用 service 字段而非"端口能连上"作判据——避免把别人的服务误认成自己。
func ourServiceRunning(listen string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + probeHostPort(listen) + "/healthz")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return false
	}
	return strings.Contains(string(body), `"service":"`+server.ServiceName+`"`)
}

// fatalExit 启动期致命错误：打印后退出。若处于交互式控制台（且不在 Docker 内），
// 先等一次回车再退出——否则双击启动时控制台窗口会一闪而过、用户什么都看不到
// （"双击没反应"的根因）。
func fatalExit(format string, args ...any) {
	log.Printf("FATAL: "+format, args...)
	if shouldAutoOpen(true) && stdinIsTerminal() {
		fmt.Fprint(os.Stderr, "按回车键退出…")
		_, _ = fmt.Scanln()
	}
	os.Exit(1)
}

// stdinIsTerminal 报告 stdin 是否为交互式终端（重定向 / 管道时为 false）。
func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// shouldAutoOpen 是否自动打开浏览器：显式关闭时否；Docker 容器内否（无浏览器，
// 且自动打开只会刷一条无意义错误）；其余情况是。
func shouldAutoOpen(flagVal bool) bool {
	if !flagVal {
		return false
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return false
	}
	return true
}

// autoOpenPanel 稍等端口就绪后打开默认浏览器（跨平台）。失败只记日志，不影响服务。
func autoOpenPanel(url string) {
	time.Sleep(400 * time.Millisecond)
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("[open] 自动打开浏览器失败（请手动访问 %s）: %v", url, err)
		return
	}
	log.Printf("[open] 已打开管理面板 %s", url)
}

// warmModelRates 启动预热各域模型积分倍率表（供积分保底的目录兜底判定）。
//
// 为什么需要：倍率表只在 FetchModels（CN）/ FetchGlobalModelInfos（global）成功时
// 填充，两者都是懒触发（被 /v1/models 或面板模型页访问才跑）。重启后到首次触发
// 之间的空窗期里 ModelRate 恒返回空串，保底的目录兜底判不出收费，触底号会被
// 当成「收费未知」放行并打穿（实测：重启后 2 分钟，97 分的账号打收费模型归零）。
//
// 失败处理：单域失败只记 WARN（不阻塞、不致命——后续懒触发仍会补上）；global 域
// 仅在其路由开关开启时预热（逃生门关锁时按 CN 处理，无需探测）。
func warmModelRates(ctx context.Context, up *upstream.Client, p *pool.Pool) {
	// 预热不得拖住进程退出：ctx 取消（SIGINT/SIGTERM）时立刻放弃剩余域。
	if ctx.Err() != nil {
		return
	}
	// CN：有可用 CN 账号才拉（与面板 models 同口径，避免无谓上游调用）。
	if uids := p.AvailableUIDsForRealm("cn"); len(uids) > 0 {
		if a := p.AuthByUID(uids[0]); a != nil {
			if _, err := up.FetchModels(a); err != nil {
				log.Printf("WARN: [upstream] warm model rates (cn): %v", err)
			} else {
				log.Printf("[upstream] warm model rates: cn ok")
			}
		}
	}
	// global：独立目录端点（workbuddy.ai），倍率按 "global" 域键存储。
	if up.GlobalEnabled && ctx.Err() == nil {
		if uids := p.AvailableUIDsForRealm("global"); len(uids) > 0 {
			if a := p.AuthByUID(uids[0]); a != nil {
				// FetchGlobalModelInfos 无错误返回（内部负缓存自行节流），
				// 仅按结果条数判断是否拿到目录。
				if infos := up.FetchGlobalModelInfos(a); len(infos) == 0 {
					log.Printf("WARN: [upstream] warm model rates (global): empty model list")
				} else {
					log.Printf("[upstream] warm model rates: global ok (%d models)", len(infos))
				}
			}
		}
	}
}

// panelListenPath 从 listen 地址提取 ":port" 形式，用于启动日志拼面板 URL
// （":7863" 或 "0.0.0.0:7863" → ":7863"；异常输入原样返回）。
func panelListenPath(listen string) string {
	for i := len(listen) - 1; i >= 0; i-- {
		if listen[i] == ':' {
			return listen[i:]
		}
	}
	return listen
}

// saveConfig 面板保存配置：校验 → 落盘 → 热应用 → 返回需重启的字段列表。
//
// 热生效范围（设计取舍）：
//   - api_key / cooldown.soft_rate / features.sanitize_blacklist_fingerprints → livecfg 快照
//   - pool.* → pool.SetBreaker/SetMaxInFlight/SetSoftRateMax/SetWeights/SetCostExploreInterval/SetPreferExpiring/SetCreditFloor
//   - schedule.* → scheduler.Reconfigure/SetBalanceInterval/SetExpiringSoonWindow
//
// 需重启（涉及监听地址、HTTP client 超时、auth_dir 等装配期依赖）：
//   - listen / auth_dir / state_file / upstream.* / upstash.* / session_sticky.*（TTL 类）
//
// 落盘用"先写 tmp 再 rename"原子替换，且优先保留磁盘上的原始 JSON 结构（只改
// 面板表单覆盖到的键），避免把用户手写的注释性字段/未知键洗掉——这里直接整体
// 序列化校验后的配置，未知键在 json.Unmarshal 时已丢失，故先合并原始 map。
func saveConfig(raw []byte, path string, live *livecfg.Holder, p *pool.Pool, up *upstream.Client, sch *scheduler.Scheduler, extMgr *extchan.Manager) ([]string, error) {
	// 1) 解析原始 JSON 为 map（保留用户手写的未知键），再叠加面板提交的键。
	oldRaw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read current config: %w", err)
	}
	var cur, incoming map[string]any
	if err := json.Unmarshal(oldRaw, &cur); err != nil {
		cur = map[string]any{}
	}
	if err := json.Unmarshal(raw, &incoming); err != nil {
		return nil, fmt.Errorf("parse submitted config: %w", err)
	}
	scrubSecretSentinels(incoming, cur)
	merged := mergeConfigMaps(cur, incoming)

	// 2) 校验（与启动同一套 Default+normalize），失败直接返回、不落盘。
	newCfg, err := ParseConfig(mergedJSON(merged))
	if err != nil {
		return nil, err
	}
	// 外部渠道出站 SSRF 校验（与启动期同红线）：面板改 base_url 指向内网/
	// 元数据地址时保存失败并回显原因。allow_private 渠道放行内网（自建聚合器），
	// 元数据端点恒拒。
	for i, ec := range newCfg.ExtChannels {
		if err := upstream.ValidateOutboundBaseAllowPrivate(ec.BaseURL, fmt.Sprintf("ext_channels[%d].base_url", i), ec.AllowPrivate); err != nil {
			return nil, err
		}
	}

	// 3) 落盘（原子替换）。
	out, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return nil, fmt.Errorf("write config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		// A single-file Docker bind mount cannot be renamed over its mount
		// target (Linux returns EBUSY / "device or resource busy"). Keep the
		// atomic path for regular files, but update the mounted file in place
		// for this specific deployment shape.
		if !errors.Is(err, syscall.EBUSY) {
			return nil, fmt.Errorf("replace config: %w", err)
		}
		f, openErr := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
		if openErr != nil {
			_ = os.Remove(tmp)
			return nil, fmt.Errorf("replace config (bind mount fallback): %w", openErr)
		}
		_, writeErr := f.Write(out)
		if writeErr == nil {
			writeErr = f.Sync()
		}
		closeErr := f.Close()
		// 写失败时保留 tmp（挂载文件已被 O_TRUNC 破坏，tmp 里是完整新内容，
		// 可手工恢复）；写成功才清理。
		if writeErr != nil {
			return nil, fmt.Errorf("replace config (bind mount fallback, 完整新内容保留在 %s): %w", tmp, writeErr)
		}
		_ = os.Remove(tmp)
		if closeErr != nil {
			return nil, fmt.Errorf("replace config (bind mount fallback): %w", closeErr)
		}
	}

	// 4) 热应用：能立即生效的字段全部应用，并列出仍需重启的字段。
	live.Store(livecfg.Snapshot{
		APIKey:               newCfg.APIKey,
		SoftCooldown:         newCfg.SoftRateDur,
		SanitizeFingerprints: newCfg.Features.SanitizeBlacklistFingerprints,
		RecordClientInfo:     newCfg.Logging.RequestClientInfo,
	})
	up.SanitizeFingerprints.Store(newCfg.Features.SanitizeBlacklistFingerprints)
	p.SetBreaker(newCfg.Pool.BreakerThreshold, newCfg.BreakerCooldownDur, newCfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(newCfg.Pool.MaxInFlight)
	p.SetMaxInFlightGlobal(newCfg.Pool.MaxInFlightGlobal)
	p.SetDegrade(newCfg.Pool.DegradeThreshold, newCfg.DegradeCooldownDur, newCfg.DegradeCooldownMaxD)
	p.SetSoftRateMax(newCfg.SoftRateMaxDur)
	p.SetCostExploreInterval(newCfg.CostExploreIntervalDur) // costTier 探索窗口热生效（0 关停）
	p.SetCreditFloor(newCfg.Pool.CreditFloor)               // 积分保底热生效（0 = 关闭）
	p.SetWeights(newCfg.Pool.IdleWeightPerHour, newCfg.Pool.IdleWeightMax)
	p.SetPreferExpiring(newCfg.Pool.PreferExpiring)
	sch.SetExpiringSoonWindow(newCfg.ExpiringSoonDur)
	sch.Reconfigure(
		newCfg.Schedule.CheckinHours, newCfg.Schedule.TravelHours,
		newCfg.Schedule.ActivityHours, newCfg.Schedule.KeepaliveHours, newCfg.Schedule.BlackcatHours,
		newCfg.Schedule.GrowthHours,
		!newCfg.Schedule.CheckinEnabled, !newCfg.Schedule.TravelEnabled,
		!newCfg.Schedule.ActivityEnabled, !newCfg.Schedule.KeepaliveEnabled, !newCfg.Schedule.BlackcatEnabled,
		!newCfg.Schedule.GrowthEnabled)
	sch.SetBalanceInterval(newCfg.BalanceRefreshInterval)
	sch.SetIncludeDisabledInTasks(newCfg.Schedule.IncludeDisabledInTasks)

	// 外部渠道热重载（多平台阶段一）：按名复用运行态（冷却/目录缓存），改 base_url
	// 丢缓存。新增/删除渠道即时生效，无需重启。
	extMgr.ReplaceAll(extChannelDefs(newCfg.ExtChannels))

	return restartRequiredFields(newCfg), nil
}

// extChannelDefs Config.ExtChannels → extchan.ChannelDef 映射（装配/热重载共用）。
func extChannelDefs(src []ExtChannel) []extchan.ChannelDef {
	out := make([]extchan.ChannelDef, 0, len(src))
	for _, ec := range src {
		def := extchan.ChannelDef{
			Name:           ec.Name,
			BaseURL:        ec.BaseURL,
			APIKey:         ec.APIKey,
			Protocol:       ec.Protocol,
			Adapter:        ec.Adapter,
			Path:           ec.Path,
			Auth:           ec.Auth.Spec(),
			Headers:        ec.Headers,
			Models:         ec.Models,
			Aliases:        ec.Aliases,
			Note:           ec.Note,
			Disabled:       ec.Disabled,
			TimeoutSeconds: ec.TimeoutSeconds,
			AllowPrivate:   ec.AllowPrivate,
		}
		for _, cred := range ec.Credentials {
			def.Credentials = append(def.Credentials, extchan.CredentialDef{
				Label:     cred.Label,
				Secret:    cred.Secret,
				ExpiresAt: cred.ExpiresAt,
				Disabled:  cred.Disabled,
				Auth:      cred.Auth.Spec(),
			})
		}
		out = append(out, def)
	}
	return out
}

// restartRequiredFields 返回本次改动中无法热生效、需要重启进程的字段名。
// 恒返回完整清单中的"与当前进程装配期依赖相关"的项——面板据此提示用户。
func restartRequiredFields(c *Config) []string {
	var out []string
	// 这些字段在进程内被监听地址/HTTP client/目录句柄等装配期对象捕获。
	if c.Listen != "" {
		out = append(out, "listen")
	}
	if c.AuthDir != "" {
		out = append(out, "auth_dir")
	}
	if c.StateFile != "" {
		out = append(out, "state_file")
	}
	out = append(out, "upstream.timeout_seconds", "upstream.header_timeout_seconds", "upstream.idle_timeout_seconds")
	// upstream.user_agent 在装配期被写进出站 client（main.go 的 up.UserAgent = ...），
	// 之后不再读取——不在 livecfg 热快照里，也无法热改。此前漏列，导致面板改完
	// 显示"已保存"却不提示需要重启，用户以为没生效（issue #102 附带发现 2）。
	out = append(out, "upstream.user_agent")
	if c.Upstash.URL != "" || c.Upstash.Token != "" {
		out = append(out, "upstash")
	}
	out = append(out, "session_sticky.ttl", "session_sticky.gc_interval")
	out = append(out, "logging.request_archive_enabled", "logging.request_retention_days", "logging.request_archive_max_mb")
	out = append(out, "server.read_timeout")
	out = append(out, "update.check_enabled", "update.check_interval_hours")
	return out
}

// scrubSecretSentinels 删除面板提交里值为脱敏哨兵（panel.SecretSentinel）的敏感键，
// 使随后的深合并保留磁盘上的原值——防止「GET 回显的掩码被当成新密钥写回」。
func scrubSecretSentinels(incoming, cur map[string]any) {
	if v, ok := incoming["api_key"].(string); ok && v == panel.SecretSentinel {
		delete(incoming, "api_key")
	}
	if up, ok := incoming["upstream"].(map[string]any); ok {
		if v, ok := up["device_token"].(string); ok && v == panel.SecretSentinel {
			delete(up, "device_token")
		}
	}
	if us, ok := incoming["upstash"].(map[string]any); ok {
		if v, ok := us["token"].(string); ok && v == panel.SecretSentinel {
			delete(us, "token")
		}
	}
	// 外部渠道 api_key 哨兵（F-004 同源）：整条渠道数组逐项检查——数组是整体
	// 替换不是深合并，哨兵留在项里会把"已设置"写成字面哨兵串。
	if ecs, ok := incoming["ext_channels"].([]any); ok {
		for _, e := range ecs {
			ec, ok := e.(map[string]any)
			if !ok {
				continue
			}
			if v, ok := ec["api_key"].(string); ok && v == panel.SecretSentinel {
				// 用磁盘上的原渠道值回填（按 name 匹配）。
				if orig := extChannelKeyByName(cur, ec); orig != "" {
					ec["api_key"] = orig
				} else {
					delete(ec, "api_key")
				}
			}
			// 凭证级 secret 哨兵（P0-1）：按 (渠道 name, 凭证 label) 从磁盘回填。
			// 找不到（新凭证误带哨兵）→ 删除该字段，绝不把哨兵写盘。
			scrubExtCredentialSentinels(cur, ec)
		}
	}
}

// scrubExtCredentialSentinels 把提交里 credentials[].secret 的脱敏哨兵替换为
// 磁盘上的原值。数组是整体替换（不做深合并），必须逐项回填——少回填一项就是
// 把"已设置"写成字面哨兵串，下次请求会把哨兵当密钥发给上游。
func scrubExtCredentialSentinels(cur, ec map[string]any) {
	creds, ok := ec["credentials"].([]any)
	if !ok || len(creds) == 0 {
		return
	}
	orig := extChannelCredsByName(cur, ec)
	for _, ce := range creds {
		cm, ok := ce.(map[string]any)
		if !ok {
			continue
		}
		v, ok := cm["secret"].(string)
		if !ok || v != panel.SecretSentinel {
			continue
		}
		label, _ := cm["label"].(string)
		if orig != nil {
			if s, ok := orig[label]; ok && s != "" {
				cm["secret"] = s
				continue
			}
		}
		delete(cm, "secret")
	}
}

// extChannelCredsByName 从磁盘配置里取「同名渠道的 label → secret」映射。
func extChannelCredsByName(cur, ec map[string]any) map[string]string {
	name, _ := ec["name"].(string)
	if name == "" {
		return nil
	}
	oldList, ok := cur["ext_channels"].([]any)
	if !ok {
		return nil
	}
	for _, e := range oldList {
		old, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if n, _ := old["name"].(string); n != name {
			continue
		}
		out := map[string]string{}
		creds, _ := old["credentials"].([]any)
		for _, ce := range creds {
			cm, ok := ce.(map[string]any)
			if !ok {
				continue
			}
			label, _ := cm["label"].(string)
			secret, _ := cm["secret"].(string)
			if label != "" {
				out[label] = secret
			}
		}
		if len(out) == 0 {
			// 老配置（只有 api_key）在面板里被改写成 credentials[{label:"default"}]
			// 时，默认凭证的 secret 从 api_key 兜底回填。
			if k, _ := old["api_key"].(string); k != "" {
				out["default"] = k
			}
		}
		return out
	}
	return nil
}

// extChannelKeyByName 从磁盘配置（cur）里找同名渠道的 api_key 原值。
// 哨兵回填专用；找不到（新渠道误带哨兵）返回空串。
func extChannelKeyByName(cur map[string]any, ec map[string]any) string {
	name, _ := ec["name"].(string)
	if name == "" {
		return ""
	}
	oldList, ok := cur["ext_channels"].([]any)
	if !ok {
		return ""
	}
	for _, e := range oldList {
		if old, ok := e.(map[string]any); ok {
			if n, _ := old["name"].(string); n == name {
				k, _ := old["api_key"].(string)
				return k
			}
		}
	}
	return ""
}

// mergeConfigMaps 把 incoming 深合并进 cur（原地），返回 cur。
// 对嵌套对象逐键覆盖而不是整体替换：面板表单只提交它管理的键，
// 未提交的兄弟键（含用户手写的未知键）保持原样。
func mergeConfigMaps(cur, incoming map[string]any) map[string]any {
	for k, v := range incoming {
		if inMap, ok := v.(map[string]any); ok {
			if curMap, ok := cur[k].(map[string]any); ok {
				cur[k] = mergeConfigMaps(curMap, inMap)
				continue
			}
		}
		cur[k] = v
	}
	return cur
}

// mergedJSON 把合并后的 map 序列化回 JSON（供 ParseConfig 校验）。
func mergedJSON(m map[string]any) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return b
}
