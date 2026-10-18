// qoderstream.go Qoder 的信封解包、错误分类与思考标签拆解（v1.5.0）。
//
// 移植依据：`C:/tmp/ref/qoder/stream.rs`（信封/思考拆解）、`errors.rs`（分类）。
//
// ── 上游的流长什么样（stream.rs:4-11）────────────────────────
// 外层信封 `{statusCodeValue, body}`，真正的响应在 `body` 字段里且是**内层
// JSON 字符串**，结构才与 OpenAI 的 chat.completion.chunk 一致：
//
//	data: {"statusCodeValue":200,"body":"{\"choices\":[{\"delta\":{\"content\":\"你\"}}]}"}
//
// 业务错误不体现在 HTTP 状态码上（HTTP 恒 200），都在信封的 statusCodeValue 里
// —— 不在这里分类，客户端会把一条额度错误当成一次正常结束的空回答。
package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// qoderThinkTags 思考内容可能包裹的标签（protocol.rs:40-45）。
var qoderThinkTags = [][2]string{
	{"<thinking>", "</thinking>"},
	{"<think>", "</think>"},
	{"<reasoning>", "</reasoning>"},
	{"<thought>", "</thought>"},
}

// ── 错误分类（errors.rs 全量移植）───────────────────────────────

// 上游业务码表（errors.rs:115-122）：都不体现在 HTTP 状态码上。
var (
	qoderQueueCodes = []string{"10605"} // 排队中
	qoderAuthCodes  = []string{"105"}   // 登录态失效
	qoderQuotaCodes = []string{         // 额度类（110/112 另有 9router 实测佐证）
		"110", "112", "113", "114", "115", "116", "117", "118", "119", "122",
	}
	qoderQuotaSignals = []string{
		"pricingurl", "insufficient", "no_quota", "quota_exceed", "exceed_quota",
		"exceeded", "credit", "upgrade", "subscription", "plan", "trial",
	}
)

// qoderUpstreamKind 上游错误类别（决定动作：换证 / 冷却 / 排队退避 / 原样透传）。
type qoderUpstreamKind int

const (
	qoderKindUnknown   qoderUpstreamKind = iota
	qoderKindQueued                      // 排队：等一会儿再发（不是错误）
	qoderKindAuth                        // 鉴权失败：重新粘贴凭证
	qoderKindForbidden                   // 裸 403：按鉴权处理，但不触发刷新
	qoderKindQuota                       // 额度不足 / 套餐不支持
	qoderKindRate                        // 限流
	qoderKindServer                      // 上游 5xx
)

// qoderQueueInfo 排队信号（errors.rs:95-102）。
type qoderQueueInfo struct {
	retryAfterSecs   *uint64
	queueType        string
	queueCount       *int64
	serviceAvailable *bool
}

// qoderClassified 分类结果（errors.rs:100-106）。
type qoderClassified struct {
	kind       qoderUpstreamKind
	message    string
	pricingURL string
	queue      qoderQueueInfo
}

// qoderErrorSignals 各层扫出的判定信号（errors.rs:107-115）。
type qoderErrorSignals struct {
	codes              []string
	queue              qoderQueueInfo
	isQueued           bool
	serviceUnavailable bool
}

func (s *qoderErrorSignals) hasCode(table []string) bool {
	for _, code := range s.codes {
		for _, want := range table {
			if code == want {
				return true
			}
		}
	}
	return false
}

// queued 排队判定（errors.rs:126-131）：业务码 10605 硬信号；isQueued 为真；
// 或「声明服务不可用 + 带了队列档位」的组合兜底。
func (s *qoderErrorSignals) queued() bool {
	return s.hasCode(qoderQueueCodes) || s.isQueued ||
		(s.queue.queueType != "" && s.queue.serviceAvailable != nil && !*s.queue.serviceAvailable)
}

// qoderScanSignals 顺着嵌套 JSON 抠业务码与排队信号（errors.rs:138-195）。
// 正文常常是多层嵌套的 JSON 字符串（message/body/data 一层层套），钻取深度 5。
func qoderScanSignals(raw string) qoderErrorSignals {
	signals := qoderErrorSignals{}
	current := raw
	for depth := 0; depth < 5; depth++ {
		var value map[string]any
		if err := json.Unmarshal([]byte(current), &value); err != nil {
			break
		}
		if code, ok := value["code"]; ok {
			switch c := code.(type) {
			case string:
				signals.codes = append(signals.codes, strings.TrimSpace(c))
			case json.Number:
				signals.codes = append(signals.codes, c.String())
			case float64:
				signals.codes = append(signals.codes, fmt.Sprintf("%d", int64(c)))
			}
		}
		if truthyQoder(value["isQueued"]) {
			signals.isQueued = true
		}
		if signals.queue.retryAfterSecs == nil {
			for _, key := range []string{"retryAfterSeconds", "waitTime"} {
				if secs := qoderSeconds(value[key]); secs != nil {
					signals.queue.retryAfterSecs = secs
					break
				}
			}
		}
		if signals.queue.queueType == "" {
			if qt, ok := value["queueType"].(string); ok {
				signals.queue.queueType = qt
			}
		}
		if signals.queue.serviceAvailable == nil {
			if _, exists := value["serviceAvailable"]; exists {
				b := truthyQoder(value["serviceAvailable"])
				signals.queue.serviceAvailable = &b
			}
		}
		// 下一层：内嵌 JSON 字符串（message / body / data / error）
		next := ""
		for _, key := range []string{"message", "body", "data", "error"} {
			if s, ok := value[key].(string); ok && strings.HasPrefix(strings.TrimLeft(s, " \t\r\n"), "{") {
				next = s
				break
			}
		}
		if next == "" {
			break
		}
		current = next
	}
	return signals
}

// qoderSeconds 秒数的两种形态（数字 / 数字字符串；errors.rs:197-203）。
func qoderSeconds(v any) *uint64 {
	switch n := v.(type) {
	case json.Number:
		if i, err := n.Int64(); err == nil && i > 0 {
			u := uint64(i)
			return &u
		}
	case float64:
		if n > 0 {
			u := uint64(n)
			return &u
		}
	case string:
		var i int64
		if _, err := fmt.Sscanf(strings.TrimSpace(n), "%d", &i); err == nil && i > 0 {
			u := uint64(i)
			return &u
		}
	}
	return nil
}

// truthyQoder JS 真值判定（Boolean(x)，protocol.rs:549-556）。
func truthyQoder(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case json.Number:
		f, err := x.Float64()
		return err == nil && f != 0
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return false
}

// qoderRawHasCode 裸文本兜底：正文不是 JSON 时也能认出业务码
// （`\"code\":\"10605\"` 的转义形态去掉反斜杠就与普通形态同形；errors.rs:207-213）。
func qoderRawHasCode(raw, code string) bool {
	flat := strings.ReplaceAll(raw, "\\", "")
	for _, form := range []string{
		"\"code\":\"" + code + "\"",
		"\"code\":" + code,
		"\"code\": \"" + code + "\"",
	} {
		if strings.Contains(flat, form) {
			return true
		}
	}
	return false
}

// qoderPricingURLOf 从文本里抠定价页链接（errors.rs:226-256，源正则
// /https?:\/\/[^"\\]*\/pricing[^"\\]*/i）。匹配用 ASCII 折叠（indexIgnoreASCIICase）：
// 不能用 strings.ToLower 取下标回切原文——Unicode 折叠不保字节长度，下标会
// 落进多字节字符中间（源实现有专门的 panic 回归测试，errors.rs:367-389）。
func qoderPricingURLOf(raw string) string {
	searchFrom := 0
	for {
		rel := indexIgnoreASCIICase(raw[searchFrom:], "http")
		if rel < 0 {
			return ""
		}
		start := searchFrom + rel
		rest := raw[start:]
		end := len(rest)
		for i, ch := range rest {
			if ch == '"' || ch == '\\' || ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' {
				end = i
				break
			}
		}
		candidate := rest[:end]
		if strings.Contains(strings.ToLower(candidate), "/pricing") {
			return candidate
		}
		searchFrom = start + 4
		if searchFrom >= len(raw) {
			return ""
		}
	}
}

// qoderClassifyUpstreamError 分类上游错误（errors.rs:267-343 的判定链）。
//
// 顺序要点：① 排队（前置于额度泛词——排队正文带 serviceAvailable，先判额度会
// 被泛词截胡）→ ② 业务码（105 / 额度族）→ ③ 额度关键词 / 定价链接 →
// ④ 状态码（429 / 401 / 403 / 5xx）。业务码必须前置于状态码：上游用 403 同时
// 表达排队 / 额度 / 鉴权三种处置，只看状态码必然误判（errors.rs:271-282）。
func qoderClassifyUpstreamError(status int, text string) qoderClassified {
	body := strings.ToLower(text)
	pricing := qoderPricingURLOf(text)
	signals := qoderScanSignals(text)

	if signals.queued() || qoderRawHasCode(text, qoderQueueCodes[0]) {
		return qoderClassified{
			kind:    qoderKindQueued,
			message: qoderQueuedMessage(signals.queue),
			queue:   signals.queue,
		}
	}
	if signals.hasCode(qoderAuthCodes) || qoderAnyRawHasCode(text, qoderAuthCodes) {
		return qoderClassified{kind: qoderKindAuth, message: "登录态已失效，请重新登录"}
	}
	if signals.hasCode(qoderQuotaCodes) || qoderAnyRawHasCode(text, qoderQuotaCodes) {
		return qoderClassified{kind: qoderKindQuota, message: "当前账号额度不足或套餐不支持该模型", pricingURL: pricing}
	}
	if pricing != "" || qoderContainsAny(body, qoderQuotaSignals) {
		return qoderClassified{kind: qoderKindQuota, message: "当前账号额度不足或套餐不支持该模型", pricingURL: pricing}
	}
	if status == http.StatusTooManyRequests || strings.Contains(body, "rate limit") || strings.Contains(body, "too many") {
		return qoderClassified{kind: qoderKindRate, message: "请求过于频繁，请稍后重试"}
	}
	if status == http.StatusUnauthorized {
		return qoderClassified{kind: qoderKindAuth, message: "登录态已失效，请重新登录"}
	}
	if status == http.StatusForbidden {
		// 走到这里没有排队、没有额度特征：按权限问题处理（**不**触发刷新）。
		return qoderClassified{kind: qoderKindForbidden, message: "上游拒绝访问，可能是登录态失效或权限不足"}
	}
	if status >= 500 {
		return qoderClassified{kind: qoderKindServer, message: "上游服务异常"}
	}
	return qoderClassified{kind: qoderKindUnknown}
}

func qoderAnyRawHasCode(text string, codes []string) bool {
	for _, code := range codes {
		if qoderRawHasCode(text, code) {
			return true
		}
	}
	return false
}

func qoderContainsAny(body string, signals []string) bool {
	for _, s := range signals {
		if strings.Contains(body, s) {
			return true
		}
	}
	return false
}

// qoderQueuedMessage 排队文案（errors.rs:345-356）：说明「不是登录态 /
// 额度问题」是关键——写错成"登录态失效"会让用户去重新登录一个正常的账号。
func qoderQueuedMessage(queue qoderQueueInfo) string {
	if queue.retryAfterSecs != nil {
		return fmt.Sprintf("上游模型排队中（模型暂不可服务，上游建议 %d 秒后重试）：这不是登录态或额度问题",
			*queue.retryAfterSecs)
	}
	return "上游模型排队中（模型暂不可服务）：这不是登录态或额度问题"
}

// ── 信封解包（stream.rs:59-113）────────────────────────────────

// qoderSSEEvent 解析一行 data: 之后的产出。
type qoderSSEEvent struct {
	kind       int // 0=skip 1=done 2=chunk 3=business error
	chunk      map[string]any
	status     int
	classified qoderClassified
	raw        string
}

const (
	qoderEventSkip = iota
	qoderEventDone
	qoderEventChunk
	qoderEventError
)

// qoderParseSSELine 解析一行 data: 内容（stream.rs:59-113）。
func qoderParseSSELine(data string) qoderSSEEvent {
	trimmed := strings.TrimSpace(data)
	if trimmed == "" {
		return qoderSSEEvent{kind: qoderEventSkip}
	}
	if trimmed == "[DONE]" {
		return qoderSSEEvent{kind: qoderEventDone}
	}
	var envelope map[string]any
	if err := json.Unmarshal([]byte(trimmed), &envelope); err != nil {
		return qoderSSEEvent{kind: qoderEventSkip}
	}
	// 业务错误在信封的 statusCodeValue 里（HTTP 状态码始终是 200）。
	if codeVal, ok := envelope["statusCodeValue"]; ok {
		var status int64
		switch c := codeVal.(type) {
		case json.Number:
			status, _ = c.Int64()
		case float64:
			status = int64(c)
		}
		if status != 0 && status != 200 {
			raw := ""
			switch b := envelope["body"].(type) {
			case string:
				raw = b
			default:
				if b != nil {
					if j, err := json.Marshal(b); err == nil {
						raw = string(j)
					}
				}
			}
			classified := qoderClassifyUpstreamError(int(status), raw)
			if len(raw) > 500 {
				raw = raw[:500]
			}
			return qoderSSEEvent{kind: qoderEventError, status: int(status), classified: classified, raw: raw}
		}
	}
	inner, ok := envelope["body"]
	if !ok {
		return qoderSSEEvent{kind: qoderEventSkip}
	}
	if s, isStr := inner.(string); isStr {
		if s == "[DONE]" {
			return qoderSSEEvent{kind: qoderEventDone}
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(s), &chunk); err != nil {
			return qoderSSEEvent{kind: qoderEventSkip}
		}
		return qoderSSEEvent{kind: qoderEventChunk, chunk: chunk}
	}
	if inner == nil {
		return qoderSSEEvent{kind: qoderEventSkip}
	}
	if m, isMap := inner.(map[string]any); isMap {
		return qoderSSEEvent{kind: qoderEventChunk, chunk: m}
	}
	return qoderSSEEvent{kind: qoderEventSkip}
}

// ── 思考标签拆解（stream.rs:205-421，跨分片长驻状态机）──────────

// qoderThinkingPiece 一段已拆解的产出。
type qoderThinkingPiece struct {
	text       string
	isThinking bool
}

// qoderThinkingParser 跨 chunk 的标签状态机。
//
// 为什么是长驻结构：拆解器要在缓冲区里留"可能是标签前缀"的尾巴（`<thi` +
// `nking>` 分两片到达）；每 chunk 现建一个的话，尾巴会在 chunk 结束时被当成
// 正文吐出去，跨分片拆解永远不会生效。
type qoderThinkingParser struct {
	out         []qoderThinkingPiece
	buffer      string
	inThinking  bool
	activeClose string // 进入思考时用的那个闭标签
	finished    bool
}

func newQoderThinkingParser() *qoderThinkingParser {
	return &qoderThinkingParser{activeClose: qoderThinkTags[0][1]}
}

// push 喂入一段增量。
func (p *qoderThinkingParser) push(chunk string) {
	if chunk == "" {
		return
	}
	p.buffer += chunk
	p.drain(false)
}

// finish 收尾：残留内容按当前状态输出（幂等）。
func (p *qoderThinkingParser) finish() {
	if p.finished {
		return
	}
	p.finished = true
	p.drain(true)
	if p.buffer != "" {
		rest := p.buffer
		p.buffer = ""
		p.emit(rest, p.inThinking)
	}
}

// take 取走当前累积的产出。
func (p *qoderThinkingParser) take() []qoderThinkingPiece {
	out := p.out
	p.out = nil
	return out
}

func (p *qoderThinkingParser) emit(text string, isThinking bool) {
	if text == "" {
		return
	}
	p.out = append(p.out, qoderThinkingPiece{text: text, isThinking: isThinking})
}

// trailingPrefixLen 尾巴有多长可能是某个标签的前缀（需留到下一片再判断）。
// 只在字符边界上、从长到短试。
func (p *qoderThinkingParser) trailingPrefixLen(text string) int {
	max := 0
	for _, pair := range qoderThinkTags {
		for _, tag := range []string{pair[0], pair[1]} {
			limit := len(text)
			if limit > len(tag)-1 {
				limit = len(tag) - 1
			}
			for l := limit; l >= 1; l-- {
				if !isRuneBoundary(text, len(text)-l) {
					continue
				}
				if strings.HasSuffix(text, tag[:l]) {
					if l > max {
						max = l
					}
					break
				}
			}
		}
	}
	return max
}

// openLenOf 由闭标签反查对应开标签的长度（进思考状态时跳过开标签用）。
func openLenOfQoder(closeTag string) int {
	for _, pair := range qoderThinkTags {
		if pair[1] == closeTag {
			return len(pair[0])
		}
	}
	return 0
}

// drain 状态机主体：正文里找开标签、思考里找闭标签。
// 空串早退 + guard 上限防病态输入下的无限循环（与源实现同目的）。
func (p *qoderThinkingParser) drain(isFinal bool) {
	guard := 0
	for p.buffer != "" && guard < 1000 {
		guard++

		if p.inThinking {
			close := p.activeClose
			if pos := strings.Index(p.buffer, close); pos >= 0 {
				if pos > 0 {
					p.emit(p.buffer[:pos], true)
				}
				rest := p.buffer[pos+len(close):]
				p.buffer = qoderStripLeadingNewline(rest)
				p.inThinking = false
				continue
			}
			if isFinal {
				text := p.buffer
				p.buffer = ""
				p.emit(text, true)
				return
			}
			keep := p.trailingPrefixLen(p.buffer)
			safe := len(p.buffer) - keep
			if safe > 0 {
				p.emit(p.buffer[:safe], true)
				p.buffer = p.buffer[safe:]
			}
			return
		}

		// 正文状态：找最早出现的开标签或闭标签。
		bestOpen := -1
		bestOpenClose := ""
		bestClose := -1
		for _, pair := range qoderThinkTags {
			if pos := strings.Index(p.buffer, pair[0]); pos >= 0 {
				if bestOpen < 0 || pos < bestOpen {
					bestOpen, bestOpenClose = pos, pair[1]
				}
			}
			if pos := strings.Index(p.buffer, pair[1]); pos >= 0 {
				if bestClose < 0 || pos < bestClose {
					bestClose = pos
				}
			}
		}
		takeOpen := false
		switch {
		case bestOpen >= 0 && bestClose >= 0:
			takeOpen = bestOpen < bestClose
		case bestOpen >= 0:
			takeOpen = true
		}

		if bestOpen >= 0 && takeOpen {
			if bestOpen > 0 {
				p.emit(p.buffer[:bestOpen], false)
			}
			openLen := openLenOfQoder(bestOpenClose)
			p.buffer = p.buffer[bestOpen+openLen:]
			p.activeClose = bestOpenClose
			p.inThinking = true
			continue
		}
		if bestClose >= 0 {
			// 单独的闭标签：丢弃它（源实现同款处理）。
			if bestClose > 0 {
				p.emit(p.buffer[:bestClose], false)
			}
			closeLen := 0
			for _, pair := range qoderThinkTags {
				if strings.HasPrefix(p.buffer[bestClose:], pair[1]) && len(pair[1]) > closeLen {
					closeLen = len(pair[1])
				}
			}
			rest := p.buffer[bestClose+closeLen:]
			p.buffer = qoderStripLeadingNewline(rest)
			continue
		}
		if isFinal {
			text := p.buffer
			p.buffer = ""
			p.emit(text, false)
			return
		}
		keep := p.trailingPrefixLen(p.buffer)
		safe := len(p.buffer) - keep
		if safe > 0 {
			p.emit(p.buffer[:safe], false)
			p.buffer = p.buffer[safe:]
		}
		return
	}
}

// isRuneBoundary 报告下标是否落在 UTF-8 字符边界上。
func isRuneBoundary(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return true
	}
	return s[i]&0xC0 != 0x80
}

// qoderStripLeadingNewline 吃掉闭标签后紧跟的换行（stream.rs:411-421）。
// 先试 `\n\n`（上游习惯闭标签后留一个空行），再 `\r\n`，再单个 `\n`。
func qoderStripLeadingNewline(text string) string {
	switch {
	case strings.HasPrefix(text, "\n\n"):
		return text[2:]
	case strings.HasPrefix(text, "\r\n"):
		return text[2:]
	case strings.HasPrefix(text, "\n"):
		return text[1:]
	}
	return text
}

// qoderStripThinkingTags 去掉文本里的思考标签（两处用途：reasoning_content
// 里偶带的标签；未启用拆解器时的兜底清理。stream.rs:424-432）。
func qoderStripThinkingTags(text string) string {
	out := text
	for _, pair := range qoderThinkTags {
		out = strings.ReplaceAll(out, pair[0], "")
		out = strings.ReplaceAll(out, pair[1], "")
	}
	return out
}
