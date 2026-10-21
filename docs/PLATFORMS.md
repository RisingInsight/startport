# 平台适配细节

本文件收录每个原生平台适配器的契约细节、已知边界与排障要点。README 只给能力矩阵；**要动手接入某个平台时看这里**。

平台事实的单一来源是 `internal/account/platform.go` 的 `platformSpecs`（适配器名 / 默认基址 / 登录流程 / 签到与刷新能力），一致性由 `platform_test.go` 强制（原生平台集合 == `provider.NativeNames()` == `acclogin.SupportedPlatforms()` == `checkin.Supported()`）。

---

## 通用渠道框架（非原生平台）

把任意上游收敛成「协议 × 鉴权 × 路径/静态头 × 凭证池」四要素，接入同一个 `/v1` 出口。新增普通平台 = 改配置，代码改动为 0。

```jsonc
{
  "ext_channels": [
    {
      // 最简形态：只填 api_key。
      "name": "siliconflow",
      "base_url": "https://api.siliconflow.cn",
      "api_key": "sk-xxx",
      "models": ["Qwen/Qwen3-235B-A22B"]
    },
    {
      // 完整形态：协议 + 鉴权 + 路径 + 静态头 + 别名 + 凭证池。
      "name": "gemini",
      "base_url": "https://generativelanguage.googleapis.com",
      "protocol": "gemini",                       // openai（缺省）/ anthropic / gemini / responses
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

### 四种出站协议

| 协议 | 报文形态 | 默认路径 |
|---|---|---|
| `openai` | Chat Completions（缺省） | `{base}/v1/chat/completions`（历史规则自动补 `/v1`） |
| `anthropic` | Messages | `POST {base}/v1/messages`，自动带 `anthropic-version: 2023-06-01` |
| `gemini` | generateContent | `POST {base}/v1beta/models/{model}:generateContent`；流式为 `:streamGenerateContent?alt=sse` |
| `responses` | OpenAI Responses | `POST {base}/v1/responses` |

入站 `/v1/chat/completions`、`/v1/messages`、`/v1/responses` 对四类渠道**都可服务**。

### 四种鉴权

| `auth.mode` | 形态 |
|---|---|
| `bearer`（缺省） | `Authorization: Bearer <secret>` |
| `x-api-key` | `x-api-key: <secret>` |
| `query` | URL 参数（默认 `key`），日志/错误全链路脱敏为 `?key=***` |
| `custom` | 指定头名；anthropic / gemini 协议默认分别是 `x-api-key` / `x-goog-api-key` |

凭证级 `auth` **整体覆盖**渠道级，不做字段级合并——半覆盖会把秘密值投到错误的头里且无报错。

### 凭证池与换证

- 每渠道 ≤20 条凭证，按配置顺序选第一条可用（未停用、非失效、不在冷却、未过期）
- **429**：只冷却该凭证（60s 指数退避封顶 30m）并**自动换下一条重试一次**
- **401/403**：标记失效退出轮换（改 secret 才恢复）
- **5xx**：渠道短冷却 30s，**不换证**
- **400/404/422**：只记最近错误，不处罚
- 传输连续失败 5 次：熔断 10 分钟
- 全部凭证不可用：`503 all_credentials_unavailable`

### 路由与别名

- 模型名 `ext:<渠道名>/<模型名或别名>` 精确路由
- 别名条目会出现在 `/v1/models`（带 `alias_of`）
- 裸名在渠道间唯一匹配时也可直达（多渠道同名 → 要求前缀消歧）
- `/v1/chat/completions` 只认前缀形态（裸名走主池）；`/v1/messages` 与 `/v1/responses` 走完整解析（含裸名回退主池）

### 连通性探测

`POST /startport/api/ext_channels/probe` 只发模型目录请求（不发 chat、不消耗额度），独立 10s 超时、8MiB 上限、错误摘要脱敏；不落盘、不改渠道状态、不计用量。

### 能力边界（刻意收窄，不是 bug）

1. **Gemini 流式有硬前提**：上游必须按 `?alt=sse` 返回 `text/event-stream`。返回 JSON 时明确报 `502 gemini_stream_unsupported` 并提示改用 `stream:false`——绝不把整包 JSON 伪装成流式。非流式完全可用。
2. **Gemini + 多轮工具调用不支持**：`functionCall` 没有 id、参数不分片；出站时 id 用 `call_<seq>` 合成，Anthropic 入站客户端回传的 `tool_use_id` 无法关联。单轮可用。
3. **图片不支持跨协议**：`/v1/messages` 的 `image` 块返回 400；OpenAI wire 里的图片 part 在转 Anthropic/Gemini 时被忽略（转码成本高、收益低，排在 P1）。
4. **ANT→ANT 走双转换**：Anthropic 入站 + Anthropic 渠道是 `Anthropic → OpenAI → Anthropic`，语义等价但上游私有字段（`cache_control`、`thinking`）不保留。需要 100% 透传请直连上游。
5. **改 label 或 secret = 新凭证**：运行态复用键是 `label + sha256(secret)`。只改 `expires_at` / `disabled` / `base_url` 不会重置冷却与失效状态。
6. **OpenAI 流式响应不回写别名**：非流式会把 `model` 回写客户端请求的名字；流式为了逐字节透传不改写帧内 `model`。
7. **凭证级用量不落盘**：进程内计数，重启清零；长期趋势看用量视图的按渠道口径（`realm=ext`）。

---

## 原生适配器（`adapter`）

`protocol` 决定报文形态，`adapter` 决定平台契约（鉴权怎么发、要不要专属头）。两者正交：同一份 OpenAI 报文，普通上游走 `bearer` 就够，而有些客户端类平台要求"同一个 session 同时出现在两个头里"，四种 `auth` 表达不了（每种只写一个头）。

```jsonc
{
  "name": "loomy",
  "base_url": "https://loomyad.xunfei.cn/api/v1",
  "protocol": "openai",     // 报文形态
  "adapter": "loomy",       // 平台契约：双头 session
  "credentials": [{ "label": "primary", "secret": "<Loomy session>" }]
  // adapter 非空时不能再配 auth（校验会直接拒绝）
}
```

`adapter` 写错一个字母不会静默回落到协议适配器——启动/保存时直接报错并列出可用值（`ext_adapter_unknown`）。

### Loomy（讯飞）

- **双头鉴权**：`token: <session>` 与 `Authorization: Bearer <session>` **同时**发。两个网关（模型 / 积分）对头名偏好不同，官方客户端也是两个都发；`headers` 里写 `token` / `Authorization` 覆盖不掉（平台契约由适配器独占）。
- **鉴权与登录分离**：聊天转发不需要 HMAC 签名——签名只服务账号面（短信登录、签到）。本项目不做登录，把客户端登录态里的 session 粘进凭证即可。
- **session 14 天、无续期**：上游没有 refresh 接口。过期有两种表达，都会被归一成 OpenAI 的 `authentication_error` / `session_expired` 信封（文案写明"14 天 / 无续期 / 请重新登录"）：
  - `HTTP 200` + `{code:"020002"}`（或 `100002`）——**状态码也会被改成 401**，否则按状态码判定的 SDK 会把过期当成功；
  - 裸 `HTTP 401` / `403`（body 是 OpenAI 信封或纯文本均可）。
  此时重新登录 Loomy 并更新该凭证的 secret（改 secret = 新凭证）。
- **业务错误不会被当成 200 成功**：其它 `{code,desc}` 业务码归成 `400` + `upstream_error`（`error.code = loomy_<code>`）。用 400 而非 5xx 是刻意的：5xx 会触发渠道短冷却，而这类错误换证或冷却都解决不了。
- **目录与聊天同源**：`GET {base}/models`、`POST {base}/chat/completions`，同一对头。
- **base 不要写 `/v1`**：Loomy 的版本段是 `/api/v1`。配 `adapter` 后不会触发 OpenAI 那条"补 /v1"的历史规则。
- **已知边界**：若流式请求返回 `200` + `{code,desc}`（而不是 SSE），适配器按原样透传，客户端会表现为流式解析失败——用非流式或先探测一次即可确认凭证是否有效。

### AutoClaw / AutoClaw Intl（智谱）

- **两个地区 = 两个适配器**：同一套客户端代码的两个构建，站点不同、账号与目录互不相通。`base_url` 由使用者确认后填写。
- **认证头是 `X-Authorization`**（`Bearer <token>`），**不是** `Authorization`——发错会稳定 401。token 从桌面端 `openclaw.json` 的 `models.providers.zai.models[].headers['X-Authorization']` 取。401 即失效（服务端会轮换）。
- **双模型标识**：`X-Request-Model` 发完整路由 ID（决定计费通道：`zai_` 会员/按量、`zaicoding_` Coding Plan、`zai_auto` 自动选型），`body.model` 发剥前缀的模型 ID。静态路由表：`glm-5.3 → zaicoding_glm-5.3`、`glm-5.3-flash → zai_glm-5.3-flash`；`zaicoding_*` / `zai_*` 显式前缀与 `x_yyy` 形态原样透传；未知名字回落 `zai_auto`。模型清单是静态表，不走 HTTP 发现。
- **system 规范化**（上游白名单闸门）：首条 system/developer 消息前置 OpenClaw 身份前缀（身份句 + `## Tooling` 段，照官方客户端形态），你的提示词逐字保留在后；已以身份句开头则不重复（幂等）；外来 harness 身份句按大小写不敏感整串改写成中性说法。user/assistant/tool 消息一字不动。
- **`max_tokens` 归一**：上游对 30..8192 的小正整数一律 406 空响应体，出站前自动抬到 10000；缺省不注入、非正整数不碰。
- **model 回写**：非流式回写客户端请求名；流式逐帧改写 `model`（本适配器是"逐字节透传"声明的例外，帧会被重建）。
- **错误分类**：401 → `authentication_error`/`token_invalid`；403 + `pay-view`/`810001` → 明确提示"需要会员或额度不足"；406 且上游没给原因 → 附可执行提示（max_tokens 已归一 / system 指纹 / 请求形态三个方向）。
- **平台头**：`X-Product` / `X-Client-Type` / `X-Tm` / `X-Version` / `X-Lang` / `X-Channel` / `x_trace_id` 按官方客户端形态；不发 `X-Harness-Type`（带上会稳定 403 pay-view）；`X-Tm` 恒 `win`——发 `linux` 会被部分账号 403。
- **登录**：国内版走手机验证码；国际版走 OAuth（Z.ai / Google）+ 阿里云滑块，回调仅接受官方登记的回环端口 `18432 / 19654 / 19723 / 53699`。签到 / 余额 / 刷新走 userapi 链路。
- **已知边界**：远程模型目录与 token 刷新都需要客户端签名，本期用静态路由表与手工粘贴替代——上游新增模型时可在渠道 `models` 里显式登记，`X-Request-Model` 走显式前缀透传。

### Qoder / Qoder Intl（阿里）

- **COSY 自签名**：推理网关不认 Bearer——每个请求动态构造：一次性 AES-128-CBC 密钥加密用户身份 JSON（`info`）、内置 RSA 公钥加密该密钥（`key`）、`MD5(payload \n key \n 时间戳 \n 编码后请求体 \n 签名路径)` 做签名，拼成 `Authorization: Bearer COSY.<payload>.<签名>` + 一组 `Cosy-*` 头。请求体**先编码再签名**（base64 → 分段重排 → 自定义字母表替换）。签名密钥随代码分发（公钥，非机密材料）。
- **凭证是整个账号 JSON**（裸 token 不够——COSY 签名需要用户与设备标识）：

  ```json
  {"mode":"global","accessToken":"jt-…","userId":"…","machineId":"…","nickname":"…","email":"…"}
  ```

  `machineId` 缺省时按凭证确定性派生（同账号跨重启稳定）。`mode` 与适配器不匹配会直接报错（两站令牌不通用）。
- **令牌分流硬规则**：作业令牌（`jt-`）必须走 `https://api2.qoder.sh`，设备流令牌（`dt-`）走 `https://api3.qoder.sh`；打错主机稳定 403 Login expired，错误文案会提示令牌类型与主机是否匹配。
- **信封与业务错误**：HTTP 恒 200，错误在 SSE 信封 `statusCodeValue` 里——网关在状态层归一：排队（业务码 10605）→ `503 qoder_model_busy`（不罚凭证也不罚渠道）；额度/限流 → `429 qoder_quota_exceeded`；401/业务码 105 → `401 token_invalid`。流式下靠**首帧预读**在向客户端写任何字节之前完成归一（换证无损）。
- **模型未知名是硬失败**：`model_not_found`，不静默回落。
- **思考拆解**：上游可能把思考混在正文里（`<thinking>` 等标签），适配器跨分片拆解成 `reasoning_content`；"关闭思考"降级为不指定（上游无法真正关闭，发 false 会断连）。
- **token 刷新**：`dt-` / `jt-` 分别走 `deviceToken/refresh` / `jobToken/refresh`，由签到调度器在登录态失效时自动刷新 + 回写 + 重试一次。
- **国际版余额**：`GET /api/v2/quota/usage` 可用（主机 `openapi.qoder.sh`）。Free 新账号通常 0 credits。每日 Credits 的 `CLAIM_BENEFIT` 领取要求客户端风控身份（`Cosy-MachineToken/Type/Code`），设备码登录账号没有该身份，面板显示「平台无此活动」。
- **仍不做**：PAT 兑换 / 上下文档位（设计文档 `deliverables/` 内）。

### TRAE（字节 · SOLO 国内）

- **转发（第一批）**：base 建议 `https://trae-api-cn.mchost.guru`；聊天 `POST /api/agent/v3/llm_utils_chat`，模型目录 `POST /api/ide/v1/get_detail_param`。只支持国内 SOLO 通道；国际版是另一套两步协议，显式拒绝。
- **三头鉴权 + 设备指纹**：同一个 accessToken 同时进 `Authorization: Cloud-IDE-JWT <token>`、`X-Cloudide-Token`、`X-Ide-Token`；`X-Uid` / `X-Machine-Id` / `X-Device-Id` 为空时**不发**该头（空值会被上游当成另一个身份）。
- **白名单 payload**：只搬 `messages` / `tools` / `tool_choice` / 采样字段；`tools[].function.parameters` 转 JSON 字符串、`tool_choice` 转裸字符串、assistant `tool_calls[].function` 改名 `function_call`、`developer` 降成 `system`。多带字段不是被忽略，是被上游拒。
- **自有 SSE**：没有 `[DONE]`（`event: done` 后由网关补）；`token_usage` 欠到下一帧；**HTTP 200 的 `event: error`** 在首帧预读阶段归一成 401/429/404/413，换证无损；非流式由网关整段聚合。
- **浏览器登录**：PKCE + 本机回环回调——`GetLoginGuidance` 定登录域 → `{loginHost}/authorization?…`（`auth_callback_url=http://127.0.0.1:<port>/authorize` 刻意不编码，官方前端正则硬校验）→ 回调携带 `authCode/refreshToken/userInfo` → `ExchangeToken`（每登录上送 P-256 SPKI 公钥，空公钥 = 业务码 20405）→ `GetUserInfo` 补身份。设备身份落 `data/trae-device-identity.json`（0600）同安装复用；上游 20401（设备位满）时先在官方客户端解绑旧设备。
- **本批边界**：签到、refreshToken 绑定续期、余额已实现契约与单测，**待真实账号验收后打开**；浏览器登录产出的凭证已含设备密钥对，粘贴凭证账号不受影响。

### Codex（ChatGPT 订阅）

- **上游**：`chatgpt.com/backend-api/codex` 的 Responses 面（SSE），凭证来自 `codex login` 产出的 `auth.json`。
- **凭证形态**（二选一）：`auth.json` 全文（`auth_mode` 必须为 `chatgpt`；取 tokens 三元组 + `account_id`，缺失时从 `id_token` 的 `chatgpt_account_id` 提取）；或裸 `access_token` 字符串（快捷形态，无刷新能力）。
- **鉴权**：`Bearer <access_token>` + `chatgpt-account-id` 头。
- **流式**：上游恒为 SSE（强制 `stream=true` + `store=false`），非流式客户端在适配器聚合。
- **静态模型表**：`gpt-5.1` / `gpt-5.1-codex-max` / `gpt-5.1-codex-mini` / `gpt-5.2` / `gpt-5.2-codex` / `gpt-5.3-codex` / `gpt-5.3-codex-spark` / `gpt-5.4` / `gpt-5.4-mini` / `gpt-5.5` / `gpt-5.6-sol` / `gpt-5.6-terra` / `gpt-5.6-luna`。
- **刷新**：`internal/checkin/codex_refresh.go` 实现 OAuth 刷新令牌换新令牌组；⚠ 契约来自公开参考实现，未用真实账号联调。
- **接入方式**：当前经「外部渠道」向导手工接入（`adapter=codex`、`protocol=responses`，凭证填 `auth.json` 全文）；账号中心的粘贴即用在下一批开放。

---

## 新增原生平台的成本

原生平台（需要客户端签名 / 设备指纹 / 有状态转发）仍要写 Go 代码，但已经不需要"靠字符串对齐"：

1. `internal/provider/<name>.go`：实现 `NativeAdapter`（`Name` / `Protocol` / `Auth` / `StaticModels` / `Do`），`init()` 里 `RegisterNative`
2. `internal/provider/presets.go`：预设条目（`base_url` 必须为空）
3. `internal/acclogin/`：登录流程（如支持浏览器登录）
4. `internal/checkin/`：签到 / 刷新 / 余额（如平台提供）
5. `internal/account/platform.go`：`platformSpecs` 加一行

漏改会红测试（`platform_test.go` 三向一致断言 + 面板 parity 测试），不会静默分叉。**能配置搞定的平台请走通用渠道，零 Go 代码。**
