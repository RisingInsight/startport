// index.go 面板静态资源与安全响应头。
//
// 资源经 go:embed 打进二进制（随服务部署，无外部构建步骤）：
//   - index.html  页面骨架
//   - core.js     全局基础设施（状态/主题/工具/trange/密钥门，最先加载）
//   - app.js      视图主体逻辑（账号池/用量/积分构成/模型/日志/配置/任务）
//   - keys.js     API 密钥视图（自包含模块）
//
// 前端脚本独立为同源文件而非内联，为了启用无需 unsafe-inline 的严格 CSP。
//
// 安全头对"面板页面与全部 /startport/api/* 响应"统一生效：CSP 限制脚本只能来自本服务，
// 禁止被 iframe 嵌套（防点击劫持），禁 MIME 嗅探，并声明不泄露 Referer 出去。
package panel

import (
	"embed"
	"net/http"
	"path"
)

//go:embed index.html
var indexHTML []byte

//go:embed app.js core.js qr.js keys.js ext.js extwiz.js acct.js captcha.js
var panelFS embed.FS

// csp 内容安全策略（严格版，无需 unsafe-inline）：
//   - default-src 'none'        默认全禁，逐个开口
//   - script-src 'self'         只跑同源脚本（app.js）；页面无内联事件处理器/内联脚本
//   - style-src 'self' 'unsafe-inline'
//     style 的内联是设计取舍：页面有少量 style="..." 属性（进度条宽度、表格列宽），
//     允许内联样式不会导致脚本执行；仍禁止外部样式域与 @import 外链。
//   - connect-src 'self'        前端 fetch 只能打本服务
//   - img-src 'self' data:      图标/内联图
//   - form-action 'none'        页面无表单提交目标（配置页是 JS 提交）
//   - frame-ancestors 'none'    禁止被任何站点 iframe 嵌套（点击劫持）
//   - base-uri 'none'          禁止注入 <base> 改写相对路径
//
// AutoClaw 国际版登录需要阿里云无痕验证（滑块）：SDK 从 o.alicdn.com 加载，
// 运行期再拉 g/x.alicdn.com 的动态脚本/样式，XHR/iframe 走 *.aliyuncs.com /
// *.alicdn.com。仅放行这组验证码域，不放宽 unsafe-inline / unsafe-eval。
const csp = "default-src 'none'; script-src 'self' https://o.alicdn.com https://*.alicdn.com https://*.aliyuncs.com; " +
	"style-src 'self' 'unsafe-inline' https://*.alicdn.com; " +
	"connect-src 'self' data: https://*.aliyuncs.com https://*.alicdn.com; " +
	"img-src 'self' data: https://*.alicdn.com; font-src 'self' data: https://*.alicdn.com; " +
	"frame-src 'self' https://*.alicdn.com https://*.aliyuncs.com; form-action 'none'; " +
	"frame-ancestors 'none'; base-uri 'none'"

// setSecurityHeaders 写入面板统一安全响应头（页面与 API 都要，API 也含 JSON 数据）。
func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("X-Content-Type-Options", "nosniff") // 禁 MIME 嗅探
	w.Header().Set("X-Frame-Options", "DENY")           // 老浏览器兜底（CSP frame-ancestors 的等价项）
	w.Header().Set("Referrer-Policy", "no-referrer")    // 不外泄面板地址给外部站点
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
}

// noStore 面板页面与脚本禁止缓存：前端资源 go:embed 在二进制里，每次升级
// 内容都变——没有本头时浏览器启发式缓存会继续用旧版页面配新版 API（或反之），
// 表现为「升级后界面没变/样式错乱」，普通刷新都未必奏效。本地面板 ~150KB
// 重复拉取零成本，宁可全量拉取也不要错版本。
const noStore = "no-store"

// index 输出面板页面（静态无秘密；数据接口 /startport/api/* 才走鉴权）。
func (p *Panel) index(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", noStore)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(indexHTML)
}

// appScript 输出前端脚本（同源脚本，供 CSP script-src 'self' 加载）。
// 多文件拆分后统一入口：按 URL 末段查 embed；路由只注册白名单，白名单外 404。
func (p *Panel) appScript(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", noStore)
	b, err := panelFS.ReadFile(path.Base(r.URL.Path))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}
