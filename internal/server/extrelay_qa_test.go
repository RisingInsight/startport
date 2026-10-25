// extrelay_qa_test.go 独立验证方（QA）用例：ext 渠道流式透传路径的心跳安全性。
//
// 为什么要单独测这条路径：extrelay.go 的心跳与主链路（upstream.StreamHint）共用
// upstream.SSEWriter，但**写入口径不同**——主链路写整帧 `data: ...\n\n`，ext 渠道
// 写的是「chatStatsReader.Read 返回的一块字节」。若这块字节可能停在 SSE 行中间，
// 心跳就会被插进行内、把 `data: {...}` 打成两半（客户端 JSON 解析失败、数据丢失）。
//
// 本文件复刻 extrelay.go:108-136 的读取-写出形态，断言：
//  1. 透传零改写：剥掉心跳后字节与上游完全一致；
//  2. 心跳只在行首：包括「单行 > 32KB（跨 Write 拆分）」与「上游在行中间静默 20s」
//     这两个最不利组合下也不越界——安全性来自 chatStatsReader 的行缓冲
//     （logging.go:241-255 ReadString('\n')），而不是运气；
//  3. 多次流式请求后无心跳 goroutine 残留。
//
// 端到端证据（真实二进制 + 真实 HTTP 栈，见交付报告）：单行 40KB + 行内静默 20s
// 时，客户端在 15026ms 收到 `: keep-alive`，在 20026ms 收到完整 40163 字节的 data 行
// 且 JSON 可解析，mid_line_heartbeat=0。
package server

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RisingInsight/startport/internal/upstream"
)

// qaRelayWriter 并发安全的 ResponseWriter 替身（心跳 goroutine 与主循环并发写）。
type qaRelayWriter struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	header http.Header
}

func newQARelayWriter() *qaRelayWriter { return &qaRelayWriter{header: http.Header{}} }

func (w *qaRelayWriter) Header() http.Header { return w.header }
func (w *qaRelayWriter) WriteHeader(int)     {}
func (w *qaRelayWriter) Flush()              {}

func (w *qaRelayWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *qaRelayWriter) bytesOut() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.buf.Bytes()...)
}

// qaStage 按「静默 + 字节」逐块吐数据，用于构造「上游在某个位置长时间静默」。
type qaStage struct {
	delay time.Duration
	data  string
}

type qaStagedReader struct {
	stages []qaStage
	idx    int
}

func (r *qaStagedReader) Read(p []byte) (int, error) {
	if r.idx >= len(r.stages) {
		return 0, io.EOF
	}
	s := &r.stages[r.idx]
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	n := copy(p, s.data)
	s.data = s.data[n:]
	if s.data == "" {
		r.idx++
	}
	return n, nil
}

// runExtRelay 复刻 extrelay.go:108-136 的流式透传循环（逐块读出 + 逐块写出），
// 唯一差别是没接真实的 ext 上游。返回值是客户端侧收到的全部字节。
func runExtRelay(t *testing.T, up io.Reader, heartbeat time.Duration) ([]byte, error) {
	t.Helper()
	w := newQARelayWriter()
	out := upstream.NewSSEWriterInterval(w, w, heartbeat)
	defer out.Close()

	stats := newChatStatsReaderSince(up, time.Now())
	buf := make([]byte, 32*1024) // 与 extrelay.go:111 同尺寸
	for {
		if err := out.Err(); err != nil {
			return w.bytesOut(), err
		}
		n, rerr := stats.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return w.bytesOut(), werr
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return w.bytesOut(), nil
			}
			return w.bytesOut(), rerr
		}
	}
}

// qaKeepAliveStats 统计心跳次数与「落在行中间」的越界次数。
func qaKeepAliveStats(out []byte) (total int, midLine []string) {
	s := string(out)
	for pos := 0; ; {
		i := strings.Index(s[pos:], ": keep-alive")
		if i < 0 {
			return total, midLine
		}
		at := pos + i
		total++
		if at != 0 && s[at-1] != '\n' {
			start := strings.LastIndex(s[:at], "\n") + 1
			midLine = append(midLine, s[start:at])
		}
		pos = at + 1
	}
}

const qaExtFrameA = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"A\"}}]}\n\n"
const qaExtFrameB = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"B\"}}]}\n\n"

// TestQA_ExtRelayPassthroughByteIdentical 心跳开启（默认 15s，正常流不触发）时，
// ext 渠道的逐字节透传必须零改写。
func TestQA_ExtRelayPassthroughByteIdentical(t *testing.T) {
	up := qaExtFrameA + qaExtFrameB + "data: [DONE]\n\n"
	got, err := runExtRelay(t, strings.NewReader(up), 30*time.Millisecond)
	if err != nil {
		t.Fatalf("中继出错: %v", err)
	}
	if string(got) != up {
		t.Fatalf("透传字节被改写:\n got=%q\nwant=%q", got, up)
	}
}

// TestQA_ExtRelayHeartbeatAtLineBoundaryDuringStall 上游帧间静默时补心跳，
// 且剥掉心跳后与上游逐字节一致（心跳只是插入，不改动任何数据字节）。
func TestQA_ExtRelayHeartbeatAtLineBoundaryDuringStall(t *testing.T) {
	const interval = 20 * time.Millisecond
	r := &qaStagedReader{stages: []qaStage{
		{data: qaExtFrameA},
		{delay: 120 * time.Millisecond, data: qaExtFrameB},
		{data: "data: [DONE]\n\n"},
	}}
	got, err := runExtRelay(t, r, interval)
	if err != nil {
		t.Fatalf("中继出错: %v", err)
	}
	total, midLine := qaKeepAliveStats(got)
	if total == 0 {
		t.Fatalf("120ms 静默（阈值 20ms）应触发心跳: %q", got)
	}
	if len(midLine) > 0 {
		t.Fatalf("心跳落在行中间（破坏 SSE 分帧）: %q（上下文 %q）", midLine, got)
	}
	stripped := strings.ReplaceAll(string(got), ": keep-alive\n\n", "")
	want := qaExtFrameA + qaExtFrameB + "data: [DONE]\n\n"
	if stripped != want {
		t.Fatalf("剥掉心跳后与上游不一致:\n got=%q\nwant=%q", stripped, want)
	}
}

// TestQA_ExtRelayOversizedLineStall 最不利组合：单行 > 32KB（必然跨 Write 拆分）
// 且上游在该行中间静默 120ms。行缓冲（chatStatsReader）必须保证心跳不越界，
// 否则超大 tool_call 参数帧会被心跳打断成两条非法行。
func TestQA_ExtRelayOversizedLineStall(t *testing.T) {
	const interval = 20 * time.Millisecond
	big := strings.Repeat("x", 40000)
	line := `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"` + big + `"}}]}` + "\n\n"

	r := &qaStagedReader{stages: []qaStage{
		{data: line[:1000]}, // 行内前 1000 字节
		{delay: 120 * time.Millisecond, data: line[1000:]}, // 行内静默后余下
		{data: "data: [DONE]\n\n"},
	}}
	got, err := runExtRelay(t, r, interval)
	if err != nil {
		t.Fatalf("中继出错: %v", err)
	}
	total, midLine := qaKeepAliveStats(got)
	if total == 0 {
		t.Fatalf("行内静默 120ms（阈值 20ms）应触发心跳")
	}
	if len(midLine) > 0 {
		t.Fatalf("超长行被心跳打断（心跳越界 %d 次），前缀=%q", len(midLine), midLine[0][:min(120, len(midLine[0]))])
	}
	stripped := strings.ReplaceAll(string(got), ": keep-alive\n\n", "")
	if stripped != line+"data: [DONE]\n\n" {
		t.Fatalf("超长行的字节透传不一致（len got=%d want=%d）", len(stripped), len(line)+len("data: [DONE]\n\n"))
	}
}

// TestQA_ExtRelayNoGoroutineLeak 连续多次 ext 流式请求后不得残留心跳 goroutine。
func TestQA_ExtRelayNoGoroutineLeak(t *testing.T) {
	run := func() {
		r := &qaStagedReader{stages: []qaStage{
			{data: qaExtFrameA},
			{delay: 25 * time.Millisecond, data: qaExtFrameB},
			{data: "data: [DONE]\n\n"},
		}}
		if _, err := runExtRelay(t, r, 5*time.Millisecond); err != nil {
			t.Fatalf("中继出错: %v", err)
		}
	}
	run()
	time.Sleep(30 * time.Millisecond)
	base := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		run()
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= base {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("20 次 ext 流式后 goroutine 从 %d 涨到 %d：心跳 goroutine 泄漏", base, runtime.NumGoroutine())
}

// TestQA_ExtRelayAbortClassification ext 渠道的上游读中断与「客户端写失败」必须可区分：
// 前者是渠道故障（OutcomeStreamError），后者是客户端走人（OutcomeInterrupted）。
func TestQA_ExtRelayAbortClassification(t *testing.T) {
	// 上游读中断：中继必须把错误原样上抛，供 extrelay 判 rerr != io.EOF。
	r := &qaStagedReader{stages: []qaStage{{data: qaExtFrameA}}}
	boom := errors.New("connection reset by peer")
	got, err := runExtRelay(t, io.MultiReader(r, &qaErrReader{err: boom}), 5*time.Millisecond)
	if err == nil {
		t.Fatal("上游读中断必须上抛错误")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("错误被吞/改写: %v", err)
	}
	if !strings.Contains(string(got), `"A"`) {
		t.Fatalf("中断前已到的帧必须已透传: %q", got)
	}
}

type qaErrReader struct{ err error }

func (r *qaErrReader) Read([]byte) (int, error) { return 0, r.err }
