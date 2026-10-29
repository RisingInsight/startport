package upstream

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/RisingInsight/startport/internal/auth"
)

// globalChatAuth 构造一个 global realm 账号（Domain 触发 BackfillRealm）。
func globalChatAuth() *auth.Auth {
	a := &auth.Auth{UID: "g-uid", AccessToken: "at", Domain: "www.workbuddy.ai"}
	a.BackfillRealm()
	return a
}

// TestChatPathsRealmSplit 路径候选按 realm 分流：global 首选 /console（/v2 对 global
// 已被渠道校验关闸，2026-10-29 实测），cn 保持 /v2 单路径（零回归）。
func TestChatPathsRealmSplit(t *testing.T) {
	c := New()
	c.GlobalEnabled = true

	got := c.chatPaths(globalChatAuth())
	want := []string{consoleChatPath, chatCompletionsPath}
	if len(got) != len(want) {
		t.Fatalf("global chatPaths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("global chatPaths = %v, want %v", got, want)
		}
	}

	cn := &auth.Auth{UID: "cn-uid", Domain: "copilot.tencent.com"}
	cn.BackfillRealm()
	if got := c.chatPaths(cn); len(got) != 1 || got[0] != chatCompletionsPath {
		t.Fatalf("cn chatPaths = %v, want [%s]", got, chatCompletionsPath)
	}

	// global 路由关闭时按 CN 处理（双保险：路径与 base 同时回落）。
	c.GlobalEnabled = false
	if got := c.chatPaths(globalChatAuth()); len(got) != 1 || got[0] != chatCompletionsPath {
		t.Fatalf("global disabled chatPaths = %v, want [%s]", got, chatCompletionsPath)
	}
}

// TestGlobalChatPrefersConsolePath 首选 /console 命中即返回，绝不回落到 /v2。
// 回归锚点：修复前 global 恒打 /v2，被 11128 渠道校验拒死。
func TestGlobalChatPrefersConsolePath(t *testing.T) {
	var hits []string
	c := &Client{
		GlobalEnabled:  true,
		ChatBaseGlobal: "https://global.example",
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			hits = append(hits, r.URL.Path)
			return jsonResp(200, "data: {}\n\n"), nil
		})},
	}

	rc, status, _, err := c.ChatStream(globalChatAuth(), []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("status=%d err=%v", status, err)
	}
	io.Copy(io.Discard, rc)
	rc.Close()

	if len(hits) != 1 || hits[0] != consoleChatPath {
		t.Fatalf("hit paths = %v, want exactly [%s]", hits, consoleChatPath)
	}
}

// TestGlobalChatFallsBackWhenConsoleRejected /console 被 11128 拒 → 自动换 /v2。
// 这是「上游把闸门挪回去」时的自愈路径，避免又需要改代码。
func TestGlobalChatFallsBackWhenConsoleRejected(t *testing.T) {
	var hits []string
	c := &Client{
		GlobalEnabled:  true,
		ChatBaseGlobal: "https://global.example",
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			hits = append(hits, r.URL.Path)
			if r.URL.Path == consoleChatPath {
				return jsonResp(400, `{"code":11128,"msg":"request illegal"}`), nil
			}
			return jsonResp(200, "data: {}\n\n"), nil
		})},
	}

	rc, status, _, err := c.ChatStream(globalChatAuth(), []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("fallback 后应成功：status=%d err=%v", status, err)
	}
	io.Copy(io.Discard, rc)
	rc.Close()

	want := []string{consoleChatPath, chatCompletionsPath}
	if len(hits) != 2 || hits[0] != want[0] || hits[1] != want[1] {
		t.Fatalf("hit paths = %v, want %v", hits, want)
	}
}

// TestGlobalChatAllPathsRejectedIsChannelReject 两条路径都被 11128 拒 → 返回
// ErrChannelReject（请求级），**不是** ErrAccountFault（账号级）。
// 这条断言是本次事故的直接防线：账号级分类会触发 Pool.Disable 永久禁用。
func TestGlobalChatAllPathsRejectedIsChannelReject(t *testing.T) {
	var hits int
	c := &Client{
		GlobalEnabled:  true,
		ChatBaseGlobal: "https://global.example",
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			hits++
			return jsonResp(400, `{"code":11128,"msg":"request illegal"}`), nil
		})},
	}

	_, status, _, err := c.ChatStream(globalChatAuth(), []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`), "", ChatMeta{})
	if hits != 2 {
		t.Fatalf("应尝试两条候选路径，实际 %d 次", hits)
	}
	if status != 400 {
		t.Fatalf("status = %d, want 400", status)
	}
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v, want *upstream.Error", err)
	}
	if ue.Kind != ErrChannelReject {
		t.Fatalf("kind = %s, want %s（账号级分类会永久禁用账号）", ue.Kind, ErrChannelReject)
	}
}

// TestGlobalChatDoesNotFallbackOnAccountFault 账号级错误不换路径：换路径救不了
// 账号状态，只会把同一个坏号打两遍、并污染「一次请求一次上游调用」的记账口径。
func TestGlobalChatDoesNotFallbackOnAccountFault(t *testing.T) {
	var hits int
	c := &Client{
		GlobalEnabled:  true,
		ChatBaseGlobal: "https://global.example",
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			hits++
			return jsonResp(403, `{"code":11140,"msg":"request illegal"}`), nil
		})},
	}

	_, status, _, err := c.ChatStream(globalChatAuth(), []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`), "", ChatMeta{})
	if hits != 1 {
		t.Fatalf("账号级错误不应换路径，实际请求 %d 次", hits)
	}
	if status != 403 {
		t.Fatalf("status = %d, want 403", status)
	}
	var ue *Error
	if !errors.As(err, &ue) || ue.Kind != ErrAccountFault {
		t.Fatalf("err = %v, want ErrAccountFault", err)
	}
}

// TestEnsureConsoleSystemAppliesForGlobal 换回 /console 后，首条非 system 会被
// 前置兜底 system —— /console 域缺它会直接 11128 "first message is not system prompt"。
func TestEnsureConsoleSystemAppliesForGlobal(t *testing.T) {
	var body string
	c := &Client{
		GlobalEnabled:  true,
		ChatBaseGlobal: "https://global.example",
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			b, _ := io.ReadAll(r.Body)
			body = string(b)
			return jsonResp(200, "data: {}\n\n"), nil
		})},
	}

	rc, status, _, err := c.ChatStream(globalChatAuth(), []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("status=%d err=%v", status, err)
	}
	io.Copy(io.Discard, rc)
	rc.Close()

	if !strings.Contains(body, `"role":"system"`) {
		t.Fatalf("global 出站 body 缺前置 system：%s", body)
	}
}
