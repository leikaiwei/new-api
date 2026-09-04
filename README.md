# New API (Fork)

Fork 自 [QuantumNous/new-api](https://github.com/QuantumNous/new-api)，在上游基础上打了以下补丁：

**#1 流式响应的上游 usage 被追加帧覆盖，导致 token 记账归零** — `relay/channel/openai/relay-openai.go`

- 症状：Anthropic 入口（`/v1/messages`）+ OpenAI 兼容渠道 + 流式这个组合下，日志里 output token 与 cache token 恒为 0。生产实测 454/454 条流式请求全为 0，而同渠道非流式 239/239 正常，同入口走另一个上游端点也正常
- 根因：`OaiStreamHandler` 逐帧无差别覆盖 `lastStreamData`，收尾只解析这最后一帧找 usage。部分上游在带 usage 的帧**之后**还会追加自有元数据帧（实测 opencode `zen/go` 端点发 `{"choices":[],"x-opencode-type":"inference-cost","cost":"...","normalizedUsage":{...}}`，而免费的 `zen` 端点不发此帧因而不受影响），于是"最后一帧"落在无 usage 的帧上，`containStreamUsage` 为 false，上游真实 usage 被整份丢弃、回退本地估算 `ResponseText2Usage`
- 为何恰好是 0 而非偏小值：`/v1/messages` 这条路的 `RelayMode` 是 `Unknown`（`Path2RelayMode` 无该分支，`GenRelayInfoClaude` 也未设），逐帧回调 `processTokenData` 的 switch 不匹配，`responseTextBuilder` 全程为空，估算结果精确为 0。同一 bug 在 `/v1/chat/completions` 上则表现为 output 偏差 + cache 恒 0
- 修复：额外记住最后一个带有效 usage 的帧；末帧不含 usage 时从它补回真实用量，以及被元数据帧清空的 `id` / `model` / `system_fingerprint`。`applyUsagePostProcessing` 的 body 参数一并改用该帧，使 DeepSeek / 智谱 / Moonshot 这类需从 body 二次提取 `cached_tokens` 的渠道同样受益（上游 PR #6328 缺的正是这块）。客户端可见的 SSE 内容不变
- 影响面：token 是限流、配额与用量分析的依据。当前该模型免费（`ModelRatio: 0`）故无计费损失，但这条路径结构上必然少记 output，若放付费模型会**少计费**
- 上游状态：[#6272](https://github.com/QuantumNous/new-api/issues/6272) open 无人处理，[#6158](https://github.com/QuantumNous/new-api/issues/6158) / [#6500](https://github.com/QuantumNous/new-api/issues/6500) 被 bot 自动判重关成 `not_planned`，三者均无人类维护者回应；[PR #6070](https://github.com/QuantumNous/new-api/pull/6070) 停滞且有冲突，[PR #6328](https://github.com/QuantumNous/new-api/pull/6328) 被作者自行关闭。上游合并后本地补丁可移除
- 上游 2026-09-03 同步后的现状（**部分收敛，本补丁仍需保留**）：上游 [#7137](https://github.com/QuantumNous/new-api/pull/7137) 加了同向修复 —— 末帧无有效 usage 时回退到 `secondLastStreamData`，并把 `applyUsagePostProcessing` 的 body 参数改用该帧（与本补丁的 `usageFrame` 同思路）。但它只回看**固定的倒数第二帧**，上游追加两个及以上元数据帧时依然失效；也不走 `handleLastResponse`，不补回被元数据帧清空的 `id` / `model` / `system_fingerprint`。本补丁记的是**最后一个带有效 usage 的帧**（`streamDataHasBillableUsage` 逐帧判定），严格覆盖上游那一格，故合并时保留本实现、吸收上游的 debug 日志，并删掉已成冗余的 `secondLastStreamData`
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

~~**#4 OpenAI→Claude 响应转换未减去缓存 token，下游按 Anthropic 语义重复计入** — `relaykit/relayconvert/internal/oai_chat/to_claude_messages_resp.go`~~

> **已由上游修复，本地补丁于 2026-09-03 移除。** 上游 [#7137](https://github.com/QuantumNous/new-api/pull/7137) 把 `buildClaudeUsageFromOpenAIUsage` 委托给 `internal/shared/claude.UsageFromOpenAI`，并把原来那个恒不成立的 `CacheWriteTokens > 0` 闸门换成 `UsageSemantic != BillingUsageSemanticAnthropic` —— 覆盖了本补丁的意图，且多一层保护：usage 本身已是 Anthropic 语义（`input_tokens` 本就不含缓存）时不再减，避免真 Anthropic 上游被双减。生产代码已回退到上游实现，只保留本补丁附带的回归测试 `TestBuildClaudeUsageFromOpenAICacheReadSubset`，它断言的契约在上游实现下依然成立且通过。以下原始记录保留备查。

- ~~症状：Claude Code → LiteLLM（Anthropic 风格）→ new-api → OpenAI 兼容渠道 这条链上，LiteLLM 侧统计的输入 token 恒定偏高，偏高量正好等于缓存命中量~~
- ~~两套语义的差异：OpenAI 的 `prompt_tokens` **已包含** `cached_tokens`（后者是子集）；Anthropic 的 `input_tokens` **不含** `cache_read_input_tokens`，两者是独立可加项。把 OpenAI 的 `prompt_tokens` 原样塞进 `input_tokens`、同时又填 `cache_read_input_tokens`，下游按 Anthropic 语义相加就把同一段前缀计了两遍~~
- ~~根因：`buildClaudeUsageFromOpenAIUsage` 里减法代码本来就有、注释也写明了语义差异，但被 `if oaiUsage.PromptTokensDetails.CacheWriteTokens > 0` 挡住。`cache_write_tokens` 是 OpenAI 原生缓存写入字段，opencode zen / DeepSeek 这条上游不报；`cached_creation_tokens` 是 new-api 内部字段、只在 Claude→OpenAI 方向赋值。两者恒为 0，减法永不执行~~
- ~~修复：去掉该闸门，无条件减。原有的负数 clamp 保留 —— 两个计数都是未调整前缀，可能重叠~~
- ~~影响面精确到一格：仅"上游报了 `cached_tokens` 但不报 `cache_write_tokens`"这一种情况行为改变。上游报 `cache_write_tokens` 的走的还是原来那条减法；完全无缓存时 `cached_tokens` 为 0，减法退化为恒等。golden 快照实测只有 `response/openai_to_claude` 一行变化（`input_tokens` 10→7，该 fixture 的 `cached_tokens` 为 3），其余 `*_to_claude` 快照因 fixture 无缓存而字节不变~~
- ~~**new-api 自身计费不受影响**：`convertOAIChatResponseToClaudeMessages` 回给计费层的是 `UsageFromChatUsage(&chatResponse.Usage)`（OpenAI 侧原值），流式侧是 `state.Usage`，两条都不经过本函数；`service/text_quota.go` 也自己做了 `baseTokens - cached` 的减法。错的一直只是回吐给客户端的那份 Anthropic 形状 usage~~
- ~~反向不会双减：`to_oai_chat_resp.go` 的 `buildOpenAIStyleUsageFromClaudeUsage` 刻意把 `PromptTokens` 造成"含缓存"的 OpenAI 语义，与本侧对称~~
- ~~下游不是 bug：LiteLLM `llms/anthropic/chat/transformation.py` 里的 `prompt_tokens += cache_read_input_tokens` 对真 Anthropic 上游是正确的，问题全在 new-api 这一侧~~
- ~~定位判据是不同上游路径的 `cache_read / text_tokens` 比值：经 new-api 的路径恒定 90.2%（n=9347），而真 Anthropic 上游（dashscope）可达 945.9%（n=388）。后者正常 —— Anthropic 语义下 raw input 只是未命中那一小块，缓存可以是它的好几倍；前者恒等于缓存命中率（上游自报 91.1%），说明 `input_tokens` 里含着整份缓存。**该判据取自 LiteLLM 侧日志统计，非本仓库实测**~~
- ~~未实测项：上线前未直接抓取 new-api 出站的 `input_tokens` 原值（不落日志、只能实时抓，且渠道有月度限额、#1/#2 已因 429 被自动禁用过）。修复正确性由上述语义分析加 golden 与单测锁定，不是端到端实测~~
- ~~为何不能靠配置绕开：`param_override` 的 11 个调用点全部作用于出站请求体，仓库内无响应侧覆盖钩子；渠道 `setting` 字段也无一与 usage 相关。唯一免改源码的路径是把该模型的 LiteLLM 入口换成 OpenAI 风格，但那会让补丁 #2 的 `client_thinking_type` 上下文字段（仅 `info.Request` 为 `*dto.ClaudeRequest` 时注入）与补丁 #3 一并失效，代价远大于收益~~

**#5 OpenAI→Claude 流式转换丢失工具调用轮次的收尾事件，客户端卡住** — `relaykit/relayconvert/internal/oai_chat/to_claude_messages_resp.go`

- 症状：Claude Code → LiteLLM（Anthropic 风格）→ new-api → OpenAI 兼容渠道 这条链上，模型输出一句话后停住不动，工具不执行、也不结束本轮。同一客户端、同样的工具集，换一个上游渠道就正常
- 根因：上游先发只带 `finish_reason` 的帧（无 usage），转换器按注释所述"把收尾推迟到下一帧"提前 return；而随后的 usage 帧仍带**非空** `choices`，本帧自身没有 `finish_reason`，`doneChunk` 为 false、`state.Done` 也为 false，`if doneChunk || state.Done` 不成立，收尾永不发生。`message_delta` 与 `message_stop` 全部丢失
- 为何换渠道就好：原逻辑只在收尾 usage 帧的 `choices` 为**空数组**时才走 `len(Choices) == 0` 那条收尾分支。两种上游形态的差别仅此一处，生产日志实测 296/296 全部落在受影响的那一侧，对照渠道 136/136 全部落在正常侧
- 客户端行为符合规范：Anthropic 流式文档的事件序列里 `message_delta` 恒在最后一个 `content_block_stop` 之后、`message_stop` 之前且只出现一次。转换层输出不完整的 SSE 才是契约违反，客户端把"没收到 message_stop"当作"流未结束"是预期行为
- 修复：`finish_reason` 已缓存且 usage 到达时即触发收尾，不再依赖本帧是否带 `finish_reason`
- 同时修正 `stop_reason`：本轮已产出 `tool_use` 块时不得兜底成 `end_turn`，否则客户端同样认为没有待执行的工具。覆盖流式三处收尾与非流式一处；非流式按 `content` 实际内容纠正，因此上游把带工具调用的一轮标成 `stop` 这种自相矛盾的响应也能兜住
- 并在 EOF 收尾处（`relay/channel/openai/helper.go`）补发 `Finalize` 作为最后兜底，思路取自上游 [PR #6721](https://github.com/QuantumNous/new-api/pull/6721)。`Finalize` 以 `state.Done` 幂等，已正常收尾的流不会重复发送
- **上游仍未修**（2026-09-03 同步 33 个提交后复查）：上游 [#7137](https://github.com/QuantumNous/new-api/pull/7137) 重构了本文件（新增 `startPendingToolBlocks`、把首帧特殊路径并入统一路径），但两个缺陷都还在 —— 收尾条件仍是 `doneChunk || state.Done`，带非空 `choices` 的 usage 帧依旧接不上；四处 `stop_reason` 也仍是 `"" → end_turn`，无 `tool_use` 判定。本补丁已在上游新结构上重新落位，回归测试全部通过
- 上游状态：[#4697](https://github.com/QuantumNous/new-api/issues/4697) 症状一致但 open 三个月无进展，维护者要求复现信息、原帖未给出 SSE 帧的精确形状；[PR #5345](https://github.com/QuantumNous/new-api/pull/5345) 思路接近但有合并冲突，[PR #6046](https://github.com/QuantumNous/new-api/pull/6046) 被作者自行关闭
- 同类实现参考：LiteLLM `AnthropicStreamWrapper` 把"delta 为空"当作独立的收尾信号（暂存 `message_delta`、等 usage 到达后合并发出，流结束前未等到则 flush 兜底），而非依赖 `choices` 是否为空数组 —— 后者正是本缺陷的脆弱之处
- 为何不能靠配置绕开：渠道设置两个结构体共 28 个字段，唯二沾响应的 `force_format` 与 `thinking_to_content` 在 `HandleStreamFormat` 里只传给 `RelayFormatOpenAI` 分支，Claude 格式走的 `handleClaudeFormat` 签名里没有这两个参数；其余字段全部作用于出站请求体或传输层。`advanced_custom` 只能选择转换器，不能改其内部逻辑
- 未实测项：修复由生产日志的帧序列比对加单测锁定，上线后尚需在客户端侧确认同形状请求不再卡住

**#6 Claude 工具的显式 `type:"custom"` 被当成 hosted tool 丢弃，Agent 客户端拿不到任何工具** — `relaykit/relayconvert/internal/toolconv/decode.go`

- 症状：Claude Code / Codely CLI 这类 Agent 客户端经 Claude→OpenAI 转换后，出站请求的 `tools` 为空数组，模型无工具可调，对话空转。日志刷 `conversion diagnostic: code="unsupported_hosted_tool" ... message="OpenAI Chat Completions cannot represent hosted tool \"custom\""`，并在 32 条后 truncated
- 引入版本：上游 [#7137](https://github.com/QuantumNous/new-api/pull/7137) 新增的 hosted-tool 转换保真层。**2026-09-03 升级 `fork-20260903.1` 时在生产暴露，已当场回滚**
- 根因：Anthropic 允许自定义工具显式写 `type:"custom"`，用于和 `computer_20241022` / `bash_20250124` 这类 hosted tool 区分，语义等同于省略 `type`；而 OpenAI Responses 的 `custom` 指的是接受自由文本输入的特殊工具，两者语义相反。`decodeClaudeDefinition` 对非空 `type` 一律走通用的 `kindFromNativeType`，该表按 Responses 语义把 `custom` 判成 `KindNative` 且不填 `Function`，各 encode 目标的 switch 便落到 `default` 分支整份丢弃
- 为何上游自己的测试没发现：上游确实有一条 `{"type":"custom","name":"apply_patch"}` 的用例，但它是 **OpenAI Responses→Gemini** 方向，断言 `assert.Empty(t, geminiReq.GetTools())` —— 在那个方向丢弃是正确的。缺的是 Claude 作为**来源**时的同名用例
- 修复：只放行 Claude 解码路径上的 `custom`，让它与省略 `type` 走同一条 `KindFunction` 分支。不动 `kindFromNativeType`，Responses 方向的判定保持不变
- 为何 `NativeType` 仍保留 `"custom"`：`KindFunction` 分支里唯一消费 `NativeType` 的是 `encode.go` 中 `set.Source == RelayFormatOpenAIResponses` 那条守卫，来源为 Claude 时不触发；保留它有利于 Claude→X→Claude 的往返保真
- 影响面：仅 Claude 格式入口、且工具定义显式带 `type:"custom"` 这一格。省略 `type` 的简写形状原本就正常，行为不变（有对照测试锁定）
- 回归覆盖两层：`toolconv` 直接断言解码契约（`Kind` / `Function` / `NativeType`，并另测 `computer` / `code_execution` / `mcp` 三个真 hosted tool 不被误伤），`relayconvert` 走生产入口 `ConvertRequest` 覆盖 tools 经 `ExtractRequest` / `AttachRequest` 的真实路径。**两者在回退本修复后都会失败，非静态推断** —— 第一版端到端测试误调了转换器内部函数 `ClaudeMessagesRequestToOpenAIChat`，回退后仍通过，因为生产路径的 tools 由 `toolconv` 接管、转换器自身那段处理会被覆盖
- 上游状态：尚未反馈

**#7 上游网关要求的会话亲和头无法从请求体推导，参数覆盖只能写静态值** — `relay/common/override.go`

- 症状：opencode Go 官方 2026-09-04 邮件通知，new-api 发出的请求（UA "Go HTTP client"）缺少 `X-Opencode-Session`，要求 09/06 起每个请求带一个 "stable per-conversation ID"，否则可能报错。该头是网关的会话亲和键，用于把同一对话钉到同一后端做 prompt cache
- 为何配置做不到：渠道 `header_override` 只支持静态值、`{api_key}`、`{client_header:<入站头>}` 与入站头透传；参数覆盖的 `set_header` 只能写常量，`copy_header` 只能从入站头复制。而真正的对话 ID 在 Claude 请求体的 `metadata.user_id` 里（新版 Claude Code 是含 `session_id` 的 JSON 串，不再是 `user_<hash>_account_<uuid>_session_<uuid>`），Claude→OpenAI 转换又会把 `metadata` 整个丢掉；中间层（LiteLLM）出站也不带任何会话头。仓库里没有一处能把请求体字段写进出站请求头
- 修复：`BuildParamOverrideContext` 新增 `client_session_id`，依次取入站头 `x-claude-code-session-id`、Claude `metadata.user_id`（兼容 JSON 串与旧形态）、OpenAI `prompt_cache_key`，都没有时退到 system 与首条 user 消息的哈希 —— 对话历史只追加，所以跨轮稳定，无 ID 的客户端与中间层健康检查也能钉住同一对话。`set_header` 的字符串值支持 `${变量}` 引用上下文，解析为空则不发该头。渠道侧只需一条规则 `{"mode":"set_header","path":"X-Opencode-Session","value":"${client_session_id}"}`
- 为何不在中间层做：LiteLLM 侧只有 Claude Code 流量能解出会话 ID（约 85%），Codex Desktop、OpenAI 格式客户端、LiteLLM 自身每 300 s 一次的健康检查都没有，且要重启 LiteLLM；new-api 是唯一能覆盖 100% 出站请求的位置
- 影响面：只有显式写了 `${…}` 模板的 `set_header` 规则受影响，静态值行为不变；`client_session_id` 仅是上下文里多一个字段，不改任何请求体。模板展开只作用于 `set_header` 的规则值，不作用于 `copy_header` 复制来的入站头值，客户端无法借头值引用上下文
- 回归：推导四种来源与「无 ID 不发空头」各一例、哈希在轮次增长与内容块形态翻转下不变、`set_header` 模板端到端走 `ApplyParamOverrideWithRelayInfo` 断言运行时请求头。**回退 `set_header` 那一行后模板三例全部失败，非静态推断**
- 上游状态：尚未反馈

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
- [Video Interface (Video)](https://docs.newapi.pro/en/docs/api/ai-model/videos/sora/createvideo)
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
| `RELAY_RESPONSE_HEADER_TIMEOUT` | How long the relay waits for upstream **response headers**, seconds; set `0` to disable. Only bounds the header wait -- streaming after the headers arrive is unaffected. Note that non-streaming upstreams usually send headers only once generation finishes, so leave headroom | `1800` |
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
