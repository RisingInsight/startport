package checkin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RisingInsight/startport/internal/account"
)

// kukuTestServer 起一个假上游：积分首页 + VIP 余量，按用例返回。
func kukuTestServer(t *testing.T, points, vip string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Cookie"); !strings.Contains(got, "BDUSS=") {
			t.Errorf("请求未带 Cookie: %q", got)
		}
		switch r.URL.Path {
		case kukuPointsPath:
			_, _ = w.Write([]byte(points))
		case kukuVIPPath:
			_, _ = w.Write([]byte(vip))
		default:
			t.Errorf("意外路径 %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
}

// TestKukuBalance 常规解析：积分首页给余额，VIP 接口补充套餐与到期时间。
func TestKukuBalance(t *testing.T) {
	srv := kukuTestServer(t,
		`{"data":{"totalPoint":1200,"usedPoint":300,"activities":[]},"errno":0,"show_msg":""}`,
		`{"status":{"code":0,"msg":"ok"},"data":{"isVip":true,"isExpire":false,"vipType":"超级会员","vipEndTime":1793285650000}}`)
	defer srv.Close()

	snap, err := queryKukuBalanceAt(context.Background(), srv.URL, "BDUSS=abc; STOKEN=def")
	if err != nil {
		t.Fatalf("queryKukuBalanceAt: %v", err)
	}
	if snap.Available != 1200 {
		t.Errorf("Available = %v, want 1200（totalPoint 是第一顺位）", snap.Available)
	}
	if snap.Unit != "积分" {
		t.Errorf("Unit = %q", snap.Unit)
	}
	if snap.PlanName != "VIP（超级会员）" {
		t.Errorf("PlanName = %q", snap.PlanName)
	}
	if snap.ExpiresAt.IsZero() {
		t.Error("ExpiresAt 应来自 vipEndTime")
	}
	if len(snap.Rows) != 2 {
		t.Errorf("Rows = %+v, want 2 行（totalPoint / usedPoint）", snap.Rows)
	}
}

// TestKukuBalanceAuthExpired errno=-6 必须翻译成 ErrAuthExpired，
// 让面板能提示「重新粘贴 Cookie」而不是笼统失败。
func TestKukuBalanceAuthExpired(t *testing.T) {
	srv := kukuTestServer(t,
		`{"data":{},"errno":-6,"show_msg":"未登录"}`,
		`{"status":{"code":200001,"msg":"user not login"}}`)
	defer srv.Close()

	_, err := queryKukuBalanceAt(context.Background(), srv.URL, "BDUSS=abc; STOKEN=def")
	if !errors.Is(err, ErrAuthExpired) {
		t.Fatalf("err = %v, want ErrAuthExpired", err)
	}
}

// TestKukuBalanceHTTP401 HTTP 401/403 同样按登录态失效处理。
func TestKukuBalanceHTTP401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := queryKukuBalanceAt(context.Background(), srv.URL, "BDUSS=abc")
	if !errors.Is(err, ErrAuthExpired) {
		t.Fatalf("err = %v, want ErrAuthExpired", err)
	}
}

// TestKukuBalanceUnknownShape 契约未验证时的核心防线：认不出额度字段就**报错**，
// 绝不返回 0 或编造数字；错误里必须带实际 key 清单，方便一次修准。
func TestKukuBalanceUnknownShape(t *testing.T) {
	srv := kukuTestServer(t,
		`{"data":{"mysteryA":1,"mysteryB":2},"errno":0}`,
		`{"status":{"code":200001,"msg":"user not login"}}`)
	defer srv.Close()

	_, err := queryKukuBalanceAt(context.Background(), srv.URL, "BDUSS=abc")
	if err == nil {
		t.Fatal("认不出额度字段时必须报错，不能返回空快照")
	}
	if !strings.Contains(err.Error(), "mysteryA") || !strings.Contains(err.Error(), "mysteryB") {
		t.Errorf("错误信息应带实际键清单，got: %v", err)
	}
}

// TestKukuBalanceVIPFailureIsolated VIP 接口失败不影响积分结果（另一套信封）。
func TestKukuBalanceVIPFailureIsolated(t *testing.T) {
	srv := kukuTestServer(t,
		`{"data":{"point":88},"errno":0}`,
		`{"status":{"code":200001,"msg":"user not login"}}`)
	defer srv.Close()

	snap, err := queryKukuBalanceAt(context.Background(), srv.URL, "BDUSS=abc")
	if err != nil {
		t.Fatalf("VIP 失败不应连坐积分: %v", err)
	}
	if snap.Available != 88 {
		t.Errorf("Available = %v, want 88", snap.Available)
	}
	if snap.PlanName != "" {
		t.Errorf("VIP 失败时 PlanName 应为空, got %q", snap.PlanName)
	}
}

// TestKukuAccountOnlySpec 平台注册表事实：账号型、有余额、无签到。
// 这条断言防止后续误把 AccountOnly 翻成 false 导致 acctchan 又去投影渠道。
func TestKukuAccountOnlySpec(t *testing.T) {
	s, ok := account.SpecOf(account.PlatformKuku)
	if !ok {
		t.Fatal("注册表缺 kuku 平台")
	}
	if !s.AccountOnly {
		t.Error("kuku 必须标记 AccountOnly（无对话 API，不能投影成渠道）")
	}
	if !s.Balance {
		t.Error("kuku 应声明 Balance 能力")
	}
	if s.Checkin {
		t.Error("kuku 签到契约未验证，Checkin 应保持 false")
	}
	if !BalanceSupported(account.PlatformKuku) {
		t.Error("BalanceSupported(kuku) 应为 true")
	}
}

// TestKukuBalanceSnapshotJSON 快照能被面板序列化（rows/unit 都在）。
func TestKukuBalanceSnapshotJSON(t *testing.T) {
	srv := kukuTestServer(t,
		`{"data":{"totalPoint":5},"errno":0}`,
		`{"status":{"code":0,"data":{}}}`)
	defer srv.Close()

	snap, err := queryKukuBalanceAt(context.Background(), srv.URL, "BDUSS=abc")
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if !strings.Contains(string(out), `"available":5`) {
		t.Errorf("JSON = %s", out)
	}
}
