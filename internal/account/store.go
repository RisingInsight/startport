// store.go 账号存储：mu 保护内存副本，5s 防抖原子落盘（0600 tmp+rename，
// 照抄 saveConfig 的原子写与 internal/usage 的 flush 模式）。
//
// 不变量（包注释 + store_test 钉住）：
//   - 同平台内 Label 唯一（重名报错，错误带 platform+label）；
//   - ID 永不复用（删除后 seq 继续走）；
//   - Cred 原样落盘（Store 不理解平台语义）。
package account

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// flushDebounce 落盘防抖窗口：最后一次变更后静默 5s 才写盘（批量操作只落一次）。
const flushDebounce = 5 * time.Second

// Store 账号存储（事实来源）。
type Store struct {
	mu      sync.Mutex
	path    string
	accts   []*Account
	seq     map[string]int // platform → label 缺省编号（只增不减）
	dirty   bool
	markAt  time.Time
	stop    chan struct{}
	stopped bool
	nowFunc func() time.Time // 测试注入（签到历史时间戳）
	logf    func(format string, args ...any)
}

// Load 加载账号库。文件缺失 = 空库（首次运行）；损坏 = 移到 <path>.corrupt-<ts>
// 后以空库继续（损坏恢复：账号数据可由用户重导，不能因坏文件拒绝启动）。
// 返回的 Store 已带防抖落盘协程，用完须 Close。
func Load(path string) (*Store, error) {
	s := &Store{
		path:    path,
		seq:     map[string]int{},
		stop:    make(chan struct{}),
		nowFunc: time.Now,
		logf: func(format string, args ...any) {
			log.Printf("[account] "+format, args...)
		},
	}
	raw, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		// 首次运行：空库。
	case err != nil:
		return nil, fmt.Errorf("read accounts: %w", err)
	default:
		if uerr := json.Unmarshal(raw, &s.accts); uerr != nil {
			// 损坏恢复：坏文件挪到旁边（保留取证），空库继续。
			aside := fmt.Sprintf("%s.corrupt-%d", path, time.Now().Unix())
			if mvErr := os.Rename(path, aside); mvErr != nil {
				aside = path + "（保留原位）"
			}
			s.logf("accounts.json 损坏已移至 %s，以空库继续（%v）", aside, uerr)
			s.accts = nil
		}
	}
	// 恢复 seq：label 缺省编号取现存最大值（防重启后重名）。
	for _, a := range s.accts {
		if n := parseSeq(a.Label); n > s.seq[a.Platform] {
			s.seq[a.Platform] = n
		}
	}
	go s.flusher()
	return s, nil
}

// parseSeq 从 "账号-<n>" 形态的 label 里取编号（非该形态返回 0）。
func parseSeq(label string) int {
	var n int
	if _, err := fmt.Sscanf(label, "账号-%d", &n); err == nil {
		return n
	}
	return 0
}

// SetNowFunc 注入时钟（测试用；须在 Load 后立即调用）。
func (s *Store) SetNowFunc(f func() time.Time) {
	s.mu.Lock()
	s.nowFunc = f
	s.mu.Unlock()
}

// SetLogf 注入日志函数（测试静音用）。
func (s *Store) SetLogf(f func(format string, args ...any)) {
	s.mu.Lock()
	s.logf = f
	s.mu.Unlock()
}

// Close 停止防抖协程并强制落盘。
func (s *Store) Close() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	close(s.stop)
	dirty := s.dirty
	s.mu.Unlock()
	if dirty {
		s.Save()
	}
}

// flusher 防抖循环：dirty 且距最后变更 ≥5s → 落盘。
func (s *Store) flusher() {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-tick.C:
			s.mu.Lock()
			due := s.dirty && time.Since(s.markAt) >= flushDebounce
			s.mu.Unlock()
			if due {
				s.Save()
			}
		}
	}
}

// Add 新增账号（platform 校验 + blob 校验 + label 唯一性）。
func (s *Store) Add(platform string, cred json.RawMessage, label, note string) (*Account, error) {
	if !ValidPlatform(platform) {
		return nil, fmt.Errorf("未知平台 %q（可用：loomy / autoclaw / autoclaw-intl / qoder / qoder-intl / custom）", platform)
	}
	if err := ValidateCred(platform, cred); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if label == "" {
		s.seq[platform]++
		label = fmt.Sprintf("账号-%d", s.seq[platform])
	}
	for _, a := range s.accts {
		if a.Platform == platform && a.Label == label {
			return nil, fmt.Errorf("平台 %s 下账号名 %q 已存在", platform, label)
		}
	}
	id, err := newAccountID()
	if err != nil {
		return nil, err
	}
	a := &Account{
		ID:        id,
		Platform:  platform,
		Label:     label,
		Cred:      append(json.RawMessage(nil), cred...),
		Note:      note,
		CreatedAt: s.now(),
	}
	s.accts = append(s.accts, a)
	s.markDirty()
	return a, nil
}

// Get 按 ID 取账号（未找到返回 nil）。返回内部指针的只读视图——调用方不得修改。
func (s *Store) Get(id string) *Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.accts {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// List 按平台列账号（platform 为空 = 全部），保持添加顺序。
func (s *Store) List(platform string) []*Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Account, 0, len(s.accts))
	for _, a := range s.accts {
		if platform == "" || a.Platform == platform {
			out = append(out, a)
		}
	}
	return out
}

// SetDisabled 停用/启用账号。
func (s *Store) SetDisabled(id string, disabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.byIDLocked(id)
	if a == nil {
		return fmt.Errorf("账号 %q 不存在", id)
	}
	a.Disabled = disabled
	s.markDirty()
	return nil
}

// Delete 删除账号（ID 不复用：seq 不回退）。
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, a := range s.accts {
		if a.ID == id {
			s.accts = append(s.accts[:i], s.accts[i+1:]...)
			s.markDirty()
			return nil
		}
	}
	return fmt.Errorf("账号 %q 不存在", id)
}

// RecordCheckin 记一次签到（环形 30 天裁剪；签到不触发防抖外的特殊处理，
// 落盘节奏与其它变更一致）。
func (s *Store) RecordCheckin(id string, r CheckinRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.byIDLocked(id)
	if a == nil {
		return fmt.Errorf("账号 %q 不存在", id)
	}
	a.Checkin = append([]CheckinRecord{r}, a.Checkin...)
	a.Checkin = TrimCheckinHistory(a.Checkin, s.now())
	s.markDirty()
	return nil
}

// SetBalance 写余额快照（P1-1 结构占位）。
func (s *Store) SetBalance(id string, snap *BalanceSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.byIDLocked(id)
	if a == nil {
		return fmt.Errorf("账号 %q 不存在", id)
	}
	a.Balance = snap
	s.markDirty()
	return nil
}

// Save 强制落盘（停机 / 需要立即持久化的场景）。
func (s *Store) Save() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return
	}
	if err := s.saveLocked(); err != nil {
		s.logf("落盘失败: %v", err)
		return
	}
	s.dirty = false
}

// saveLocked 原子写（tmp+rename 0600；与 saveConfig 同款，不含 EBUSY 回退——
// accounts.json 是数据文件，不进单文件 bind mount）。
func (s *Store) saveLocked() error {
	out, err := json.MarshalIndent(s.accts, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal accounts: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return fmt.Errorf("write accounts: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("replace accounts: %w", err)
	}
	return nil
}

// markDirty 标脏（持锁调用）。
func (s *Store) markDirty() {
	s.dirty = true
	s.markAt = time.Now()
}

// byIDLocked 按 ID 查（持锁调用）。
func (s *Store) byIDLocked(id string) *Account {
	for _, a := range s.accts {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// now 当前时间（测试注入点）。
func (s *Store) now() time.Time {
	if s.nowFunc != nil {
		return s.nowFunc()
	}
	return time.Now()
}

// newAccountID "acc-" + 8 hex（crypto/rand；稳定且不涉密）。
func newAccountID() (string, error) {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成账号 ID 失败: %w", err)
	}
	return "acc-" + hex.EncodeToString(buf), nil
}
