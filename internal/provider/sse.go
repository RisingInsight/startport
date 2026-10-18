// sse.go SSE 帧解析与「流式转换读取器」。
//
// 为什么要自己写：Anthropic / Gemini / OpenAI 三种上游都是 SSE，但事件形态与
// 结束语义不同（OpenAI 用 data: [DONE]，Anthropic 用 event: message_stop，
// Gemini 用最后一帧的 finishReason）。这里只做「按事件块切分」，语义在各自
// 适配器里处理；转换读取器保证**逐事件增量**下发，不做整包缓冲
// （否则客户端 TTFT 观测全错，等于伪流式）。
package provider

import (
	"bufio"
	"bytes"
	"io"
	"strings"
)

// sseFrame 一个 SSE 事件块（event 名 + 拼接后的 data 行）。
type sseFrame struct {
	Event string
	Data  string
}

// sseDecoder 按事件块读取 SSE：空行分隔；多行 data: 用 \n 拼接。
type sseDecoder struct {
	br *bufio.Reader
}

// newSSEDecoder 构造解码器（缓冲 64KB，单行上限由 bufio 保证——SSE 单行
// 超过 64KB 的合法场景不存在，超长行会被切成多段 data，JSON 侧仍能解析）。
func newSSEDecoder(r io.Reader) *sseDecoder {
	return &sseDecoder{br: bufio.NewReaderSize(r, 64*1024)}
}

// next 读取下一个事件块。ok=false 表示流结束（err 亦为 nil）；err 非 nil 表示
// 读失败（含上游断连）。
func (d *sseDecoder) next() (sseFrame, bool, error) {
	var (
		event   string
		data    []string
		hasData bool
	)
	for {
		line, err := d.br.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				// 事件块结束。
				if hasData || event != "" {
					return sseFrame{Event: event, Data: strings.Join(data, "\n")}, true, nil
				}
			case strings.HasPrefix(line, ":"):
				// 注释/心跳帧（": ping"），忽略。
			case strings.HasPrefix(line, "event:"):
				event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				hasData = true
				v := strings.TrimPrefix(line, "data:")
				v = strings.TrimPrefix(v, " ")
				data = append(data, v)
			}
		}
		if err != nil {
			if hasData || event != "" {
				return sseFrame{Event: event, Data: strings.Join(data, "\n")}, true, nil
			}
			if err == io.EOF {
				return sseFrame{}, false, nil
			}
			return sseFrame{}, false, err
		}
	}
}

// frameHandler 处理单个事件块：返回要写出的字节片段、是否终止、错误。
type frameHandler func(f sseFrame) (out [][]byte, done bool, err error)

// transformReader 把上游 SSE 逐事件转换为下游 SSE（增量下发）。
type transformReader struct {
	src    io.ReadCloser
	dec    *sseDecoder
	buf    bytes.Buffer
	handle frameHandler
	onEOF  func() [][]byte // 上游正常结束但未给显式终止帧时的收尾帧
	done   bool
	err    error
}

// newTransformReader 构造转换读取器。
func newTransformReader(src io.ReadCloser, h frameHandler, onEOF func() [][]byte) *transformReader {
	return &transformReader{src: src, dec: newSSEDecoder(src), handle: h, onEOF: onEOF}
}

// Read 实现 io.Reader：循环取事件直到 buffer 有内容或流结束。
func (t *transformReader) Read(p []byte) (int, error) {
	for t.buf.Len() == 0 && !t.done && t.err == nil {
		f, ok, err := t.dec.next()
		if err != nil {
			t.err = err
			break
		}
		if !ok {
			// 上游 EOF：补收尾帧（finish/usage/[DONE]），然后结束。
			if t.onEOF != nil {
				for _, o := range t.onEOF() {
					t.buf.Write(o)
				}
				t.onEOF = nil
			}
			t.done = true
			break
		}
		outs, done, herr := t.handle(f)
		if herr != nil {
			t.err = herr
			break
		}
		for _, o := range outs {
			t.buf.Write(o)
		}
		if done {
			t.done = true
		}
	}
	if t.buf.Len() > 0 {
		return t.buf.Read(p)
	}
	if t.err != nil {
		return 0, t.err
	}
	return 0, io.EOF
}

// Close 关闭上游流（幂等）。
func (t *transformReader) Close() error {
	if t.src == nil {
		return nil
	}
	return t.src.Close()
}

// sseData 把 payload 包成一条 SSE data 帧。
func sseData(payload []byte) []byte {
	out := make([]byte, 0, len(payload)+8)
	out = append(out, "data: "...)
	out = append(out, payload...)
	out = append(out, '\n', '\n')
	return out
}

// sseDone 终止帧。
func sseDone() []byte { return []byte("data: [DONE]\n\n") }
