// acctlogin_env_test.go 登录端点 env 覆盖（v1.7.0 发布前补充）：
// env 注入 → helper 输出覆盖值；空值不产生键；键级回落由 acclogin 的
// TestEndpointPerKeyFallback 锚定。
package main

import (
	"context"
	"strings"
	"testing"

	"github.com/RisingInsight/startport/internal/acclogin"
	"github.com/RisingInsight/startport/internal/account"
)

func TestAccLoginEndpointsFromEnv(t *testing.T) {
	t.Setenv("STARPORT_ACCLOGIN_QODER_OPENAPI_CN", "https://cn-mirror.example")
	t.Setenv("STARPORT_ACCLOGIN_QODER_WEB_INTL", "https://intl-web.example")
	t.Setenv("STARPORT_ACCLOGIN_AUTOCLAW_USERAPI", "https://ac-mirror.example")
	// INTL openapi 与 CN web 不设——应缺键（回落官方）。
	openAPI, web, autoClaw := accloginEndpointsFromEnv()
	if openAPI[false] != "https://cn-mirror.example" {
		t.Errorf("openapi cn = %q", openAPI[false])
	}
	if _, exists := openAPI[true]; exists {
		t.Errorf("openapi intl 未设 env 不应出现键（键级回落交给 NewManager）")
	}
	if web[false] != "" {
		t.Errorf("web cn 未设 env 应缺键, got %q", web[false])
	}
	if web[true] != "https://intl-web.example" {
		t.Errorf("web intl = %q", web[true])
	}
	if autoClaw != "https://ac-mirror.example" {
		t.Errorf("autoclaw = %q", autoClaw)
	}

	// 全空：map 为空、autoclaw 空串（NewManager 回落官方默认）。
	t.Setenv("STARPORT_ACCLOGIN_QODER_OPENAPI_CN", "")
	t.Setenv("STARPORT_ACCLOGIN_QODER_WEB_INTL", "")
	t.Setenv("STARPORT_ACCLOGIN_AUTOCLAW_USERAPI", "")
	openAPI, web, autoClaw = accloginEndpointsFromEnv()
	if len(openAPI) != 0 || len(web) != 0 || autoClaw != "" {
		t.Errorf("空 env 应产出零覆盖: %+v %+v %q", openAPI, web, autoClaw)
	}
}

// 键级回落端到端：只覆盖 cn 的 web → Manager 的 qoder 授权 URL 用覆盖值
// （offline：login_url 只由 QoderWeb 拼出，不发网络请求）；intl 侧回落官方
// 由 acclogin 包的 TestEndpointPerKeyFallback 结构断言。
func TestAccLoginEnvWiringReachesManager(t *testing.T) {
	t.Setenv("STARPORT_ACCLOGIN_QODER_WEB_CN", "https://cn-web-mirror.example")
	openAPI, web, autoClaw := accloginEndpointsFromEnv()
	store, err := account.Load(t.TempDir() + "/accounts.json")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.SetLogf(func(string, ...any) {})
	mgr, err := acclogin.NewManager(acclogin.Config{
		Store:           store,
		QoderOpenAPI:    openAPI,
		QoderWeb:        web,
		AutoClawUserAPI: autoClaw,
		MachineIDFile:   t.TempDir() + "/qoder-machine-id",
		Logf:            func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	res, err := mgr.Start(context.Background(), "qoder", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.LoginURL, "https://cn-web-mirror.example/device/selectAccounts") {
		t.Errorf("env 覆盖未到达 Manager: %s", res.LoginURL)
	}
}
