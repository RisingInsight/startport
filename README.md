<h1 align="center">StarPort 星港</h1>

<p align="center">
  <b>自托管网关：把腾讯 CodeBuddy 账号封装为 OpenAI 兼容 API，附 Web 运维面板</b><br>
  账号池轮转 · 熔断冷却 · 会话粘性 · 定时保号 · 多密钥分发 · 单文件 / Docker 部署
</p>

<p align="center">
  <img alt="Go" src="https://img.shields.io/badge/Go-1.22.5-00ADD8?logo=go&logoColor=white&style=flat-square">
  <img alt="API" src="https://img.shields.io/badge/API-OpenAI_Compatible-412991?style=flat-square">
  <img alt="Deploy" src="https://img.shields.io/badge/Deploy-Single_Binary%20%7C%20Docker-2496ED?style=flat-square">
  <img alt="Transport" src="https://img.shields.io/badge/Transport-SSE%20%2F%20Streaming-0DBD8B?style=flat-square">
</p>

---

## 这是什么

CodeBuddy（`copilot.tencent.com`）没有开放的 OpenAI 形态 API。StarPort 通过官方客户端同款的 OAuth 设备授权拿到账号凭证，在本地网关侧完成凭证续期、多账号调度和流量治理，对客户端只暴露一个标准的 `/v1/chat/completions`——现有的 SDK、CLI 工具、前端应用零改造接入。

一句话概括三层角色：

- **对客户端**：一个带鉴权的 OpenAI 兼容服务（流式 / 非流式都支持）；
- **对账号**：一个管家——定时签到、token 保活、连登兑换、猫猫旅行、成长任务自动推进；
- **对运维者**：一个 Web 面板——账号健康、用量统计、在线改配置、密钥分发、日志审计都在浏览器里完成。

## 功能特性

### 网关核心

- **OpenAI 兼容出站**：强制 `stream:true` 出站、SSE 帧按规范白名单重建、非流式本地聚合；`reasoning_content` 思维链多轮回填，DeepSeek 系模型的思维链注入与 effort 档位自动降级
- **原生工具调用还原**：上游在 `tools` 声明丢失时把工具调用以 DSML 标记文本混进正文（Codex 等客户端会因此一次工具都不执行）；网关在出站响应里识别并还原为结构化 `tool_calls`，流式非流式都覆盖
- **多账号池调度**：按模型实测成本分层 → 快过期积分加权 → 积分比例 + 闲置补偿加权随机；Top-5 候选短名单防惊群
- **熔断与冷却**：429 软冷却指数退避、402 硬冷却到次日签到、404 短冷却、连续失败熔断、在途租约限流；模型级限流（6004）按模型冷却而非罚整号
- **会话粘性**：同一会话绑定同一账号（TTL 滚动续期、失败解绑、可镜像 Redis）；客户端不发会话 ID 时按消息内容派生粘性键
- **指纹脱敏与提示词体系**：出站请求体黑名单指纹清洗 + 系统提示词三种模式（`custom` / `append` / `passthrough`），从源头规避上游对 CLI 模板句的逐字误杀；`passthrough` 被拦自动换中性提示词重试

### 账号保号（全自动）

签到（09/21 点）、活跃上报（10 点）、猫猫旅行（09/21 点）、token 保活（22 点）、夜猫子补足（23 点）、连登档位兑换与抽奖（挂在签到排程末尾）——六类动作独立开关，幂等可重复执行。成长计划 18 个任务中 17 个可在面板一键纯 API 完成并自动领奖（唯一例外是需要真实捐款的任务）。

### 外部渠道（多平台 Provider 框架）

把**任意上游平台**收敛成「协议 × 鉴权 × 路径/静态头 × 凭证池」四要素接入同一个 `/v1` 出口，与 CodeBuddy 账号池并存。新增平台 = 改配置，代码改动为 0（需要客户端签名/指纹的原生平台除外，见能力边界）。

```jsonc
{
  "ext_channels": [
    {
      // 老配置形态（阶段一）：只填 api_key，行为逐字节不变。
      "name": "siliconflow",
      "base_url": "https://api.siliconflow.cn",
      "api_key": "sk-xxx",
      "models": ["Qwen/Qwen3-235B-A22B"]
    },
    {
      // 新形态：协议 + 鉴权 + 路径 + 静态头 + 别名 + 凭证池。
      "name": "gemini",
      "base_url": "https://generativelanguage.googleapis.com",
      "protocol": "gemini",                       // openai（缺省）/ anthropic / gemini
      "auth": {"mode": "custom", "header": "x-goog-api-key"},
      "headers": {"X-Title": "StarPort"},          // 禁 Host/Content-Length/Connection 等传输层头
      "models": ["gemini-2.0-flash"],
      "aliases": {"fast": "gemini-2.0-flash-8b"},  // ext:gemini/fast → 上游 gemini-2.0-flash-8b
      "credentials": [
        {"label": "primary", "secret": "AIza-xxx", "expires_at": "2027-01-01"},
        {"label": "backup",  "secret": "AIza-yyy", "disabled": false,
         "auth": {"mode": "query", "param": "key"}}
      ]
    },
    {
      "name": "ark",
      "base_url": "https://ark.cn-beijing.volces.com/api/v3",
      "protocol": "openai",
      "path": "/chat/completions",                 // 非 /v1 形态必须显式覆盖路径
      "api_key": "sk-yyy",
      "disabled": true
    }
  ]
}
```

- **三种出站协议**：`openai`（Chat Completions，缺省，URL 规则逐字节兼容阶段一）、`anthropic`（Messages，`POST {base}/v1/messages`，自动带 `anthropic-version: 2023-06-01`）、`gemini`（`POST {base}/v1beta/models/{model}:generateContent`，流式为 `:streamGenerateContent?alt=sse`）。入站 `/v1/chat/completions` 与 `/v1/messages` 对三类渠道**都可服务**。
- **四种鉴权**：`bearer`（缺省，`Authorization: Bearer`）、`x-api-key`、`query`（URL 参数，默认 `key`，日志/错误全链路脱敏）、`custom`（指定头名；anthropic/gemini 的协议默认分别是 `x-api-key` / `x-goog-api-key`）。凭证级 `auth` **整体覆盖**渠道级（不做字段级合并——半覆盖会把秘密值投到错误的头里且无报错）。
- **凭证池与换证**：渠道下可配 ≤20 条凭证，按配置顺序选第一条可用（未停用、非失效、不在冷却、未过期）。**429 只冷却该凭证**（60s 指数退避封顶 30m）并**自动换下一条重试一次**；**401/403 标记失效**退出轮换（改 secret 才恢复）；5xx 只让渠道短冷却 30s、**不换证**；400/404/422 只记最近错误不处罚；传输连续失败 5 次熔断 10 分钟。全部凭证不可用 → `503 all_credentials_unavailable`。
- **别名与路由**：模型名 `ext:<渠道名>/<模型名或别名>` 精确路由；别名条目会出现在 `/v1/models`（带 `alias_of`）。裸名在渠道间唯一匹配时也可直达（多渠道同名 → 要求前缀消歧）。入站 `/v1/chat/completions` 只认前缀形态（裸名仍走 CodeBuddy 池，与阶段一一致）；`/v1/messages` 走完整解析（含裸名）。
- **面板与向导**：面板「外部渠道」视图是**渠道卡片 + 凭证列表 + 新建/编辑弹层**：每张卡显示协议、派生状态（可用 / 冷却 / 全部凭证失效 / 全部凭证冷却 / 已停用）、凭证可用数、模型数、最近成功、最近错误；每条凭证显示 label / 状态 / 尝试数（上游往返次数，**含被 429/401 淘汰的失败尝试**；tokens 仍只计成功服务那次）/ token 累计 / 过期时间 / 最近错误，并可单独「测试」。**写路径只有一条**：`POST /startport/api/config` 提交 `ext_channels` 数组。
- **连通性测试（只读）**：`POST /startport/api/ext_channels/probe` 只发模型目录请求（不发 chat、不消耗额度），独立 10s 超时、8MiB 上限、错误摘要脱敏；**不落盘、不改渠道状态、不计用量**。
- **情报 → 草稿**：面板内置 23 个平台预设（`GET /startport/api/ext_presets`），情报表格的「生成草稿」会把渠道名、建议协议/鉴权/原生适配器、来源链接预填到向导；`base_url` **永远为空**、凭证必须你自己填（情报是第三方数据，会过期）。`native_required=true` 的平台（CatPaw / 华为云码道 / MonkeyCode / Antigravity / WorkBuddy）按钮置灰并说明原因——它们需要客户端签名、设备指纹或有状态转发。Loomy / AutoClaw / Qoder / TRAE 已由原生适配器覆盖（见下文「原生 Agent 适配器」），不再置灰。
- **观测**：`/status` 新增 `ext_channels` 键（协议、派生状态、凭证计数与逐凭证运行态，**只增键**，账号池字段不变）；面板渠道卡片展示同样的数据。凭证用量为**本进程内累计**（重启清零），长期趋势请看用量视图的按渠道口径（`realm=ext`）。

**能力边界（不是 bug，已刻意收窄）**

1. **Gemini 流式有硬前提**：上游必须按 `?alt=sse` 返回 `text/event-stream`。上游返回 JSON 时明确返回 `502 gemini_stream_unsupported` 并提示改用 `stream:false`——**绝不把整包 JSON 伪装成流式**（伪流式会让客户端 TTFT/吞吐观测全错）。非流式 Gemini 完全可用。
2. **Gemini + 多轮工具调用不支持**：Gemini 的 `functionCall` 没有 id、参数不分片；出站时 id 用 `call_<seq>` 合成，Anthropic 入站客户端回传的 `tool_use_id` 与上游无法关联。**单轮工具调用可用**，多轮请改用 OpenAI/Anthropic 协议的渠道。
3. **图片不支持跨协议**：`/v1/messages` 的 `image` 块返回 400；OpenAI wire 里的图片 part 在转 Anthropic/Gemini 时被忽略（图片转码成本高、收益低，P1 再做）。
4. **ANT→ANT 走双转换**：Anthropic 入站 + Anthropic 渠道不是字节透传，而是 `Anthropic → OpenAI → Anthropic`（复用同一条链路）。语义等价（system / tools / tool_use / tool_result / stop_reason / usage 都能折回），但上游私有字段（如 `cache_control`、`thinking`）不会保留。需要 100% 透传请直连上游。
5. **改 label 或 secret = 新凭证**：运行态复用键是 `label + sha256(secret)`。改 secret 会重置冷却/失效状态（这是预期：换 key 就是换凭证）；改 label 同样视为新凭证。只改 `expires_at`/`disabled`/`base_url` 不会重置。
6. **OpenAI 流式响应不回写别名**：非流式响应会把 `model` 回写客户端请求的名字（别名），流式为了逐字节透传不改写帧内 `model`（仍是上游真实模型名）。
7. **凭证级用量不落盘**：P0-9 采用进程内计数（详见上文「观测」），`internal/usage` 的桶格式与既有用量视图口径一字未改。
8. **老配置（只有 `api_key`）在面板里编辑**：向导不显示 `api_key`；直接保存会带回脱敏哨兵并由服务端按渠道名回填，鉴权不丢。但**改渠道名**时无法按名回填，向导会要求你先添加一条凭证（否则会静默丢 key）。

**安全边界**

- 任何接口、日志、错误体都不出现凭证明文：`GET /startport/api/config` 对 `api_key` 与 `credentials[].secret` 一律返回哨兵 `__STARPORT_SECRET_SET__`，保存时按 (渠道名, 凭证 label) 从磁盘回填原值；`query` 鉴权的 URL 在日志/错误里先做 `?key=***` 脱敏；上游错误原文截断后再脱敏。
- `base_url` 在**启动与保存双期**做 SSRF 校验（内网/链路本地/云元数据字面地址拒绝；自建聚合器可显式 `allow_private: true` 放行内网，元数据端点恒拒）；`path` 不能是完整 URL、不能含 `..`；`headers` 禁止 `Host`/`Content-Length`/`Transfer-Encoding`/`Connection`/`Proxy-*`，`Authorization` 必须走 `auth` 字段。
- 探测与真实请求不共用超时体系（独立 client），但共用 Transport 复用 TLS 连接池；探测有并发闸（4）。

#### 原生 Agent 适配器（`adapter`）

`protocol` 决定**报文形态**（URL 模板 + body 编解码），`adapter` 决定**平台契约**（鉴权怎么发、要不要专属头）。两者正交：同一份 OpenAI 报文，普通上游走 `bearer` 就够了，而有些客户端类平台要求"同一个 session 同时出现在两个头里"，四种 `auth` 表达不了（每种只写一个头）。这类平台走命名适配器：

```jsonc
{
  "name": "loomy",
  "base_url": "https://loomyad.xunfei.cn/api/v1",
  "protocol": "openai",     // 报文形态
  "adapter": "loomy",       // 平台契约：双头 session
  "credentials": [{ "label": "primary", "secret": "<Loomy session>" }]
  // 注意：adapter 非空时不能再配 auth（校验会直接拒绝）
}
```

当前内置：**`loomy` / `autoclaw` / `autoclaw-intl` / `qoder` / `qoder-intl` / `trae`**

- **双头鉴权**：`token: <session>` 与 `Authorization: Bearer <session>` **同时**发。两个网关（模型 / 积分）对头名偏好不同，官方客户端也是两个都发；`headers` 里写 `token`/`Authorization` 覆盖不掉（平台契约由适配器独占）。
- **鉴权与登录分离**：聊天转发**不需要 HMAC 签名**——签名只服务账号面（短信登录、签到），本项目不做登录，你只需把客户端登录态里的 session 粘进凭证。
- **session 14 天、无续期**：上游没有 refresh 接口。过期有两种表达，都会被归一成 OpenAI 的 `authentication_error` / `session_expired` 信封（文案写明"14 天 / 无续期 / 请重新登录"）：
  - `HTTP 200` + `{code:"020002"}`（或 `100002`）——**状态码也会被改成 401**，否则按状态码判定的 SDK 会把过期当成功；
  - 裸 `HTTP 401` / `403`（body 是 OpenAI 信封或纯文本均可）。
  此时请重新登录 Loomy 并更新该凭证的 secret（改 secret = 新凭证，冷却/失效状态会重置）。
- **业务错误不会被当成 200 成功**：其它 `{code,desc}` 业务码（内容审核 / 参数 / 额度等）会被归成 **`400`** + `upstream_error`（`error.code = loomy_<code>`）。用 400 而不是 5xx 是刻意的：5xx 会触发渠道短冷却，而这类错误换证或冷却都解决不了；4xx/5xx 自带语义的状态码保持原样（例如 429 仍然是 429，冷却策略按限流退避走）。
- **目录与聊天同源**：`GET {base}/models`、`POST {base}/chat/completions`，都用同一对头。
- **base 不要写 `/v1`**：Loomy 的版本段是 `/api/v1`。`base_url` 建议 `https://loomyad.xunfei.cn/api/v1`（以官方为准）；配 `adapter` 后**不会**触发 OpenAI 那条"补 /v1"的历史规则。
- **配置错就是硬错**：`adapter` 写错一个字母不会静默回落到协议适配器，启动/保存时直接报错并列出可用值（`ext_adapter_unknown`）。

> 已知边界：Loomy 若在**流式**请求上返回 `200` + `{code,desc}`（而不是 SSE），适配器按"原样透传"处理，客户端会表现为流式解析失败——用非流式或先探测一次即可确认凭证是否有效。

**`autoclaw` / `autoclaw-intl`（智谱 AutoClaw，国内版 / 国际版）**

- **两个地区 = 两个适配器**：同一套客户端代码的两个构建，站点不同、账号与目录互不相通——`autoclaw`（国内，`https://autoglm-acceleration-api.zhipuai.cn/autoclaw-proxy/proxy/autoclaw`）与 `autoclaw-intl`（国际，`https://autoglm-api.autoglm.ai/autoclaw-proxy/proxy/autoclaw`）。`base_url` 仍由你确认后填写（以官方为准）。
- **认证头是 `X-Authorization`**（`Bearer <token>`），**不是** `Authorization`——发错会稳定 401。token 从桌面端 `openclaw.json` 的 `models.providers.zai.models[].headers['X-Authorization']` 取，手工粘贴；401 即失效（服务端会轮换），重新取一次更新 secret 即可。`headers` 里写认证头覆盖不掉（适配器独占）。
- **双模型标识**：`X-Request-Model` 发完整路由 ID（决定计费通道：`zai_` 会员/按量、`zaicoding_` Coding Plan、`zai_auto` 自动选型），`body.model` 发剥前缀的模型 ID。静态路由表：`glm-5.3 → zaicoding_glm-5.3`、`glm-5.3-flash → zai_glm-5.3-flash`（展示名大小写不敏感也命中）；`zaicoding_*` / `zai_*` 显式前缀与 `x_yyy` 形态原样透传；未知名字回落 `zai_auto`。模型清单是**静态表**（`StaticModels`），不走 HTTP 发现、探测也不打上游。
- **system 规范化**（上游白名单闸门）：首条 system/developer 消息前置 OpenClaw 身份前缀（身份句 + `## Tooling` 段，逐字照抄官方客户端），你的提示词逐字保留在后；已以身份句开头则不重复（幂等）；外来 harness 身份句（ZCode / Claude Code / Codex / DeepSeek Harness）按大小写不敏感整串改写成中性说法。user/assistant/tool 消息一字不动。
- **`max_tokens` 归一**：上游对 30..8192 的小正整数一律 406 空响应体（预校验），出站前自动抬到 10000；缺省不注入、非正整数不碰；`max_completion_tokens` 不参与。
- **model 回写**：上游会回自己的路由名（`zai_auto` 等）——非流式回写客户端请求名；**流式逐帧改写 `model`**（本适配器是"逐字节透传"声明的例外，帧会被重建）。
- **错误分类**：401 → `authentication_error`/`token_invalid`（提示重新粘贴）；403 + `pay-view`/`810001` → 明确提示"需要会员或额度不足"；406 且上游没给原因 → 附可执行提示（max_tokens 已归一 / system 指纹 / 请求形态风控三个方向）；其余照通用信封。所有文案过脱敏。
- **平台头**：`X-Product`/`X-Client-Type`/`X-Tm`/`X-Version`(1.17.8)/`X-Lang`/`X-Channel`/`x_trace_id` 照官方客户端形态；**不发 `X-Harness-Type`**（带上会稳定 403 pay-view）；`X-Tm` 恒 `win`（mac 构建除外）——发 `linux` 会被部分账号 403。
- **转发适配器本身不做登录 / 签到 / 积分 / token 刷新**：这些由账号中心完成。国内版走手机验证码；国际版走 OAuth（Z.ai / Google）+ 阿里云滑块，回调仅接受官方登记的回环端口 `18432 / 19654 / 19723 / 53699`（`http://localhost:<port>/auth/callback-zai`），captcha 参数由面板前端阿里云 SDK 生成、后端只透传。签到 / 余额 / 刷新走 userapi 链路。

> 已知边界（AutoClaw）：① 远程模型目录（`/proxy/autoclaw-model-config`）与 token 刷新都需要客户端签名，本期用静态路由表与手工粘贴替代——上游新增模型时，可在渠道 `models` 里显式登记，`X-Request-Model` 走显式前缀透传；② 上游闸门（头集合 / system 白名单 / max_tokens）是**实测行为**，可能随上游版本变化，`406` 文案里列了排查方向。

**`qoder` / `qoder-intl`（阿里 Qoder，中国版 / 国际版）**

- **COSY 自签名**：推理网关不认 Bearer——每个请求动态构造：一次性 AES-128-CBC 密钥加密用户身份 JSON（`info`）、内置 RSA 公钥加密该密钥（`key`）、`MD5(payload \n key \n 时间戳 \n 编码后请求体 \n 签名路径)` 做签名，拼成 `Authorization: Bearer COSY.<payload>.<签名>` + 一组 `Cosy-*` 头。请求体**先编码再签名**（base64 → 分段重排 → 自定义字母表替换）。签名密钥随代码分发（公钥，非机密材料）。
- **凭证是整个账号 JSON**（裸 token 不够——COSY 签名需要用户与设备标识）：
  ```json
  {"mode":"global","accessToken":"jt-…","userId":"…","machineId":"…","nickname":"…","email":"…"}
  ```
  `machineId` 缺省时按凭证确定性派生（同账号跨重启稳定）。`mode` 与适配器不匹配会直接报错（两站令牌不通用）。
- **两个地区 = 两个适配器 + 令牌分流硬规则**：中国版 `qoder`（base `https://gateway.qoder.com.cn`）、国际版 `qoder-intl`——**作业令牌（`jt-`）必须走 `https://api2.qoder.sh`、设备流令牌（`dt-`）走 `https://api3.qoder.sh`**，打错主机稳定 403 Login expired；错误文案里会提示令牌类型与主机是否匹配。
- **信封与业务错误**：HTTP 恒 200，错误在 SSE 信封 `statusCodeValue` 里——网关在状态层归一：排队（业务码 10605）→ **503** `qoder_model_busy`（不是登录态/额度问题，稍后重试即可，不罚凭证也不罚渠道）；额度/限流 → 429 `qoder_quota_exceeded`；401/业务码 105 → 401 `token_invalid`（重新粘贴）。流式下靠**首帧预读**在向客户端写任何字节之前完成归一（换证无损）。
- **模型未知名是硬失败**：`model_not_found`，**不**静默回落（与 AutoClaw 的 `zai_auto` 回退不同）——静态表在 README/预设里，上游新增模型请等适配器更新。
- **思考拆解**：上游可能把思考混在正文里（`<thinking>` 等标签），适配器跨分片拆解成 `reasoning_content`；"关闭思考"降级为不指定（上游无法真正关闭，发 false 会断连）。
- **token 刷新**：设备流令牌（`dt-`）与作业令牌（`jt-`）分别走 `deviceToken/refresh` / `jobToken/refresh`，由平台签到调度器在登录态失效时**自动刷新 + 回写 accounts.json + 重试一次**（与 AutoClaw 同款闭环）。刷新链来自参考实现，仍需真实账号验收。
- **国际版余额与签到**：`GET /api/v2/quota/usage` 可用（Bearer，主机 `openapi.qoder.sh`）；Free 新账号通常为 0 credits（实测 `userQuota.total=0`，无 addOn/org 桶）。每日 Credits 的 `CLAIM_BENEFIT` 领取要求 Qoder 客户端风控身份（`Cosy-MachineToken/Type/Code`，由客户端 `runtime-info.exe` 生成；AgentHub 亦注明同一限制），设备码登录账号没有该身份，因此面板显示「平台无此活动」，不是漏实现。
- **仍不做**：PAT 兑换 / 上下文档位（设计文档 §1.2，后果与替代见 `starport-qoder-design.md`）。

**`trae`（字节 Trae SOLO，国内）**

- **原生适配器（第一批：转发）**：base 建议 `https://trae-api-cn.mchost.guru`；聊天 `POST /api/agent/v3/llm_utils_chat`，模型目录 `POST /api/ide/v1/get_detail_param`。**只支持国内 SOLO 通道**；国际版是另一套两步协议，显式拒绝。
- **三头鉴权 + 设备指纹**：同一个 accessToken 同时进 `Authorization: Cloud-IDE-JWT <token>`、`X-Cloudide-Token`、`X-Ide-Token`；`X-Uid` / `X-Machine-Id` / `X-Device-Id` 为空时**不发**该头（空值会被上游当成另一个身份）。
- **白名单 payload**：只搬 `messages` / `tools` / `tool_choice` / 采样字段；`tools[].function.parameters` 转 JSON 字符串、`tool_choice` 转裸字符串、assistant `tool_calls[].function` 改名 `function_call`、`developer` 降成 `system`——多带字段不是被忽略，是被上游拒。
- **自有 SSE**：没有 `[DONE]`（`event: done` 后由网关补）；`token_usage` 欠到下一帧；**HTTP 200 的 `event: error`** 在首帧预读阶段归一成 401/429/404/413，换证无损；非流式由网关整段聚合。
- **模型目录**：远程 `get_detail_param`，与 `X-Ide-Version-Code` 绑定；过滤不可见/占位/已下线/本通道必死的配置（如 `deepseek-v4-pro`）。
- **浏览器登录（v1.7.6 接入）**：PKCE + 本机回环回调——`GetLoginGuidance` 定登录域 → `{loginHost}/authorization?…`（`auth_callback_url=http://127.0.0.1:<port>/authorize` **刻意不编码**，官方前端正则硬校验）→ 回调携带 `authCode/refreshToken/userInfo` → `ExchangeToken`（**每登录上送 P-256 SPKI 公钥**，空公钥 = 业务码 20405）→ `GetUserInfo` 补身份。设备身份（machine/device/密钥对）落 `data/trae-device-identity.json`（0600）**同安装复用**，重复登录不再新增设备位；上游 20401（设备位满）时先在官方客户端解绑旧设备。仅国内 SOLO 通道；国际版显式拒绝。
- **本批边界**：签到、refreshToken 绑定续期（DeviceProof）、余额在第二批接入；本批浏览器登录产出的凭证已含设备密钥对，粘贴凭证账号不受影响。

### 账号中心（v1.6.0 / v1.7.0 登录式添加）

把各平台账号（非 CodeBuddy）统一收进一个页面管理：**账号是运行数据**（落 `data/accounts.json`，0600），凭证**单向投影**进 config.json 的 managed 渠道（自动挂进转发引擎，`ext:<平台>/<model>` 路由不变）。

**统一账号视图（默认「全部平台账号」）**：Hero 置顶（金色「星港」+ 平台名、可用率 / 账号数 / 在途请求 / 今日签到、状态图例）、需要关注清单、顶栏平台下拉筛选、状态筛选与搜索；账号行带平台、健康原因、余额、签到、最近使用；点击行打开详情抽屉（平台能力、托管渠道状态、签到历史、测试/刷新/签到/停用/删除）。**CodeBuddy 池账号也计入「全部」的账号数、可用率、在途与签到，并以只读行出现在统一表中**（操作列「账号池」跳回专用视图）；选择「CodeBuddy 账号池」仍显示原账号池视图（积分/到期/负载趋势）。平台切换使用顶栏下拉框，不再使用分页签。新增平台只需补平台能力，不再需要新的账号表 DOM。

**同构平台池（v1.7.6）**：Loomy / AutoClaw / Qoder / TRAE / 通用平台各自是一个「XXX 账号池」下拉入口，内部 Hero、负载趋势、在途与最近请求、模型锁池、积分到期卡与 CodeBuddy 同构。Qoder / AutoClaw 池内切换国内/国际；Loomy / TRAE / 通用平台是单区域池，不显示无效的区域页签。通用平台每账号投影 `c-<id>` 渠道，模型键 `ext:c-<id>/<model>` 在锁池表中按裸模型名归并。

**添加方式一：浏览器登录（v1.7.0，与 CodeBuddy 账号同体验）**

「添加账号」向导首选 Tab。选平台 → 「获取授权链接」→ 浏览器打开登录 → 面板显示"等待授权完成，自动检测中…" → **成功后自动入池**（无需粘贴任何 token）：

| 平台 | 登录方式 | 说明 |
|---|---|---|
| `qoder` / `qoder-intl` | PKCE 设备码轮询（官方登录页） | **无需本机客户端**；每平台同时只允许一个进行中会话 |
| `autoclaw`（国内） | 手机号 + 短信验证码 | 60 秒发码频控 |
| `autoclaw-intl`（国际） | OAuth（Z.ai / Google）+ 阿里云滑块 | 回环端口 18432/19654/19723/53699；需联网加载阿里云 SDK |
| `trae`（SOLO 国内） | PKCE + 本机回环回调（随机端口） | 15 分钟一轮；设备身份同安装复用；回调页成败由面板轮询展示 |
| 其余平台 | 暂用「粘贴凭证」 | `zcode` 等随各自转发适配器在后续版本开放 |

- 会话 5 分钟超时（TRAE 一轮登录 15 分钟）；取消/过期/被新会话顶替都会停止后台轮询；全链路脱敏（授权链接不含 verifier，日志手机号打码）。
- 本地联调/镜像可用环境变量覆盖登录端点：`STARPORT_ACCLOGIN_QODER_OPENAPI_CN/_INTL`、`STARPORT_ACCLOGIN_QODER_WEB_CN/_INTL`、`STARPORT_ACCLOGIN_AUTOCLAW_USERAPI`、`STARPORT_ACCLOGIN_AUTOCLAW_INTL_USERAPI`、`STARPORT_ACCLOGIN_AUTOCLAW_OAUTH_API`（空值=官方默认；AutoClaw OAuth 的 URL/换码走加速站，captcha-config 仍走国际站 userapi）。

**添加方式二：粘贴凭证**（v1.6.0 起支持，仍是 loomy/custom 等平台的入口）

**支持平台**

| 平台 | 凭证 blob（最小形态） | 平台签到 | 转发 |
|---|---|---|---|
| `loomy` | `{"session":"…"}` | ✅ 每日登录积分 | ✅ |
| `autoclaw` / `autoclaw-intl` | `{"token":"…","refresh_token":"…(可选)"}` | ✅ 每日 task-complete（两地独立） | ✅ |
| `qoder`（中国版） | `{"access_token":"dt-…/jt-…","user_id":"…"}` | ✅ sash 活动领取 | ✅ |
| `qoder-intl` | 同上 | — 平台无此活动 | ✅ |
| `trae`（国内 SOLO） | `{"accessToken":"…","uid":"…","machineId":"…","deviceId":"…","refreshToken":"…(可选)"}` | —（第二批） | ✅（无登录/签到） |
| `custom`（通用） | `{"base_url":"…","secret":"…"}` | — | ✅（普通渠道） |

- **定时签到**：默认每天 **09:00** 对全账号跑一轮，失败 30 分钟后重试最多 2 次（重试仍失败 → 账号行标红）；手动按钮绕过窗口。签到成功后自动复活被"耗尽/待重登"判死的凭证。
- **登录态刷新**：AutoClaw / Qoder 账号可点账号行「刷新」（或由签到遇到登录态失效时自动触发）——刷新成功后回写 `accounts.json` 并**立即重投影** managed 渠道，转发面马上使用新 token；blob 缺 `refresh_token` 时不显示该按钮。
- **迁入 / 回退**：普通渠道在渠道设置里可「迁入账号中心」（每条凭证 → 一个账号，渠道转托管；部分失败整体回滚、不产生半迁移态）；托管渠道可「回退为普通渠道」（只动 3 个标记键，**凭证原样保留、零数据损失**；账号从账号中心删除）。
- **托管边界**：managed 渠道的 `credentials` 由账号中心投影——经配置表单修改会被拒绝（报错带"账号中心托管"）；渠道级字段（base_url / models / 别名 / 备注）**永远以 config 为准**，账号中心不改写。误删渠道会在下次启动/账号操作时自动重建（账号是凭证事实来源）。
- **老配置兼容**：全部新增 JSON 键 `omitempty`，没有 accounts.json 时行为与 v1.5.1 逐字节一致。
- **⚠ 真实账号验收标红**：三平台签到（loomy / autoclaw / qoder）、Qoder 令牌刷新与 Qoder COSY 转发的接口形态来自参考实现逆向，**尚未用真实账号联调**——联调不过时签到会在面板台账显式报"平台接口可能已变更"（可观测，不静默）。

后续规划：`trae` 第一批转发已接入；第二批补浏览器登录（回环回调 + EC P-256）、签到与 refreshToken 轮换。`zcode` 等其余平台继续按同一套 `RegisterNative` 接口逐个补齐。
### StarPort 相对上游的自有增量

本仓库在继承上游全部能力之外，重点补强了以下方向（均为上游 `workbuddy2api-panel` 所没有的）：

| 能力 | 说明 |
|---|---|
| **外部渠道（多平台）** | 任意上游（OpenAI / Anthropic / Gemini 三种协议）与 CodeBuddy 池并存：四种鉴权（bearer / x-api-key / query / custom）、凭证池与 429 换证、路径/静态头覆盖、别名路由、只读探测、SSRF 校验、密钥脱敏；面板卡片/凭证列表/新建向导 + 情报草稿（详见上节） |
| **Agent 接入密钥** | 面板「API 密钥」视图创建 / 吊销多把接入密钥：仅开放 `/v1/*` 聊天与模型列表，不开放面板与账号凭证；存储侧只留 SHA-256 摘要，全钥仅创建时可见一次。把网关分享给自己的多台设备 / 工具时不必交出管理密钥。这是面板第 9 个视图（上游为 7 个） |
| **出站 SSRF 防护** | 可配置的上游地址在启动时校验：拒绝非 http(s) scheme、内网 / 链路本地 / CGNAT / 云元数据字面 IP 与内部主机名，校验失败直接拒启（`internal/upstream/ssrf.go`；外部渠道 `base_url` 复用同一红线） |
| **XFF 信任边界** | 默认从不采信 `X-Forwarded-For` / `X-Real-IP`（客户端可伪造）；仅当直连地址落在 `server.trusted_proxies` 白名单内才读取转发头，防伪造审计来源与污染上游风控判据 |
| **版本检查提醒** | 后台定期从固定可信源（GitHub API + jsDelivr `version.json` 兜底）检查新版本，面板顶部横幅提示；只提示不下载、强制 HTTPS、禁跨主机重定向、版本单调防回放 |

## 项目沿革

| 阶段 | 仓库 | 状态 |
|---|---|---|
| 起源 | [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) | 已**不可公开访问**（删除或转私密，无法确证；作者账号仍在，名下另有其他仓库在线） |
| 活跃分支 | [linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel) | 社区维护中，本仓库的同步基准（`upstream` 远程） |
| 本项目 | **StarPort** | 在上游基础上深度定制并保持长期同步 |

与上游的同步方式：不定期的多轮吸收（截至 2026-10-13 已完成多轮，覆盖至当时上游最新提交 `6b31db6`，本地打有 `upstream-synced-6b31db6` 标签）。每轮同步走独立分支、全量 build / vet / test 验证后合回。此后新增能力以上表「自有增量」为准，与上游逐步分化。

> 使用本代码请遵守上游的 MIT 协议义务：再分发时保留原始出处（Sliverkiss/workbuddy2api）的版权与许可声明。

## 使用边界（必读）

本项目只适合一个用途：**管理你自己的账号**——自动签到、保活、给自己的工具接一个私有 API 入口。它免费开源、按「原样」提供。

明确反对并警告的行为：

1. **商用与售卖**。面向公众的 API 售卖、账号池出租、共享池、代充、二次加壳捆绑卡密——一律与本项目无关，后果（封号、条款违约、法律风险）自负。
2. **批量注册小号刷额度**。违反目标平台服务条款，且正在毁掉所有正常用户的环境。
3. **购买任何「收费版」「卡密版」**。本项目永远免费；收费分发版是他人篡改的产物，无法审计，存在窃取你明文凭证（`auths/` 里是 accessToken / refreshToken）的风险。

MIT 协议赋予的是代码层面的自由，不赋予以本项目名义宣传售卖或要求作者背书的权利。若滥用持续，作者保留停止维护、关闭仓库的权利。

## 快速开始

**准备**：一个或多个 CodeBuddy 账号；Docker（或直接跑单文件二进制）；从源码构建需要 Go ≥ 1.22。

### 方式一：GHCR 镜像（免克隆）

```bash
mkdir -p auths data && cp config.example.json config.json   # 建议编辑 api_key，或留空自动生成

docker run -d --name starport \
  -p 127.0.0.1:7863:7863 -e TZ=Asia/Shanghai -e STARPORT_LISTEN=0.0.0.0:7863 \
  -v ./auths:/app/auths -v ./data:/app/data -v ./config.json:/app/config.json \
  ghcr.io/risinginsight/startport:latest

curl -s http://localhost:7863/healthz
```

镜像 tag：`main` 分支推 `latest` / `main` / `sha-xxxxxx`；打 `v*` tag 额外发布语义化版本。首次发布后需在仓库 Packages 设置里把包设为 Public，否则拉取要先 `docker login ghcr.io`。

### 方式二：Docker Compose

```bash
git clone https://github.com/RisingInsight/startport.git && cd startport
cp config.example.json config.json    # compose 挂载此文件，缺失会启动失败
docker compose up -d --build
```

常用命令：`docker compose logs -f`（日志）、`restart`（重启）、`down`（停止；`./auths` 与 `./data` 不受影响）。

### 方式三：Windows 单文件

```powershell
go build -trimpath -ldflags="-s -w" -o starport.exe ./cmd/server
.\starport.exe                    # 双击即可：启动服务并自动打开面板
.\starport.exe -open=false        # 不自动开浏览器
```

单文件自包含（前端资源已嵌入二进制），拷到任意 Windows 机器可跑，只需 `auths/` 与 `data/` 目录可写。开机自启：把快捷方式放进 `shell:startup`。重复双击时新实例探测到已有实例会直接打开面板后退出，不报端口错误。注意不要与 Docker 同时跑（都监听 7863）。

### 添加账号

推荐在面板（`http://127.0.0.1:7863/startport/`）点「**添加账号**」：浏览器内完成 OAuth 设备授权，凭证落盘后**热加载进池，免重启**，顺带完成首次签到。Linux / macOS 也可用命令行 `./login.sh`（依赖 python3，授权 → 轮询 → 落盘 → 容器重启一条龙）。

### 验证

```bash
curl -s http://localhost:7863/v1/models  -H "Authorization: Bearer your-api-key"
curl -s http://localhost:7863/status     -H "Authorization: Bearer your-api-key"
curl -sN http://localhost:7863/v1/chat/completions \
  -H "Authorization: Bearer your-api-key" -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true}'
```

## 配置参考

`config.example.json` 是最完整的字段参考（示例值全是占位符，无真实密钥）。启动期安全校验：`api_key` 为已知弱口令（`test_key` / `changeme` 等）拒绝启动；空密钥 + 非回环监听会告警。

### 关键字段

| 字段 | 默认 | 说明 |
|---|---|---|
| `listen` | `127.0.0.1:7863` | 监听地址。默认仅本机回环；对外暴露需改 `0.0.0.0` 并**必须设强 `api_key`** |
| `api_key` | 空 | 管理密钥（面板 + `/v1/*` 全部接口）；空 = 不鉴权。另可在面板创建仅限 `/v1/*` 的 agent 密钥，见「Agent 接入密钥」 |
| `auth_dir` / `state_file` | `./auths` / `./data/state.json` | 凭证目录 / 池状态落盘 |
| `server.max_body_mb` | `0` | 入站请求体上限（MiB），`0` 不限制（缺省，超大 body 交由上游判定）。公网多租户建议设宽松上限防单请求打爆内存，超限返回 413 |
| `server.trusted_proxies` | `[]` | 可信反代网段（CIDR）。空 = 从不采信 `X-Forwarded-For`，见「StarPort 相对上游的自有增量」 |
| `server.read_timeout` | `300s` | 入站读取总时长上限，`0` 不限；改配置需重启 |
| `cooldown.soft_rate` / `soft_rate_max` | `600s` / `2h` | 429 软冷却基数与指数退避封顶 |
| `pool.max_in_flight` / `max_in_flight_global` | `3` / `2` | 单账号在途上限（global 域 WAF 更紧，单独压低） |
| `pool.breaker_threshold` / `breaker_cooldown(_max)` | `3` / `30m` / `6h` | 连败熔断阈值与退避 |
| `pool.credit_floor` | `100` | 积分保底：余额低于该值时不再承接实测收费模型（tier 2），防把余额打穿后免费模型也 402 冷却到次日；`0` 关闭 |
| `pool.prefer_expiring` / `expiring_soon` | `true` / `168h` | 快过期积分加权：窗口内有有效快过期批次的账号选号权重 ×3（软偏好，弱于粘性与成本分层） |
| `pool.cost_explore_interval` | `30m` | 成本分层探索：免费层垄断且存在未知号时，周期性搭车探测一个未知号的真实成本档 |
| `session_sticky.enabled` / `ttl` / `gc_interval` | `true` / `30m` / `5m` | 会话粘性开关 / 绑定时长 / GC 周期 |
| `prompt.mode` / `prompt.file` | `custom` / 空 | 提示词模式（`custom` 替换 / `append` 追加 / `passthrough` 透传）；file 指向自定义提示词整体替换内置默认 |
| `features.sanitize_blacklist_fingerprints` | `true` | 出站请求体指纹脱敏 |
| `schedule.*_hours` / `*_enabled` | 见功能特性 | 各保号任务的时点与开关（签到 / 活跃 / 旅行 / 保活 / 夜猫子）；`*_hours` 空数组 / `null` 表示回落默认值而非禁用，禁用用 `*_enabled: false` |
| `schedule.include_disabled_in_tasks` | `false` | `true` 时签到 / 保活 / 活跃 / 余额刷新四任务对已禁用账号照跑（禁用只关选号不停保号） |
| `upstream.timeout_seconds` / `header_timeout_seconds` / `idle_timeout_seconds` | `120` / 回落 / `300` | 三段超时分别管：短 RPC 总时长 / 聊天首字节前 / 流中空闲。聊天无总时长上限，长思考不掐断 |
| `upstream.user_agent` | 空 | 出站 UA 覆盖；空保持现状（可配 `WorkBuddy/<ver>` 让官网「使用端」正确归因） |
| `upstash.url` / `token` | 空 | Redis 镜像（可选），粘性绑定与池状态防重启丢失 |
| `ext_channels[]` | `[]` | 外部渠道（多平台 Provider）：`name` / `base_url` / `protocol`（openai 缺省 / anthropic / gemini）/ `adapter`（原生适配器名，空 = 按协议；当前可用 `loomy` / `autoclaw` / `autoclaw-intl` / `qoder` / `qoder-intl` / `trae`，非空时禁配 `auth`）/ `path`（路径覆盖，支持 `{model}`）/ `auth`（bearer / x-api-key / query / custom）/ `headers`（静态头，禁传输层头）/ `models`（显式名单，空 = 自动发现）/ `aliases`（别名 → 上游模型名）/ `credentials[]`（`label`/`secret`/`expires_at`/`disabled`/`auth`，≤20 条）/ `note` / `disabled` / `timeout_seconds` / `allow_private`。`api_key` 为兼容字段（与 `credentials` 互斥，非空时合成 `label="default"` 凭证）。模型名 `ext:<渠道>/<模型或别名>` 路由；凭证池按配置顺序选证，遇 429 / 401 / 403 自动换下一条凭证重试（单请求最多换 1 次、共 2 次上游往返）；任何接口不回显 secret（只回脱敏哨兵，保存时按渠道名/label 回填磁盘原值）；`base_url` 在**启动与保存双期**做 SSRF 校验（内网/元数据字面地址拒绝，`allow_private` 显式放行内网） |
| `ext_intel.enabled` / `url` / `interval_hours` | `true` / token-fbi / `12` | 面板额度情报拉取（仅展示，不参与路由）；`enabled=false` 或 `url=""` 关闭外联 |
| `update.check_enabled` / `check_interval_hours` | `true` / `6` | 版本检查开关与间隔；只提示不自动下载，离线场景可关 |
| `logging.request_archive_enabled` / `retention_days` / `max_mb` | `true` / `7` / `100` | 请求元数据 JSONL 归档（不含提示词、响应正文与凭证）与保留策略 |

环境变量覆盖：JSON 之后加载 `STARPORT_*` 变量（非空才覆盖），覆盖监听 / 密钥 / 目录 / 上游超时 / 提示词 / 脱敏 / 池参数 / 调试（`STARPORT_DEBUG_CHAT` 打印出站请求头，敏感头掩码）等，完整清单见 `cmd/server/config.go`。

面板在线改配置**热生效**的字段：`api_key`、`cooldown.*`、`features.*`、`logging.*`、`pool.*`、`schedule.*`、`ext_channels`（按名复用运行态：冷却与目录缓存保留，改 `base_url` 丢目录缓存）；装配期字段（`listen`、`auth_dir`、`upstream.*`、`upstash.*` 等）保存后提示需重启。写入采用深合并 + 原子替换，手写的未知键原样保留。

## 运行行为要点

### 错误分类与账号处置

| 错误 | 账号处置 |
|---|---|
| 402 余额耗尽 | 硬冷却到**次日 04:00**，签到回血自动解冻 |
| 429 频控 | 软冷却 600s 起、连续触发指数退避封顶 2h；`code 6004`（模型级）带重置时间时只冷却该模型到墙钟，切模型立即可用 |
| Session 失效（12153） | **连续 3 次**才永久禁用（一次多为网络抖动）；刷新成功 / 手工复活清计数 |
| 上游 404 | 固定 60s 短冷却 |
| ≥500 | 计入熔断连续失败计数 |
| 请求体畸形（400 `11101`）/ 内容拦截 | **不罚账号**（问题在请求内容）；拦截时 `passthrough` 模式换中性提示词同请求重试一次 |

### 会话粘性

键提取顺序 `metadata.conversation_id` → camelCase → `user_id` → 顶层字段（snake_case 优先）；TTL 滚动续期、失败解绑、成功跟随最终账号；可镜像 Redis（7 天 TTL）。

### API 端点

| 端点 | 鉴权 | 说明 |
|---|---|---|
| `POST /v1/chat/completions` | Bearer | OpenAI 兼容补全，流式 / 非流式；请求体默认无上限（`max_body_mb` 可配） |
| `GET /v1/models` | Bearer | 模型列表（缓存 1h），附上下文长度、最大输出、思考档位、积分倍率等上游真实字段 |
| `GET /status` | Bearer | 账号池汇总 + 每号详情 + 按模型聚合的限流锁池视图 |
| `GET /healthz` | 无 | 健康检查；响应带 `service: "starport"` 标识与 `X-Service` 头，供宿主探活区分同端口的其他服务 |

面板接口在 `/startport/api/*` 下（overview / usage / packages / models / logs / config / tasks / login / 密钥 CRUD 等分组），与页面同一 Bearer 鉴权，可脚本化调用。

## Web 管理面板

浏览器打开 `http://127.0.0.1:7863/startport/`（明暗主题，八个视图）：

| 视图 | 内容 |
|---|---|
| **账号池** | 统计条 + 账号表（健康状态、积分量条、成功失败计数、在途、最近用量）；单号签到 / 余额 / 解冻 / 禁用 / 暂停选号；批量签到 / 旅行 / 活跃 / 保活；模型锁池表 |
| **用量** | 指标卡、Token 时序图、按账号 / 模型 / 域的用量明细、积分扣除历史（折算积分 / 1M Token） |
| **积分构成** | 积分到期分布、账号对比、逐包明细（折叠策略可配 `panel.package_detail_limit`） |
| **任务中心** | 全账号成长任务扫描、执行队列（账号内串行、账号间并发 1-3）、开学季状态卡 |
| **模型与档位** | 实时查询上游模型：生效价与牌价、思考档位、上下文与输出上限；支持探测数据叠加实测钳制告警。**外部渠道模型（`ext:<平台>/<模型>`）也在此列出**，可按具体平台筛选并复制完整接入名；Qoder 元数据走远程目录（倍率/思考档位/上下文/输出），取不到时回退静态表 |
| **API 密钥** | Agent 接入密钥的创建与吊销（StarPort 独有，见上） |
| **配置** | 在线编辑 config.json，热生效字段立即应用 |
| **运行日志** | 请求记录表（时间 / 结果 / 模型 / 账号 / **来源 IP·UA** / 耗时 / token / 请求 ID）+ 按任务 / 对话 / 系统分频道的运行日志；时间范围查询（预设与自定义区间，今天与自定义区间由浏览器按本地时区计算） |

顶部还有「刷新」（全量查余额回写）与「添加账号」。请求级 stdout 日志每请求一行：序号、时刻、模型、模式、状态码、账号前 8 位、TTFB、token 速率、总时长——不含任何凭证明文。

## 部署与运维

### Docker 镜像

多阶段构建（`golang:1.23-alpine` → `alpine:3.20`），一次编译全部二进制；以 `app` 用户（uid 10001）运行，内置 `HEALTHCHECK`。挂载卷：`./auths`、`./data`、`./config.json`。

容器挂载目录权限报错（`permission denied`）时，优先以自己的 uid 运行：`PUID=$(id -u) PGID=$(id -g) docker compose up -d --force-recreate`；或 `chown -R 10001:10001` 交给容器默认用户。

### 工具脚本

| 脚本 | 用途 |
|---|---|
| `./login.sh` | OAuth 登录 → 落盘凭证 → 重启容器 |
| `./signin.sh` | 批量签到（过期先刷新） |
| `./credit.sh` | 积分日报（`-json` 出原始 JSON） |
| `python3 scripts/probe_active.py` | 活跃上报诊断 / 补跑（写操作默认 dry-run，需 `--yes`） |
| `python3 scripts/probe_max_tokens.py` | 探测各模型真实输出上限（上游声称值普遍虚高），结果可进面板展示 |

二进制不入库，脚本首次使用自动 `go build`（Docker 镜像内已预编译）。

### 账号管理

多账号 = 复制 `auths/workbuddy-<uid>.json`，池启动自动对齐目录。Session 失效被禁用的账号：重新登录覆盖凭证，或调用 `Pool.ReviveDisabled(uid)` 复活。备份 = `auths/` + `data/state.json`；配了 Upstash 则状态另镜像 Redis。

## 安全模型

- **凭证**：`auths/*.json` 为明文 token，容器内 0600 原子写回；**切勿提交 git**（`.gitignore` 已排除 `auths/`、`data/`、`config.json`、`*.key` 等）
- **网络**：默认监听回环；无内置 TLS，公网必须强 `api_key` + HTTPS 反代；启动期拒绝弱口令密钥
- **日志敏感度**：stdout 流水与 JSONL 归档均不含 token / api_key 明文，不读取 Authorization 头；来源 IP 属个人信息，由 `logging.request_client_info` 控制（缺省开，面板可关）
- **鉴权实现**：SHA-256 摘要 + 常量时间比较（`internal/httpauth`）；agent 密钥与管理密钥分离，泄露面最小化；`uid` 白名单校验防路径穿越
- **出站 SSRF**：可配上游地址启动时校验（见自有增量）；只做字面判定不解析 DNS（避免 TOCTOU），空值 = 内置默认
- **响应安全头**：面板与 API 统一带 CSP（`default-src 'none'`、禁嵌套）、`nosniff`、`X-Frame-Options: DENY` 等；前端无内联脚本
- **发布完整性**：镜像 / 二进制均由本地或 GitHub Actions 从源码构建，无第三方预编译产物

## 常见问题

**429 code=6004 是账号被限流了吗？**
不是，是该模型用量超限（msg 带「将在 … 重置」）。账号只对该模型冷却到重置墙钟（封顶 2h），换其他模型立即可用。

**系统提示词被内容策略误杀？**
默认 `prompt.mode=custom` 已用网关自有提示词替换客户端 system，从源头消除大部分误报；用户消息里的指纹串由指纹脱敏兜底清洗。两层互不替代。

**多图会话请求体超限？**
默认无网关侧上限（对齐上游行为，超限由上游自然报错，信息量更大）；公网部署建议配 `server.max_body_mb` 给宽松上限。

**官网「使用端」列怎么显示 WorkBuddy？**
官网按出站 UA 归因。配 `upstream.user_agent: "WorkBuddy/2.x.x"` 即可；默认保持 `CLI/2.63.2 CodeBuddy/2.63.2`（指纹净化的可配考量，不写死）。

**如何彻底关闭定时任务？**
所有 `schedule.*_enabled` 设 `false`（注意 `*_hours` 置空不是禁用，是回落默认值）。关签到会连带关闭「余额恢复即解冻」，硬冷却账号只能等次日 04:00 自然到期。

## 代码索引

按主题定位实现（文件 : 标识符）：

| 主题 | 位置 |
|---|---|
| 出站改写管线（stream 强制 / 归一化） | `internal/upstream/payload.go` · `PrepareBodyOptWithEffortsAndDefault` |
| 思维链注入与 effort 降级 | `internal/upstream/thinking.go` · `defaultDeepSeekEffort` |
| 原生工具调用还原 | `internal/upstream/dsml.go` |
| 错误分类 / 6004 解析 | `internal/upstream/client.go` · `Classify` |
| 上游常量（端点 / UA） | `internal/upstream/travel.go` 等各文件顶部 |
| 账号池 / 熔断 / 冷却 / 选号 | `internal/pool/pool.go` · `sessionDeadThreshold`、`CooldownUntilTomorrow4AM`、`ReviveDisabled` |
| 调度器 / streak 自检 | `internal/scheduler/scheduler.go` |
| 会话粘性 / Redis 镜像 | `internal/server` / `internal/redisstore` |
| 中性提示词 / 降级重试 | `internal/prompt/prompt.go` · `Degraded`；`internal/server/degrade.go` |
| Agent 密钥 | `internal/apikeys/apikeys.go`（包注释含完整设计说明） |
| 外部渠道（多平台） | `internal/extchan/extchan.go`（Manager / 渠道状态）、`credential.go`（凭证池状态机）、`route.go`（前缀/别名路由）、`do.go`（选证→适配→换证主链路 + `adapterFor`）、`probe.go`（只读探测）、`intel.go`（额度情报）；`internal/provider/`（三协议适配器 + `native.go` 命名适配器注册表 + `loomy.go` 原生适配器 + 鉴权 + 预设）；`internal/server/extrelay.go`（chat 转发）、`exthropic.go`（Anthropic /v1/messages 双向转换） |
| 更新检查 | `internal/update/update.go` |
| SSRF 校验 | `internal/upstream/ssrf.go` |
| 配置定义与默认值 | `cmd/server/config.go` |
| 面板装配与路由 | `internal/panel/` |

## 免责声明

本项目仅供学习研究。使用需遵守 CodeBuddy 服务条款，账号封禁、条款违约等风险自负；作者不对使用本项目产生的任何直接或间接损失负责。

## License

[MIT](LICENSE)。再分发时请保留原始出处 Sliverkiss/workbuddy2api 的版权与许可声明。本项目不授予任何上游（CodeBuddy / 腾讯）接口或服务的权利。
