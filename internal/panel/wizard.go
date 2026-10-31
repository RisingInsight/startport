// wizard.go 「接入向导」端点：为各 agent 生成指向本网关的配置片段提供
// 后端才知道的事实——操作系统与各 agent 配置文件的默认路径。
//
// 片段本体由前端拼装（base = location.origin，密钥用户自填、只在浏览器本地
// 拼进片段）。为什么后端不回显密钥：agent 密钥全钥只在创建响应出现一次，
// 存储侧只留 SHA-256 摘要（apikeys 包注释的安全边界）——向导不能也不该绕开它。
// 为什么只生成片段、不直接写用户的配置文件：改写他人配置属于高杠杆动作
// （TOML 无标准库支持、JSON 合并会丢注释/键序），在真实多工具验收前先做
// 「生成 + 复制 + 精确路径提示」的最小闭环。
package panel

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
)

// wizardAgent 一个可接入工具的描述。
type wizardAgent struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	ConfigPath string `json:"config_path,omitempty"`
	// Note 交给前端的补充说明（协议形态 / 环境变量覆盖等）。
	Note string `json:"note,omitempty"`
}

// wizardAgents GET /startport/api/wizard/agents → {goos, agents:[...]}。
func (p *Panel) wizardAgents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"goos":   runtime.GOOS,
		"agents": wizardAgentList(),
	})
}

// wizardAgentList 各工具的配置文件默认路径（按当前操作系统）。顺序即向导下拉顺序。
func wizardAgentList() []wizardAgent {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "~"
	}
	unix := runtime.GOOS != "windows"
	join := func(parts ...string) string {
		if unix {
			out := home
			for _, p := range parts {
				out += "/" + p
			}
			return out
		}
		return filepath.Join(append([]string{home}, parts...)...)
	}

	return []wizardAgent{
		{
			ID:         "codex",
			Name:       "Codex CLI",
			ConfigPath: join(".codex", "config.toml"),
			Note:       "wire_api = \"responses\" 或 \"chat\" 均可——网关两条入站协议都开。",
		},
		{
			ID:         "claude",
			Name:       "Claude Code",
			ConfigPath: join(".claude", "settings.json"),
			Note:       "把 env 并入现有 JSON（保留已有键）；设了 CLAUDE_CONFIG_DIR 时以它为准。",
		},
		{
			ID:   "openai",
			Name: "通用 OpenAI SDK / 其他工具",
			Note: "任何支持 OpenAI 兼容端点的工具：base_url 填 网关地址 + /v1，模型名在请求体 model 字段指定。",
		},
	}
}
