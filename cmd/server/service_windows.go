//go:build windows

// service_windows.go Windows 服务宿主：进程守护 + 开机自启（借鉴 magpie 的
// daemon/autostart 职责）。三部分：
//
//  1. `starport service install|uninstall|start|stop|restart|status` 子命令——
//     把当前 exe 注册成自动启动的服务（崩溃后由恢复动作自动重启）；
//  2. SCM 拉起路径：`--service` + svc.Run，停止请求转 ctx cancel，
//     serverMain 的优雅停机（30s 排空）原样生效；
//  3. 服务宿主适配：日志落配置目录的 starport-service.log（简单尺寸轮转）、
//     工作目录切到配置目录（残余相对路径不再落 system32）、绝不弹浏览器。
//
// 升级流程：service stop → 覆盖 exe → service start（Windows 锁定运行中的
// exe 文件，替换必须先停）。
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// serviceName 注册进 SCM 的服务显示名（net start/stop、services.msc 里可见）。
const serviceName = "StarPort"

func isWindowsService() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

// runWindowsService SCM 拉起入口（main 在 --service + IsWindowsService 时进入）。
func runWindowsService(cfgPath string) {
	abs, err := filepath.Abs(cfgPath)
	if err == nil {
		cfgPath = abs
	}
	_ = svc.Run(serviceName, &starportService{cfgPath: cfgPath})
}

// starportService svc.Handler：把 serverMain 挂进服务生命周期。
type starportService struct {
	cfgPath string
}

func (s *starportService) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown
	changes <- svc.Status{State: svc.StartPending, WaitHint: 20000}
	serviceMode.Store(true)
	// CWD 切到配置目录：serverMain 内的残余相对路径（若有）落在数据目录而非 system32。
	if dir := filepath.Dir(s.cfgPath); dir != "" {
		_ = os.Chdir(dir)
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop() // 主体自行退出（配置致命错误等）时也撤销 ctx，防泄漏
	done := make(chan struct{})
	go func() {
		defer close(done)
		serverMain(ctx, s.cfgPath, false) // 服务无桌面会话：绝不弹浏览器
	}()
	changes <- svc.Status{State: svc.Running, Accepts: accepted}
	for {
		select {
		case <-done:
			return false, 0 // SCM 按恢复动作重启
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending, WaitHint: 40000}
				stop() // serverMain 内 30s 优雅排空 < 40s WaitHint
				<-done
				return false, 0
			}
		}
	}
}

// runServiceCommand `starport service <sub>` 子命令分发。
func runServiceCommand(args []string) {
	if len(args) == 0 {
		serviceUsage()
		os.Exit(2)
	}
	switch args[0] {
	case "install":
		serviceInstall(args[1:])
	case "uninstall", "remove":
		serviceUninstall()
	case "start":
		serviceControl(true)
	case "stop":
		serviceControl(false)
	case "restart":
		serviceControl(false)
		serviceControl(true)
	case "status":
		serviceStatus()
	default:
		fmt.Printf("未知子命令 %q\n", args[0])
		serviceUsage()
		os.Exit(2)
	}
}

func serviceUsage() {
	fmt.Println("用法: starport service <install|uninstall|start|stop|restart|status>")
	fmt.Println("  install              把当前 exe 注册为自启服务（崩溃自动重启；--config <路径> 可指定配置，缺省当前目录 config.json）")
	fmt.Println("  uninstall            停止并删除服务（凭证与数据文件不动）")
	fmt.Println("  start | stop | restart | status")
}

// serviceInstall 注册服务。约定：配置文件必须已存在（先手动运行一次生成/校验），
// 服务参数里写死**绝对路径**——SCM 启动的进程 CWD 是 system32，相对路径会指错。
func serviceInstall(args []string) {
	exe, err := os.Executable()
	if err != nil {
		fmt.Println("无法定位当前 exe:", err)
		os.Exit(1)
	}
	exe, _ = filepath.Abs(exe)
	cfgPath := "config.json"
	for i := 0; i < len(args); i++ {
		if args[i] == "--config" && i+1 < len(args) {
			i++
			cfgPath = args[i]
		}
	}
	abs, err := filepath.Abs(cfgPath)
	if err != nil {
		fmt.Println("配置路径解析失败:", err)
		os.Exit(1)
	}
	if _, err := os.Stat(abs); err != nil {
		fmt.Println("配置文件不存在：" + abs)
		fmt.Println("先在目标目录手动运行一次本程序生成并校验配置，再 service install --config <路径>。")
		os.Exit(1)
	}

	m, err := mgr.Connect()
	if err != nil {
		fmt.Println("连接服务管理器失败（需要管理员权限）:", err)
		os.Exit(1)
	}
	defer m.Disconnect()
	if s, err := m.OpenService(serviceName); err == nil {
		s.Close()
		fmt.Println("服务 " + serviceName + " 已存在。先执行 service uninstall 再安装。")
		os.Exit(1)
	}
	// 服务宿主 exe 复制到 %ProgramData%\StarPort\starport.exe（固定名固定位）：
	// 用户桌面/下载目录的 ACL 会挡住 SYSTEM 拉起服务（实测：StartService 5 拒绝
	// 访问，中文用户名的桌面目录尤甚），ProgramData 是 Windows 服务的标准落位，
	// SYSTEM 可执行、升级路径清晰（stop → 覆盖 → start）。
	dest := filepath.Join(os.Getenv("ProgramData"), "StarPort", "starport.exe")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		fmt.Println("创建服务目录失败:", err)
		os.Exit(1)
	}
	if err := copyFile(exe, dest); err != nil {
		fmt.Println("复制服务 exe 失败（服务正在运行时先 stop/uninstall 再 install）:", err)
		os.Exit(1)
	}
	binPath := fmt.Sprintf("%q --config %q --service --open=false", dest, abs)
	s, err := m.CreateService(serviceName, binPath, mgr.Config{
		StartType:   mgr.StartAutomatic,
		Description: "StarPort 星港：本地多协议 AI 网关（账号池 / 平台签到 / 管理面板）。崩溃后自动重启。",
	})
	if err != nil {
		fmt.Println("创建服务失败（需要管理员权限）:", err)
		os.Exit(1)
	}
	defer s.Close()
	// 崩溃自动重启：5s / 30s / 60s 三档，24h 后重置失败计数。
	ra := []mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.ServiceRestart, Delay: time.Minute},
	}
	if err := s.SetRecoveryActions(ra, 86400); err != nil {
		fmt.Println("警告: 设置崩溃恢复动作失败（服务已安装但崩溃后不会自动重启）:", err)
	}
	fmt.Println("服务已安装：", serviceName)
	fmt.Println("  exe:", dest)
	fmt.Println("  config:", abs)
	fmt.Println("  启动:", serviceName+" 会随系统自动启动；现在启动请执行 starport service start（管理员）")
}

// serviceUninstall 停止（若在跑）并删除服务；不碰任何数据/凭证文件。
func serviceUninstall() {
	m, err := mgr.Connect()
	if err != nil {
		fmt.Println("连接服务管理器失败（需要管理员权限）:", err)
		os.Exit(1)
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		fmt.Println("服务 " + serviceName + " 未安装。")
		return
	}
	defer s.Close()
	if st, err := s.Query(); err == nil && st.State != svc.Stopped {
		fmt.Println("停止服务中…")
		if _, err := s.Control(svc.Stop); err != nil {
			fmt.Println("警告: 停止请求失败:", err)
		}
		waitStopped(s, 45*time.Second)
	}
	if err := s.Delete(); err != nil {
		fmt.Println("删除服务失败:", err)
		os.Exit(1)
	}
	fmt.Println("服务已卸载（数据与凭证文件未动）。")
}

func serviceControl(start bool) {
	m, err := mgr.Connect()
	if err != nil {
		fmt.Println("连接服务管理器失败（需要管理员权限）:", err)
		os.Exit(1)
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		fmt.Println("服务 " + serviceName + " 未安装（先 service install）。")
		os.Exit(1)
	}
	defer s.Close()
	if start {
		if err := s.Start(); err != nil {
			fmt.Println("启动失败:", err)
			os.Exit(1)
		}
		fmt.Println(serviceName + " 已启动。日志：配置目录 starport-service.log；面板 http://127.0.0.1:7863/startport/")
		return
	}
	if _, err := s.Control(svc.Stop); err != nil {
		fmt.Println("停止失败:", err)
		os.Exit(1)
	}
	waitStopped(s, 45*time.Second)
	fmt.Println(serviceName + " 已停止。")
}

func serviceStatus() {
	m, err := mgr.Connect()
	if err != nil {
		fmt.Println("连接服务管理器失败:", err)
		os.Exit(1)
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		fmt.Println(serviceName + ": 未安装")
		return
	}
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		fmt.Println("查询失败:", err)
		os.Exit(1)
	}
	name := "未知"
	switch st.State {
	case svc.Stopped:
		name = "已停止"
	case svc.Running:
		name = "运行中"
	case svc.StartPending:
		name = "启动中"
	case svc.StopPending:
		name = "停止中"
	}
	fmt.Printf("%s: %s (pid=%d)\n", serviceName, name, st.ProcessId)
}

func waitStopped(s *mgr.Service, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if st, err := s.Query(); err == nil && st.State == svc.Stopped {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// serviceLogFile 服务模式日志文件（配置目录/starport-service.log，≥8MB 轮转为 .old）。
// 并发安全（log 包串行调用 Write，但 chat 日志与主日志可能来自不同 goroutine）。
func serviceLogFile(dir string) *rotateWriter {
	if dir == "" {
		dir = "."
	}
	w := &rotateWriter{path: filepath.Join(dir, "starport-service.log"), max: 8 << 20}
	if err := w.open(); err != nil {
		return nil // 打不开就静默退化为无文件日志（面板运行日志仍在）
	}
	return w
}

type rotateWriter struct {
	mu   sync.Mutex
	path string
	max  int64
	f    *os.File
	size int64
}

func (w *rotateWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.f, w.size = f, st.Size()
	return nil
}

func (w *rotateWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return 0, os.ErrClosed
	}
	if w.size >= w.max {
		w.f.Close()
		_ = os.Rename(w.path, w.path+".old")
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}
