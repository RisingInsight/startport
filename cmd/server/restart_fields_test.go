package main

import (
	"reflect"
	"testing"
)

// TestRestartRequiredFieldsChangedOnlyReportsTouched 面板保存后提示的「需重启」
// 清单必须只包含本次真正改动的字段：此前恒返回 14 项完整清单，用户改一个能热
// 生效的字段（如签到时段）也会被告知要重启，误导性极强。
func TestRestartRequiredFieldsChangedOnlyReportsTouched(t *testing.T) {
	prev, err := ParseConfig([]byte(`{"listen":"127.0.0.1:7863","schedule":{"checkin_hours":[9,21]}}`))
	if err != nil {
		t.Fatalf("parse prev: %v", err)
	}
	// 只改热生效字段：签到时段（scheduler.Reconfigure 热改）+ 池并发。
	next, err := ParseConfig([]byte(`{"listen":"127.0.0.1:7863","pool":{"max_in_flight":9},"schedule":{"checkin_hours":[8,20]}}`))
	if err != nil {
		t.Fatalf("parse next: %v", err)
	}
	if got := restartRequiredFieldsChanged(prev, next); len(got) != 0 {
		t.Errorf("hot-only change must not require restart, got %v", got)
	}

	// 改监听地址：只报 listen 一项。
	next2, err := ParseConfig([]byte(`{"listen":"127.0.0.1:9000"}`))
	if err != nil {
		t.Fatalf("parse next2: %v", err)
	}
	if got := restartRequiredFieldsChanged(prev, next2); !reflect.DeepEqual(got, []string{"listen"}) {
		t.Errorf("got %v want [listen]", got)
	}

	// 改多个需重启字段：按固定顺序报出，不夹带未改项。
	// 注意 header/idle 在 normalize() 里由 timeout 派生（未显式设置时跟随），
	// 所以只改 timeout_seconds 也会如实带上 header_timeout_seconds——它确实变了。
	next3, err := ParseConfig([]byte(`{"listen":"127.0.0.1:9000","upstream":{"timeout_seconds":60},"logging":{"request_retention_days":3}}`))
	if err != nil {
		t.Fatalf("parse next3: %v", err)
	}
	want := []string{"listen", "upstream.timeout_seconds", "upstream.header_timeout_seconds", "logging.request_retention_days"}
	if got := restartRequiredFieldsChanged(prev, next3); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}

	// 旧配置读不到（prev=nil）→ 退回完整清单，保证不漏提示。
	if got := restartRequiredFieldsChanged(nil, next); len(got) == 0 {
		t.Error("nil prev must fall back to the full restart list")
	}
}
