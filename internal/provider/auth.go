// auth.go 凭证注入形态（bearer / x-api-key / query / custom）与脱敏工具。
//
// 安全红线（PRD P0-4 / P0-5）：
//   - 传输层头（Host/Content-Length/Transfer-Encoding/Connection/...）一律禁止
//     由配置覆盖——Host 由 base_url 决定，覆盖它等于把请求发到别处；
//   - query 方式注入的秘密值只存在于 URL 里，任何日志/错误体必须先用
//     ScrubURL / ScrubSecrets 脱敏（Gemini 的 ?key= 是最常见的泄露点；
//     上游 400 有时会把请求 URL 原样回显在错误 message 里）。
package provider

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// AuthMode 鉴权注入形态。
const (
	// AuthBearer Authorization: Bearer <secret>（缺省；老配置行为不变）。
	AuthBearer = "bearer"
	// AuthXAPIKey x-api-key: <secret>（Anthropic 原生形态）。
	AuthXAPIKey = "x-api-key"
	// AuthQuery URL 查询参数 <param>=<secret>（Gemini 兼容开关）。
	AuthQuery = "query"
	// AuthCustom 自定义头 <header>: <secret>（如 Gemini 的 x-goog-api-key）。
	AuthCustom = "custom"
)

// AuthSpec 凭证注入规格。Mode 为空 = 协议默认（openai→bearer、
// anthropic→x-api-key、gemini→x-goog-api-key）。
type AuthSpec struct {
	Mode   string `json:"mode,omitempty"`   // bearer / x-api-key / query / custom
	Param  string `json:"param,omitempty"`  // query 方式的参数名，缺省 "key"
	Header string `json:"header,omitempty"` // custom 方式的头名
}

// IsZero 报告是否为零值（凭证级 auth 为空 = 继承渠道级）。
func (a AuthSpec) IsZero() bool {
	return strings.TrimSpace(a.Mode) == "" && strings.TrimSpace(a.Param) == "" && strings.TrimSpace(a.Header) == ""
}

// authModes 合法鉴权模式清单（错误文案用）。
var authModes = []string{AuthBearer, AuthXAPIKey, AuthQuery, AuthCustom}

// headerNameRe 头名合法性（RFC 7230 token 的宽松子集）。
var headerNameRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// queryParamRe 查询参数名（Gemini 系还有 key[0] 这种形态）。
var queryParamRe = regexp.MustCompile(`^[A-Za-z0-9_\[\].-]{1,32}$`)

// reservedHeaders 传输层头黑名单：命中即拒绝（Host 由 base_url 决定）。
var reservedHeaders = map[string]bool{
	"host":                true,
	"content-length":      true,
	"transfer-encoding":   true,
	"connection":          true,
	"upgrade":             true,
	"trailer":             true,
	"te":                  true,
	"proxy-connection":    true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
}

// HeaderForbidden 报告头名是否属于传输层黑名单（大小写不敏感）。
// Proxy-* 前缀整段禁止（逐跳代理头）。
func HeaderForbidden(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return true
	}
	if reservedHeaders[n] {
		return true
	}
	return strings.HasPrefix(n, "proxy-")
}

// Validate 校验鉴权规格（config 层与面板探测共用，错误文案含字段语义）。
func (a AuthSpec) Validate(field string) error {
	mode := strings.ToLower(strings.TrimSpace(a.Mode))
	switch mode {
	case "", AuthBearer, AuthXAPIKey:
		if a.Param != "" {
			return fmt.Errorf("%s.param: 只有 mode=query 时才能声明 param", field)
		}
		if a.Header != "" {
			return fmt.Errorf("%s.header: 只有 mode=custom 时才能声明 header", field)
		}
		return nil
	case AuthQuery:
		p := strings.TrimSpace(a.Param)
		if p == "" {
			p = "key" // 缺省参数名
		}
		if !queryParamRe.MatchString(p) {
			return fmt.Errorf("%s.param: %q 含非法字符（允许字母/数字/下划线/中括号/点/连字符，≤32）", field, p)
		}
		if a.Header != "" {
			return fmt.Errorf("%s.header: mode=query 时不能声明 header", field)
		}
		return nil
	case AuthCustom:
		h := strings.TrimSpace(a.Header)
		if h == "" {
			return fmt.Errorf("%s.header: mode=custom 时必须声明 header", field)
		}
		if !headerNameRe.MatchString(h) {
			return fmt.Errorf("%s.header: %q 含非法字符（允许字母/数字/连字符，≤64）", field, h)
		}
		if HeaderForbidden(h) {
			return fmt.Errorf("%s.header: %q 是传输层头，禁止覆盖（Host 由 base_url 决定）", field, h)
		}
		return nil
	default:
		return fmt.Errorf("%s.mode: %q 不是合法值（可用：%s）", field, a.Mode, strings.Join(authModes, " / "))
	}
}

// Normalized 返回生效的鉴权规格：显式声明优先；空则按协议默认。
//
// 协议默认：openai→bearer；anthropic→自定义 x-api-key（Anthropic 原生形态）；
// gemini→自定义 x-goog-api-key（官方推荐 header 形态，不把 key 落 URL）。
func (a AuthSpec) Normalized(p Protocol) AuthSpec {
	if strings.TrimSpace(a.Mode) != "" {
		out := AuthSpec{
			Mode:   strings.ToLower(strings.TrimSpace(a.Mode)),
			Param:  strings.TrimSpace(a.Param),
			Header: strings.TrimSpace(a.Header),
		}
		if out.Mode == AuthQuery && out.Param == "" {
			out.Param = "key"
		}
		return out
	}
	switch p {
	case ProtocolAnthropic:
		return AuthSpec{Mode: AuthCustom, Header: "x-api-key"}
	case ProtocolGemini:
		return AuthSpec{Mode: AuthCustom, Header: "x-goog-api-key"}
	default:
		return AuthSpec{Mode: AuthBearer}
	}
}

// AuthHeaderName 返回该规格最终写入的 HTTP 头名（观测定性用；query 返回空）。
func (a AuthSpec) AuthHeaderName() string {
	switch a.Mode {
	case AuthBearer:
		return "Authorization"
	case AuthXAPIKey:
		return "x-api-key"
	case AuthCustom:
		return a.Header
	default:
		return ""
	}
}

// Apply 把秘密值注入请求（头或 URL 查询参数）。
//
// 规则：
//   - secret 为空 = 不注入任何鉴权（自建内网 one-api 场景合法）；
//   - 目标头已有值（静态 headers 显式给了）时以静态头为准——避免两个秘密值
//     争夺同一个头，也支持 "Authorization: <raw>" 这种无 Bearer 前缀的平台；
//   - query 方式直接改 u.RawQuery（调用方需保证 u 非 nil）。
func Apply(h http.Header, u *url.URL, spec AuthSpec, secret string) {
	if secret == "" {
		return
	}
	switch spec.Mode {
	case AuthBearer:
		if h.Get("Authorization") == "" {
			h.Set("Authorization", "Bearer "+secret)
		}
	case AuthXAPIKey:
		if h.Get("x-api-key") == "" {
			h.Set("x-api-key", secret)
		}
	case AuthCustom:
		if spec.Header == "" || HeaderForbidden(spec.Header) {
			return // 校验层已拒绝；防御性跳过（绝不覆盖传输层头）
		}
		if h.Get(spec.Header) == "" {
			h.Set(spec.Header, secret)
		}
	case AuthQuery:
		param := spec.Param
		if param == "" {
			param = "key"
		}
		if u == nil {
			return
		}
		q := u.Query()
		q.Set(param, secret)
		u.RawQuery = q.Encode()
	}
}

// sensitiveQueryKeys URL 查询参数里需要脱敏的键（小写比较）。
var sensitiveQueryKeys = map[string]bool{
	"key":          true,
	"api_key":      true,
	"apikey":       true,
	"token":        true,
	"access_token": true,
	"secret":       true,
	"x-api-key":    true,
	"alt":          false, // 非敏感（Gemini 的 alt=sse 要保留可读）
}

// ScrubURL 抹掉 URL 查询参数里的秘密值（保留 host/path 与参数名）。
// 解析失败时退化为「不输出 query」的保守形态。
func ScrubURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		if i := strings.IndexByte(raw, '?'); i >= 0 {
			return raw[:i] + "?***"
		}
		return raw
	}
	if u.RawQuery == "" {
		return raw
	}
	q := u.Query()
	changed := false
	for k := range q {
		if sensitiveQueryKeys[strings.ToLower(k)] {
			q.Set(k, "***")
			changed = true
		}
	}
	if !changed {
		// 无法判定键名语义时也不原样输出 query（宁可少一个观测维度也不泄露）。
		u.RawQuery = "***"
		return u.String()
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// ScrubSecrets 把文本里出现的秘密值字面量替换为 ***，并对其中出现的 URL 再做
// 一次 ScrubURL（上游错误常把完整请求 URL 回显在 message 里）。
func ScrubSecrets(s string, secrets ...string) string {
	out := s
	for _, sec := range secrets {
		sec = strings.TrimSpace(sec)
		if sec == "" || len(sec) < 4 {
			continue // 太短的值替换会误伤正文
		}
		out = strings.ReplaceAll(out, sec, "***")
	}
	// URL 形态的片段（http/https 开头，到空白或引号为止）单独脱敏。
	out = scrubURLsInText(out)
	return out
}

// urlInTextRe 文本里的 URL 片段（保守匹配：到空白/引号/尖括号为止）。
var urlInTextRe = regexp.MustCompile(`https?://[^\s"'<>]+`)

// scrubURLsInText 对文本中每个 URL 片段做 query 脱敏。
func scrubURLsInText(s string) string {
	return urlInTextRe.ReplaceAllStringFunc(s, func(m string) string {
		if strings.Contains(m, "?") {
			return ScrubURL(m)
		}
		return m
	})
}
