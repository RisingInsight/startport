package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runBackup：config + 数据目录整体进快照、幂等（当天重复跳过）、
// 保留份数裁剪、0600 权限保留。
func TestRunBackupSnapshotAndPrune(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(p, c string, mode os.FileMode) {
		if err := os.WriteFile(p, []byte(c), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(cfgPath, `{"listen":"127.0.0.1:0"}`, 0o600)
	secret := filepath.Join(dataDir, "apikeys.json")
	write(secret, `{"keys":[]}`, 0o600)
	plain := filepath.Join(dataDir, "state.json")
	write(plain, `{"accounts":{}}`, 0o644)
	write(filepath.Join(dataDir, "sub", "nested.json"), `{}`, 0o644)

	// 预置两份旧快照（名字比今天的新快照更早）→ keep=2 时旧快照应被裁到只剩
	// 最新的两份（今天这份 + 一份预置）。
	outRoot := filepath.Join(dataDir, "backups")
	if err := os.MkdirAll(filepath.Join(outRoot, "backup-20200101-000000"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(outRoot, "backup-20200102-000000"), 0o755); err != nil {
		t.Fatal(err)
	}

	logs := []string{}
	if err := runBackup(cfgPath, dataDir, 2, func(f string, a ...any) { logs = append(logs, f) }); err != nil {
		t.Fatalf("runBackup: %v", err)
	}

	// 快照内容：config + 数据文件（含子目录），backups 自身不进快照。
	snaps, err := os.ReadDir(outRoot)
	if err != nil {
		t.Fatal(err)
	}
	var todaySnap string
	for _, e := range snaps {
		if strings.HasPrefix(e.Name(), "backup-"+time.Now().Format("20060102")) {
			todaySnap = e.Name()
		}
	}
	if todaySnap == "" {
		t.Fatalf("today snapshot missing: %v", snaps)
	}
	for _, want := range []string{"config.json", "apikeys.json", "state.json", filepath.Join("sub", "nested.json")} {
		if _, err := os.Stat(filepath.Join(outRoot, todaySnap, want)); err != nil {
			t.Errorf("snapshot missing %s: %v", want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(outRoot, todaySnap, "backups")); !os.IsNotExist(err) {
		t.Error("backups dir must not be recursed into snapshot")
	}
	// 权限保留契约：拷贝件 mode == 源件 mode（Windows 的 POSIX 位是模拟值，
	// 不能断言具体 0600——保留语义本身才是 copyFile 的契约）。
	srcSt, err := os.Stat(secret)
	if err != nil {
		t.Fatal(err)
	}
	dstSt, err := os.Stat(filepath.Join(outRoot, todaySnap, "apikeys.json"))
	if err != nil {
		t.Fatal(err)
	}
	if srcSt.Mode().Perm() != dstSt.Mode().Perm() {
		t.Errorf("backup mode = %v, want source %v", dstSt.Mode().Perm(), srcSt.Mode().Perm())
	}
	// 幂等：同一天再跑不产生新快照。
	before := len(snaps)
	if err := runBackup(cfgPath, dataDir, 2, nil); err != nil {
		t.Fatal(err)
	}
	snaps, _ = os.ReadDir(outRoot)
	if len(snaps) != before {
		t.Fatalf("second same-day backup created a new snapshot (%d → %d)", before, len(snaps))
	}
	// 裁剪：keep=2 → 只剩今天的 + 最新的一份预置。
	remaining := []string{}
	for _, e := range snaps {
		remaining = append(remaining, e.Name())
	}
	if len(remaining) != 2 {
		t.Fatalf("prune = %v, want 2 entries", remaining)
	}
	if !contains(remaining, "backup-20200102-000000") {
		t.Fatalf("prune kept wrong set: %v", remaining)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
