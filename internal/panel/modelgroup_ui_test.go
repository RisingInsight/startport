package panel

import (
	"os"
	"strings"
	"testing"
)

// 路由组 UI 契约：ext.js 有组管理逻辑（读 config.model_groups、写 POST 只提交
// model_groups 键、编辑弹窗回填与校验），index.html 有抽屉区块与弹窗输入件。
func TestModelGroupUIContract(t *testing.T) {
	extJS, err := os.ReadFile("ext.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"function loadExtGroups", "function renderExtGroups",
		"function extRawGroups", "function extMutateGroups",
		"function openMgForm", "function submitMgForm",
		"model_groups",                 // GET 读取与 POST 提交同一键
		"group/' + esc(g.name)",        // 卡片展示对外模型名
		"/^[a-z0-9][a-z0-9_-]{0,63}$/", // 组名前缀校验与后端同规
	} {
		if !strings.Contains(string(extJS), want) {
			t.Errorf("ext.js 缺路由组片段 %q", want)
		}
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`id="mgCards"`, `id="btnMgNew"`, `id="mgNote"`,
		`id="mgFormVeil"`, `id="mgNameInput"`, `id="mgRoutingSelect"`,
		`id="mgMembersInput"`, `id="btnMgFormOk"`, `id="btnMgFormCancel"`,
	} {
		if !strings.Contains(string(html), want) {
			t.Errorf("index.html 缺路由组输入件 %q", want)
		}
	}
}
