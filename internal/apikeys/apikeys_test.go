package apikeys

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateVerifyRoundtrip(t *testing.T) {
	st := New("")
	full, k, err := st.Create("测试 agent")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.HasPrefix(full, "sp-") || len(full) != len("sp-")+32 {
		t.Fatalf("key shape = %q, want sp-+32hex", full)
	}
	if k.Prefix != full[:12] || k.Name != "测试 agent" {
		t.Fatalf("public row = %+v", k)
	}
	if !st.Verify(full) {
		t.Fatal("正确的全钥未通过校验")
	}
	if st.Verify(full+"x") || st.Verify("sp-00000000000000000000000000000000") || st.Verify("") {
		t.Fatal("错误/空全钥通过了校验")
	}
	if st.AuthRequest(httptest.NewRequest("POST", "/v1/chat/completions", nil)) {
		t.Fatal("缺 Authorization 头不应通过")
	}
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer "+full)
	if !st.AuthRequest(r) {
		t.Fatal("AuthRequest 未命中有效密钥")
	}
}

func TestDisableAndDeleteInvalidate(t *testing.T) {
	st := New("")
	full, k, _ := st.Create("a")
	if !st.Verify(full) {
		t.Fatal("precondition: verify failed")
	}
	if _, err := st.SetDisabled(k.ID, true); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if st.Verify(full) {
		t.Fatal("禁用密钥仍通过校验")
	}
	if _, err := st.SetDisabled(k.ID, false); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if !st.Verify(full) {
		t.Fatal("重新启用后未恢复")
	}
	if err := st.Delete(k.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if st.Verify(full) {
		t.Fatal("删除后密钥仍通过校验")
	}
	for _, op := range []func() error{
		func() error { _, err := st.Rename(k.ID, "x"); return err },
		func() error { _, err := st.SetDisabled(k.ID, true); return err },
		func() error { return st.Delete(k.ID) },
	} {
		if op() != ErrNotFound {
			t.Fatal("对已删除密钥的操作应返回 ErrNotFound")
		}
	}
}

func TestPersistRoundtripKeepsUsage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "apikeys.json")
	st := New(path)
	full1, _, _ := st.Create("one")
	full2, _, _ := st.Create("two")
	if !st.Verify(full1) {
		t.Fatal("precondition failed")
	}
	// 模拟防抖窗口内重启:New 重新加载,校验与用量计数不丢。
	st2 := New(path)
	if !st2.Verify(full1) {
		t.Fatal("重启后密钥丢失")
	}
	if st2.Verify(full2 + "x") {
		t.Fatal("重启后错误密钥通过")
	}
	rows := st2.List()
	if len(rows) != 2 || rows[0].UsedCount == 0 || rows[0].LastUsed.IsZero() {
		t.Fatalf("last_used/used_count 未持久化: %+v", rows)
	}
}

func TestLimitAndNameValidation(t *testing.T) {
	st := New("")
	for i := 0; i < MaxKeys; i++ {
		if _, _, err := st.Create("k" + string(rune('a'+i%26)) + itoa(i)); err != nil {
			t.Fatalf("create #%d: %v", i, err)
		}
	}
	if _, _, err := st.Create("over"); err != ErrLimit {
		t.Fatalf("超限错误 = %v, want ErrLimit", err)
	}
	if _, _, err := st.Create("   "); err != ErrBadName {
		t.Fatalf("空名 = %v, want ErrBadName", err)
	}
	if _, _, err := st.Create(strings.Repeat("长", MaxNameLen+1)); err != ErrBadName {
		t.Fatalf("超长名 = %v, want ErrBadName", err)
	}
}

func TestListNeverExposesSecrets(t *testing.T) {
	st := New("")
	full, _, _ := st.Create("secret-check")
	for _, row := range st.List() {
		if strings.Contains(row.Prefix, full[12:]) {
			t.Fatal("列表暴露了前缀之外的密钥内容")
		}
	}
	// 全钥只出现一次的契约由 create 响应承担:这里确认 List 不含 hash 字段
	// (Public 结构体本就没有 Hash/全钥字段,编译期即保证)。
	_ = full
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	digits := ""
	for i > 0 {
		digits = string(rune('0'+i%10)) + digits
		i /= 10
	}
	return digits
}

// 隐藏名单:设置/恢复/持久化往返;旧版裸数组文件自动迁移。
func TestHiddenNamesRoundtripAndLegacyCompat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "apikeys.json")
	// 先写一个旧版裸数组文件
	legacy := `[{"id":"x","name":"old","hash":"aa","prefix":"sp-aa","created_at":"2026-01-01T00:00:00Z"}]`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	st := New(path)
	rows := st.List()
	if len(rows) != 1 || rows[0].Name != "old" {
		t.Fatalf("旧格式迁移失败: %+v", rows)
	}
	st.SetHidden("probe-agent", true)
	st.SetHidden("", true) // 管理密钥组
	st.SetHidden("keep", false)
	if got := st.HiddenNames(); len(got) != 2 || got[0] != "" || got[1] != "probe-agent" {
		t.Fatalf("hidden = %v", got)
	}
	// 重启后保留(持久化的是新格式)
	st2 := New(path)
	if got := st2.HiddenNames(); len(got) != 2 {
		t.Fatalf("重启后 hidden = %v", got)
	}
	if len(st2.List()) != 1 {
		t.Fatal("重启后密钥行丢失")
	}
	st2.SetHidden("probe-agent", false)
	if got := st2.HiddenNames(); len(got) != 1 || got[0] != "" {
		t.Fatalf("恢复后 hidden = %v", got)
	}
}
