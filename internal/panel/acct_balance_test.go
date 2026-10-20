package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/RisingInsight/startport/internal/account"
)

// TestAcctPlatformBalanceEndpoint 平台余额查询：写回 balance 快照并在列表回显；
// 不支持余额的平台返回 400，未知账号 404。
func TestAcctPlatformBalanceEndpoint(t *testing.T) {
	e := newAcctEnv(t)
	e.pan.cfg.BalanceAccount = func(_ context.Context, _ *account.Account) (*account.BalanceSnapshot, error) {
		return &account.BalanceSnapshot{
			Available: 325, Unit: "credits",
			Rows:      []account.BalanceRow{{Name: "个人套餐", Available: 225, Unit: "credits"}},
			FetchedAt: time.Now(),
		}, nil
	}
	id, rec := e.add("qoder", `{"access_token":"dt-1","user_id":"u-1"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add status = %d: %s", rec.Code, rec.Body.String())
	}
	rec = e.do("POST", "/startport/api/accounts/"+id+"/platform_balance", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("balance status = %d: %s", rec.Code, rec.Body.String())
	}
	got := e.store.Get(id)
	if got.Balance == nil || got.Balance.Available != 325 || got.Balance.Unit != "credits" {
		t.Errorf("Store balance = %+v", got.Balance)
	}

	rec = e.do("GET", "/startport/api/accounts", "")
	var list struct {
		Accounts []struct {
			ID      string `json:"id"`
			Balance *struct {
				Available float64 `json:"available"`
				Unit      string  `json:"unit"`
			} `json:"balance"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	for _, a := range list.Accounts {
		if a.ID == id && (a.Balance == nil || a.Balance.Available != 325) {
			t.Errorf("列表余额未回显: %+v", a.Balance)
		}
	}

	traeID, _ := e.add("trae", `{"accessToken":"jwt","uid":"u","machineId":"m","deviceId":"d"}`)
	rec = e.do("POST", "/startport/api/accounts/"+traeID+"/platform_balance", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("不支持余额的平台 status = %d, want 400", rec.Code)
	}
	rec = e.do("POST", "/startport/api/accounts/no-such-id/platform_balance", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("未知账号 status = %d, want 404", rec.Code)
	}
}
