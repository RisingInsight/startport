package panel

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/RisingInsight/startport/internal/pool"
)

// TestCodeBuddyAccountView CodeBuddy 池账号并入统一清单：只读行带积分/状态/签到。
func TestCodeBuddyAccountView(t *testing.T) {
	v := codeBuddyAccountView(pool.Status{
		UID: "u-1", Nickname: "主号", Realm: "cn", Credits: 3209,
		CheckinDone: true, InFlight: 2, LastSuccessTime: time.Now().Add(-time.Minute),
	})
	if v.Platform != "codebuddy" || v.Source != "codebuddy" || v.PlatformName != "CodeBuddy 国内版" ||
		v.Label != "主号" || v.Note != "" || v.Balance == nil || v.Balance.Available != 3209 ||
		v.Channel == nil || v.Channel.State != "healthy" || v.Channel.InFlight != 2 ||
		!v.Capabilities.Checkin || v.Checkin.LastStatus != "ok" {
		t.Errorf("CodeBuddy 统一行异常: %+v", v)
	}

	intl := codeBuddyAccountView(pool.Status{UID: "u-intl", Realm: "global", Enterprise: true})
	if intl.PlatformName != "CodeBuddy 国际版" || intl.Note != "企业版" {
		t.Errorf("国际版平台名/备注异常: %+v", intl)
	}

	paused := codeBuddyAccountView(pool.Status{UID: "u-2", Paused: true})
	if paused.Channel == nil || paused.Channel.State != "paused" || paused.HealthReason != "暂停选号" {
		t.Errorf("暂停行异常: %+v", paused)
	}
	disabled := codeBuddyAccountView(pool.Status{UID: "u-3", Disabled: true, DisabledReason: "session dead"})
	if !disabled.Disabled || disabled.Channel.State != "disabled" || disabled.HealthReason != "session dead" {
		t.Errorf("禁用行异常: %+v", disabled)
	}
}

// TestZeroBalanceVisible 0 余额是有效快照（Qoder 国际 Free 账户），不能因 omitempty 退化成「—」。
func TestZeroBalanceVisible(t *testing.T) {
	b, err := json.Marshal(platformAccountView{
		Platform: "qoder-intl", PlatformName: "Qoder 国际",
		Balance: &platformBalanceView{Available: 0, Unit: "credits", PlanName: "Free"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Balance struct {
			Available *float64 `json:"available"`
			Unit      string   `json:"unit"`
			PlanName  string   `json:"plan_name"`
		} `json:"balance"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Balance.Available == nil || *out.Balance.Available != 0 ||
		out.Balance.Unit != "credits" || out.Balance.PlanName != "Free" {
		t.Fatalf("零余额快照被省略: %s", b)
	}
}

// TestAcctUnifiedViewCapabilities 统一账号视图依赖的能力位/平台名/健康原因。
func TestAcctUnifiedViewCapabilities(t *testing.T) {
	e := newAcctEnv(t)
	qoderID, _ := e.add("qoder", `{"access_token":"dt-1","user_id":"u-1","refresh_token":"rt|u-1|m-1"}`)
	loomyID, _ := e.add("loomy", `{"session":"sess-0123456789abcdef"}`)
	e.add("trae", `{"accessToken":"jwt","userName":"u","machineId":"m","deviceId":"d"}`)

	type caps struct {
		Login   bool `json:"login"`
		Checkin bool `json:"checkin"`
		Refresh bool `json:"refresh"`
		Probe   bool `json:"probe"`
		Balance bool `json:"balance"`
	}
	type row struct {
		ID           string `json:"id"`
		Platform     string `json:"platform"`
		PlatformName string `json:"platform_name"`
		HealthReason string `json:"health_reason"`
		Capabilities caps   `json:"capabilities"`
		Channel      *struct {
			State string `json:"state"`
		} `json:"channel"`
	}
	var list struct {
		Accounts []row `json:"accounts"`
	}
	rec := e.do("GET", "/startport/api/accounts", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	byPlatform := map[string]row{}
	for _, a := range list.Accounts {
		byPlatform[a.Platform] = a
	}

	q := byPlatform["qoder"]
	if q.PlatformName == "" || !q.Capabilities.Login || !q.Capabilities.Checkin ||
		!q.Capabilities.Refresh || !q.Capabilities.Probe || !q.Capabilities.Balance {
		t.Errorf("qoder 能力位异常: %+v", q)
	}
	lo := byPlatform["loomy"]
	if lo.PlatformName == "" || lo.Capabilities.Login || !lo.Capabilities.Checkin ||
		lo.Capabilities.Refresh || !lo.Capabilities.Probe || !lo.Capabilities.Balance {
		t.Errorf("loomy 能力位异常: %+v", lo)
	}
	tr := byPlatform["trae"]
	// v1.7.6：TRAE 接入浏览器登录（Login=true）；签到/余额/刷新仍属第二批。
	if tr.PlatformName == "" || !tr.Capabilities.Login || tr.Capabilities.Checkin ||
		tr.Capabilities.Refresh || !tr.Capabilities.Probe || tr.Capabilities.Balance {
		t.Errorf("trae 能力位异常: %+v", tr)
	}

	// 停用 → 统一视图的健康原因必须是「账号已停用」。
	rec = e.do("POST", "/startport/api/accounts/"+loomyID+"/state", `{"disabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable status = %d: %s", rec.Code, rec.Body.String())
	}
	rec = e.do("GET", "/startport/api/accounts", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	for _, a := range list.Accounts {
		if a.ID != loomyID {
			continue
		}
		if a.HealthReason != "账号已停用" {
			t.Errorf("health_reason = %q, want 账号已停用", a.HealthReason)
		}
		if a.Channel == nil || a.Channel.State != "disabled" {
			t.Errorf("channel state = %+v, want disabled", a.Channel)
		}
	}
	_ = qoderID
}
