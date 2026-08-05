# New API (Fork)

Fork 自 [QuantumNous/new-api](https://github.com/QuantumNous/new-api)，在上游基础上打了以下补丁：

**#1 流式响应的上游 usage 被追加帧覆盖，导致 token 记账归零** — `relay/channel/openai/relay-openai.go`

- 症状：Anthropic 入口（`/v1/messages`）+ OpenAI 兼容渠道 + 流式这个组合下，日志里 output token 与 cache token 恒为 0。生产实测 454/454 条流式请求全为 0，而同渠道非流式 239/239 正常，同入口走另一个上游端点也正常
- 根因：`OaiStreamHandler` 逐帧无差别覆盖 `lastStreamData`，收尾只解析这最后一帧找 usage。部分上游在带 usage 的帧**之后**还会追加自有元数据帧（实测 opencode `zen/go` 端点发 `{"choices":[],"x-opencode-type":"inference-cost","cost":"...","normalizedUsage":{...}}`，而免费的 `zen` 端点不发此帧因而不受影响），于是"最后一帧"落在无 usage 的帧上，`containStreamUsage` 为 false，上游真实 usage 被整份丢弃、回退本地估算 `ResponseText2Usage`
- 为何恰好是 0 而非偏小值：`/v1/messages` 这条路的 `RelayMode` 是 `Unknown`（`Path2RelayMode` 无该分支，`GenRelayInfoClaude` 也未设），逐帧回调 `processTokenData` 的 switch 不匹配，`responseTextBuilder` 全程为空，估算结果精确为 0。同一 bug 在 `/v1/chat/completions` 上则表现为 output 偏差 + cache 恒 0
- 修复：额外记住最后一个带有效 usage 的帧；末帧不含 usage 时从它补回真实用量，以及被元数据帧清空的 `id` / `model` / `system_fingerprint`。`applyUsagePostProcessing` 的 body 参数一并改用该帧，使 DeepSeek / 智谱 / Moonshot 这类需从 body 二次提取 `cached_tokens` 的渠道同样受益（上游 PR #6328 缺的正是这块）。客户端可见的 SSE 内容不变
- 影响面：token 是限流、配额与用量分析的依据。当前该模型免费（`ModelRatio: 0`）故无计费损失，但这条路径结构上必然少记 output，若放付费模型会**少计费**
- 上游状态：[#6272](https://github.com/QuantumNous/new-api/issues/6272) open 无人处理，[#6158](https://github.com/QuantumNous/new-api/issues/6158) / [#6500](https://github.com/QuantumNous/new-api/issues/6500) 被 bot 自动判重关成 `not_planned`，三者均无人类维护者回应；[PR #6070](https://github.com/QuantumNous/new-api/pull/6070) 停滞且有冲突，[PR #6328](https://github.com/QuantumNous/new-api/pull/6328) 被作者自行关闭。上游合并后本地补丁可移除
- 已知遗留（本轮未修）：`Path2RelayMode` 缺 `/v1/messages` 分支这个缺陷本身仍在。补丁 #1 生效后走的是上游真实 usage，本地估算那条死路不会被触达，故无需修；且维护者在 [PR #3340](https://github.com/QuantumNous/new-api/pull/3340) 明确拒绝过在此处加分支，要求各渠道 adaptor 自行适配

**#2 跨格式转换丢弃 thinking，参数覆盖无法识别下游是否主动关闭思考** — `relay/common/override.go`

- 症状：Claude Code → LiteLLM（Anthropic 风格）→ new-api → OpenAI 风格上游 这条链上，渠道参数覆盖里带 `keep_origin: true` 的 `set thinking` 操作恒生效，客户端已开启思考的请求也被强制关闭；同时条件里引用 `thinking.type` 的操作永不命中
- 根因：`/v1/messages` 走 `ClaudeHelper`，先 `ConvertClaudeRequest` 转成 OpenAI 格式、**之后**才应用参数覆盖（`relay/claude_handler.go`）。而 Claude→OpenAI 转换器仅在 OpenRouter 方言下处理 `thinking`（映射为 `reasoning`），普通 OpenAI 渠道该字段被直接丢弃。等覆盖执行时 `thinking` 已不存在，`keep_origin` 的"字段已存在则跳过"判断恒为 false
- 为何客户端意图不可恢复：`thinking` 是承载该意图的唯一字段（`output_config` 同为 Claude 专有、一并被丢弃），转换后出站请求体里"客户端要思考"与"不要思考"两种情况没有任何字段差异，只能靠转换前的原始请求判断
- 修复：`BuildParamOverrideContext` 透出 `client_thinking_present`（bool）与 `client_thinking_type`（string）两个只读上下文字段，取自 `info.Request` 中未经改写的下游原始请求（`ClaudeHelper` 先 `DeepCopy` 再改写 thinking，故 `info.Request` 全程保持原始值）。不改动发往上游的请求体；条件求值在请求体找不到路径时会自动回退到上下文
- 影响面：纯 opt-in。仅引用这两个字段的渠道脚本受影响，未引用的渠道行为完全不变；非 Claude 格式入口不透出该字段
- 用法：判断"客户端是否**明确要求**思考"，而不是判断字段有无。`client_thinking_type` 不为 `enabled` 且不为 `adaptive`（两条 `invert: true` 条件 + `logic: AND`）时显式关闭上游思考。上游默认开启思考的模型（如 DeepSeek 官方）需要这条才能尊重客户端的关闭意图
- Claude Code 的真实语义：**开 = 传 `thinking:{"type":"adaptive"}`，关 = 整个字段不传**。`output_config.effort` 是独立的强度档，关闭思考后仍停在上次取值
- **别用 `client_thinking_present == false` 判断"客户端关闭了思考"** — 中间层可能恒定注入 `thinking`，字段有无不携带信号。生产实测（LiteLLM 1.95.0，`/v1/messages` 入站）：`messages/transformation.py` 的 `_translate_adaptive_effort_for_non_adaptive_model`，闸门是「`output_config.effort` 非空 **或** `thinking.type` 为 `adaptive`」，任一成立即改写，**与 `thinking` 字段是否存在无关**。`deepseek-v4-flash` 的三个能力探针为 False/False/True，落进 legacy 降级分支，`effort` 被造成 `{"type":"enabled","budget_tokens":N}`（high=4096、xhigh=8192）并摘掉 `effort`。因两态 `effort` 相同，出站字节级一致，`present` 恒为 true。即便中间层修正为关闭态发 `disabled`，`present` 在两态下**依然都是 true** —— 判据只能是 `type` 白名单
- 作用域提醒：同一网关上可能有多个客户端形状。实测另有一路请求原生发 `thinking:{"type":"enabled","budget_tokens":N}` 且不经上述改写，其思考开关从未失效，本规则对它是"跳过"、行为正确 —— 不要假设所有请求都是坏的那个形状
- 已知局限："开启思考"依赖**上游默认开启**（规则跳过时出站不含 `thinking`），而非显式指令。出站只会写 `disabled` 或不写，`enabled`/`adaptive` 能否被上游接受未验证；若上游某天改默认为关，开启态会静默失效
- 为何不改转换器：让转换器直接映射 `thinking` 只需几行，但会给**所有** Claude→OpenAI 渠道的上游请求默认加上该字段，不认识它的上游可能 400。改上下文则影响面可控，也更利于长期 rebase 上游

**#3 Claude→OpenAI 请求转换丢弃 thinking 块，导致思考模式多轮工具调用被上游拒绝** — `relaykit/relayconvert/internal/claude_messages/to_oai_chat_req.go`

- 症状：Claude Code → LiteLLM（Anthropic 风格）→ new-api → opencode Go → DeepSeek 官方 这条链上，思考模式下发生过 tool call 的多轮会话被上游拒绝：``[invalid_request_error] The `reasoning_content` in the thinking mode must be passed back to the API``。客户端确实把 thinking 块发回来了，但请求到不了上游
- 上游契约：[DeepSeek 官方文档](https://api-docs.deepseek.com/guides/thinking_mode/)明确「发生过 tool call 时，中间 assistant 的 `reasoning_content` must participate in the context concatenation and must be **passed back to the API**」，缺失即 400；位置是 assistant 消息顶层字段（"at the same level as `content`"）。反之**未**发生 tool call 时该字段不必回传，传了也会被忽略
- 根因：`ClaudeMessagesRequestToOpenAIChat` 遍历 content 数组的 switch 只有 `text` / `image` / `tool_use` / `tool_result` 四个 case，`thinking` 块无分支命中，既不进 `mediaMessages` 也不进 `toolCalls`，被静默丢弃。而 `dto.ClaudeMediaMessage` 本来就有 `Thinking` / `Signature` 字段，数据已解析进内存，只是没人消费
- 这是个不对称缺陷：同一个包的**响应**方向做了映射（`to_oai_chat_resp.go` 非流式与流式都会写 `ReasoningContent`），**请求**方向没做。`dto.Message.ReasoningContent` 此前只被响应方向写过
- 修复：新增 `case "thinking"` 累积思考文本，在已有的 `len(toolCalls) > 0` 分支内赋值给 `openAIMessage.ReasoningContent`。有 thinking 块则透传真实内容，没有则补空串占位（`*string` + `omitempty` 只跳过 nil，指向空串的指针会正常序列化成 `""`）
- 两处刻意的边界：一是**只在带 `tool_calls` 时写**，因为官方明确无 tool call 时传了也会被忽略，这是 40+ provider 共用的通用转换器，不扩大面积；二是**加 `Role == "assistant"` 守卫**，那个 switch 同时处理 user 消息的内容块（`tool_result` 就在 user 里），而 `reasoning_content` 只能挂 assistant
- 覆盖面（LiteLLM 侧生产抽样 2777 条带 `tool_use` 的 assistant 消息）：77% 有 thinking 块 → 透传真实内容；1.3% 既无 thinking 块也无下游兜底 → 空串占位修好；余下 21% 由下游按 tool id 认领、本就不报错。注意消息口径与请求口径差异巨大 —— 单条消息 1.3% 的风险，在一个携带约 52 条这类消息的请求里被放大成多数请求受影响
- 为何空串占位安全（**LiteLLM 侧实测，非本仓库实测**）：以 `prompt_tokens` 为判据，带下游兜底 id 的消息补空占位后 3/3 与地板持平（411），而长文本对照为 616（+205）证明该字段确实计入输入 —— 即空串未引入任何实质内容，也没有挤掉下游原有的思考上下文。另测得思考关闭态（`thinking: {"type":"disabled"}`，`reasoning_tokens=0`）下带 `reasoning_content` 的真实文本 / 空串 / 单空格均为 200，故本补丁无需判断当前是否思考模式
- 已知保留：上条实验为 n=3、单一时点。若下游将来改为真的回填缓存 reasoning，无条件赋值会盖掉它 —— 后果是丢失思考上下文这一质量退化，不是 400。按现有数据风险很低，但不是零
- `redacted_thinking` 未单独处理：`ClaudeMediaMessage` 没有承载其密文的 `data` 字段，取不到明文，落到通用的空串占位路径即可
- OpenRouter 方言未隔离：该方言走请求级 `reasoning` 参数而非消息级 `reasoning_content`，理论上不冲突；不分叉的理由是对称性 —— 响应方向对所有方言都写该字段
- 影响面：仅 Claude 格式入口 → OpenAI 兼容渠道、且 assistant 消息带 `tool_calls` 这一格。无工具调用的流量字节级不变（有回归测试锁定）
- 与补丁 #2 的关系：#2 控制**本轮**是否让上游思考，#3 修**历史**思考内容能否回传，两者互不重叠。上线顺序提醒：链路上游若有为绕过此缺陷而做的 `reasoning_content` 注入，必须等本补丁上线后再拆，反序会让无 thinking 块的那部分流量直接 400

**CI：fork 专用 GHCR 镜像构建** — `.github/workflows/fork-ghcr-release.yml`

- 发布 release 时自动构建 amd64 + arm64 推送到 `ghcr.io/leikaiwei/new-api`，不走 Docker Hub
- 触发条件是 release published 而非 push tag，避免同步上游时几十个 tag 批量触发构建

---

<div align="center">

![new-api](/web/public/logo.png)

# New API

🍥 **Next-Generation LLM Gateway and AI Asset Management System**

<p align="center">
  <a href="./README.zh_CN.md">简体中文</a> |
  <a href="./README.zh_TW.md">繁體中文</a> |
  <strong>English</strong> |
  <a href="./README.fr.md">Français</a> |
  <a href="./README.ja.md">日本語</a>
</p>

<p align="center">
  <a href="https://raw.githubusercontent.com/Calcium-Ion/new-api/main/LICENSE">
    <img src="https://img.shields.io/github/license/Calcium-Ion/new-api?color=brightgreen" alt="license">
  </a><!--
  --><a href="https://github.com/Calcium-Ion/new-api/releases/latest">
    <img src="https://img.shields.io/github/v/release/Calcium-Ion/new-api?color=brightgreen&include_prereleases" alt="release">
  </a><!--
  --><a href="https://hub.docker.com/r/CalciumIon/new-api">
    <img src="https://img.shields.io/badge/docker-dockerHub-blue" alt="docker">
  </a>
  <a href="https://atomgit.com/QuantumNous/new-api" target="_blank">
    <img alt="AtomGit G-Star" src="https://atomgit.com/QuantumNous/new-api/star/badge.svg"/>
  </a>
</p>

<p align="center">
  <a href="https://trendshift.io/repositories/20180" target="_blank">
    <img src="https://trendshift.io/api/badge/repositories/20180" alt="QuantumNous%2Fnew-api | Trendshift" style="width: 250px; height: 55px;" width="250" height="55"/>
  </a>
  <br>
  <a href="https://hellogithub.com/repository/QuantumNous/new-api" target="_blank">
    <img src="https://api.hellogithub.com/v1/widgets/recommend.svg?rid=539ac4217e69431684ad4a0bab768811&claim_uid=tbFPfKIDHpc4TzR" alt="Featured｜HelloGitHub" style="width: 250px; height: 54px;" width="250" height="54" />
  </a><!--
  -->
  <a href="https://atomgit.com/QuantumNous/new-api" target="_blank">
    <img alt="AtomGit G-Star" src="https://atomgit.com/QuantumNous/new-api/star/new_badge.svg" width="250" height="55" />
  </a>
</p>

<p align="center">
  <a href="#-quick-start">Quick Start</a> •
  <a href="#-key-features">Key Features</a> •
  <a href="#-deployment">Deployment</a> •
  <a href="#-documentation">Documentation</a> •
  <a href="#-help-support">Help</a>
</p>

</div>

## 📝 Project Description

> [!IMPORTANT]
> - This project is intended solely for lawful and authorized AI API gateway, organization-level authentication, multi-model management, usage analytics, cost accounting, and private deployment scenarios.
> - Users must lawfully obtain upstream API keys, accounts, model services, and interface permissions, and must comply with upstream terms of service and applicable laws and regulations.
> - Users should ensure their use complies with upstream terms of service and applicable laws and regulations.
> - When providing generative AI services to the public, users should comply with applicable regulatory requirements and fulfill all filing, licensing, content safety, real-name verification, log retention, tax, and upstream authorization obligations required by their jurisdiction.

---

## 🤝 Trusted Partners

<p align="center">
  <em>No particular order</em>
</p>

<p align="center">
  <a href="https://www.cherry-ai.com/" target="_blank">
    <img src="./docs/images/cherry-studio.png" alt="Cherry Studio" height="80" />
  </a><!--
  --><a href="https://github.com/iOfficeAI/AionUi/" target="_blank">
    <img src="./docs/images/aionui.png" alt="Aion UI" height="80" />
  </a><!--
  --><a href="https://bda.pku.edu.cn/" target="_blank">
    <img src="./docs/images/pku.png" alt="Peking University" height="80" />
  </a><!--
  --><a href="https://www.compshare.cn/?ytag=GPU_yy_gh_newapi" target="_blank">
    <img src="./docs/images/ucloud.png" alt="UCloud" height="80" />
  </a><!--
  --><a href="https://www.aliyun.com/" target="_blank">
    <img src="./docs/images/aliyun.png" alt="Alibaba Cloud" height="80" />
  </a><!--
  --><a href="https://io.net/" target="_blank">
    <img src="./docs/images/io-net.png" alt="IO.NET" height="80" />
  </a>
</p>

---

## 🙏 Special Thanks

<p align="center">
  <a href="https://www.jetbrains.com/?from=new-api" target="_blank">
    <img src="https://resources.jetbrains.com/storage/products/company/brand/logos/jb_beam.png" alt="JetBrains Logo" width="120" />
  </a>
</p>

<p align="center">
  <strong>Thanks to <a href="https://www.jetbrains.com/?from=new-api">JetBrains</a> for providing free open-source development license for this project</strong>
</p>

---

## 🚀 Quick Start

### Using Docker Compose (Recommended)

```bash
# Clone the project
git clone https://github.com/QuantumNous/new-api.git
cd new-api

# Edit docker-compose.yml configuration
nano docker-compose.yml

# Start the service
docker-compose up -d
```

<details>
<summary><strong>Using Docker Commands</strong></summary>

```bash
# Pull the latest image
docker pull calciumion/new-api:latest

# Using SQLite (default)
docker run --name new-api -d --restart always \
  -p 3000:3000 \
  -e TZ=Asia/Shanghai \
  -v ./data:/data \
  calciumion/new-api:latest

# Using MySQL
docker run --name new-api -d --restart always \
  -p 3000:3000 \
  -e SQL_DSN="root:123456@tcp(localhost:3306)/oneapi" \
  -e TZ=Asia/Shanghai \
  -v ./data:/data \
  calciumion/new-api:latest
```

> **💡 Tip:** `-v ./data:/data` will save data in the `data` folder of the current directory, you can also change it to an absolute path like `-v /your/custom/path:/data`

</details>

---

🎉 After deployment is complete, visit `http://localhost:3000` to start using!

> [!WARNING]
> When operating this project as a public generative AI service or API resale service, users should first complete all required filing, licensing, content safety, real-name verification, log retention, tax, payment, and upstream authorization obligations.

📖 For more deployment methods, please refer to [Deployment Guide](https://docs.newapi.pro/en/docs/installation)

---

## 📚 Documentation

<div align="center">

### 📖 [Official Documentation](https://docs.newapi.pro/en/docs) | [![Ask DeepWiki](https://deepwiki.com/badge.svg)](https://deepwiki.com/QuantumNous/new-api)

</div>

**Quick Navigation:**

| Category | Link |
|------|------|
| 🚀 Deployment Guide | [Installation Documentation](https://docs.newapi.pro/en/docs/installation) |
| ⚙️ Environment Configuration | [Environment Variables](https://docs.newapi.pro/en/docs/installation/config-maintenance/environment-variables) |
| 📡 API Documentation | [API Documentation](https://docs.newapi.pro/en/docs/api) |
| ❓ FAQ | [FAQ](https://docs.newapi.pro/en/docs/support/faq) |
| 💬 Community Interaction | [Communication Channels](https://docs.newapi.pro/en/docs/support/community-interaction) |

---

## ✨ Key Features

> For detailed features, please refer to [Features Introduction](https://docs.newapi.pro/en/docs/guide/wiki/basic-concepts/features-introduction)

### 🎨 Core Functions

| Feature | Description |
|------|------|
| 🎨 New UI | Modern user interface design |
| 🌍 Multi-language | Supports Simplified Chinese, Traditional Chinese, English, French, Japanese |
| 🔄 Data Compatibility | Fully compatible with the original One API database |
| 📈 Data Dashboard | Visual console and statistical analysis |
| 🔒 Permission Management | Token grouping, model restrictions, user management |

### 💰 Authorized Usage Accounting and Billing

- ✅ Internal top-up and quota allocation for lawful authorized scenarios (EPay, Stripe)
- ✅ Organization-level per-request, usage-based, and cache-hit cost accounting
- ✅ Cache billing statistics for OpenAI, Azure, DeepSeek, Claude, Qwen, and supported models
- ✅ Flexible billing policies for internal management or authorized enterprise customers

### 🔐 Authorization and Security

- 😈 Discord authorization login
- 🤖 LinuxDO authorization login
- 📱 Telegram authorization login
- 🔑 OIDC unified authentication
- 🔍 Key quota query usage (with [new-api-key-tool](https://github.com/Calcium-Ion/new-api-key-tool))

### 🚀 Advanced Features

**API Format Support:**
- ⚡ [OpenAI Responses](https://docs.newapi.pro/en/docs/api/ai-model/chat/openai/create-response)
- ⚡ [OpenAI Realtime API](https://docs.newapi.pro/en/docs/api/ai-model/realtime/create-realtime-session) (including Azure)
- ⚡ [Claude Messages](https://docs.newapi.pro/en/docs/api/ai-model/chat/create-message)
- ⚡ [Google Gemini](https://doc.newapi.pro/en/api/google-gemini-chat)
- 🔄 [Rerank Models](https://docs.newapi.pro/en/docs/api/ai-model/rerank/create-rerank) (Cohere, Jina)

**Intelligent Routing:**
- ⚖️ Channel weighted random
- 🔄 Automatic retry on failure
- 🚦 User-level model rate limiting

**Format Conversion:**
- 🔄 **OpenAI Compatible ⇄ Claude Messages**
- 🔄 **OpenAI Compatible → Google Gemini**
- 🔄 **Google Gemini → OpenAI Compatible** - Text only, function calling not supported yet
- 🚧 **OpenAI Compatible ⇄ OpenAI Responses** - In development
- 🔄 **Thinking-to-content functionality**

**Reasoning Effort Support:**

<details>
<summary>View detailed configuration</summary>

**OpenAI series models:**
- `o3-mini-high` - High reasoning effort
- `o3-mini-medium` - Medium reasoning effort
- `o3-mini-low` - Low reasoning effort
- `gpt-5-high` - High reasoning effort
- `gpt-5-medium` - Medium reasoning effort
- `gpt-5-low` - Low reasoning effort

**Claude thinking models:**
- `claude-3-7-sonnet-20250219-thinking` - Enable thinking mode

**Google Gemini series models:**
- `gemini-2.5-flash-thinking` - Enable thinking mode
- `gemini-2.5-flash-nothinking` - Disable thinking mode
- `gemini-2.5-pro-thinking` - Enable thinking mode
- `gemini-2.5-pro-thinking-128` - Enable thinking mode with thinking budget of 128 tokens
- You can also append `-low`, `-medium`, or `-high` to any Gemini model name to request the corresponding reasoning effort (no extra thinking-budget suffix needed).

</details>

---

## 🤖 Model Support

> For details, please refer to [API Documentation - Gateway Interface](https://docs.newapi.pro/en/docs/api)

| Model Type | Description | Documentation |
|---------|------|------|
| 🤖 OpenAI-Compatible | OpenAI compatible models | [Documentation](https://docs.newapi.pro/en/docs/api/ai-model/chat/openai/createchatcompletion) |
| 🤖 OpenAI Responses | OpenAI Responses format | [Documentation](https://docs.newapi.pro/en/docs/api/ai-model/chat/openai/createresponse) |
| 🎨 Midjourney-Proxy | [Midjourney-Proxy(Plus)](https://github.com/novicezk/midjourney-proxy) | [Documentation](https://doc.newapi.pro/api/midjourney-proxy-image) |
| 🎵 Suno-API | [Suno API](https://github.com/Suno-API/Suno-API) | [Documentation](https://doc.newapi.pro/api/suno-music) |
| 🔄 Rerank | Cohere, Jina | [Documentation](https://docs.newapi.pro/en/docs/api/ai-model/rerank/creatererank) |
| 💬 Claude | Messages format | [Documentation](https://docs.newapi.pro/en/docs/api/ai-model/chat/createmessage) |
| 🌐 Gemini | Google Gemini format | [Documentation](https://docs.newapi.pro/en/docs/api/ai-model/chat/gemini/geminirelayv1beta) |
| 🔧 Dify | ChatFlow mode | - |
| 🎯 Custom upstream | Supports configuring legally authorized upstream endpoints | - |

### 📡 Supported Interfaces

<details>
<summary>View complete interface list</summary>

- [Chat Interface (Chat Completions)](https://docs.newapi.pro/en/docs/api/ai-model/chat/openai/createchatcompletion)
- [Response Interface (Responses)](https://docs.newapi.pro/en/docs/api/ai-model/chat/openai/createresponse)
- [Image Interface (Image)](https://docs.newapi.pro/en/docs/api/ai-model/images/openai/post-v1-images-generations)
- [Audio Interface (Audio)](https://docs.newapi.pro/en/docs/api/ai-model/audio/openai/create-transcription)
- [Video Interface (Video)](https://docs.newapi.pro/en/docs/api/ai-model/audio/openai/createspeech)
- [Embedding Interface (Embeddings)](https://docs.newapi.pro/en/docs/api/ai-model/embeddings/createembedding)
- [Rerank Interface (Rerank)](https://docs.newapi.pro/en/docs/api/ai-model/rerank/creatererank)
- [Realtime Conversation (Realtime)](https://docs.newapi.pro/en/docs/api/ai-model/realtime/createrealtimesession)
- [Claude Chat](https://docs.newapi.pro/en/docs/api/ai-model/chat/createmessage)
- [Google Gemini Chat](https://docs.newapi.pro/en/docs/api/ai-model/chat/gemini/geminirelayv1beta)

</details>

---

## 🚢 Deployment

> [!TIP]
> **Latest Docker image:** `calciumion/new-api:latest`

### 📋 Deployment Requirements

| Component | Requirement |
|------|------|
| **Local database** | SQLite (Docker must mount `/data` directory)|
| **Remote database** | MySQL ≥ 5.7.8 or PostgreSQL ≥ 9.6 |
| **Container engine** | Docker / Docker Compose |
| **System architecture** | 64-bit only (amd64 / arm64); 32-bit systems are not supported |

### ⚙️ Environment Variable Configuration

<details>
<summary>Common environment variable configuration</summary>

| Variable Name | Description | Default Value |
|--------|------|--------|
| `SESSION_SECRET` | Authentication signing secret; must be identical on every node | - |
| `SESSION_COOKIE_SECURE` | `false`/unset disables the refresh/logout OriginGuard for local HTTP dev proxies; `true` enables the Secure cookie and strict Origin checks | `false` |
| `SESSION_COOKIE_TRUSTED_URL` | Required with Secure mode: comma-separated exact HTTPS Origins allowed to call refresh/logout; not a relay CORS allowlist | - |
| `TRUSTED_PROXIES` | Unset/blank trusts loopback, RFC 1918 and IPv6 ULA with a startup warning; `none` trusts no proxies; an explicit proxy IP/CIDR list replaces the defaults | `127.0.0.0/8, ::1, 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, fc00::/7` |
| `USER_SESSION_ACTIVE_LIMIT` | Maximum active login Sessions per user | `50` |
| `USER_SESSION_ISSUANCE_LIMIT` | Maximum Sessions created per user within the issuance window, including revoked Sessions | `100` |
| `USER_SESSION_ISSUANCE_WINDOW_SECONDS` | Per-user Session issuance window; clamped to the revoked retention period when configured higher | `86400` |
| `USER_SESSION_REVOKED_RETENTION_DAYS` | Days to retain revoked Session rows for audit and issuance accounting | `7` |
| `USER_SESSION_HOURLY_ALERT_THRESHOLD` | Global Sessions created per hour that triggers an alert only; it never blocks login | `5000` |
| `CRYPTO_SECRET` | HMAC secret for cache keys; nodes sharing Redis must use the same effective value | Defaults to `SESSION_SECRET` |
| `SQL_DSN` | Database connection string | - |
| `REDIS_CONN_STRING` | Redis connection string | - |
| `RELAY_IDLE_CONN_TIMEOUT` | Idle keep-alive timeout for relay HTTP clients, seconds. Defaults to Go standard library behavior; set `0` to disable | `90` |
| `STREAMING_TIMEOUT` | Streaming timeout (seconds) | `300` |
| `STREAM_SCANNER_MAX_BUFFER_MB` | Max per-line buffer (MB) for the stream scanner; increase when upstream sends huge image/base64 payloads | `64` |
| `MAX_REQUEST_BODY_MB` | Max request body size (MB, counted **after decompression**; prevents huge requests/zip bombs from exhausting memory). Exceeding it returns `413` | `32` |
| `AZURE_DEFAULT_API_VERSION` | Azure API version | `2025-04-01-preview` |
| `ERROR_LOG_ENABLED` | Error log switch | `false` |
| `PYROSCOPE_URL` | Pyroscope server address | - |
| `PYROSCOPE_APP_NAME` | Pyroscope application name | `new-api` |
| `PYROSCOPE_BASIC_AUTH_USER` | Pyroscope basic auth user | - |
| `PYROSCOPE_BASIC_AUTH_PASSWORD` | Pyroscope basic auth password | - |
| `PYROSCOPE_MUTEX_RATE` | Pyroscope mutex sampling rate | `5` |
| `PYROSCOPE_BLOCK_RATE` | Pyroscope block sampling rate | `5` |
| `HOSTNAME` | Hostname tag for Pyroscope | `new-api` |

📖 **Complete configuration:** [Environment Variables Documentation](https://docs.newapi.pro/en/docs/installation/config-maintenance/environment-variables)

</details>

### 🔧 Deployment Methods

<details>
<summary><strong>Method 1: Docker Compose (Recommended)</strong></summary>

```bash
# Clone the project
git clone https://github.com/QuantumNous/new-api.git
cd new-api

# Edit configuration
nano docker-compose.yml

# Start service
docker-compose up -d
```

</details>

<details>
<summary><strong>Method 2: Docker Commands</strong></summary>

**Using SQLite:**
```bash
docker run --name new-api -d --restart always \
  -p 3000:3000 \
  -e TZ=Asia/Shanghai \
  -v ./data:/data \
  calciumion/new-api:latest
```

**Using MySQL:**
```bash
docker run --name new-api -d --restart always \
  -p 3000:3000 \
  -e SQL_DSN="root:123456@tcp(localhost:3306)/oneapi" \
  -e TZ=Asia/Shanghai \
  -v ./data:/data \
  calciumion/new-api:latest
```

> **💡 Path explanation:**
> - `./data:/data` - Relative path, data saved in the data folder of the current directory
> - You can also use absolute path, e.g.: `/your/custom/path:/data`

</details>

<details>
<summary><strong>Method 3: BaoTa Panel</strong></summary>

1. Install BaoTa Panel (≥ 9.2.0 version)
2. Search for **New-API** in the application store
3. One-click installation

📖 [Tutorial with images](./docs/BT.md)

</details>

### ⚠️ Multi-machine Deployment Considerations

> [!WARNING]
> - All nodes must use the same primary database and the same `SESSION_SECRET`; otherwise Access Tokens, refresh sessions, and temporary authentication flows cannot be verified consistently.
> - Nodes connected to the same Redis must also use the same `CRYPTO_SECRET`, or their cache-key digests will differ and shared entries cannot be reused consistently.

The database is authoritative for login Sessions and for the per-user active/issuance limits. Redis Session entries are short-lived caches whose TTL follows `SYNC_FREQUENCY` (60 seconds by default) and never exceeds the Session's remaining lifetime.

| Redis topology | Session propagation | Rate limiting |
| --- | --- | --- |
| Shared Redis | Revocations and version publications normally propagate immediately | Redis limits are shared across nodes |
| Independent Redis per node | Nodes converge from the database within the effective `SYNC_FREQUENCY`; a newly rotated token may receive a temporary 401 on a node with stale cache | Each node has its own allowance, so aggregate capacity can reach roughly the configured limit multiplied by the node count |
| No Redis | Every Session validation reads the database | In-memory limits are independent per node |

A shorter `SYNC_FREQUENCY` reduces the independent-Redis staleness window but causes one additional primary-key Session lookup per active SID, per node, per TTL. These guarantees make Session authentication bounded-stale across the supported topologies; rate limits and other Redis-backed control-plane caches remain topology-dependent.

See [User authentication and login sessions](./docs/authentication.md) for the token, Origin-check and PAT contracts.

### 🔄 Channel Retry and Cache

**Retry configuration:** `Settings → Operation Settings → General Settings → Failure Retry Count`

**Cache configuration:**
- `REDIS_CONN_STRING`: Redis cache (recommended)
- `MEMORY_CACHE_ENABLED`: Memory cache

---

## 🔗 Related Projects

### Upstream Projects

| Project | Description |
|------|------|
| [One API](https://github.com/songquanpeng/one-api) | Original project base |
| [Midjourney-Proxy](https://github.com/novicezk/midjourney-proxy) | Midjourney interface support |

### Supporting Tools

| Project | Description |
|------|------|
| [new-api-key-tool](https://github.com/Calcium-Ion/new-api-key-tool) | Key quota query tool |
| [new-api-horizon](https://github.com/Calcium-Ion/new-api-horizon) | New API high-performance optimized version |

---

## 💬 Help Support

### 📖 Documentation Resources

| Resource | Link |
|------|------|
| 📘 FAQ | [FAQ](https://docs.newapi.pro/en/docs/support/faq) |
| 💬 Community Interaction | [Communication Channels](https://docs.newapi.pro/en/docs/support/community-interaction) |
| 🐛 Issue Feedback | [Issue Feedback](https://docs.newapi.pro/en/docs/support/feedback-issues) |
| 📚 Complete Documentation | [Official Documentation](https://docs.newapi.pro/en/docs) |

### 🤝 Contribution Guide

Welcome all forms of contribution!

- 🐛 Report Bugs
- 💡 Propose New Features
- 📝 Improve Documentation
- 🔧 Submit Code

---

## 📜 License

This project is licensed under the [GNU Affero General Public License v3.0 (AGPLv3)](./LICENSE).

Additional terms under AGPLv3 Section 7 apply. Modified versions must preserve
the author attribution notice `Frontend design and development by New API
contributors.` in the appropriate legal notices and in any prominent about,
legal, footer, or attribution location presented by the user interface.

Modified versions that present a user interface must also preserve a visible
link to the original project: <https://github.com/QuantumNous/new-api>.

This is an open-source project developed based on [One API](https://github.com/songquanpeng/one-api) (MIT License).

If your organization's policies do not permit the use of AGPLv3-licensed software, or if you wish to avoid the open-source obligations of AGPLv3, please contact us at: [support@quantumnous.com](mailto:support@quantumnous.com)

---

## 🌟 Star History

<div align="center">

[![Star History Chart](https://api.star-history.com/svg?repos=Calcium-Ion/new-api&type=Date)](https://star-history.com/#Calcium-Ion/new-api&Date)

</div>

---

<div align="center">

### 💖 Thank you for using New API

If this project is helpful to you, welcome to give us a ⭐️ Star！

**[Official Documentation](https://docs.newapi.pro/en/docs)** • **[Issue Feedback](https://github.com/Calcium-Ion/new-api/issues)** • **[Latest Release](https://github.com/Calcium-Ion/new-api/releases)**

<sub>Built with ❤️ by QuantumNous</sub>

</div>
