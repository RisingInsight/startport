// service_log.go 服务模式日志文件（全平台编译；serverMain 的 serviceMode 分支调用）。
// 简单尺寸轮转：starport-service.log ≥8MB 改名 .old 重开。Windows 服务宿主与
// 未来 Linux systemd 单元共用同一实现。
package main

import (
	"os"
	"path/filepath"
	"sync"
)

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
