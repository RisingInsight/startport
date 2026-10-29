// store_test.go 账号存储的不变量与持久化（架构 §3.2）。
package account

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "accounts.json")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s.SetLogf(func(string, ...any) {}) // 测试静音
	t.Cleanup(s.Close)
	return s
}

func loomyCred() json.RawMessage { return json.RawMessage(`{"session":"sess-0123456789abcdef"}`) }

// Add / Get / List 基本流；label 缺省编号按平台独立递增。
func TestStoreAddAndList(t *testing.T) {
	s := newTestStore(t)
	a1, err := s.Add(PlatformLoomy, loomyCred(), "", "")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !strings.HasPrefix(a1.ID, "acc-") || len(a1.ID) != len("acc-")+8 {
		t.Errorf("ID 形态异常: %q", a1.ID)
	}
	if a1.Label != "账号-1" {
		t.Errorf("缺省 label = %q", a1.Label)
	}
	a2, _ := s.Add(PlatformLoomy, loomyCred(), "", "")
	if a2.Label != "账号-2" {
		t.Errorf("第二个缺省 label = %q", a2.Label)
	}
	// 另一平台的编号独立。
	a3, err := s.Add(PlatformQoder, json.RawMessage(`{"access_token":"dt-1","user_id":"u"}`), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if a3.Label != "账号-1" {
		t.Errorf("新平台缺省 label = %q", a3.Label)
	}
	if got := len(s.List(PlatformLoomy)); got != 2 {
		t.Errorf("loomy 账号数 = %d, want 2", got)
	}
	if got := len(s.List("")); got != 3 {
		t.Errorf("全部账号数 = %d, want 3", got)
	}
}

// 同平台 label 唯一（重名报错带 platform+label）。
func TestStoreLabelUniquePerPlatform(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Add(PlatformLoomy, loomyCred(), "主号", ""); err != nil {
		t.Fatal(err)
	}
	_, dupErr := s.Add(PlatformLoomy, loomyCred(), "主号", "")
	if dupErr == nil || !strings.Contains(dupErr.Error(), "loomy") || !strings.Contains(dupErr.Error(), "主号") {
		t.Errorf("重名报错应带 platform+label: %v", dupErr)
	}
	// 不同平台同名合法。
	if _, err := s.Add(PlatformQoder, json.RawMessage(`{"access_token":"dt-1","user_id":"u"}`), "主号", ""); err != nil {
		t.Errorf("不同平台同名应合法: %v", err)
	}
}

// ID 永不复用（删除后新账号拿到不同 ID）。
func TestStoreIDNeverReused(t *testing.T) {
	s := newTestStore(t)
	a1, _ := s.Add(PlatformLoomy, loomyCred(), "", "")
	if err := s.Delete(a1.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	a2, _ := s.Add(PlatformLoomy, loomyCred(), "", "")
	if a2.ID == a1.ID {
		t.Errorf("ID 被复用: %s", a2.ID)
	}
	if s.Get(a1.ID) != nil {
		t.Error("删除后 Get 仍返回账号")
	}
}

// Cred 原样落盘（Store 不理解平台语义；多余字段保留）。
func TestStoreCredPreserved(t *testing.T) {
	s := newTestStore(t)
	raw := json.RawMessage(`{"session":"sess-0123456789abcdef","extra":"keep-me","n":3}`)
	a, _ := s.Add(PlatformLoomy, raw, "", "")
	got := s.Get(a.ID)
	var blob map[string]any
	if err := json.Unmarshal(got.Cred, &blob); err != nil {
		t.Fatalf("Cred 不是 JSON: %s", got.Cred)
	}
	if blob["extra"] != "keep-me" || blob["n"] != float64(3) {
		t.Errorf("Cred 字段被改写: %s", got.Cred)
	}
}

// 持久化：Save → 新 Store Load 读回同数据（含签到历史与 30 天裁剪）。
func TestStorePersistAndCheckinTrim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	s, _ := Load(path)
	s.SetLogf(func(string, ...any) {})
	base := time.Now()
	s.SetNowFunc(func() time.Time { return base })
	a, _ := s.Add(PlatformLoomy, loomyCred(), "", "")
	// 31 天前的一条应被裁掉；今天与昨天的保留。
	old := base.Add(-31 * 24 * time.Hour)
	s.SetNowFunc(func() time.Time { return old })
	if err := s.RecordCheckin(a.ID, CheckinRecord{At: s.now(), Status: CheckinFailed, Error: "old"}); err != nil {
		t.Fatal(err)
	}
	s.SetNowFunc(func() time.Time { return base })
	_ = s.RecordCheckin(a.ID, CheckinRecord{At: base, Status: CheckinOK, Reward: "100 credits"})
	_ = s.RecordCheckin(a.ID, CheckinRecord{At: base.Add(-time.Hour), Status: CheckinAlready})
	s.Save()

	s2, err := Load(path)
	if err != nil {
		t.Fatalf("重载: %v", err)
	}
	s2.SetLogf(func(string, ...any) {})
	defer s2.Close()
	got := s2.Get(a.ID)
	if got == nil {
		t.Fatal("重启后账号丢失")
	}
	if len(got.Checkin) != 2 {
		t.Errorf("签到历史 = %d 条, want 2（31 天前的那条被裁剪）", len(got.Checkin))
	}
	if got.Checkin[0].Status != CheckinAlready {
		t.Errorf("最新记录应在首位: %+v", got.Checkin[0])
	}
	// 权限位：0600。
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Windows 不落 unix 权限位：只在 unix 上断言 0600。
	if runtime.GOOS != "windows" {
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("accounts.json 权限 = %o, want 600", perm)
		}
	}
}

// 损坏恢复：坏文件移到 .corrupt-*，空库继续（不拒绝启动）。
func TestStoreCorruptRecovery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")
	if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatalf("损坏文件不应导致 Load 失败: %v", err)
	}
	s.SetLogf(func(string, ...any) {})
	defer s.Close()
	if len(s.List("")) != 0 {
		t.Error("损坏恢复后应为空库")
	}
	entries, _ := os.ReadDir(dir)
	found := false
	for _, e := range entries {
		if strings.Contains(e.Name(), ".corrupt-") {
			found = true
		}
	}
	if !found {
		t.Error("坏文件未保留取证副本")
	}
}

// 校验：各平台 blob schema fail fast + custom 的 SSRF 红线。
func TestValidateCred(t *testing.T) {
	if err := ValidateCred(PlatformLoomy, json.RawMessage(`{"session":""}`)); err == nil {
		t.Error("空 session 应报错")
	}
	if err := ValidateCred(PlatformLoomy, json.RawMessage(`{"session":"short"}`)); err == nil {
		t.Error("过短 session 应报错")
	}
	if err := ValidateCred(PlatformLoomy, loomyCred()); err != nil {
		t.Errorf("合法 loomy blob 报错: %v", err)
	}
	if err := ValidateCred(PlatformAutoClaw, json.RawMessage(`{"token":""}`)); err == nil {
		t.Error("空 token 应报错")
	}
	if err := ValidateCred(PlatformQoder, json.RawMessage(`{"access_token":"dt-1"}`)); err == nil {
		t.Error("缺 user_id 应报错")
	}
	if err := ValidateCred(PlatformQoder, json.RawMessage(`{"access_token":"jt-1","user_id":"u"}`)); err != nil {
		t.Errorf("合法 qoder blob 报错: %v", err)
	}
	if err := ValidateCred(PlatformCustom, json.RawMessage(`{"base_url":"https://api.example.com","secret":"s"}`)); err != nil {
		t.Errorf("custom 公网地址应放行: %v", err)
	}
	if err := ValidateCred(PlatformCustom, json.RawMessage(`{"base_url":"http://127.0.0.1:9999","secret":"s","allow_private":true}`)); err != nil {
		t.Errorf("custom 本机地址 + allow_private 应放行: %v", err)
	}
	if err := ValidateCred(PlatformCustom, json.RawMessage(`{"base_url":"http://169.254.169.254/","secret":"s"}`)); err == nil {
		t.Error("custom 指向元数据端点应拒绝（SSRF 红线）")
	}
	if err := ValidateCred("bogus", loomyCred()); err == nil {
		t.Error("未知平台应报错")
	}
	// 库库AI：凭证是网页登录 Cookie，必须带 BDUSS（登录态核心键）。
	if err := ValidateCred(PlatformKuku, json.RawMessage(`{"cookie":""}`)); err == nil {
		t.Error("空 cookie 应报错")
	}
	if err := ValidateCred(PlatformKuku, json.RawMessage(`{"cookie":"short"}`)); err == nil {
		t.Error("过短 cookie 应报错")
	}
	if err := ValidateCred(PlatformKuku, json.RawMessage(`{"cookie":"foo=bar; baz=qux; longenough"}`)); err == nil {
		t.Error("缺 BDUSS 的 cookie 应报错")
	}
	if err := ValidateCred(PlatformKuku, json.RawMessage(`{"cookie":"BDUSS=abcdefghijklmnop; STOKEN=xyz"}`)); err != nil {
		t.Errorf("合法 kuku blob 报错: %v", err)
	}
}

// PrimarySecret：qoder 产出适配器形态的账号 JSON（mode 按平台推导）。
func TestPrimarySecretQoderJSON(t *testing.T) {
	got, err := PrimarySecret(PlatformQoder, json.RawMessage(
		`{"access_token":"jt-1","user_id":"u-1","machine_id":"m-1","nickname":"N"}`))
	if err != nil {
		t.Fatalf("PrimarySecret: %v", err)
	}
	var parsed struct {
		Mode        string `json:"mode"`
		AccessToken string `json:"accessToken"`
		UserId      string `json:"userId"`
		MachineId   string `json:"machineId"`
		Nickname    string `json:"nickname"`
	}
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("主凭证不是适配器 JSON: %s", got)
	}
	if parsed.Mode != "cn" || parsed.AccessToken != "jt-1" || parsed.UserId != "u-1" || parsed.MachineId != "m-1" {
		t.Errorf("适配器 JSON 字段异常: %+v", parsed)
	}
	// 国际版 mode=global。
	got, _ = PrimarySecret(PlatformQoderIntl, json.RawMessage(`{"access_token":"dt-1","user_id":"u"}`))
	if !strings.Contains(got, `"mode":"global"`) {
		t.Errorf("qoder-intl mode 应为 global: %s", got)
	}
}

// BaseURLOf：blob 覆盖优先，缺省用平台官方基址。
func TestBaseURLOf(t *testing.T) {
	got, _ := BaseURLOf(PlatformLoomy, loomyCred())
	if got != "https://loomyad.xunfei.cn/api/v1" {
		t.Errorf("loomy 缺省 base = %q", got)
	}
	got, _ = BaseURLOf(PlatformLoomy, json.RawMessage(`{"session":"sess-0123456789abcdef","base_url":"https://custom.example/api"}`))
	if got != "https://custom.example/api" {
		t.Errorf("blob 覆盖未生效: %q", got)
	}
}
