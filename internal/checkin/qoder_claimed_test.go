package checkin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RisingInsight/startport/internal/account"
)

func newQoderServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// 真实上游形状：data.campaigns 里 CLAIM_BENEFIT 已 CLAIMED，benefit.amount=100。
// 这条必须是 already + 明确的「今日已领取」，不能写成像失败的文案。
func TestQoderClaimedCampaignMessage(t *testing.T) {
	srv := newQoderServer(t, `{"data":{"uid":"u","campaigns":[
		{"campaignId":"c1","actionType":"VIEW_DETAILS","claimStatus":"CLAIMABLE"},
		{"campaignId":"c2","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMED",
		 "benefit":{"kind":"CREDITS","amount":100,"validity":{"mode":"RELATIVE_DAYS","days":30}}}
	]}}`)
	c := newQoderCheckerAt(srv.URL)
	res, err := c.CheckIn(context.Background(), newQoderAccount())
	if err != nil {
		t.Fatalf("CheckIn: %v", err)
	}
	if res.Status != StatusAlready {
		t.Fatalf("status = %s, want already", res.Status)
	}
	if !strings.Contains(res.Reward, "100 credits") || !strings.Contains(res.Reward, "30 天") {
		t.Errorf("reward = %q，应带 100 credits / 30 天", res.Reward)
	}
	if !strings.Contains(res.Message, "今日已领取") || strings.Contains(res.Message, "失败") {
		t.Errorf("message = %q，应为明确的今日已领取", res.Message)
	}
}

// 调度器落账：already 的说明进 Message，不写 Error（台账不再像失败）。
type alreadyChecker struct{}

func (alreadyChecker) Platform() string { return account.PlatformQoder }
func (alreadyChecker) CheckIn(context.Context, *account.Account) (Result, error) {
	return Result{Status: StatusAlready, Reward: "100 credits", Message: "今日已领取 100 credits，无需重复签到"}, nil
}

func TestSchedulerAlreadyStoresMessageNotError(t *testing.T) {
	store, _ := account.Load(t.TempDir() + "/accounts.json")
	store.SetLogf(func(string, ...any) {})
	defer store.Close()
	a, err := store.Add(account.PlatformQoder, qoderCred(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	Register(alreadyChecker{})
	defer Register(newQoderChecker())
	s := NewScheduler(SchedulerConfig{Store: store, Logf: func(string, ...any) {}})
	defer s.Close()
	res, err := s.CheckinAccount(context.Background(), a.ID)
	if err != nil || res.Status != StatusAlready {
		t.Fatalf("CheckinAccount: %+v %v", res, err)
	}
	got := store.Get(a.ID)
	if len(got.Checkin) == 0 {
		t.Fatal("签到历史为空")
	}
	rec := got.Checkin[0]
	if rec.Error != "" {
		t.Errorf("already 不应写 Error: %+v", rec)
	}
	if !strings.Contains(rec.Message, "今日已领取") {
		t.Errorf("Message = %q", rec.Message)
	}
}
