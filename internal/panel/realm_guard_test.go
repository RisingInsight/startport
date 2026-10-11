package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RisingInsight/startport/internal/auth"
	"github.com/RisingInsight/startport/internal/pool"
)

// newGlobalAuth 构造一个 IsGlobal 的账号：显式 realm=global（与面板登录
// global 用户落盘凭证的形态一致）。default globalEnabled=true，无需开关辅助。
func newGlobalAuth(t *testing.T) *auth.Auth {
	t.Helper()
	a := &auth.Auth{UID: "global-uid", Nickname: "intl-test"}
	if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
		t.Fatalf("BackfillRealmFor(global): %v", err)
	}
	if !a.IsGlobal() {
		t.Fatal("test setup: account is not IsGlobal")
	}
	return a
}

// TestGuardGrowthRealmBlocksGlobalAccount 端到端：国际版账号调用成长任务端点
// 必须 400 + 固定文案，且不得触达上游（测试里 Upstream 为 nil，一旦越门即 panic，
// 等价于「未打上游」断言）。CN 账号不受门控影响（400 之外的路径属于 nil-Upstream
// 越界，不在此断言——只需证明守卫未拦截，与回归前行为一致）。
func TestGuardGrowthRealmBlocksGlobalAccount(t *testing.T) {
	p := New(Config{Version: "test", Pool: pool.New("")})
	p.cfg.Pool.Add(newGlobalAuth(t))

	// guardGrowthRealm 单元级：global 拦截（400 + 文案），cn 放行。
	rec := httptest.NewRecorder()
	if !guardGrowthRealm(rec, newGlobalAuth(t)) {
		t.Fatal("guardGrowthRealm(global) = false, want true (intercepted)")
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("guard status=%d want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "国际版账号无成长任务/签到体系") {
		t.Errorf("guard body=%q want 国际版文案", rec.Body.String())
	}
	recCN := httptest.NewRecorder()
	if guardGrowthRealm(recCN, &auth.Auth{UID: "cn-uid"}) {
		t.Error("guardGrowthRealm(cn) = true, want false (must pass through)")
	}

	// handler 级：真实请求路径经过 accountByUID → 守卫，返回 400 且零上游调用。
	req := httptest.NewRequest("GET", "/startport/api/accounts/global-uid/tasks", nil)
	req.SetPathValue("uid", "global-uid")
	hrec := httptest.NewRecorder()
	p.accountTasks(hrec, req)
	if hrec.Code != http.StatusBadRequest {
		t.Errorf("accountTasks(global) status=%d want 400", hrec.Code)
	}
	if !strings.Contains(hrec.Body.String(), "国际版账号无成长任务/签到体系") {
		t.Errorf("accountTasks(global) body=%q want 国际版文案", hrec.Body.String())
	}
}
