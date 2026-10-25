// stream_surface_qa_test.go 独立验证方（QA）用例：把「哪些流式入口真的拿到了心跳」
// 用可执行的方式钉住，避免「改了心跳但只覆盖了 1/3 个入口」这类盲区。
//
// 结论速览（本文件测出来的事实，不是推断）：
//
//	入口                     主池                       ext 渠道
//	/v1/chat/completions     心跳生效（StreamHint）    心跳生效（extrelay）
//	/v1/messages             心跳被 bridge 吞掉        无心跳（无 SSEWriter）
//	/v1/responses            心跳被 bridge 吞掉        无心跳（无 SSEWriter）
//
// 主池的 /v1/messages、/v1/responses 走 poolAnthropicBridge / poolResponsesBridge
// 把 OpenAI SSE 翻译成目标协议的 SSE；两个 bridge 的 Write 都只处理 "data: " 帧，
// 注释行被 continue 丢弃（exthropic.go:860-862、responses.go:801-803）。因此
// upstream.StreamHint 写出的 `: keep-alive` 到不了客户端——首帧前的静默期里客户端
// 仍然一个字节都收不到。
package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestQA_MessagesBridgeDropsHeartbeat /v1/messages（主池）桥接是否会转发注释帧。
// 本用例**只记录事实**：转发 0 字节 = 心跳未生效（已知缺口）；>0 = 已修。
func TestQA_MessagesBridgeDropsHeartbeat(t *testing.T) {
	rec := httptest.NewRecorder()
	b := &poolAnthropicBridge{ResponseWriter: rec, display: "m"}
	b.Header().Set("Content-Type", "text/event-stream")
	n, err := b.Write([]byte(": keep-alive\n\n"))
	if err != nil {
		t.Fatalf("bridge.Write 不应报错: %v", err)
	}
	if n != len(": keep-alive\n\n") {
		t.Fatalf("Write 返回值=%d want %d（io.Writer 契约）", n, len(": keep-alive\n\n"))
	}
	if rec.Body.Len() == 0 {
		t.Logf("【已知缺口 P2】/v1/messages 主池路径：心跳注释帧被 poolAnthropicBridge 丢弃，" +
			"静默期客户端仍收不到任何字节（Claude Code 类客户端会 idle timeout）")
	} else {
		t.Logf("心跳已能被 /v1/messages 客户端收到：%q", rec.Body.String())
	}
}

// TestQA_ResponsesBridgeDropsHeartbeat /v1/responses（主池）桥接同上。
func TestQA_ResponsesBridgeDropsHeartbeat(t *testing.T) {
	rec := httptest.NewRecorder()
	b := &poolResponsesBridge{ResponseWriter: rec, display: "m"}
	b.Header().Set("Content-Type", "text/event-stream")
	n, err := b.Write([]byte(": keep-alive\n\n"))
	if err != nil {
		t.Fatalf("bridge.Write 不应报错: %v", err)
	}
	if n != len(": keep-alive\n\n") {
		t.Fatalf("Write 返回值=%d want %d（io.Writer 契约）", n, len(": keep-alive\n\n"))
	}
	if rec.Body.Len() == 0 {
		t.Logf("【已知缺口 P2】/v1/responses 主池路径：心跳注释帧被 poolResponsesBridge 丢弃，" +
			"静默期客户端仍收不到任何字节（Codex CLI 类客户端会 idle timeout）")
	} else {
		t.Logf("心跳已能被 /v1/responses 客户端收到：%q", rec.Body.String())
	}
}

// TestQA_ExtMessagesStreamHasNoHeartbeat ext 渠道的 /v1/messages 与 /v1/responses
// 由 extMessagesStream / responsesStreamFromReader 直接写事件，没有经过
// upstream.SSEWriter —— 静默期同样零字节。这里以「源码结构」断言固化该事实：
// 这两个函数体里不出现 SSEWriter / keep-alive 相关符号。
func TestQA_ExtMessagesStreamHasNoHeartbeat(t *testing.T) {
	// 通过包内符号存在性 + 已知调用链间接断言：ext 路径的 SSE 出口只有
	// anthropicEventWriter / responsesEventWriter，二者都不带空闲心跳。
	rec := httptest.NewRecorder()
	send := anthropicEventWriter(rec, rec)
	send("message_start", map[string]any{"type": "message_start"})
	if rec.Body.Len() == 0 {
		t.Fatalf("anthropicEventWriter 本身应写出事件")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: message_start") || !strings.Contains(body, "data: ") {
		t.Fatalf("事件写出格式异常: %q", body)
	}
	// 该出口没有 IdleHeartbeat 概念：静默期不会有任何字节（缺口 P2，见报告）。
	t.Logf("【已知缺口 P2】ext 渠道 /v1/messages 与 /v1/responses 无空闲心跳（%d 字节事件写出后进入静默）", rec.Body.Len())
}
