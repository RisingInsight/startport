// Package apikeys 面板生成的「agent 接入密钥」：存储、生成与校验。
//
// 与面板登录密钥（config.json 的 api_key）的关系与边界：
//   - api_key 是管理密钥：明文存 config.json，面板 /startport/api/* 与 /v1/* 都认它；
//   - agent 密钥由用户在面板创建，仅用于 /v1/*（聊天 + 模型列表），不开放面板——
//     agent 密钥泄露最多被白嫖积分，面板、账号凭证与配置不受影响；
//   - 全钥只在创建响应里出现一次，存储侧只保留 SHA-256 摘要（与 GitHub/DeepSeek
//     同做法）。密钥是 128 位随机数而非人类密码，摘要校验即可，无需慢哈希。
//
// 并发与持久化：内存为权威、脏标记 + 30s 防抖落盘（同 usage/state 模式）；
// CRUD 立即落盘（创建的全钥只出现一次，丢了就得重建，不能等防抖窗口）。
package apikeys

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	bearerPrefix = "Bearer "
	keyPrefix    = "sp-" // 全钥前缀：sp- + 32 hex（16 字节 = 128 位熵）
	keyEntropy   = 16    // 随机字节数
	MaxKeys      = 50    // 单实例密钥数上限（个人场景富余；防无界增长）
	MaxNameLen   = 40    // 名称长度上限（rune 计）
	persistEvery = 30 * time.Second
)

var (
	ErrNotFound = errors.New("key not found")
	ErrLimit    = errors.New("key limit reached")
	ErrBadName  = errors.New("invalid key name")
)

// Key 单把密钥的存储行。全钥永不落盘，只存 SHA-256 摘要；Prefix 保留明文
// 前 12 位（"sp-xxxxxxxx"）供列表辨认。
type Key struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Hash      string    `json:"hash"` // SHA-256 hex（全钥）
	Prefix    string    `json:"prefix"`
	CreatedAt time.Time `json:"created_at"`
	LastUsed  time.Time `json:"last_used,omitempty"`
	UsedCount int64     `json:"used_count,omitempty"`
	Disabled  bool      `json:"disabled,omitempty"`
}

// Public 列表/响应里对外暴露的行：不带摘要（摘要虽不可逆，也不必下发）。
type Public struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Prefix    string    `json:"prefix"`
	CreatedAt time.Time `json:"created_at"`
	LastUsed  time.Time `json:"last_used,omitempty"`
	UsedCount int64     `json:"used_count,omitempty"`
	Disabled  bool      `json:"disabled,omitempty"`
}

func (k Key) public() Public {
	return Public{
		ID: k.ID, Name: k.Name, Prefix: k.Prefix,
		CreatedAt: k.CreatedAt, LastUsed: k.LastUsed,
		UsedCount: k.UsedCount, Disabled: k.Disabled,
	}
}

// APIKeys 密钥存储。并发安全；零值不可用，经 New 构建。
type APIKeys struct {
	mu    sync.Mutex
	path  string
	keys  []Key
	dirty bool
	stop  chan struct{}
	done  chan struct{}
	// hiddenNames 分组用量里被隐藏的密钥名(键=密钥名;空串 = 管理密钥组)。
	// 纯显示偏好:桶数据不动,仅面板 usage 响应过滤该组;删除密钥后其历史
	// 分组仍可被隐藏/恢复。
	hiddenNames map[string]bool
}

// fileFormat 落盘结构。v1 是裸数组([]Key),v2 起为对象(带隐藏名单);
// 加载时两种格式都认,保存恒为新格式(旧二进制读新文件会退化为空表+告警,
// 与任何 schema 升级同代价)。
type fileFormat struct {
	Keys        []Key    `json:"keys"`
	HiddenNames []string `json:"hidden_names,omitempty"`
}

// New 构建存储并尝试从 path 加载；path 为空 = 纯内存（测试用）。
// 文件损坏不 fail-closed 到拒绝启动：告警后按空表运行，行为退化为
// 「仅 api_key 可用」——比带着半截密钥表启动安全。
func New(path string) *APIKeys {
	st := &APIKeys{path: path, stop: make(chan struct{}), done: make(chan struct{})}
	if path != "" {
		if raw, err := os.ReadFile(path); err == nil {
			var ff fileFormat
			if err := json.Unmarshal(raw, &ff); err != nil {
				// 旧格式(裸数组):一次性迁移。
				var legacy []Key
				if err2 := json.Unmarshal(raw, &legacy); err2 != nil {
					log.Printf("[apikeys] %s 解析失败（按空表运行，已有 agent 密钥需重建）: %v", path, err)
				} else {
					ff.Keys = legacy
				}
			}
			st.keys = ff.Keys
			if len(ff.HiddenNames) > 0 {
				st.hiddenNames = make(map[string]bool, len(ff.HiddenNames))
				for _, n := range ff.HiddenNames {
					st.hiddenNames[n] = true
				}
			}
		}
	}
	return st
}

// Start 启动防抖落盘 goroutine（last_used 的持久化通道）。
func (st *APIKeys) Start() {
	go func() {
		defer close(st.done)
		t := time.NewTicker(persistEvery)
		defer t.Stop()
		for {
			select {
			case <-st.stop:
				st.flush()
				return
			case <-t.C:
				st.flush()
			}
		}
	}()
}

// Stop 停止后台落盘并做最后一次落盘（幂等）。
func (st *APIKeys) Stop() {
	if st.stop != nil {
		select {
		case <-st.stop:
		default:
			close(st.stop)
		}
	}
	<-st.done
}

// List 返回全部密钥的对外视图（无摘要）。
func (st *APIKeys) List() []Public {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]Public, 0, len(st.keys))
	for _, k := range st.keys {
		out = append(out, k.public())
	}
	return out
}

// Create 生成一把新密钥：返回全钥（仅此一次）与对外视图。
// name 去首尾空白后须非空且 ≤ MaxNameLen（rune 计）；重名允许（同 DeepSeek）。
func (st *APIKeys) Create(name string) (full string, k Public, err error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > MaxNameLen {
		return "", Public{}, ErrBadName
	}
	// 竖线是用量桶键的分隔符,名称里统一转义,避免聚合键被撞。
	name = strings.ReplaceAll(name, "|", "_")
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.keys) >= MaxKeys {
		return "", Public{}, ErrLimit
	}
	raw := make([]byte, keyEntropy)
	if _, err := rand.Read(raw); err != nil {
		return "", Public{}, err
	}
	full = keyPrefix + hex.EncodeToString(raw)
	idRaw := make([]byte, 6)
	if _, err := rand.Read(idRaw); err != nil {
		return "", Public{}, err
	}
	kRow := Key{
		ID:        hex.EncodeToString(idRaw),
		Name:      name,
		Hash:      hashHex(full),
		Prefix:    full[:12],
		CreatedAt: time.Now(),
	}
	st.keys = append(st.keys, kRow)
	st.persistLocked()
	return full, kRow.public(), nil
}

// Rename 改名（不影响摘要与既有接入）。
func (st *APIKeys) Rename(id, name string) (Public, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > MaxNameLen {
		return Public{}, ErrBadName
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	for i := range st.keys {
		if st.keys[i].ID == id {
			st.keys[i].Name = name
			st.persistLocked()
			return st.keys[i].public(), nil
		}
	}
	return Public{}, ErrNotFound
}

// SetDisabled 启用/禁用。禁用立即失效（校验侧跳过），行保留供重新启用。
func (st *APIKeys) SetDisabled(id string, disabled bool) (Public, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for i := range st.keys {
		if st.keys[i].ID == id {
			st.keys[i].Disabled = disabled
			st.persistLocked()
			return st.keys[i].public(), nil
		}
	}
	return Public{}, ErrNotFound
}

// Delete 删除（该密钥立即失效；接入方需换用其他密钥）。
func (st *APIKeys) Delete(id string) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	for i := range st.keys {
		if st.keys[i].ID == id {
			st.keys = append(st.keys[:i], st.keys[i+1:]...)
			st.persistLocked()
			return nil
		}
	}
	return ErrNotFound
}

// SetHidden 设置某密钥名的历史分组在「分组用量·按密钥」里的显示/隐藏。
// 纯显示偏好:桶数据不动;name 为空串 = 管理密钥组。
func (st *APIKeys) SetHidden(name string, hidden bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.hiddenNames == nil {
		st.hiddenNames = map[string]bool{}
	}
	if hidden {
		st.hiddenNames[name] = true
	} else {
		delete(st.hiddenNames, name)
	}
	st.dirty = true
	st.persistLocked()
}

// HiddenNames 返回被隐藏的密钥名(升序)。
func (st *APIKeys) HiddenNames() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]string, 0, len(st.hiddenNames))
	for n := range st.hiddenNames {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Verify 用全钥校验；命中任一启用密钥返回 true 并在内存累计 last_used/次数
// （防抖落盘，不阻塞请求路径）。禁用/删除的密钥恒不命中。
func (st *APIKeys) Verify(full string) bool {
	_, ok := st.verify(full)
	return ok
}

// verify 内部校验:命中返回命中行(已累计使用)。调用方须持锁语义由本方法自理。
func (st *APIKeys) verify(full string) (Key, bool) {
	if full == "" {
		return Key{}, false
	}
	sum := sha256.Sum256([]byte(full))
	want := hex.EncodeToString(sum[:])
	st.mu.Lock()
	defer st.mu.Unlock()
	for i := range st.keys {
		k := &st.keys[i]
		if k.Disabled {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(k.Hash), []byte(want)) == 1 {
			k.UsedCount++
			k.LastUsed = time.Now()
			st.dirty = true
			return *k, true
		}
	}
	return Key{}, false
}

// AuthRequest 从请求的 Authorization: Bearer 提取全钥并校验。
// 缺头/方案不对返回 false（不泄露耗时形状差异：Verify 对空串恒 false）。
func (st *APIKeys) AuthRequest(r *http.Request) bool {
	return st.AuthName(r) != ""
}

// AuthName 同 AuthRequest,但返回命中的密钥名——chatAuth 把它放进请求上下文,
// 供用量记录按密钥归属。未命中返回空串。
func (st *APIKeys) AuthName(r *http.Request) string {
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, bearerPrefix) {
		if st.Verify("") {
			return "" // 全钥为空的命中只发生在「未启用鉴权」形态,无密钥名可归属
		}
		return ""
	}
	if k, ok := st.verify(authz[len(bearerPrefix):]); ok {
		return k.Name
	}
	return ""
}

func hashHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// flush 落盘（脏标记为真时）。供后台 ticker 与 Stop 调用。
func (st *APIKeys) flush() {
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.dirty {
		return
	}
	st.persistLocked()
}

// persistLocked 原子写（tmp + rename，0600）。调用方须持锁。
func (st *APIKeys) persistLocked() {
	st.dirty = false
	if st.path == "" {
		return
	}
	ff := fileFormat{Keys: st.keys}
	for n := range st.hiddenNames {
		ff.HiddenNames = append(ff.HiddenNames, n)
	}
	sort.Strings(ff.HiddenNames)
	raw, err := json.MarshalIndent(ff, "", "  ")
	if err != nil {
		log.Printf("[apikeys] 序列化失败: %v", err)
		return
	}
	if dir := filepath.Dir(st.path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("[apikeys] 落盘失败: %v", err)
		return
	}
	if err := os.Rename(tmp, st.path); err != nil {
		log.Printf("[apikeys] 落盘失败: %v", err)
	}
}
