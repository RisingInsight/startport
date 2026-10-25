// sse_heartbeat_surface_test.go 客户端保活心跳在「另外两个协议面」的回归用例。
//
// 背景（QA 独立验证发现的两个覆盖缺口）：/v1/chat/completions 已有心跳，但
//   - 主池 /v1/messages、/v1/responses 走翻译桥（poolAnthropicBridge /
//     poolResponsesBridge），桥只处理 "data: " 帧，把网关写的 `: keep-alive`
//     注释帧静默吞掉（StreamHint 侧 Write 成功、客户端 0 字节）；
//   - ext 渠道的这两个面根本不经过 SSEWriter，静默期同样零字节。
//
// Claude Code 走 /v1/messages、Codex CLI 走 /v1/responses——这两个入口的客户端
// 同样有 300s 级空闲超时，所以缺口必须补上并钉住。
package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RisingInsight/startport/internal/extchan"
)

// heartbeatLine 心跳注释帧的逐字节原文（与 upstream.sseHeartbeatFrame 同值；
// 这里用字面量是有意的：断言的是「客户端实际收到的字节」，不该跟着实现改名漂移）。
const heartbeatLine = ": keep-alive\n\n"

// shortenHeartbeat 把包级心跳阈值临时降到毫秒级（测试用），返回时自动还原。
func shortenHeartbeat(t *testing.T) {
	t.Helper()
	old := sseHeartbeatInterval
	sseHeartbeatInterval = 20 * time.Millisecond
	t.Cleanup(func() { sseHeartbeatInterval = old })
}

// surfaceStep 一段字节 + 该段之前要静默的时长（模拟上游 prefill / 帧间停顿）。
type surfaceStep struct {
	delay time.Duration
	data  string
}

// surfacePacedReader 按段吐出字节，每段之前先睡 delay；数据吐完返回 io.EOF。
type surfacePacedReader struct {
	steps []surfaceStep
	cur   int
	off   int
	ready bool
}

func (p *surfacePacedReader) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	for p.cur < len(p.steps) {
		st := p.steps[p.cur]
		if !p.ready {
			if st.delay > 0 {
				time.Sleep(st.delay)
			}
			p.ready = true
		}
		n := copy(b, st.data[p.off:])
		p.off += n
		if p.off >= len(st.data) {
			p.cur++
			p.off = 0
			p.ready = false
		}
		if n > 0 {
			return n, nil
		}
	}
	return 0, io.EOF
}

// TestPoolBridgesForwardHeartbeat 主池两条翻译桥必须把 SSE 注释帧原样转发给客户端，
// 同时继续丢弃其他非 data 行（上游 event:/id: 等，翻译语义不变）。
func TestPoolBridgesForwardHeartbeat(t *testing.T) {
	cases := []struct {
		name string
		wrap func(w http.ResponseWriter) io.Writer
	}{
		{"messages", func(w http.ResponseWriter) io.Writer {
			return &poolAnthropicBridge{ResponseWriter: w, display: "m"}
		}},
		{"responses", func(w http.ResponseWriter) io.Writer {
			return &poolResponsesBridge{ResponseWriter: w, display: "m"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			rec.Header().Set("Content-Type", "text/event-stream")
			bw := tc.wrap(rec)

			n, err := io.WriteString(bw, heartbeatLine)
			if err != nil {
				t.Fatalf("桥 Write 注释帧不应报错: %v", err)
			}
			if n != len(heartbeatLine) {
				t.Fatalf("Write 返回值=%d want %d（io.Writer 契约）", n, len(heartbeatLine))
			}
			if got := rec.Body.String(); got != heartbeatLine {
				t.Fatalf("注释帧未转发给客户端（被静默吞掉 = 该入口静默期零字节）: %q", got)
			}

			// 非注释的非 data 行（上游 event:/id:）继续丢弃：翻译语义不变。
			if _, werr := io.WriteString(bw, "event: ping\n\nid: 7\n\n"); werr != nil {
				t.Fatalf("Write 非 data 行: %v", werr)
			}
			if got := rec.Body.String(); got != heartbeatLine {
				t.Fatalf("非注释的非 data 行不应转发: %q", got)
			}
		})
	}
}

// TestExtMessagesStreamEmitsHeartbeat ext 渠道 /v1/messages：上游 200 开流后迟迟不吐
// 首帧时，客户端必须收到 `: keep-alive`（Claude Code 的入口）。
func TestExtMessagesStreamEmitsHeartbeat(t *testing.T) {
	shortenHeartbeat(t)
	frame := `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n\n"
	r := &surfacePacedReader{steps: []surfaceStep{
		{delay: 120 * time.Millisecond, data: frame + "data: [DONE]\n\n"},
	}}
	rec := httptest.NewRecorder()
	h := &Handler{}
	h.extMessagesStream(rec, &extchan.Channel{Name: "ext-test"}, nil, "m", io.NopCloser(r), time.Now(), "k")

	body := rec.Body.String()
	if !strings.Contains(body, heartbeatLine) {
		t.Fatalf("首帧之前没有心跳（ext /v1/messages 静默期仍零字节）: %q", body)
	}
	if !strings.Contains(body, "event: message_start") || !strings.Contains(body, "event: message_stop") {
		t.Fatalf("翻译事件被心跳破坏或缺失: %q", body)
	}
	firstBeat := strings.Index(body, heartbeatLine)
	firstEvent := strings.Index(body, "event: ")
	if firstEvent < 0 || firstBeat < firstEvent {
		t.Fatalf("心跳不该出现在首事件之前（此时已写出 message_start）: %q", body)
	}
}

// TestExtResponsesStreamEmitsHeartbeat ext 渠道 /v1/responses 同上（Codex CLI 入口）。
func TestExtResponsesStreamEmitsHeartbeat(t *testing.T) {
	shortenHeartbeat(t)
	frame := `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n\n"
	r := &surfacePacedReader{steps: []surfaceStep{
		{delay: 120 * time.Millisecond, data: frame + "data: [DONE]\n\n"},
	}}
	rec := httptest.NewRecorder()
	h := &Handler{}
	h.responsesStreamFromReader(rec, nil, "m", io.NopCloser(r), "k",
		&extchan.Channel{Name: "ext-test"}, nil, time.Now())

	body := rec.Body.String()
	if !strings.Contains(body, heartbeatLine) {
		t.Fatalf("首帧之前没有心跳（ext /v1/responses 静默期仍零字节）: %q", body)
	}
	if !strings.Contains(body, "event: response.created") || !strings.Contains(body, "event: response.completed") {
		t.Fatalf("翻译事件被心跳破坏或缺失: %q", body)
	}
}

// TestExtChannelNoteStreamAbortReportsStreak ext 渠道的断流必须能进渠道熔断计数：
// 此前 server 侧只记 outcome，200 之后卡死的渠道永远不进冷却（越用越死）。
func TestExtChannelNoteStreamAbortReportsStreak(t *testing.T) {
	ch := &extchan.Channel{Name: "ext-test"}
	if ch.Cooldown() {
		t.Fatal("初始不应冷却")
	}
	// extchan 内部阈值是 5 连败 → 熔断；前 4 次只累计，第 5 次进入冷却。
	for i := 0; i < 4; i++ {
		ch.NoteStreamAbort("stream aborted mid-stream")
	}
	if ch.Cooldown() {
		t.Fatal("未达阈值不应冷却（避免单次断流把渠道打挂）")
	}
	ch.NoteStreamAbort("stream aborted mid-stream")
	if !ch.Cooldown() {
		t.Fatal("连续 5 次流中断必须触发渠道熔断（否则卡死的渠道会被反复选中）")
	}
}
