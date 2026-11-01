// backup.go 本地定时备份（进程守护场景的廉价保险）：每日把 config.json 与
// 数据目录整体快照到 data/backups/backup-<时间戳>/，保留最近 Keep 份。
//
// 设计边界：
//   - 只做本机文件拷贝（远程/WebDAV 上传判为过度设计，明确不做）；
//   - 快照含密钥文件（config.json / apikeys.json）→ 拷贝保留源文件权限（0600），
//     备份目录与数据目录同盘同保护域；
//   - 幂等：当天（按本地日期前缀）已有快照则跳过——进程重启不产生重复份；
//   - config.json 是原子替换写的，拷贝瞬间理论上可能撞上 rename 窗口（ENOENT）：
//     失败只 WARN 留待下一天，不做进程内重试。
//
// 配置热生效：循环每个 tick 重读 config.json 的 backup 节，改配置无需重启。
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// backupLoop 每日本地备份循环（serverMain 用 ctx 挂钩启停）。
func backupLoop(ctx context.Context, cfgPath string, logf func(format string, args ...any)) {
	t := time.NewTicker(30 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cfg, err := Load(cfgPath)
		if err != nil {
			logf("[backup] 读取配置失败，本轮跳过: %v", err)
			continue
		}
		if !cfg.Backup.Enabled {
			continue
		}
		now := time.Now()
		if now.Hour() < cfg.Backup.Hour {
			continue // 还没到触发小时
		}
		dataDir := filepath.Dir(cfg.StateFile)
		if err := runBackup(cfgPath, dataDir, cfg.Backup.Keep, logf); err != nil {
			logf("WARN: [backup] %v", err)
			continue
		}
	}
}

// runBackup 执行一次快照（测试锚点）。dataDir 内的 backups/ 自身不递归进快照；
// 保留最近 keep 份（超出按目录名时间戳序删除）。
func runBackup(configPath, dataDir string, keep int, logf func(format string, args ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if keep <= 0 {
		keep = 7
	}
	outRoot := filepath.Join(dataDir, "backups")
	today := time.Now().Format("20060102")
	// 幂等：当天已有快照（backup-<today>* 前缀）则跳过。
	if entries, err := os.ReadDir(outRoot); err == nil {
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), "backup-"+today) {
				logf("[backup] 今日已有快照，跳过（%s）", e.Name())
				return nil
			}
		}
	}
	dest := filepath.Join(outRoot, "backup-"+time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return fmt.Errorf("创建备份目录: %w", err)
	}
	// 1) config.json（可能不在数据目录里——仓库根/任意路径）。
	if _, err := os.Stat(configPath); err == nil {
		if err := copyFile(configPath, filepath.Join(dest, "config.json")); err != nil {
			return fmt.Errorf("拷贝 config.json: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat config.json: %w", err)
	}
	// 2) 数据目录整体（排除 backups 自身，防递归自拷）。
	n := 0
	err := filepath.WalkDir(dataDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == filepath.Join(dataDir, "backups") {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(dataDir, path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(dest, filepath.Dir(rel)), 0o755); err != nil {
			return err
		}
		if err := copyFile(path, filepath.Join(dest, rel)); err != nil {
			return err
		}
		n++
		return nil
	})
	if err != nil {
		return fmt.Errorf("拷贝数据目录: %w", err)
	}
	// 3) 保留最近 keep 份（目录名时间戳序 = 时间序；解析失败的名字排最后删）。
	entries, err := os.ReadDir(outRoot)
	if err != nil {
		return fmt.Errorf("列备份目录: %w", err)
	}
	var snaps []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "backup-") {
			snaps = append(snaps, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(snaps)))
	removed := 0
	for i := keep; i < len(snaps); i++ {
		if err := os.RemoveAll(filepath.Join(outRoot, snaps[i])); err == nil {
			removed++
		}
	}
	logf("[backup] 快照完成：%s（%d 个数据文件，保留 %d 份，清理 %d 份旧快照）", filepath.Base(dest), n, keep, removed)
	return nil
}

// copyFile 拷贝单文件并**保留源文件权限**（config.json / apikeys.json 都是
// 0600——备份丢了权限位等于把密钥摊给同机其他用户）。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, st.Mode())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
