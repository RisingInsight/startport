package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/RisingInsight/startport/internal/account"
)

// TestAcctManualRefresh 手动刷新：回写 accounts.json → 立即投影 managed 渠道，
// 面板视图同时下发 refresh_supported。
func TestAcctManualRefresh(t *testing.T) {
	e := newAcctEnv(t)
	e.pan.cfg.RefreshAccount = func(_ context.Context, a *account.Account) (map[string]any, error) {
		m, err := account.DecodeBlob(a.Cred)
		if err != nil {
			return nil, err
		}
		m["access_token"] = "dt-NEW"
		return m, nil
	}

	id, rec := e.add("qoder", `{"access_token":"dt-old","user_id":"u-1","refresh_token":"rt|u-1|m-1"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add status = %d: %s", rec.Code, rec.Body.String())
	}

	// 视图：可刷新。
	rec = e.do("GET", "/startport/api/accounts", "")
	var list struct {
		Accounts []struct {
			ID               string `json:"id"`
			RefreshSupported bool   `json:"refresh_supported"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range list.Accounts {
		if a.ID == id {
			found = true
			if !a.RefreshSupported {
				t.Error("有 refresh_token 的 Qoder 账号应下发 refresh_supported=true")
			}
		}
	}
	if !found {
		t.Fatal("账号未出现在列表")
	}

	before := e.saves
	rec = e.do("POST", "/startport/api/accounts/"+id+"/refresh", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh status = %d: %s", rec.Code, rec.Body.String())
	}
	if e.saves != before+1 {
		t.Errorf("投影次数 = %d, want %d（刷新后必须立即重投影）", e.saves, before+1)
	}

	// accounts.json（Store）已更新。
	got := e.store.Get(id)
	var blob map[string]any
	if err := json.Unmarshal(got.Cred, &blob); err != nil {
		t.Fatal(err)
	}
	if blob["access_token"] != "dt-NEW" {
		t.Errorf("Store access_token = %v, want dt-NEW", blob["access_token"])
	}

	// managed 渠道凭证已换成新 token（Qoder 投影为适配器 JSON）。
	projected := false
	for _, def := range e.defs {
		if def.Name != account.PlatformQoder || len(def.Credentials) == 0 {
			continue
		}
		var ident map[string]any
		if err := json.Unmarshal([]byte(def.Credentials[0].Secret), &ident); err != nil {
			t.Fatalf("投影凭证不是 JSON: %v", err)
		}
		if ident["accessToken"] != "dt-NEW" {
			t.Errorf("投影 accessToken = %v, want dt-NEW", ident["accessToken"])
		}
		projected = true
	}
	if !projected {
		t.Error("未找到 Qoder 托管渠道投影")
	}
}

// TestAcctManualRefreshUnavailable 无 refresh_token → 400，且视图不显示刷新按钮。
func TestAcctManualRefreshUnavailable(t *testing.T) {
	e := newAcctEnv(t)
	id, rec := e.add("qoder", `{"access_token":"dt-1","user_id":"u-1"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add status = %d: %s", rec.Code, rec.Body.String())
	}
	rec = e.do("POST", "/startport/api/accounts/"+id+"/refresh", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("refresh status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	rec = e.do("GET", "/startport/api/accounts", "")
	if body := rec.Body.String(); strings.Contains(body, `"refresh_supported":true`) {
		t.Errorf("无 refresh_token 不应下发 refresh_supported=true: %s", body)
	}
	rec = e.do("POST", "/startport/api/accounts/no-such-id/refresh", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("未知账号 status = %d, want 404", rec.Code)
	}
}
