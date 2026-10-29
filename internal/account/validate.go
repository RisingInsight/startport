// validate.go 平台凭证 blob 的最小 schema 校验与主凭证提取（架构 §3.1 表）。
//
// Store 不理解平台语义（Cred 原样落盘）；平台校验集中在本文件，供 Add 与
// 面板预检共用（fail fast，报错带字段名）。主凭证提取（PrimarySecret）与
// 基址解析（BaseURLOf）供 acctchan 投影使用。
package account

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/RisingInsight/startport/internal/upstream"
)

// DecodeBlob 宽松解码 blob（导出给 acctchan 读 protocol/base_url 等投影字段；
// Store 本身不理解平台语义）。
func DecodeBlob(cred json.RawMessage) (map[string]any, error) {
	return decodeBlob(cred)
}

// blob 解码用的宽松形态：字段名/大小写按各平台 schema，未列出的字段原样保留
// 在 map 里（Cred 落盘不动）。
func decodeBlob(cred json.RawMessage) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal(cred, &m); err != nil {
		return nil, fmt.Errorf("凭证 blob 必须是 JSON 对象: %w", err)
	}
	if m == nil {
		return nil, fmt.Errorf("凭证 blob 不能为空")
	}
	return m, nil
}

func blobString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok {
			if v := strings.TrimSpace(s); v != "" {
				return v
			}
		}
	}
	return ""
}

func errField(field, why string) error {
	return fmt.Errorf("凭证字段 %s: %s", field, why)
}

// ValidateCred 平台凭证 blob 校验（架构 §3.1 表；fail fast 报错带字段名）。
//
// custom 平台的 base_url 走与配置层同一条 SSRF 红线
// （upstream.ValidateOutboundBaseAllowPrivate）——账号导入与手写配置同一标准。
func ValidateCred(platform string, cred json.RawMessage) error {
	m, err := decodeBlob(cred)
	if err != nil {
		return err
	}
	switch platform {
	case PlatformLoomy:
		session := blobString(m, "session")
		if session == "" {
			return errField("session", "不能为空（Loomy 客户端登录态里的 session）")
		}
		if len(session) < 16 {
			return errField("session", "过短（长度 ≥16）")
		}
		return nil
	case PlatformAutoClaw, PlatformAutoClawIntl:
		token := blobString(m, "token")
		if token == "" {
			return errField("token", "不能为空（桌面端 openclaw.json 的 X-Authorization token）")
		}
		return nil // refresh_token/device_id/user_id 可选；缺失 = 无法自动刷新（账号页提示）
	case PlatformQoder, PlatformQoderIntl:
		token := blobString(m, "access_token", "accessToken")
		if token == "" {
			return errField("access_token", "不能为空（dt-/jt- 前缀皆可，分流在适配器）")
		}
		if uid := blobString(m, "user_id", "userId"); uid == "" {
			return errField("user_id", "不能为空（COSY 签名需要用户标识）")
		}
		return nil
	case PlatformTrae:
		if traeField(m, "accessToken", "access_token") == "" {
			return errField("accessToken", "不能为空（Trae 登录态里的 Cloud-IDE-JWT）")
		}
		switch strings.ToLower(traeField(m, "variant")) {
		case "intl", "solo-intl":
			return errField("variant", "国际版暂未接入（当前仅支持国内 SOLO 通道）")
		}
		return nil
	case PlatformKuku:
		// 库库AI 的凭证是**网页版登录 Cookie**（BDUSS/STOKEN 为 httpOnly，只有
		// 浏览器里那份完整 Cookie 能用），没有 OAuth 流程。
		// 这里只做形态校验：非空 + 长度下限 + 必须带 BDUSS（百度登录态的核心键）。
		// 不做联网校验——账号中心允许先入库、后点「余额」再验证登录态。
		cookie := blobString(m, "cookie")
		if cookie == "" {
			return errField("cookie", "不能为空（浏览器里复制的整串 Cookie，需含 BDUSS=…）")
		}
		if len(cookie) < 16 {
			return errField("cookie", "过短（长度 ≥16，应是完整 Cookie 串）")
		}
		if !strings.Contains(cookie, "BDUSS=") {
			return errField("cookie", "缺少 BDUSS（百度登录态的核心 Cookie，请复制完整串）")
		}
		return nil
	case PlatformCustom:
		base := blobString(m, "base_url")
		if base == "" {
			return errField("base_url", "不能为空（custom 平台的地址属于账号）")
		}
		u, err := url.Parse(base)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return errField("base_url", "必须是 http(s) URL")
		}
		if blobString(m, "secret") == "" {
			return errField("secret", "不能为空")
		}
		// SSRF 红线沿用配置层（内网需 allow_private 显式放行；元数据端点恒拒）。
		allowPrivate := false
		if ap, ok := m["allow_private"].(bool); ok {
			allowPrivate = ap
		}
		return upstream.ValidateOutboundBaseAllowPrivate(base, "cred.base_url", allowPrivate)
	default:
		return fmt.Errorf("未知平台 %q", platform)
	}
}

// PrimarySecret 投影用主凭证（架构 §4.1 规则 4）：
// loomy→session；autoclaw(-intl)→token；qoder(-intl)→**适配器形态的账号 JSON**
// （provider/qoder.go 的 parseQoderSecret 吃整个 JSON，需要 uid/machine_id）；
// custom→secret。其余 blob 字段（refresh_token 等）不进渠道——它们属于账号
// 生命周期，留在 accounts.json。
func PrimarySecret(platform string, cred json.RawMessage) (string, error) {
	m, err := decodeBlob(cred)
	if err != nil {
		return "", err
	}
	switch platform {
	case PlatformLoomy:
		if s := blobString(m, "session"); s != "" {
			return s, nil
		}
		return "", errField("session", "不能为空")
	case PlatformAutoClaw, PlatformAutoClawIntl:
		if s := blobString(m, "token"); s != "" {
			return s, nil
		}
		return "", errField("token", "不能为空")
	case PlatformKuku:
		// 账号型平台（AccountOnly）不投影成聊天渠道，本函数实际不会被投影链路
		// 调用；仍然返回 cookie 以保持"主凭证"语义完整（面板/导出需要它）。
		if s := blobString(m, "cookie"); s != "" {
			return s, nil
		}
		return "", errField("cookie", "不能为空")
	case PlatformQoder, PlatformQoderIntl:
		token := blobString(m, "access_token", "accessToken")
		if token == "" {
			return "", errField("access_token", "不能为空")
		}
		userID := blobString(m, "user_id", "userId")
		if userID == "" {
			return "", errField("user_id", "不能为空")
		}
		// 适配器形态（provider/qoder.go parseQoderSecret 的键名约定）。
		// mode 按 platform 推导（qoder→cn、qoder-intl→global），blob.mode 可覆盖。
		mode := "cn"
		if platform == PlatformQoderIntl {
			mode = "global"
		}
		if mMode := blobString(m, "mode"); mMode != "" {
			mode = mMode
		}
		identity := map[string]any{
			"mode":        mode,
			"accessToken": token,
			"userId":      userID,
			"machineId":   blobString(m, "machine_id", "machineId"),
			"nickname":    blobString(m, "nickname", "name"),
			"email":       blobString(m, "email"),
		}
		out, err := json.Marshal(identity)
		if err != nil {
			return "", fmt.Errorf("序列化 Qoder 凭证失败: %w", err)
		}
		return string(out), nil
	case PlatformTrae:
		return traePrimarySecret(m)
	case PlatformCustom:
		if s := blobString(m, "secret"); s != "" {
			return s, nil
		}
		return "", errField("secret", "不能为空")
	default:
		return "", fmt.Errorf("未知平台 %q", platform)
	}
}

// traeField 读 Trae blob 字段：嵌套 auth{} 优先，其次平铺。
func traeField(m map[string]any, keys ...string) string {
	auth := m
	if nested, ok := m["auth"].(map[string]any); ok {
		auth = nested
	}
	if s := blobString(auth, keys...); s != "" {
		return s
	}
	return blobString(m, keys...)
}

// traePrimarySecret 把 Trae 账号 blob 归一成适配器形态（平铺 camelCase）。
// 兼容嵌套 CPA 形状 {auth:{…},account:{…}} 与手工粘贴的平铺形态；只保留
// 已知字段，未知扩展字段不进入渠道 secret。
func traePrimarySecret(m map[string]any) (string, error) {
	auth := m
	if nested, ok := m["auth"].(map[string]any); ok {
		auth = nested
	}
	accountPart, _ := m["account"].(map[string]any)
	pick := func(keys ...string) string {
		for _, src := range []map[string]any{auth, m, accountPart} {
			if src == nil {
				continue
			}
			if s := blobString(src, keys...); s != "" {
				return s
			}
		}
		return ""
	}
	token := pick("accessToken", "access_token")
	if token == "" {
		return "", errField("accessToken", "不能为空")
	}
	variant := pick("variant")
	if variant == "" {
		variant = "solo"
	}
	out := map[string]any{"accessToken": token, "variant": variant}
	for key, aliases := range map[string][]string{
		"refreshToken":     {"refreshToken", "refresh_token"},
		"domain":           {"domain"},
		"apiHost":          {"apiHost", "api_host"},
		"machineId":        {"machineId", "machine_id"},
		"deviceId":         {"deviceId", "device_id"},
		"uid":              {"uid", "user_id", "userId"},
		"enterpriseId":     {"enterpriseId", "enterprise_id"},
		"nickname":         {"nickname"},
		"devicePublicKey":  {"devicePublicKey", "device_public_key"},
		"devicePrivateKey": {"devicePrivateKey", "device_private_key"},
		"authClientId":     {"authClientId", "clientId", "ClientID"},
	} {
		if v := pick(aliases...); v != "" {
			out[key] = v
		}
	}
	// expiresAt 可能是秒/毫秒数字或字符串——原样保留，适配器侧不依赖它。
	for _, src := range []map[string]any{auth, m} {
		if src == nil {
			continue
		}
		for _, k := range []string{"expiresAt", "expires_at"} {
			if v, ok := src[k]; ok && v != nil {
				out["expiresAt"] = v
				break
			}
		}
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("序列化 Trae 凭证失败: %w", err)
	}
	return string(raw), nil
}

// BaseURLOf 投影用的对话基址（架构 §4.1 规则 1 的配套决策）：
//   - custom：blob.base_url（必填）；
//   - 原生平台：blob.base_url 可覆盖，缺省用各平台官方默认基址（来源：各适配器
//     的 preset note 与 ref endpoints）——"导入即用"要求投影必须能给出 base_url，
//     而渠道级字段以 config 为准：已存在的 managed 渠道永远保留用户改过的值。
func BaseURLOf(platform string, cred json.RawMessage) (string, error) {
	m, err := decodeBlob(cred)
	if err != nil {
		return "", err
	}
	if override := blobString(m, "base_url"); override != "" {
		return strings.TrimRight(override, "/"), nil
	}
	spec, ok := SpecOf(platform)
	if !ok {
		return "", fmt.Errorf("未知平台 %q", platform)
	}
	if spec.DefaultBaseURL == "" {
		// custom（及其它无默认基址的平台）：base_url 属于账号，必须由用户填写。
		return "", errField("base_url", "不能为空")
	}
	return spec.DefaultBaseURL, nil
}

// AllowPrivateOf blob 显式放行内网的标记（架构 §4.1 配套：账号 blob 的
// allow_private 会被投影带进渠道的 allow_private——SSRF 红线不变：放行内网
// 但元数据端点恒拒；校验仍由 config 层执行）。
func AllowPrivateOf(platform string, cred json.RawMessage) bool {
	m, err := decodeBlob(cred)
	if err != nil {
		return false
	}
	if ap, ok := m["allow_private"].(bool); ok {
		return ap
	}
	return false
}

// AdapterOf 平台对应的原生适配器名（custom = 空，投影为普通渠道）。
func AdapterOf(platform string) string {
	s, ok := SpecOf(platform)
	if !ok || !s.Native {
		return ""
	}
	return s.Adapter
}
