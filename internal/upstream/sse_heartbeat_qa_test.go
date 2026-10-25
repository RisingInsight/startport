// sse_heartbeat_qa_test.go 独立验证方（QA）用例：验证「客户端保活心跳」与
// 「上游读中断哨兵」两项修复是否真的达成目标，以及是否引入新缺陷。
//
// 与工程师用例（sse_test.go / handler_test.go）**刻意不重叠**：这里只断言
// 「从外部可观测的字节流语义」与「错误三态正交性」，不复用其夹具与命名。
// 文件名带 _qa_test.go 后缀，避免与工程师用例的符号/命名冲突。
package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- 夹具 ----------

// qaSyncWriter 是并发安全的 http.ResponseWriter 替身。
// 心跳 goroutine 与主循环会并发写，httptest.ResponseRecorder 不是并发安全的，
// 直接用它跑 -race 会误报，故这里自带一把锁。
type qaSyncWriter struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	header   http.Header
	flushes  int
	status   int
	writeErr error // 非 nil 时所有 Write 立即失败（模拟客户端断连）
}

func newQASyncWriter() *qaSyncWriter {
	return &qaSyncWriter{header: http.Header{}, status: 0}
}

func (w *qaSyncWriter) Header() http.Header { return w.header }

func (w *qaSyncWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.status = code
	}
}

func (w *qaSyncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return w.buf.Write(p)
}

func (w *qaSyncWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.flushes++
}

func (w *qaSyncWriter) bytesOut() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.buf.Bytes()...)
}

// qaPacedReader 按「数据 + 该数据之前的静默时长」逐块吐字节，
// 用来精确复现「上游 200 之后长时间静默」与「流中间长静默」。
type qaPacedReader struct {
	chunks []qaChunk
	idx    int
}

type qaChunk struct {
	delay time.Duration
	data  string
}

func (r *qaPacedReader) Read(p []byte) (int, error) {
	if r.idx >= len(r.chunks) {
		return 0, io.EOF
	}
	c := &r.chunks[r.idx]
	if c.delay > 0 {
		time.Sleep(c.delay)
	}
	n := copy(p, c.data)
	c.data = c.data[n:]
	if c.data == "" {
		r.idx++
	}
	return n, nil
}

// qaAbortReader 先吐给定字节，再返回一个非 EOF 的读错误（上游掐流/看门狗掐流）。
type qaAbortReader struct {
	data string
	err  error
}

func (r *qaAbortReader) Read(p []byte) (int, error) {
	if r.data != "" {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	return 0, r.err
}

// qaDataFrame 拼一个最小合法 chat.completion.chunk（内容由参数决定）。
func qaDataFrame(content string) string {
	return fmt.Sprintf(
		`data: {"id":"chatcmpl-qa","object":"chat.completion.chunk","created":1753600000,"model":"glm-5.3-flash","choices":[{"index":0,"delta":{"role":"assistant","content":%q}}]}`+"\n\n",
		content)
}

// qaSplitLines 把出站字节按行切开（保留空行，便于判定分帧）。
func qaSplitLines(out []byte) []string {
	return strings.Split(string(out), "\n")
}

// qaHeartbeatLineOffsets 返回所有 ": keep-alive" 出现位置是否落在「行首」。
// 返回 midLine 列表（每个元素是它前面那一行的原文片段，便于失败时定位）。
func qaHeartbeatLineOffsets(out []byte) (count int, midLine []string) {
	s := string(out)
	off := 0
	for {
		i := strings.Index(s[off:], sseHeartbeatFrame[:len(sseHeartbeatFrame)-2]) // ": keep-alive"
		if i < 0 {
			return count, midLine
		}
		pos := off + i
		count++
		if pos != 0 && s[pos-1] != '\n' {
			// 找到本行行首
			start := strings.LastIndex(s[:pos], "\n") + 1
			midLine = append(midLine, s[start:pos])
		}
		off = pos + 1
	}
}

// qaDataPayloads 取出所有 "data: xxx" 的 payload（不含 [DONE]）。
func qaDataPayloads(out []byte) (payloads []string, done int) {
	for _, line := range qaSplitLines(out) {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		p := strings.TrimPrefix(line, "data: ")
		if p == "[DONE]" {
			done++
			continue
		}
		payloads = append(payloads, p)
	}
	return payloads, done
}

// ---------- 用例 1：心跳不破坏分帧（主链路 StreamHint） ----------

// TestQA_HeartbeatDoesNotSplitDataFrames 复现 P0 现场并在**字节层**验证分帧安全：
// 上游 200 之后静默 120ms 才吐首帧（阈值 20ms → 首帧前必然有多次心跳），
// 之后帧间再静默 120ms。断言：
//  1. 首帧之前确实写入了心跳（证明「首帧前不再零字节」这一目标达成）；
//  2. 每一处 ": keep-alive" 都落在行首——心跳绝不插进 data: 帧的内部字节；
//  3. 剥掉心跳后，数据帧的 JSON 与内容顺序完全无损。
func TestQA_HeartbeatDoesNotSplitDataFrames(t *testing.T) {
	const interval = 20 * time.Millisecond
	r := &qaPacedReader{chunks: []qaChunk{
		{delay: 120 * time.Millisecond, data: qaDataFrame("A")},
		{delay: 120 * time.Millisecond, data: qaDataFrame("B")},
		{data: "data: [DONE]\n\n"},
	}}
	w := newQASyncWriter()

	if err := StreamHint(w, r, nil, WithHeartbeatInterval(interval)); err != nil {
		t.Fatalf("StreamHint 正常流不应返回错误: %v", err)
	}
	out := w.bytesOut()

	// 1) 首帧之前必须有心跳字节，否则「首帧前零字节」的老故障仍在。
	firstData := bytes.Index(out, []byte("data: "))
	if firstData < 0 {
		t.Fatalf("没有任何 data 帧，输出=%q", out)
	}
	if n := strings.Count(string(out[:firstData]), ": keep-alive"); n == 0 {
		t.Fatalf("首帧前没有任何心跳帧（客户端仍会 idle timeout）: %q", out[:firstData])
	}

	// 2) 心跳绝不插进帧内部。
	count, midLine := qaHeartbeatLineOffsets(out)
	if len(midLine) > 0 {
		t.Fatalf("心跳被插入到行中间，破坏 SSE 分帧: %q（上下文: %q）", midLine, out)
	}
	if count < 2 {
		t.Fatalf("心跳次数=%d，两段静默各应触发至少一次心跳: %q", count, out)
	}

	// 3) 数据帧无损：条数、JSON 可解析、内容顺序。
	payloads, done := qaDataPayloads(out)
	if done != 1 {
		t.Fatalf("[DONE] 数量=%d want 1: %q", done, out)
	}
	if len(payloads) != 2 {
		t.Fatalf("data 帧数=%d want 2（心跳不得被算成数据帧）: %q", len(payloads), out)
	}
	for i, want := range []string{"A", "B"} {
		var obj map[string]any
		if err := jsonUnmarshalStrict(payloads[i], &obj); err != nil {
			t.Fatalf("第 %d 帧 JSON 被心跳破坏（%v）: %q", i, err, payloads[i])
		}
		got := payloadContent(obj)
		if got != want {
			t.Fatalf("第 %d 帧内容=%q want %q", i, got, want)
		}
	}
}

// TestQA_NegativeIntervalDisablesHeartbeat 关闭开关必须真的零心跳（供逐字节比对）。
func TestQA_NegativeIntervalDisablesHeartbeat(t *testing.T) {
	r := &qaPacedReader{chunks: []qaChunk{
		{delay: 80 * time.Millisecond, data: qaDataFrame("A")},
		{data: "data: [DONE]\n\n"},
	}}
	w := newQASyncWriter()
	if err := StreamHint(w, r, nil, WithHeartbeatInterval(-time.Second)); err != nil {
		t.Fatalf("StreamHint: %v", err)
	}
	if n := strings.Count(string(w.bytesOut()), "keep-alive"); n != 0 {
		t.Fatalf("负间隔应当关闭心跳，实际写了 %d 次: %q", n, w.bytesOut())
	}
}

// ---------- 用例 2：Close 语义 / goroutine 泄漏 ----------

// TestQA_NoGoroutineLeakAcrossStreams 连续跑 20 条流式请求（每条都带活跃心跳），
// 心跳 goroutine 必须随 Close 全部回收；且 Close 之后不再有字节写出。
func TestQA_NoGoroutineLeakAcrossStreams(t *testing.T) {
	runOnce := func() {
		r := &qaPacedReader{chunks: []qaChunk{
			{delay: 30 * time.Millisecond, data: qaDataFrame("A")},
			{data: "data: [DONE]\n\n"},
		}}
		w := newQASyncWriter()
		if err := StreamHint(w, r, nil, WithHeartbeatInterval(5*time.Millisecond)); err != nil {
			t.Fatalf("StreamHint: %v", err)
		}
	}

	runOnce() // 预热：让 testing/http 相关一次性 goroutine 先起
	time.Sleep(50 * time.Millisecond)
	base := runtime.NumGoroutine()

	for i := 0; i < 20; i++ {
		runOnce()
	}
	// 给调度器一点时间回收已退出的 goroutine。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= base {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > base {
		t.Fatalf("20 条流后 goroutine 从 %d 涨到 %d：心跳 goroutine 泄漏", base, got)
	}
}

// TestQA_CloseWaitsThenRejectsWrites Close 返回后不得再有字节写出，且写调用报
// io.ErrClosedPipe（调用方缺陷要显式可见，而不是静默丢帧）。
func TestQA_CloseWaitsThenRejectsWrites(t *testing.T) {
	w := newQASyncWriter()
	out := NewSSEWriterInterval(w, w, 5*time.Millisecond)
	time.Sleep(20 * time.Millisecond) // 至少触发一次心跳
	out.Close()
	snapshot := len(w.bytesOut())

	time.Sleep(50 * time.Millisecond) // Close 之后不应再有字节
	if n := len(w.bytesOut()); n != snapshot {
		t.Fatalf("Close 后仍写出 %d 字节（close 后写）", n-snapshot)
	}
	if _, err := out.Write([]byte("data: X\n\n")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Close 后 Write err=%v want io.ErrClosedPipe", err)
	}
	out.Close() // 幂等：不得 panic / 不得阻塞
}

// ---------- 用例 3：错误三态正交 ----------

// TestQA_ErrorTrichotomy 判据必须两两正交，否则会「误罚健康号」或「漏罚卡死号」：
//   - 上游读中断     → IsUpstreamAbortError 为真，且不是空流、不是写失败；
//   - 客户端写失败   → 非空 flow 错误，但**绝不**是 IsUpstreamAbortError（否则
//     用户每关一次窗口就给健康号喂一次失败）；
//   - 上游 EOF 正常收 → err == nil（io.EOF 仍按正常结束处理）。
func TestQA_ErrorTrichotomy(t *testing.T) {
	// (a) 上游读中断
	t.Run("upstream_abort", func(t *testing.T) {
		r := &qaAbortReader{data: qaDataFrame("A"), err: errors.New("read: connection reset by peer")}
		w := newQASyncWriter()
		err := StreamHint(w, r, nil, WithHeartbeatInterval(-time.Second))
		if !IsUpstreamAbortError(err) {
			t.Fatalf("上游读中断未被识别为 abort: %v", err)
		}
		if IsEmptyStreamError(err) {
			t.Fatalf("上游读中断被误判成空流: %v", err)
		}
	})

	// (b) 客户端写失败
	t.Run("client_write_error", func(t *testing.T) {
		r := &qaPacedReader{chunks: []qaChunk{{data: qaDataFrame("A")}, {data: "data: [DONE]\n\n"}}}
		w := newQASyncWriter()
		w.writeErr = errors.New("write tcp: broken pipe")
		err := StreamHint(w, r, nil, WithHeartbeatInterval(-time.Second))
		if err == nil {
			t.Fatal("客户端写失败必须返回错误")
		}
		if IsUpstreamAbortError(err) {
			t.Fatalf("客户端写失败被误判成上游中断（会误罚健康号）: %v", err)
		}
		if IsEmptyStreamError(err) {
			t.Fatalf("客户端写失败被误判成空流: %v", err)
		}
	})

	// (c) 上游正常 EOF（含 [DONE]）
	t.Run("clean_eof", func(t *testing.T) {
		r := &qaPacedReader{chunks: []qaChunk{{data: qaDataFrame("A")}, {data: "data: [DONE]\n\n"}}}
		w := newQASyncWriter()
		if err := StreamHint(w, r, nil, WithHeartbeatInterval(-time.Second)); err != nil {
			t.Fatalf("正常结束不应报错: %v", err)
		}
	})
}

// ---------- 用例 4：空流兜底语义（心跳开启时不得被改坏） ----------

// TestQA_EmptyStreamFallbackIntact 上游 200 + 0 有效帧（含「只有心跳、没有数据」）：
// 必须仍然恰好写出 1 个 error 帧 + 1 个 [DONE]，并返回 IsEmptyStreamError。
// 心跳是注释行，不得被算成有效帧、也不得把兜底帧挤掉。
func TestQA_EmptyStreamFallbackIntact(t *testing.T) {
	r := &qaPacedReader{chunks: []qaChunk{{delay: 40 * time.Millisecond}}} // 静默后 EOF
	w := newQASyncWriter()
	err := StreamHint(w, r, nil, WithHeartbeatInterval(5*time.Millisecond))
	if !IsEmptyStreamError(err) {
		t.Fatalf("空流未被识别（err=%v）", err)
	}
	out := string(w.bytesOut())
	if n := strings.Count(out, `"code":"upstream_parse"`); n != 1 {
		t.Fatalf("兜底 error 帧数=%d want 1: %q", n, out)
	}
	payloads, done := qaDataPayloads(w.bytesOut())
	if done != 1 {
		t.Fatalf("[DONE] 数量=%d want 1: %q", done, out)
	}
	if len(payloads) != 1 {
		t.Fatalf("data 帧数=%d want 1（仅 error 兜底帧；心跳不算数据帧）: %q", len(payloads), out)
	}
	if ok := strings.Contains(out, ": keep-alive"); !ok {
		t.Fatalf("空流静默期也应有心跳（否则客户端在兜底前就 idle timeout）: %q", out)
	}
}

// ---------- 用例 5：心跳与「宿主写入口径」的契约 ----------

// TestQA_HeartbeatIsStandaloneLineUnderStall 说明心跳的分帧安全前提：
// SSEWriter 只保证「每次 Write 的字节块是原子的」，不保证「块内是完整行」——
// 调用方必须以整行/整帧为写入单位。
//
// 主链路（sse.go writeSSE）以 "data: ...\n\n" 整帧写入 → 天然满足；
// ext 渠道（extrelay.go:121）以 chatStatsReader 的行缓冲输出为单位写入 → 同样满足
// （见 server 包 extrelay_qa_test.go 的端到端验证）。
//
// 本用例把前提写成断言：只要写入是整行的，任何静默下心跳都只会落在行首。
func TestQA_HeartbeatIsStandaloneLineUnderStall(t *testing.T) {
	const interval = 15 * time.Millisecond
	r := &qaPacedReader{chunks: []qaChunk{
		{data: qaDataFrame("A")},
		{delay: 60 * time.Millisecond, data: qaDataFrame("B")},
		{data: "data: [DONE]\n\n"},
	}}

	w := newQASyncWriter()
	out := NewSSEWriterInterval(w, w, interval)
	defer out.Close()

	// 以「整帧」为写入单位（与 sse.go writeSSE / extrelay 的行缓冲输出同构）。
	buf := make([]byte, 64*1024)
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				t.Fatalf("写失败: %v", werr)
			}
		}
		if rerr != nil {
			break
		}
	}

	got := w.bytesOut()
	if n := strings.Count(string(got), ": keep-alive"); n == 0 {
		t.Fatalf("整行写入 + 60ms 静默应触发心跳: %q", got)
	}
	if _, midLine := qaHeartbeatLineOffsets(got); len(midLine) > 0 {
		t.Fatalf("整行写入前提下心跳仍落在行中: %q", got)
	}
	// 剥掉心跳后必须与上游输入逐字节一致（透传零改写）。
	stripped := strings.ReplaceAll(string(got), sseHeartbeatFrame, "")
	want := qaDataFrame("A") + qaDataFrame("B") + "data: [DONE]\n\n"
	if stripped != want {
		t.Fatalf("剥掉心跳后字节与上游不一致:\n got=%q\nwant=%q", stripped, want)
	}
}

// ---------- 小工具 ----------

func jsonUnmarshalStrict(s string, v any) error {
	return json.Unmarshal([]byte(s), v)
}

func payloadContent(obj map[string]any) string {
	chs, _ := obj["choices"].([]any)
	if len(chs) == 0 {
		return ""
	}
	c, _ := chs[0].(map[string]any)
	d, _ := c["delta"].(map[string]any)
	s, _ := d["content"].(string)
	return s
}
