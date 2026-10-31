package panel

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
)

// wizardAgents 端点：返回 goos 与 ≥3 个 agent（codex/claude/openai），
// 需要配置文件的 agent 带非空路径，路径分隔符与 goos 匹配。
func TestWizardAgentsEndpoint(t *testing.T) {
	p := New(Config{})
	rec := httptest.NewRecorder()
	p.wizardAgents(rec, httptest.NewRequest("GET", "/startport/api/wizard/agents", nil))
	if rec.Code != 200 {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	var out struct {
		Goos   string        `json:"goos"`
		Agents []wizardAgent `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Goos != runtime.GOOS {
		t.Errorf("goos = %q, want %q", out.Goos, runtime.GOOS)
	}
	byID := map[string]wizardAgent{}
	for _, a := range out.Agents {
		byID[a.ID] = a
	}
	for _, id := range []string{"codex", "claude", "openai"} {
		if _, ok := byID[id]; !ok {
			t.Errorf("agent %q missing", id)
		}
	}
	if a := byID["codex"]; a.ConfigPath == "" {
		t.Error("codex config_path empty")
	} else if runtime.GOOS == "windows" && strings.Contains(a.ConfigPath, "/") {
		t.Errorf("windows path uses slash: %q", a.ConfigPath)
	}
	if a := byID["claude"]; a.ConfigPath == "" {
		t.Error("claude config_path empty")
	}
	if a := byID["openai"]; a.ConfigPath != "" {
		t.Errorf("openai config_path = %q, want empty（通用工具无固定路径）", a.ConfigPath)
	}
}

// 前端契约：keys.js 有向导逻辑（片段生成/复制/本地密钥口径），
// index.html 有向导区块与输入件。
func TestWizardFrontendContract(t *testing.T) {
	keysJS, err := os.ReadFile("keys.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"function loadWizard", "function wizSnippetFor", "function renderWizard",
		"wizard/agents", "ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN",
		"[model_providers.starport]", "wire_api", "OPENAI_BASE_URL",
		"location.origin",
	} {
		if !strings.Contains(string(keysJS), want) {
			t.Errorf("keys.js 缺接入向导片段 %q", want)
		}
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`id="wizAgent"`, `id="wizModel"`, `id="wizKey"`,
		`id="btnWizCopy"`, `id="wizSnippet"`, `id="wizBase"`, `id="wizAgentNote"`,
	} {
		if !strings.Contains(string(html), want) {
			t.Errorf("index.html 缺向导输入件 %q", want)
		}
	}
}
