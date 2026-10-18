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
	case PlatformCustom:
		if s := blobString(m, "secret"); s != "" {
			return s, nil
		}
		return "", errField("secret", "不能为空")
	default:
		return "", fmt.Errorf("未知平台 %q", platform)
	}
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
	switch platform {
	case PlatformLoomy:
		return "https://loomyad.xunfei.cn/api/v1", nil
	case PlatformAutoClaw:
		return "https://autoglm-acceleration-api.zhipuai.cn/autoclaw-proxy/proxy/autoclaw", nil
	case PlatformAutoClawIntl:
		return "https://autoglm-api.autoglm.ai/autoclaw-proxy/proxy/autoclaw", nil
	case PlatformQoder:
		return "https://gateway.qoder.com.cn", nil
	case PlatformQoderIntl:
		return "https://api2.qoder.sh", nil
	case PlatformCustom:
		return "", errField("base_url", "不能为空")
	default:
		return "", fmt.Errorf("未知平台 %q", platform)
	}
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
	if NativePlatform(platform) {
		return platform
	}
	return ""
}
