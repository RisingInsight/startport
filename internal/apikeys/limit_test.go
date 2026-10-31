package apikeys

import (
	"errors"
	"testing"
)

// SetLimit：合法周期落行、非法周期拒绝、limit<=0 清空 period、清除后恢复不限。
func TestSetLimitRoundTrip(t *testing.T) {
	st := New("")
	full, k, err := st.Create("预算密钥")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if full == "" || k.ID == "" {
		t.Fatalf("create returned empty key")
	}

	if _, err := st.SetLimit(k.ID, 1000, "hourly"); !errors.Is(err, ErrBadPeriod) {
		t.Fatalf("bad period err = %v, want ErrBadPeriod", err)
	}

	pub, err := st.SetLimit(k.ID, 1000, "day")
	if err != nil {
		t.Fatalf("set limit: %v", err)
	}
	if pub.LimitTokens != 1000 || pub.Period != "day" {
		t.Fatalf("public = %+v, want limit 1000/day", pub)
	}
	rows := st.List()
	if len(rows) != 1 || rows[0].LimitTokens != 1000 || rows[0].Period != "day" {
		t.Fatalf("list = %+v, want limit persisted", rows)
	}

	// 清除：limit 0 连带清空 period。
	pub, err = st.SetLimit(k.ID, 0, "day")
	if err != nil {
		t.Fatalf("clear limit: %v", err)
	}
	if pub.LimitTokens != 0 || pub.Period != "" {
		t.Fatalf("cleared public = %+v, want zeroed", pub)
	}

	// 负数视同清除。
	if _, err := st.SetLimit(k.ID, -5, "week"); err != nil {
		t.Fatalf("negative limit: %v", err)
	}
	if rows = st.List(); rows[0].LimitTokens != 0 || rows[0].Period != "" {
		t.Fatalf("negative clear = %+v, want zeroed", rows[0])
	}
}
