// handler_qa_test.go 独立验证方（QA）用例：P0「上游 200 后不吐帧、被掐流却记成功」。
//
// 与工程师用例的差异（刻意不重复）：
//   - 工程师用例的夹具是「先吐一帧、再读中断」；
//   - 本文件复现**真正的事故现场**：上游返回 200 后**一帧都不吐**，静默到网关
//     空闲看门狗（idle.go）cancel 掉请求 ctx，读侧报非 EOF 错误。这条路径上
//     validFrames==0，必须走「上游中断」分支而**不能**被空流分支吞掉、更不能记成功。
//   - 另外钉住「客户端还在时断流」与「客户端已走时断流」的正交性。
package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RisingInsight/startport/internal/auth"
	"github.com/RisingInsight/startport/internal/upstream"
)

// qaSilentAbortBody 上游 200 之后一个字节都不给，直接返回非 EOF 读错误
// （等价于 idle.go 的空闲看门狗 cancel 请求 ctx 后，底层 body 读断开）。
type qaSilentAbortBody struct {
	delayReads int // 前 N 次 Read 返回 (0,nil)，模拟「静默但连接还在」
	read       int
	err        error
}

func (b *qaSilentAbortBody) Read(p []byte) (int, error) {
	if b.read < b.delayReads {
		b.read++
		return 0, nil
	}
	return 0, b.err
}

func (b *qaSilentAbortBody) Close() error { return nil }

// qaAbortClient 造一个「200 + 指定 body」的假上游。
func qaAbortClient(body io.ReadCloser) *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       body,
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

const qaChatBody = `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`

// TestQA_P0_SilentUpstreamAbortIsNotSuccess 真正的事故现场：200 之后零帧 + 读中断。
// 断言账号侧「不能被记成功」且「吃到连败计数」——这是切断「卡死号被反复选中」的前提。
func TestQA_P0_SilentUpstreamAbortIsNotSuccess(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := qaAbortClient(&qaSilentAbortBody{delayReads: 2, err: errors.New("context canceled")})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(qaChatBody)))

	if rec.Code != http.StatusOK {
		t.Fatalf("头已发出只能是 200，得 %d", rec.Code)
	}
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("账号状态缺失")
	}
	if st.SuccessCount != 0 {
		t.Fatalf("零帧断流被记成功（success_count=%d）——这正是 P0：卡死号永远熔断不掉", st.SuccessCount)
	}
	if st.ConsecutiveFails != 1 {
		t.Fatalf("连败计数=%d want 1：断流必须喂 NoteFailures，否则出池计数永远是 0", st.ConsecutiveFails)
	}
	// 断流时网关拿不到上游的收尾帧：客户端只能靠「没有 [DONE]」判断异常。
	// 这里只记录事实（不断言为缺陷）：本次输出既无 error 帧也无 [DONE]。
	if strings.Contains(rec.Body.String(), "[DONE]") {
		t.Logf("本次断流路径补写了 [DONE]（行为变更，请核对客户端重试语义）")
	}
}

// TestQA_P0_KilledAccountLeavesPool 事故的最终目标：持续卡死的账号必须真的出池。
// 连败达阈值（config 默认 5）后账号不再出现在可用池里，直到降权到期。
func TestQA_P0_KilledAccountLeavesPool(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: qaAbortClient(&qaSilentAbortBody{err: errors.New("context canceled")})})

	for i := 0; i < 5; i++ { // degrade_threshold 默认 5
		h.ServeHTTP(httptest.NewRecorder(),
			httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(qaChatBody)))
	}

	st, _ := p.Status("u1")
	if st.SuccessCount != 0 {
		t.Fatalf("5 次断流后 success_count=%d want 0", st.SuccessCount)
	}
	if st.DegradeUntil.IsZero() {
		t.Fatalf("5 次断流后仍未降权出池：卡死号会被下一个请求继续选中（P0 未闭环）")
	}
	if uids := p.AvailableUIDs(); len(uids) != 0 {
		t.Fatalf("降权期内账号仍留在可用池: %v", uids)
	}
	// 关键：再发一次请求也不得把降权状态/成功计数抹掉（原来每次都被 NoteSuccess 清零）。
	h.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(qaChatBody)))
	st2, _ := p.Status("u1")
	if st2.SuccessCount != 0 {
		t.Fatalf("再次断流被记成功（success_count=%d）：P0 回归", st2.SuccessCount)
	}
	if st2.DegradeUntil.IsZero() || st2.DegradeUntil.Before(st.DegradeUntil) {
		t.Fatalf("降权截止被清零/缩短（%v → %v）", st.DegradeUntil, st2.DegradeUntil)
	}
}

// TestQA_ClientGoneAbortNotPunished 反向保护：客户端已走（ctx 取消）时的读中断只是
// 连带现象，账号健康——必须保持记成功、不喂连败。否则用户每关一次窗口就罚一次好号。
func TestQA_ClientGoneAbortNotPunished(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	body := &qaClientGoneBody{cancel: cancel, err: errors.New("context canceled")}
	h := NewHandler(Config{Pool: p, Upstream: qaAbortClient(body)})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(qaChatBody)).WithContext(ctx)
	h.ServeHTTP(httptest.NewRecorder(), req)

	st, _ := p.Status("u1")
	if st.SuccessCount != 1 {
		t.Fatalf("客户端断连时 success_count=%d want 1（人走了不算账号故障）", st.SuccessCount)
	}
	if st.ConsecutiveFails != 0 {
		t.Fatalf("客户端断连被喂了连败计数（%d）：会误罚健康号", st.ConsecutiveFails)
	}
	if !st.DegradeUntil.IsZero() {
		t.Fatalf("客户端断连触发了降权: %v", st.DegradeUntil)
	}
}

// qaClientGoneBody 先取消请求 ctx（= 客户端断连），再返回读错误。
type qaClientGoneBody struct {
	cancel context.CancelFunc
	err    error
	fired  bool
}

func (b *qaClientGoneBody) Read([]byte) (int, error) {
	if !b.fired {
		b.fired = true
		b.cancel()
		return 0, b.err
	}
	return 0, io.EOF
}

func (b *qaClientGoneBody) Close() error { return nil }
