// platform.go 平台能力注册表（账号中心 · 单一事实来源）。
//
// 平台事实过去散落在 account 常量 / validate / acctchan / acclogin / checkin /
// presets / 前端多处，新增平台靠字符串对齐，漏一处就是"账号能加但不投影/不能
// 登录/没有签到"。本表只收口**后端静态事实**：是否原生平台、适配器名、投影
// 默认 base_url、登录流程类型、签到/刷新能力。
//
// 登录/签到协议仍由 acclogin / checkin 各自注册实现；一致性由
// platform_test.go 强制（native == provider.NativeNames()、
// login == acclogin.SupportedPlatforms()、checkin == checkin.Platforms()）。
package account

// LoginMode 登录式添加的流程类型（面板能力开关与 acclogin 共用）。
type LoginMode string

const (
	// LoginNone 无浏览器登录流程，只能粘贴凭证。
	LoginNone LoginMode = ""
	// LoginDeviceCode PKCE 设备码轮询（qoder / qoder-intl）。
	LoginDeviceCode LoginMode = "device_code"
	// LoginSMS 手机号 + 短信验证码（autoclaw 国内）。
	LoginSMS LoginMode = "sms"
	// LoginCallback 本机回环回调（预留给 trae / lobsterai 一类平台）。
	LoginCallback LoginMode = "callback"
	// LoginOAuth 浏览器 OAuth + 本机回环回调（autoclaw-intl：阿里云滑块前置）。
	LoginOAuth LoginMode = "oauth"
)

// PlatformSpec 一个平台的静态能力描述。
type PlatformSpec struct {
	Key            string    `json:"key"`
	Name           string    `json:"name"`
	Native         bool      `json:"native"`
	Adapter        string    `json:"adapter,omitempty"`
	DefaultBaseURL string    `json:"default_base_url,omitempty"`
	Login          LoginMode `json:"login,omitempty"`
	Checkin        bool      `json:"checkin"`
	// Refresh 平台有登录态刷新实现（仍需 blob 带 refresh_token 才可执行）。
	Refresh bool `json:"refresh"`
	// Balance 平台有余额/额度查询实现（面板「余额」动作与 Hero 积分列）。
	Balance bool `json:"balance"`
}

// platformSpecs 平台清单（顺序即面板展示顺序；custom 恒在末尾）。
var platformSpecs = []PlatformSpec{
	{
		Key: PlatformLoomy, Name: "Loomy", Native: true, Adapter: PlatformLoomy,
		DefaultBaseURL: "https://loomyad.xunfei.cn/api/v1",
		Checkin:        true,
		Balance:        true,
	},
	{
		Key: PlatformAutoClaw, Name: "AutoClaw 国内", Native: true, Adapter: PlatformAutoClaw,
		DefaultBaseURL: "https://autoglm-acceleration-api.zhipuai.cn/autoclaw-proxy/proxy/autoclaw",
		Login:          LoginSMS,
		Checkin:        true,
		Refresh:        true,
		Balance:        true,
	},
	{
		Key: PlatformAutoClawIntl, Name: "AutoClaw 国际", Native: true, Adapter: PlatformAutoClawIntl,
		DefaultBaseURL: "https://autoglm-api.autoglm.ai/autoclaw-proxy/proxy/autoclaw",
		Login:          LoginOAuth,
		Checkin:        true,
		Refresh:        true,
		Balance:        true,
	},
	{
		Key: PlatformQoder, Name: "Qoder 国内", Native: true, Adapter: PlatformQoder,
		DefaultBaseURL: "https://gateway.qoder.com.cn",
		Login:          LoginDeviceCode,
		Checkin:        true,
		Refresh:        true,
		Balance:        true,
	},
	{
		Key: PlatformQoderIntl, Name: "Qoder 国际", Native: true, Adapter: PlatformQoderIntl,
		DefaultBaseURL: "https://api2.qoder.sh",
		Login:          LoginDeviceCode,
		Refresh:        true,
		Balance:        true,
	},
	{
		Key: PlatformTrae, Name: "TRAE（字节 · SOLO 国内）", Native: true, Adapter: PlatformTrae,
		DefaultBaseURL: "https://trae-api-cn.mchost.guru",
		Login:          LoginCallback,
		// 第二批实现与契约单测已就位（checkin/trae.go、trae_balance.go；⚠ 未真实
		// 账号验收——契约来自参考实现逆向，失败在台账显式报错不静默）。
		// 能力接线（本三项翻 true + checkin 注册 + refresh/QueryBalance 分发）在
		// 真实账号验收通过后一次性打开（参见 TraeChecker/RefreshTrae 注释）。
	},
	{
		Key: PlatformCodex, Name: "Codex（ChatGPT 订阅）", Native: true, Adapter: PlatformCodex,
		DefaultBaseURL: "https://chatgpt.com/backend-api/codex",
		// 刷新机已实现（checkin/codex_refresh.go，OAuth 刷新令牌换新令牌组，
		// ⚠ 未真实账号验收——契约来自参考实现逆向，失败在台账显式报错不静默）。
		// 无签到 / 无余额（ChatGPT 订阅无积分体系）；账号中心的凭证校验与投影
		// 接线在下一批打开（届时粘贴 auth.json 即用）。
		Refresh: true,
	},
	{
		Key: PlatformCustom, Name: "通用平台",
	},
}

// platformSpecByKey 查询索引（装配期构建，运行期只读）。
var platformSpecByKey = func() map[string]PlatformSpec {
	m := make(map[string]PlatformSpec, len(platformSpecs))
	for _, s := range platformSpecs {
		m[s.Key] = s
	}
	return m
}()

// SpecOf 按平台键取能力描述。
func SpecOf(platform string) (PlatformSpec, bool) {
	s, ok := platformSpecByKey[platform]
	return s, ok
}

// Specs 返回平台清单副本（顺序稳定；面板/测试用）。
func Specs() []PlatformSpec {
	out := make([]PlatformSpec, len(platformSpecs))
	copy(out, platformSpecs)
	return out
}

// CanRefresh 报告账号是否可执行登录态刷新（平台有实现 + blob 带 refresh_token）。
func CanRefresh(a *Account) bool {
	if a == nil {
		return false
	}
	s, ok := SpecOf(a.Platform)
	if !ok || !s.Refresh {
		return false
	}
	m, err := decodeBlob(a.Cred)
	if err != nil {
		return false
	}
	return blobString(m, "refresh_token", "refreshToken") != ""
}

// LoginPlatforms 支持登录式添加的平台清单（顺序同 Specs）。
func LoginPlatforms() []string {
	out := make([]string, 0, 3)
	for _, s := range platformSpecs {
		if s.Login != LoginNone {
			out = append(out, s.Key)
		}
	}
	return out
}
