// sse_heartbeat.go 流式转发的「客户端保活心跳」出口。
//
// 解决的问题（真实故障）：上游返回 HTTP 200 之后，超大上下文（实测 33–44 万
// token）的 prefill 可以持续数十秒、极端时数分钟才吐第一个 SSE 数据帧；极端情况
// 下上游还会在流中间长时间静默。此前网关在这两段静默期内**一个字节都不写**，
// 客户端（dsh 的 pi-ai、zcode）按自己的「流空闲超时」判定连接已死并重试：
//
//	pi-ai stream idle timeout after 300000ms
//
// 用户侧表现就是界面一直转圈、没有任何反应。
//
// 修法：在空闲超过阈值时补写 SSE **注释帧**（`: keep-alive`）。SSE 规范规定以
// ':' 开头的行是注释、必须被解析器忽略，因此它不改变任何数据帧语义，也不会被
// 客户端当成新 chunk 追加；但它是真实字节，能穿过客户端/中间代理的「无数据即
// 空闲」判定，让链路上的每一跳都看到「连接仍然活着」。
//
// 并发模型（关键）：主 goroutine 阻塞在上游读取（bufio.Reader.ReadString）上，
// 心跳必须由独立 goroutine 定时触发；两条路径都会写客户端，所以**所有**写出
// 必须经 SSEWriter 的同一把 mutex 串行化——否则注释帧有可能插进一帧 `data:`
// 的中间字节，把整条流的分帧破坏掉（客户端会解析出坏 JSON）。
package upstream

import (
	"io"
	"net/http"
	"sync"
	"time"
)

// streamHeartbeatInterval 是向客户端补写心跳注释帧的空闲阈值。
//
// 取值 15s：客户端侧的空闲阈值实测是 300s（pi-ai），15s 留了 20 倍余量，能扛住
// 「上游一次静默好几分钟」的极端 prefill；同时它远大于正常流的帧间隔（正常流
// 每帧几十毫秒），正常场景下心跳几乎不会触发，不给链路增加可见噪声。
const streamHeartbeatInterval = 15 * time.Second

// sseHeartbeatFrame 是心跳注释帧的逐字节原文：`:` 开头的注释行 + 空行分隔。
//
// 单独抽成常量是为了让实现与测试断言共用同一份字节，避免「实现改了文案、测试
// 还按旧文案断言」这种假绿。空行分隔是必须的——SSE 以空行分派事件，注释帧同样
// 需要以空行结束才能被解析器立即消费（否则会被攒进下一个事件的缓冲里）。
const sseHeartbeatFrame = ": keep-alive\n\n"

// DefaultHeartbeatInterval 是心跳阈值的公开口径（= streamHeartbeatInterval）。
//
// 存在的唯一理由：server 包的多个流式出口（extrelay / ext 的 messages、responses）
// 必须共用同一阈值，且测试需要用毫秒级阈值才能在夹具里观察到心跳。生产路径不应
// 改写它（那些出口只是把它读进自己的包级变量）。
const DefaultHeartbeatInterval = streamHeartbeatInterval

// SSEWriter 把流式响应 writer 包装成「并发安全 + 空闲心跳」的 SSE 出口。
//
// 它同时是 io.Writer（单次 Write = 一次完整的出站字节块）与心跳调度器：
//   - Write：加锁 → 写 → Flush → 刷新「上次写出时刻」。任何写失败（客户端断连）
//     都被记住并持久化（Err），后续写直接返回同一错误，主循环凭此终止。
//   - 后台 goroutine：按周期检查「距上次写出是否超过阈值」，超过才补注释帧，
//     因此不会给繁忙的流插帧。
//   - Close：停 ticker 并等待后台 goroutine 退出（幂等），保证「Close 返回后
//     绝不会再有字节被写出」——这是 defer 收尾能覆盖正常结束/出错/断连三条
//     路径的前提，也是「无 goroutine 泄漏、无 close 后写 panic」的保证。
type SSEWriter struct {
	mu       sync.Mutex
	w        io.Writer
	fl       http.Flusher
	interval time.Duration

	last   time.Time // 最近一次成功写出的时刻（心跳判定的基准）
	err    error     // 首个写失败（通常是客户端断连）；置位后不再尝试写
	closed bool      // Close 已执行；之后所有写是 no-op（返回 io.ErrClosedPipe）

	// lineStart 报告「上一次写出的最后一个字节是否为 '\n'」，即当前是否处于行边界。
	//
	// 心跳**只能**在行边界插入。主链路（sse.go）的每次 Write 都是一整帧
	// `data: ...\n\n`，天然落在帧边界；但 ext 渠道是逐字节透传（Read(buf) →
	// Write(buf[:n])，32KB 裸块），块边界可能落在 `data: {"a":1` 的中间——此时
	// 插一条注释会把半行撑成 `data: {"a":1}: keep-alive`，该行 JSON 直接坏掉、
	// 客户端解析失败并丢帧。故行内一律不发，等回到行边界再补（少一两次心跳远
	// 好于毁掉一帧数据）。
	lineStart bool

	stop      chan struct{} // 关闭信号（Close 关闭它）
	done      chan struct{} // 后台 goroutine 已退出
	closeOnce sync.Once
}

// NewSSEWriter 用默认心跳间隔（streamHeartbeatInterval）构造。
// w 通常是 http.ResponseWriter；fl 为 nil 时只写不 flush（非流式 writer）。
func NewSSEWriter(w io.Writer, fl http.Flusher) *SSEWriter {
	return NewSSEWriterInterval(w, fl, streamHeartbeatInterval)
}

// NewSSEWriterInterval 允许指定心跳间隔：
//   - 0（零值）→ 用默认 streamHeartbeatInterval，方便调用方直接透传配置零值；
//   - 负数 → 关闭心跳（只保留写锁），供「必须逐字节比对」的场景使用；
//   - 正数 → 自定义间隔（测试用短间隔验证首帧前/流中心跳）。
func NewSSEWriterInterval(w io.Writer, fl http.Flusher, interval time.Duration) *SSEWriter {
	if interval == 0 {
		interval = streamHeartbeatInterval
	}
	s := &SSEWriter{w: w, fl: fl, interval: interval, last: time.Now(), lineStart: true}
	if interval > 0 {
		s.stop = make(chan struct{})
		s.done = make(chan struct{})
		go s.heartbeatLoop()
	}
	return s
}

// Write 写出一段完整字节并 Flush，同时刷新心跳基准。
// 实现 io.Writer，因此 io.WriteString 可直接使用（保持调用点与原代码同形）。
func (s *SSEWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeLocked(p)
}

// writeLocked 是唯一的物理写入口（调用方必须持锁）。
func (s *SSEWriter) writeLocked(p []byte) (int, error) {
	if s.closed {
		// Close 之后的写属于调用方缺陷（多写了收尾帧）；明确报错而不是静默丢弃。
		return 0, io.ErrClosedPipe
	}
	if s.err != nil {
		// 已经断连：不重复写（重复写在被复用的连接上可能产生半帧字节）。
		return 0, s.err
	}
	n, err := s.w.Write(p)
	if err != nil {
		s.err = err
		return n, err
	}
	if n > 0 {
		// 记录行边界状态：心跳只能插在 '\n' 之后（见 lineStart 注释）。
		s.lineStart = p[n-1] == '\n'
	}
	s.last = time.Now()
	if s.fl != nil {
		s.fl.Flush()
	}
	return n, nil
}

// Err 返回首个写失败（客户端断连）。主循环可在阻塞读之前用它提前收手。
// 心跳与主循环共享这把锁的开销可以忽略：一次加锁远小于一次 bufio 读 + JSON 解析。
func (s *SSEWriter) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Close 停止心跳并等待后台 goroutine 退出。幂等（sync.Once）。
//
// 顺序很重要：先 close(stop) 并等 done（确保后台 goroutine 不在写），再置
// closed——反过来会出现「置位后后台仍持有写权限」的窗口。
func (s *SSEWriter) Close() {
	s.closeOnce.Do(func() {
		if s.stop != nil {
			close(s.stop)
			<-s.done
		}
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
	})
}

// heartbeatLoop 是后台心跳 goroutine：按半周期轮询，攒够一个阈值才补一帧注释。
//
// 轮询周期取 interval/2：心跳实际间隔落在 [interval, interval*1.5)，既不会因为
// 「刚写完就 tick」而把间隔拖到 2×interval（那会让阈值失去意义），也不会空转。
func (s *SSEWriter) heartbeatLoop() {
	defer close(s.done)
	period := s.interval / 2
	if period <= 0 {
		period = s.interval
	}
	t := time.NewTicker(period)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.emitHeartbeat()
		}
	}
}

// emitHeartbeat 在空闲已达阈值时写一帧注释。已在写路径失败/已 Close → 静默跳过。
func (s *SSEWriter) emitHeartbeat() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.err != nil {
		return
	}
	if time.Since(s.last) < s.interval {
		return // 主循环正在持续吐帧：不需要心跳，也不给流添字节
	}
	if !s.lineStart {
		// 上游正卡在「半行中间」（末尾没有换行）：此处插注释会把半行 data 撑成坏
		// JSON（客户端解析失败、数据丢失），比客户端空闲超时更糟。所以本轮不发，
		// 保持 last 不变，等回到行边界后的下一个 tick 再补。
		return
	}
	if _, err := io.WriteString(s.w, sseHeartbeatFrame); err != nil {
		// 客户端已断开：记下来让主循环尽快收手（Err() / 下一次 Write 都会命中）。
		s.err = err
		return
	}
	s.lineStart = true // 心跳帧自身以 "\n\n" 结尾，之后仍在行边界
	s.last = time.Now()
	if s.fl != nil {
		s.fl.Flush()
	}
}
