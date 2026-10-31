//go:build !windows

// service_other.go 非 Windows 平台的服务模式桩：本仓库的进程守护目标是
// Windows 桌面/服务器部署（Windows 服务）。Linux/Docker 用 systemd unit 或
// 容器编排的 restart policy（--service 标志在那里永远不会命中分支）。
package main

import (
	"fmt"
	"os"
)

func isWindowsService() bool { return false }

// runWindowsService 非 Windows 不可达（main 的分支被 isWindowsService 挡住），
// 保留符号以满足交叉编译。
func runWindowsService(cfgPath string) {}

func runServiceCommand(args []string) {
	fmt.Println("service 子命令仅支持 Windows（Linux/Docker 请用 systemd unit 或容器 restart policy）。")
	os.Exit(2)
}
