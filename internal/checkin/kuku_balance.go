// kuku_balance.go 库库AI（百度 GenFlow）积分 / 额度查询。
//
// ── 上游形态（逆向来源：kuku.baidu.com 的 pc-genflow-pro 1.7.0 前端 bundle）──
//
//	积分首页  GET {base}/api/genflowpro/freepoint/homenew
//	          → {"data":{...},"errno":0,"show_msg":"","request_id":…}
//	VIP 余量  GET {base}/bizapi/gfpro/getgfvipremain
//	          → {"status":{"code":0,"msg":"ok"},"data":{isVip,isExpire,vipType,vipEndTime,…}}
//	我的资料  GET {base}/api/genflowpro/settings/profile
//	          → {"data":{uk,nickname,phone,…},"errno":0}
//
// 鉴权：**网页版登录 Cookie**（BDUSS/STOKEN 为 httpOnly），没有 OAuth，也没有
// refresh——Cookie 失效只能重新登录再粘贴。
//
// ── errno 语义（前端 credits-activity-api 与实测未登录响应确认）──────────
//
//	 0        成功
//	-6        未登录（Cookie 失效）
//	 2        参数错误（未登录时服务端拿不到 uk，也报这个）
//	 151004   该活动未开放（前端按「无活动」处理，不报错）
//
// ── 契约状态：⚠ 未用真实账号验收 ──────────────────────────────
// `data` 里额度字段的**确切键名**还没见过（未登录响应只有 `{"activities":[]}`），
// 所以本实现采用「按候选键名宽松提取 + 提不到就显式报错」的策略：
//   - 不编造数字：一个候选都没命中就返回错误并带上实际 key 清单，便于一次修正；
//   - 字段名一旦由 scripts/probe_kuku.py 确认，把 kukuPointFields 收紧即可。
package checkin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/RisingInsight/startport/internal/account"
	"github.com/RisingInsight/startport/internal/netdiag"
)

// kukuDefaultBase 库库AI 网页版根地址（账号 blob 的 base_url 可覆盖，便于换域）。
const kukuDefaultBase = "https://kuku.baidu.com"

const (
	kukuPointsPath  = "/api/genflowpro/freepoint/homenew"
	kukuVIPPath     = "/bizapi/gfpro/getgfvipremain"
	kukuProfilePath = "/api/genflowpro/settings/profile"
)

// kukuPointFields 额度字段候选名（小写比较；按「越靠前越权威」排序）。
//
// 这份表是**待确认契约**：真实响应回来之前，靠它兜住最常见的几种命名；
// 确认后应当收敛到确切键名，避免把无关数值（如任务进度）误当余额。
var kukuPointFields = []string{
	"totalpoint", "total_point", "totalpoints",
	"point", "points",
	"credit", "credits",
	"balance", "available", "remaining", "remain", "left", "quota",
}

// queryKukuBalance 账号中心「余额」动作的实现。
func queryKukuBalance(ctx context.Context, a *account.Account) (*account.BalanceSnapshot, error) {
	blob, err := decodeBlob(a)
	if err != nil {
		return nil, err
	}
	cookie := blobString(blob, "cookie")
	if cookie == "" {
		return nil, authExpiredf("账号 blob 缺少 cookie")
	}
	base := blobString(blob, "base_url")
	if base == "" {
		base = kukuDefaultBase
	}
	return queryKukuBalanceAt(ctx, base, cookie)
}

// queryKukuBalanceAt 便于测试注入 base（与 loomy/qoder 的同名函数同构）。
func queryKukuBalanceAt(ctx context.Context, base, cookie string) (*account.BalanceSnapshot, error) {
	base = strings.TrimRight(base, "/")

	// 1) 积分首页：主来源。拿不到就直接失败——这是余额的定义性接口。
	data, err := kukuGetEnvelope(ctx, base+kukuPointsPath, cookie)
	if err != nil {
		return nil, err
	}
	nums := kukuCollectNumbers(data, 2)
	rows, available := kukuPickRows(nums)
	if len(rows) == 0 {
		return nil, fmt.Errorf("积分响应里没有可识别的额度字段（平台接口可能已变更；实际键：%s）",
			kukuKeysOf(data))
	}

	snap := &account.BalanceSnapshot{
		Available: available,
		Unit:      "积分",
		Rows:      rows,
		FetchedAt: time.Now(),
	}

	// 2) VIP 余量：**尽力而为**。失败不影响积分结果（该接口是另一套信封，
	//    偶发 200001「user not login」时积分可能仍然可用，不连坐）。
	if vip, err := kukuGetVIP(ctx, base+kukuVIPPath, cookie); err == nil && vip != nil {
		if vip.IsVip && !vip.IsExpire {
			snap.PlanName = "VIP"
			if vip.VipType != "" {
				snap.PlanName = "VIP（" + vip.VipType + "）"
			}
		}
		if !vip.ExpiresAt.IsZero() {
			snap.ExpiresAt = vip.ExpiresAt
		}
		if vip.Remain > 0 {
			snap.Total = vip.Remain
		}
	}
	return snap, nil
}

// kukuGetEnvelope 发一次 GET 并解出 `{data, errno, show_msg}` 信封的 data。
//
// errno 语义见文件头；-6 / HTTP 401/403 一律翻译成 ErrAuthExpired，
// 让调度器与面板能给出「请重新粘贴 Cookie」而不是笼统的失败。
func kukuGetEnvelope(ctx context.Context, url, cookie string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("构造库库AI请求失败")
	}
	kukuHeaders(req, cookie)
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("库库AI请求失败（网络）%s", netdiag.ProxyHint(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, authExpiredf("HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Data    json.RawMessage `json:"data"`
		Errno   int             `json:"errno"`
		ShowMsg string          `json:"show_msg"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("库库AI响应形状不符（平台接口可能已变更）")
	}
	switch payload.Errno {
	case 0:
		// ok
	case -6:
		return nil, authExpiredf("业务码 -6（未登录）")
	case 2:
		// 未登录时服务端拿不到 uk 也会报 2；有 show_msg 时带上便于排查。
		return nil, authExpiredf("业务码 2（%s）", kukuMsg(payload.ShowMsg))
	case 151004:
		// 活动未开放：不是错误，返回空 data 让上层按「无额度字段」处理。
		return map[string]any{}, nil
	default:
		return nil, fmt.Errorf("库库AI返回业务错误 errno=%d：%s", payload.Errno, kukuMsg(payload.ShowMsg))
	}
	var data map[string]any
	if len(payload.Data) == 0 {
		return map[string]any{}, nil
	}
	if err := json.Unmarshal(payload.Data, &data); err != nil {
		return nil, fmt.Errorf("库库AI数据形状不符（平台接口可能已变更）")
	}
	// 有的接口把实体再包一层 data.data（与 loomy 的同款兼容）。
	if inner, ok := data["data"].(map[string]any); ok {
		return inner, nil
	}
	return data, nil
}

// kukuVIPInfo VIP 余量接口的解析结果（字段名取自前端 vip-remain-store）。
type kukuVIPInfo struct {
	IsVip     bool
	IsExpire  bool
	VipType   string
	ExpiresAt time.Time
	Remain    float64
}

func kukuGetVIP(ctx context.Context, url, cookie string) (*kukuVIPInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	kukuHeaders(req, cookie)
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var payload struct {
		Status struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		} `json:"status"`
		Data map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	if payload.Status.Code != 0 {
		return nil, fmt.Errorf("VIP 接口 code=%d %s", payload.Status.Code, payload.Status.Msg)
	}
	info := &kukuVIPInfo{}
	if v, ok := payload.Data["isVip"].(bool); ok {
		info.IsVip = v
	}
	if v, ok := payload.Data["isExpire"].(bool); ok {
		info.IsExpire = v
	}
	if v, ok := payload.Data["vipType"].(string); ok {
		info.VipType = v
	}
	if ms := jsonFloat(payload.Data["vipEndTime"]); ms > 0 {
		info.ExpiresAt = time.UnixMilli(int64(ms))
	}
	for _, k := range []string{"remain", "remaining", "balance", "point", "points"} {
		if v, ok := payload.Data[k]; ok {
			if f := jsonFloat(v); f > 0 {
				info.Remain = f
				break
			}
		}
	}
	return info, nil
}

// kukuHeaders 出站头：Cookie 是唯一凭证，其余头对齐网页端最小集合。
// 秘密值只进 Cookie 头，绝不进 URL / 日志 / 错误体。
func kukuHeaders(req *http.Request, cookie string) {
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Origin", kukuDefaultBase)
	req.Header.Set("Referer", kukuDefaultBase+"/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "+
		"(KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36")
}

// kukuCollectNumbers 把 data 里「键名命中候选表」的数值收集起来（深度受限）。
//
// 深度上限 2：`data` 与 `data.data` 两层已覆盖现有信封，再深就有把任务进度、
// 奖励数量等无关数值误当余额的风险。
func kukuCollectNumbers(data map[string]any, depth int) map[string]float64 {
	out := map[string]float64{}
	var walk func(m map[string]any, d int)
	walk = func(m map[string]any, d int) {
		for k, v := range m {
			if child, ok := v.(map[string]any); ok && d > 0 {
				walk(child, d-1)
				continue
			}
			lower := strings.ToLower(k)
			hit := false
			for _, f := range kukuPointFields {
				if lower == f || strings.HasSuffix(lower, "_"+f) || strings.HasSuffix(lower, f) {
					hit = true
					break
				}
			}
			if !hit {
				continue
			}
			if f := jsonFloat(v); f != 0 {
				// 同名键取绝对值较大者（两层信封里可能同时出现占位 0 与真实值）。
				if cur, ok := out[k]; !ok || f > cur {
					out[k] = f
				}
			}
		}
	}
	walk(data, depth)
	return out
}

// kukuPickRows 把候选数值整理成展示行，并挑出「可用总量」。
//
// 权威顺序：先按 kukuPointFields 的先后，再按数值大小。命中第一顺位即为
// Available；都不命中时退化为各行之和（宁可给个偏保守的数，也不返回 0 误导）。
func kukuPickRows(nums map[string]float64) ([]account.BalanceRow, float64) {
	if len(nums) == 0 {
		return nil, 0
	}
	type item struct {
		key string
		val float64
		pri int
	}
	items := make([]item, 0, len(nums))
	for k, v := range nums {
		lower := strings.ToLower(k)
		pri := len(kukuPointFields) + 1
		for i, f := range kukuPointFields {
			if lower == f || strings.HasSuffix(lower, "_"+f) || strings.HasSuffix(lower, f) {
				pri = i
				break
			}
		}
		items = append(items, item{key: k, val: v, pri: pri})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].pri != items[j].pri {
			return items[i].pri < items[j].pri
		}
		return items[i].key < items[j].key
	})

	rows := make([]account.BalanceRow, 0, len(items))
	for _, it := range items {
		rows = append(rows, account.BalanceRow{Name: it.key, Available: it.val, Unit: "积分"})
	}
	// 第一顺位就是最权威的额度字段；若它明显是"已用"类字段，求和更安全。
	available := items[0].val
	if len(items) > 1 && strings.Contains(strings.ToLower(items[0].key), "used") {
		available = 0
		for _, it := range items {
			available += it.val
		}
	}
	return rows, available
}

// kukuKeysOf 列出 data 的顶层键（报错时带上，方便一次修准契约）。
func kukuKeysOf(data map[string]any) string {
	if len(data) == 0 {
		return "（空对象）"
	}
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

func kukuMsg(s string) string {
	if strings.TrimSpace(s) == "" {
		return "上游未给原因"
	}
	return strings.TrimSpace(s)
}
