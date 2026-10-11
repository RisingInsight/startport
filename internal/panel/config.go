// config.go 面板配置页接口：读取当前配置、校验并保存（热生效 + 重启项标注）。
//
// 分工：cmd/server 持有 Config 类型与校验逻辑（Load/normalize），此处只做
// HTTP 编排——GET 回显、POST 透传给注入的 SaveConfig 闭包（由 main 完成
// "校验 → 落盘 → 热应用 → 返回需重启字段列表"）。
package panel

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
)

// SecretSentinel 敏感字段的脱敏哨兵。GET /startport/api/config 不回显真实密钥，
// 以该值占位（前端识别后清空输入框并改占位文案）；保存时该值被忽略（main 侧
// scrubSecretSentinels 删除该键），深合并即保留原值——避免"掩码被当成新密钥写回"。
const SecretSentinel = "__STARPORT_SECRET_SET__"

// getConfig 返回当前配置文件内容与路径（前端按 schema 渲染表单）。
//
// 脱敏（F-004）：api_key / upstream.device_token / upstash.token 属密钥，不回显
// 明文——它们会落到浏览器（表单/内存/devtools），非必要不暴露。非空值替换为
// SecretSentinel（"已设置但未回显"），空值原样返回（前端据此区分"未设置"）。
func (p *Panel) getConfig(w http.ResponseWriter, r *http.Request) {
	if p.cfg.LoadConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config api not available")
		return
	}
	cfg, err := p.cfg.LoadConfig()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load config: "+err.Error())
		return
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "marshal config: "+err.Error())
		return
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		writeErr(w, http.StatusInternalServerError, "decode config: "+err.Error())
		return
	}
	maskSecret(m, "api_key")
	if up, ok := m["upstream"].(map[string]any); ok {
		maskSecret(up, "device_token")
	}
	if us, ok := m["upstash"].(map[string]any); ok {
		maskSecret(us, "token")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"path":   p.cfg.ConfigPath,
		"config": m,
	})
}

// maskSecret 把 m[key] 的非空字符串值替换为脱敏哨兵（空值保持空）。
func maskSecret(m map[string]any, key string) {
	if v, ok := m[key].(string); ok && v != "" {
		m[key] = SecretSentinel
	}
}

// saveConfig 保存配置：body 直接是配置 JSON（前端按 schema 组装完整对象）。
// SaveConfig 闭包内部完成校验+落盘+热应用；校验失败返回 400 且不写盘。
func (p *Panel) saveConfig(w http.ResponseWriter, r *http.Request) {
	if p.cfg.SaveConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config api not available")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	restartRequired, err := p.cfg.SaveConfig(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if restartRequired == nil {
		restartRequired = []string{}
	}
	log.Printf("panel: 配置已保存（热生效完成；需重启字段 %d 个）", len(restartRequired))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"restart_required": restartRequired,
	})
}
