package panel

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/RisingInsight/startport/internal/account"
)

// TestAcctPlatformCatalogParity 后端注册表新增平台时，前端必须同步补齐
// 页签/面板/表格/添加下拉，否则账号能加但面板看不见。这条测试把
// "加平台漏改前端"从线上白屏变成 CI 失败。
func TestAcctPlatformCatalogParity(t *testing.T) {
	js, err := os.ReadFile("acct.js")
	if err != nil {
		t.Fatal(err)
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}

	block := regexp.MustCompile(`(?s)const ACCT_PLATFORMS = \{(.*?)\n\};`).FindSubmatch(js)
	if block == nil {
		t.Fatal("acct.js 缺少 ACCT_PLATFORMS 对象")
	}
	keyRe := regexp.MustCompile(`(?m)^  '?([a-z0-9_-]+)'?: \{`)
	jsKeys := map[string]bool{}
	for _, m := range keyRe.FindAllSubmatch(block[1], -1) {
		jsKeys[string(m[1])] = true
	}

	specKeys := map[string]bool{}
	for _, s := range account.Specs() {
		specKeys[s.Key] = true
		if !jsKeys[s.Key] {
			t.Errorf("acct.js 的 ACCT_PLATFORMS 缺平台 %q（后端注册表已声明）", s.Key)
		}
		for _, want := range []string{
			`id="acctPanel-` + s.Key + `"`,
			`id="acBody-` + s.Key + `"`,
			`<option value="` + s.Key + `">`,
		} {
			if !strings.Contains(string(html), want) {
				t.Errorf("index.html 缺平台 %q 的接线片段 %q", s.Key, want)
			}
		}
	}
	for k := range jsKeys {
		if !specKeys[k] {
			t.Errorf("acct.js 声明了后端不存在的平台 %q", k)
		}
	}
}
