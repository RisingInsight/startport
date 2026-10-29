// kuku.go 库库AI（百度 GenFlow）**占位适配器**——只负责注册，不做转发。
//
// ── 为什么需要这个文件 ──────────────────────────────────────
// 库库AI 是消费级智能体产品（kuku.baidu.com），没有对话 API：它的「对话」走
// `/wenchain/genflowpro/sendmsg` + SSE 任务协议，背后是技能/工作区/多步任务，
// 无法映射成 OpenAI Chat Completions 的语义。本项目接它的目的只有一个——
// 把积分/额度纳入账号中心看板（PlatformSpec.AccountOnly=true）。
//
// 但注册表有硬约束：`Native` 平台必须能在 provider.NativeNames() 里找到同名
// 适配器（internal/account/platform_test.go 强制三向一致），否则账号能加、
// 投影却找不到适配器。所以这里放一个**只报错、不转发**的实现占位。
//
// ── 硬约束 ──────────────────────────────────────────────────
//   - 任何出站方法都必须返回可读错误，**绝不发请求**——库库AI 的 Cookie 一旦
//     被误用于非预期端点，等于拿用户登录态乱打上游；
//   - 错误文案必须点明「去账号中心看额度」，避免用户以为是网关故障；
//   - 不做任何凭证解析（Cookie 只给 checkin/kuku_balance.go 用）。
package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
)

// KukuAdapterName 库库AI 的适配器注册名（= account.PlatformKuku）。
const KukuAdapterName = "kuku"

// ErrKukuNoChat 库库AI 不支持对话转发（哨兵：调用方/测试可判定）。
var ErrKukuNoChat = errors.New("库库AI 是账号/额度型平台，不提供对话转发：请在账号中心查看额度，不要把它当作模型渠道使用")

type kukuAdapter struct{}

// init 装配期注册（与其它原生适配器同一条路径）。
func init() { RegisterNative(kukuAdapter{}) }

func (kukuAdapter) Name() string { return KukuAdapterName }

// Protocol 声明 openai 形态仅为满足接口（永远不会真正出站）。
func (kukuAdapter) Protocol() Protocol { return ProtocolOpenAI }

func (kukuAdapter) BuildRequest(context.Context, *Target, []byte, bool) (*http.Request, error) {
	return nil, ErrKukuNoChat
}

func (kukuAdapter) ConvertResponse(*Target, int, []byte) []byte {
	// 防御：正常流程走不到（BuildRequest 已失败）。仍返回合法 OpenAI 错误信封，
	// 避免上游把 nil/空体当成功。
	return []byte(`{"error":{"message":"` + ErrKukuNoChat.Error() + `","type":"api_error","code":"kuku_no_chat"}}`)
}

func (kukuAdapter) ConvertStream(*Target, string, io.ReadCloser) (io.ReadCloser, error) {
	return nil, ErrKukuNoChat
}

func (kukuAdapter) BuildModels(context.Context, *Target) (*http.Request, error) {
	return nil, ErrKukuNoChat
}

func (kukuAdapter) DecodeModels(*Target, int, []byte) ([]string, error) {
	return nil, ErrKukuNoChat
}
