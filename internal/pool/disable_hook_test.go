package pool

import (
	"testing"

	"github.com/RisingInsight/startport/internal/auth"
)

// OnDisabled 钩子：首次判死通知一次；重复 Disable 不再刷；复活后再判死重通知。
// 12153 达阈路径（NoteSessionDead）与手工 Disable 走同一钩子。
func TestOnDisabledHook(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "一号"})
	var events []string
	p.OnDisabled = func(uid, nickname, reason string) {
		events = append(events, uid+"|"+nickname+"|"+reason)
	}

	p.Disable("u1", "第一次")
	p.Disable("u1", "重复禁用不刷通知") // 已禁用：钩子不再触发
	if len(events) != 1 || events[0] != "u1|一号|第一次" {
		t.Fatalf("events = %v, want exactly one disable notice", events)
	}

	p.ReviveDisabled("u1")
	p.Disable("u1", "复活后再死")
	if len(events) != 2 || events[1] != "u1|一号|复活后再死" {
		t.Fatalf("events = %v, want second notice after revive", events)
	}

	// 12153 达阈路径同样走钩子（阈值 3）。
	p.ReviveDisabled("u1")
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	if len(events) != 2 {
		t.Fatalf("events = %v, want no notice below threshold", events)
	}
	if !p.NoteSessionDead("u1") {
		t.Fatal("3rd session dead should disable")
	}
	if len(events) != 3 {
		t.Fatalf("events = %v, want notice on threshold disable", events)
	}
}
