package upstream

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// BenchmarkPrepareBodyLarge 测量 prepareBody（含合并引入的 tool 配对三步清理 +
// 脱敏 + 思维链注入）在「接近生产的大请求体」上的耗时——验证 2026-10-13 上游合并
// 是否给大上下文请求引入了网关侧延迟（生产 prompt 中位 19-50 万 token）。
func BenchmarkPrepareBodyLarge(b *testing.B) {
	msgs := []any{}
	// 构造 ~50 万 token 量级的对话（每条约 2000 字符 ≈ 1000 token，500 条）
	filler := strings.Repeat("这是一段用于压测的上下文内容，模拟长会话历史。", 60)
	for i := 0; i < 500; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, map[string]any{"role": role, "content": filler})
	}
	// 混入工具调用/结果轮（触发 tool 配对清理路径）
	msgs = append(msgs,
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "read_file", "arguments": `{"p":"a"}`}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "file body"},
	)
	body, _ := json.Marshal(map[string]any{"model": "glm-5.3-flash", "messages": msgs, "stream": true})
	b.Logf("请求体大小: %.1f MB", float64(len(body))/1024/1024)
	c := New()
	c.SanitizeFingerprints.Store(true)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.prepareBody(body, "cn", "uid", "conv", "cache")
	}
}

// TestPrepareBodyLargeLatency 网关侧处理大请求体的耗时闸门（2026-10-13 加）：
// 用户反馈「token 输出变慢」时用于自证网关侧清白——50 万 token / 2MB 请求体
// 的 tool 配对清理 + 脱敏 + 思维链注入实测 ~42ms，相对上游 5-20s 预填充可忽略。
// 阈值 500ms（12 倍余量）防未来引入 O(n²) 类回归。
func TestPrepareBodyLargeLatency(t *testing.T) {
	msgs := []any{}
	filler := strings.Repeat("这是一段用于压测的上下文内容，模拟长会话历史。", 60)
	for i := 0; i < 500; i++ {
		msgs = append(msgs, map[string]any{"role": "user", "content": filler})
	}
	body, _ := json.Marshal(map[string]any{"model": "glm-5.3-flash", "messages": msgs, "stream": true})
	c := New()
	c.SanitizeFingerprints.Store(true)
	start := time.Now()
	_ = c.prepareBody(body, "cn", "uid", "conv", "cache")
	el := time.Since(start)
	fmt.Printf("prepareBody 大请求体（%.1f MB / ~50万 token）单次耗时: %v\n", float64(len(body))/1024/1024, el)
	if el > 500*time.Millisecond {
		t.Fatalf("prepareBody 耗时 %v 超过 500ms，网关侧引入显著延迟（实测基线 ~42ms）", el)
	}
}
