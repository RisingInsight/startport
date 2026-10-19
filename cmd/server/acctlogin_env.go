// acctlogin_env.go 登录端点的环境变量覆盖（账号中心 v1.7.0；本地联调/镜像
// 场景）。沿用 ref 的 env override 哲学：**空值 = 不覆盖**（acclogin.NewManager
// 键级回落官方默认）。刻意不进配置文件——避免面板暴露端点改写面。
//
// 变量清单：
//
//	STARPORT_ACCLOGIN_QODER_OPENAPI_CN    qoder 中国版 openapi（轮询/userinfo）
//	STARPORT_ACCLOGIN_QODER_OPENAPI_INTL  qoder 国际版 openapi
//	STARPORT_ACCLOGIN_QODER_WEB_CN        qoder 中国版门户（授权页）
//	STARPORT_ACCLOGIN_QODER_WEB_INTL      qoder 国际版门户
//	STARPORT_ACCLOGIN_AUTOCLAW_USERAPI    autoclaw 国内版 userapi（短信登录）
package main

import "os"

// accloginEndpointsFromEnv 读登录端点的 env 覆盖。
// 返回的 map 只含**非空覆盖项**；空缺键由 acclogin.NewManager 回落官方默认。
func accloginEndpointsFromEnv() (qoderOpenAPI, qoderWeb map[bool]string, autoClawUserAPI string) {
	qoderOpenAPI = map[bool]string{}
	qoderWeb = map[bool]string{}
	if v := os.Getenv("STARPORT_ACCLOGIN_QODER_OPENAPI_CN"); v != "" {
		qoderOpenAPI[false] = v
	}
	if v := os.Getenv("STARPORT_ACCLOGIN_QODER_OPENAPI_INTL"); v != "" {
		qoderOpenAPI[true] = v
	}
	if v := os.Getenv("STARPORT_ACCLOGIN_QODER_WEB_CN"); v != "" {
		qoderWeb[false] = v
	}
	if v := os.Getenv("STARPORT_ACCLOGIN_QODER_WEB_INTL"); v != "" {
		qoderWeb[true] = v
	}
	autoClawUserAPI = os.Getenv("STARPORT_ACCLOGIN_AUTOCLAW_USERAPI")
	return qoderOpenAPI, qoderWeb, autoClawUserAPI
}
