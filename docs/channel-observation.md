# 渠道观测（逐请求渠道分布）—— 判定、更正与实现说明

结论先行：

- 渠道信号有**两个已上线的来源**，取自 CPA 的不同位置：
  1. **usage 钩子（P1）**：宿主在每个请求结束后回调一次，给的是 CPA 为这条请求选中的**凭据**（`Provider` / `AuthID` / `AuthIndex` / `AuthType`）加上宿主自己测的 TTFT / token 账。它覆盖所有客户端协议（含 `POST /v1/responses`），是**记录（record）**的来源，页面现有的「渠道」区取自它；
  2. **CPA 请求日志（P2，Phase 2 已上线）**：CPA 自己看到的**上游响应原文**里有 Cline 网关的 `provider_metadata.gateway.routing`，只有这些日志能看到。它是网关级渠道**事实（fact）**的来源（`finalProvider` / `resolvedProvider` / 尝试次数 / 网关成本）。
- **本文旧版有两处因果写错，已更正**（`docs/request-log-channel-plan.md` §8 指定的更正）：
  - 旧版把 P2 判为「本部署下是死路」。成立的只是「不动 `commercial-mode` 时是死路」；关掉之后 P2 是网关级渠道的**唯一**来源，现已实现并上线（§2.2）。
  - 旧版把「`/v1/responses` 的响应里没有渠道块」当成**上游**事实。错：上游响应里有这个块，是 CPA 把上游 `chat/completions` 响应翻译成 Responses 事件时把它丢掉了；而 clinepass 上游**根本没有 `/responses` 端点**（直连实测 404）。块消失的位置是**客户端可见的流**，不是上游（§2.3）。
- **P3（响应侧分片拦截器）退役不变，但理由按同一更正重写**：它挂在翻译之后，只能看到客户端可见的流，因此在约 99% 的 `POST /v1/responses` 流量上永远看不到渠道块。
- 代价与边界：P2 要求 CPA 侧同时打开请求日志（`observability.logs.request-log: true` **且** `server.commercial-mode: false`），日志里是**明文 prompt**，长上下文请求单文件 4.0–5.7 MB，实测目录增速约 8 MB/分钟、日增量约 3.5 GB；开关前后的代价对照见 §4.3。
- **fact 已并进记录并画到页面上**（Phase 3/4，构建 `0.3.0-dev.181/182`）。两个维度分列：记录与页面把网关级 `finalProvider` 记在 `final_provider` / `resolved_provider` / `gateway_*`（`channel_source` 标明是否来自日志），把 CPA 侧凭据记在 **`cpa_provider`**（schema v3 改名；v1/v2 旧行的 `final_provider` 仍在读时按凭据解释）。`off_baseline` 现在只对**真渠道**判定，默认 `deepseek` 才真正成立；fail 的请求上游不返回路由块，渠道列显示「无渠道块」而不是记 0。join 用**持久化的事实文件**（见 §3.5）做读时匹配，因此重启后历史窗口仍可判定。

本文回答一个问题，并记录它的取证过程：**过去 24 小时里，clinepass 的请求有多少条 / 多大比例没有落在基准渠道**，
以及这些请求的 TTFT 与解码速度是多少。

本文的判定先于实现：`§2` 的路径结论是在写第一行实现代码之前、用可复现命令在 oracle 上取证得出的（命令与原始输出见各小节）。
`§2.3` 记录的流式分片路径（`response_stream_interceptor`）**曾选定、后被退役**：它只能看到 OpenAI
`/v1/chat/completions` 响应里的 `provider_metadata.gateway.routing`，在 `/v1/responses` 的流上实测 34 帧里 0 帧命中，
而那正是生产上约 99% 的流量（24 小时 5,843 条请求里）——即旧设计只能记到验收探针，记不到真实流量。
退役理由与数字见 §2.3、§4.5。
`§4` 的性能数字分三批：`§4.1` 是 P1（usage 钩子）的成本模型（量化实测**待做**），`§4.2`／`§4.3` 是 P2（CPA 请求日志）
的体积与代价实测（2026-10-02），`§4.4` 是为**退役设计**测的三态数字（保留，但必须当历史读）。

宿主侧源码引用（`sdk/...`、`internal/pluginhost/...` 的行号）来自本仓库 `.reference/CLIProxyAPI` 的 **v8.0.8** 检出；
本插件自身依赖的是 `CLIProxyAPI/v7 v7.3.8`（`go.mod`）。`internal/channellog/...` 是本仓库自己的代码。

## 1. 目标与硬约束

目标（不可退让的部分）：

- 面板能回答「过去 24h 有多少条 / 多大比例的 clinepass 请求没有落在基准渠道」，并且能被抽查核对；
- 同时给出这些请求的 TTFT 与解码速度；
- 口径是**全量采集**（每条完成的请求都要进去，比例必须是精确值，不是抽样估计）；
- 记录保留 3 天、磁盘上限 512 MB。

硬约束：

1. **不许回到请求路径上**。v0.2.0 删掉逐请求统计的原因就是 `ResponseBeforeTranslator` 会让宿主为每一帧
   clone 并向插件 JSON 序列化两份请求体（当时 7–16 ms/帧、cpa CPU 106–192%、357 → ~100 t/s）。
   现实现只声明 `UsagePlugin` 这一个数据能力，且宿主在请求**结束后**才调用它；除此之外不声明任何请求侧 /
   响应侧 / 翻译 / 归一化能力（`ResponseBeforeTranslator`、`ResponseAfterTranslator`、`ResponseTranslator`、
   `StreamChunkInterceptor` 全都不声明）。P2 是**旁路读文件**，也不在请求路径上（§2.2）。
2. 插件**永不阻塞**请求：任何失败都必须 fail-open（宿主捕获 panic 并熔断插件，见 §3.4）。
3. v0.2.x 的能力不得回退：套餐/额度/官方用量、凭据自动发现、`/health` 自诊断都必须照旧。
4. 不改 CPA 源码、不改写请求或响应、不干预路由与固定（pinning）。
5. **信号分层（本轮更正）**：usage 载荷里**没有** Cline 网关级的 `provider_metadata.gateway.routing.finalProvider`。
   - 默认部署（P2 关闭）：渠道口径是 **CPA 侧凭据**（`Provider` / `AuthID` / `AuthIndex` / `AuthType`）与上游模型
     slug（`upstream_model`）；网关内部是否发生回退（例如这条请求落到了 `alibaba` / `particle` 而不是 `deepseek`）
     **看不到**，这是默认配置下的已知代价。
   - 打开 P2（`channel_log_enabled: true`，且 CPA 侧已开请求日志）：网关级 `finalProvider` 可见，代价见 §4.2 / §4.3；
     记录与页面把两套值分列（`cpa_provider` 与 `final_provider`/`gateway_*`），判定基准口径用后者（§2.4）。
6. **明文 prompt 只在必要时间内存在**。P2 读的日志含客户端明文 prompt，所以 `channel_log_delete_after_read`
   默认 `true`（fact 落盘后才 unlink 源文件），并且生产上不得把请求日志长期常开（§5.2、§6「仍未做」）。

## 2. Step 0：三条路径的判定

渠道信息在 CPA 内部有三处可及：宿主自己的 usage 记录、CPA 写下的请求日志（内含上游响应原文）、
以及插件侧响应拦截钩子（只能看翻译之后的流）。判定如下：

| 路径 | 能否拿到渠道 | 覆盖哪些流量 | 结论 |
| --- | --- | --- | --- |
| P1 usage 钩子（`UsagePlugin` / `usage.handle`） | 能：CPA 为这条请求**选中的凭据**（`Provider` + `AuthID` / `AuthIndex` / `AuthType`） | **所有客户端协议**，每请求一次 | **选定（记录来源）** |
| P2 解析 CPA 自身 request-log | 能：上游响应原文里的**网关路由块**（`finalProvider` 等） | 全量（每请求一个日志文件），但必须先开 CPA 请求日志并**重启容器** | **Phase 2 已上线（网关渠道来源）** |
| P3 响应侧拦截器（`response_stream_interceptor`） | 翻译之后看不到渠道块，只有客户端可见流里侥幸出现的帧 | 只在翻译前带 `provider_metadata` 的 `/v1/chat/completions` 响应上有 | **退役（理由已更正）** |

### 2.1 P1：usage 钩子 —— 记录来源（选定）

- `sdk/pluginapi/types.go:1315-1316`：`type UsagePlugin interface { HandleUsage(context.Context, UsageRecord) }`，
  每个请求结束调用一次。
- `UsageRecord` 字段（`sdk/pluginapi/types.go:1472-1528`）：`RequestID:1474`、`TraceID:1476`、`Provider:1478`、
  `BaseURL:1480`、`ExecutorType:1482`、`Model:1484`、`Alias:1486`、`APIKey:1488`、`SessionID:1490`、
  `ParentSessionID:1492`、`AuthID:1494`、`AuthIndex:1496`、`AuthType:1498`、`Source:1500`、
  `ReasoningEffort:1502`、`ServiceTier:1504`、`ResponseServiceTier:1506`、`ResponseModel:1508`、`Generate:1511`、
  `Stream:1513`、`RequestedAt:1515`、`Latency:1517`、`TTFT:1519`、`Failed:1521`、`Failure:1523`（`UsageFailure`
  `{StatusCode,Body}`，`1531-1536`）、`Detail:1525`（`UsageDetail` 的 token 计数，`1539-1554`）、
  `ResponseHeaders:1527`。结构体**没有 json tag**，所以跨 ABI 的 JSON 键就是这些 Go 字段名。
- ABI 与方法名：`sdk/pluginabi/types.go:82 MethodUsageHandle = "usage.handle"`；注册侧能力键在
  `internal/pluginhost/rpc_schema.go:45`（`UsagePlugin bool`，JSON 键 `usage_plugin`）。
- 装配与调用链：`internal/pluginhost/adapters_usage_translation.go:19-33 RegisterUsagePlugins()`（仅当
  `record.plugin.Capabilities.UsagePlugin != nil` 时注册，装配点在 `sdk/cliproxy/service_plugins.go:127`）
  → `:132 usageAdapter.HandleUsage` → `:170` 组装 `pluginapi.UsageRecord` → `:605-611`
  `rpcPluginAdapter.HandleUsage` 走 `callPlugin(…, pluginabi.MethodUsageHandle, record)`；调用侧
  `sdk/cliproxy/usage/manager.go:459 plugin.HandleUsage(ctx, record)`，外面是 `safeInvoke`（`:452-461`，panic 被捕获并记日志）。
- **为什么它就是渠道**：`Provider` 是 CPA 自己给这次上游执行挑的渠道/凭据名（生产上是 `openai-compatible-cline2`
  这类），`AuthID` / `AuthIndex` / `AuthType` 进一步指到具体凭据（`openai-compatibility:cline2:bcaef0dbf8d3`、
  `7dff1d7b60577eec`、`apikey`）。旧判定把 P1 判为「拿不到渠道」，是因为当时只在找网关的 `provider_metadata`；
  真实答案是 **`UsageRecord` 没有响应体字段，但它有 CPA 侧的落点**，而这正是「这条请求走了哪条渠道」的代理指标。
- **它拿不到什么**：网关级 `finalProvider`（回退可见性，见 §1 约束 5）、网关成本、原始响应帧相关字段
  （`frames` / `protocol` / `pinned_provider` / `affinity` 等）。这些字段现在由 P2 的 fact 提供（§2.2 / §3.5），
  但尚未与记录合并。
- **它的代价**：宿主每完成一个请求调用一次插件（一次 JSON 解码 + 一条记录构建）。没有逐帧工作、不读响应字节、
  不克隆请求体；关闭观测时不声明该能力，这次调用不存在。

### 2.2 P2：解析 CPA 自己的 request-log —— 网关级渠道的唯一来源（Phase 2 已上线）

**旧版写错的地方（`docs/request-log-channel-plan.md` §8 指定的更正）**：

| 旧版的写法 | 更正后的事实 |
| --- | --- |
| 「本部署下是死路」 | 在**不动 `commercial-mode`** 的前提下确实出不来文件（旧版的取证成立）；关掉 `commercial-mode` 并打开 `observability.logs.request-log` 后，P2 是网关级渠道的**唯一**来源，已实现并上线 |
| 「代价与约束 1 矛盾」 | 不矛盾。约束 1 禁的是**插件回到请求路径**；P2 是旁路读 CPA 写好的日志文件，不声明任何请求侧 / 响应侧 / 翻译能力。真正的代价是 CPA 侧要开一套请求日志与 HTTP 中间件，以及**明文 prompt 落盘**（§4.2 / §4.3） |

**网关确实返回渠道块。** Cline 网关在响应里给出：

- `provider_metadata.gateway.routing`：`finalProvider` / `resolvedProvider` / `canonicalSlug` / `originalModelId` /
  `modelAttemptCount` / `totalProviderAttemptCount` / `affinity.pinnedProvider`；
- `provider_metadata.gateway.cost`。

**clinepass（`api.cline.bot`）没有 `/responses` 端点。** 直连它的 `/responses` 返回
`404 {"error":"Not Found","success":false}`，四种变体都试过（流式 / 非流式 / 带 `providerOptions` / 带 `metadata`）。
CPA 给客户端提供 `/v1/responses` 的方式，是把它自己的上游 `chat/completions` 调用**翻译**成 Responses 事件，
而翻译过程丢掉 `provider_metadata`。所以：

- 渠道块的缺失发生在**翻译之后、客户端可见的流**里；
- `POST /v1/responses`（生产上约 99% 的真实流量）的渠道，只能从 **CPA 自己看到的上游响应**里取，
  也就是 CPA 的请求日志——这是 P2 存在的唯一理由。

**前置条件（实测）**：

- 两个开关同时满足才写日志：`observability.logs.request-log: true` **且** `server.commercial-mode: false`。
  源码：`internal/runtime/executor/helps/logging_helpers.go` 的
  `requestLogCaptureEnabled = cfg.RequestLog && !cfg.CommercialMode`。
- **reload 不够，必须重启容器。** 实测：管理接口 reload 成功（日志里出现
  `config successfully reloaded, triggering client reload`）但**零个** request-log 文件；`docker restart cpa`
  之后一分钟内出现两个文件。源码原因：中间件与 `sdkCfg.RequestLog` 在 server 构造时装配
  （`internal/api/server.go`、`internal/api/server_options.go`）。
- 生产主机：CPA **v8.0.8**（commit `fd48ea6`，build 2026-10-01），容器 `eceasy/cli-proxy-api:latest`，
  容器内日志目录 `/CLIProxyAPI/logs`（宿主 `/opt/cpa/logs`）。

**旧版取证（保留，结论已按上表修正）**：`PUT /v0/management/request-log`，body `{"value":true}`
（处理函数 `internal/api/handlers/management/handler.go:426-436 updateBoolField` 只读 `Value *bool`）→
reload 确认 `request-log: false -> true` → 打一条真实流式请求（37 帧，含 `provider_metadata`）→
3 分钟内在 `/opt/cpa/logs` 里**除 `main.log` 外没有出现任何新文件**。根因是当时 `server.commercial-mode: True`：
`internal/api/server.go:156 if !cfg.CommercialMode {` 才安装 `RequestLoggingMiddleware`；
`internal/api/server_options.go:51-52 if cfg.CommercialMode { sdkCfg.RequestLog = false }` 在商业模式下强制关闭。
**修正**：这证明的是「不动 `commercial-mode` 时开不出日志」，不是「本部署下此路不通」。

**日志形态（真实，v8.0.8）**：按出现顺序分段 `REQUEST INFO`（`Version` / `URL` / `Method` /
`Downstream Transport` / `Upstream Transport` / `Timestamp`）、`HEADERS`（`Authorization` 已被 CPA 掩码成
`sk-4...cnF7`；部分客户端带 `Session_id: session-<uuid>`）、`REQUEST BODY`（**明文 prompt**，实测某条请求
`Content-Length: 2501640`）、`API REQUEST 1`、可选的 `API ERROR RESPONSE`、`API RESPONSE 1`、`RESPONSE`。
重试过的请求里会先有错误段、再跟被服务的响应段，解析器必须取**最后一个成功的 `API RESPONSE`**。

文件名：`<路径下划线化>-<YYYY-MM-DDTHHMMSS>-<CPA 内部 UUIDv7 的后 8 位>.log`。这个 id **不**出现在 usage 载荷里，
所以文件名不是 join key。

**join key（实测，12 条里对上 10 条）**：日志 `HEADERS` 里的 `Session_id` 头是 `session-<uuid>`；
usage 记录的 `session_id` 是 `codex:session-<uuid>`（同一个 uuid）。匹配规则：该 uuid 出现在记录的 `session_id` 里
**且** `|log.Timestamp − record.time| < 2s`（会话匹配的窗口）；实测偏移 **72–218 ms**（记录的时间是 usage 上报的 `RequestedAt`，
晚于日志的到达时间戳）。**不发 `Session_id` 头的客户端**（其记录形如 `lcp:v1:<hex>`）走 token/时间兜底（2026-10-03 已实现：fact 新增 `prompt_tokens`/`completion_tokens`，两个计数器精确相等 + 候选唯一；
窗口 **5 秒**，2026-10-05 由 2 秒放宽——客户端整段重发上下文时请求体 90 MB 量级，`RequestedAt` 比日志到达戳晚 **1.99–2.38 s**，2 秒窗口正好把这类请求的渠道丢成「无渠道块」）。

### 2.3 P3：响应侧拦截器 —— 曾选定，已退役（因果已更正）

**当时为什么选它。** 上游响应字节的可用入口只有这些（除三个被禁的 Response\*Translator 之外）：

| 钩子 | 粒度 | 当时结论 |
| --- | --- | --- |
| `ResponseBeforeTranslator` | 每响应 | **禁止**（约束 1，最贵） |
| `ResponseTranslator` / `ResponseAfterTranslator` | 每响应（非流式） | 不用：流式响应不走它 |
| `Capabilities.ResponseInterceptor`（`ResponseInterceptRequest` `sdk/pluginapi/types.go:1190-1204`） | 每个非流式响应 | 覆盖不了流式 |
| `Capabilities.StreamChunkInterceptor`（`StreamChunkInterceptRequest` `sdk/pluginapi/types.go:1216-1247`） | **每个 SSE 分片** | 当时**选用**：渠道帧与 usage 帧都在分片里 |
| `WebSocketResponseObserver`（`WebSocketResponseEvent:1265`） | 每个 ws 事件 | 只对上游 ws executor 生效，本场景用不上 |

当时的宿主行为记录（说明它为什么曾经是安全的）：

- **零开销开关**：`internal/pluginhost/rpc_client.go:150-151` 只在 `resp.Capabilities.StreamChunkInterceptor`
  为真时挂 adapter；`internal/pluginhost/adapters_interceptors.go:303 HasStreamInterceptors()` 在能力为 nil 时返回 false，
  于是 `sdk/api/handlers/handlers_stream.go:96 streamInterceptorsActive := streamInterceptorsEnabled(interceptorHost)`
  为假 → 不启用观测时请求路径不付出代价。
- **fail-open**：`internal/pluginhost/adapters_interceptors.go:56-72 callStreamChunkInterceptor` 捕获 panic 并
  `fusePlugin`；插件返回错误只会让 `ok=false`，分片照原样转发。
- **分片载荷**：`internal/pluginhost/adapters_interceptors.go:257 InterceptStreamChunkExcept` 每片 `bytes.Clone(req.Body)`；
  schema ≥3 剥掉 `OriginalRequest` / `RequestBody`，schema ≥5 剥掉 `HistoryChunks`。
- 分片钩子的 `RequestID` 与 `UsageRecord.RequestID` 是**两个不同的 UUID**，唯一的公共键 `TraceID` 并不在
  `StreamChunkInterceptRequest` 里（`:1216-1247`）——所以渠道记录无法与 usage 记录按键 join，这正是当时
  「渠道信息只从响应帧自给自足地取全」的原因。这条缺口现在由 usage 载荷直接补齐（一条记录里同时有
  `Provider` 与 `Detail`），而网关渠道由 P2 的 fact 补齐。

**因果更正（本节旧版写错的地方）。** 旧版写「渠道块只出现在 OpenAI `/v1/chat/completions` 的响应帧里」、
「`POST /v1/responses` 的流式响应一帧都没有 `provider_metadata`」，并把这两句当成上游的事实。正确说法是：

- 渠道块在**上游**（CPA → `api.cline.bot` 的 `chat/completions`）响应里有，而且一直在；
- CPA 把上游 chat 帧翻译成 Responses 事件时丢掉 `provider_metadata`，所以**客户端可见的** `/v1/responses` 流里
  一帧都没有；实测 dump 了 34 帧、含渠道帧 0 帧，只有 `response.completed` 带 `usage`；
- 拦截钩子 `StreamChunkInterceptor` 的注释写明它在 "before downstream delivery"，即**翻译之后**，
  所以它天然看不到上游原文。这就是「本地看不到渠道块」的原因，不是上游没给。

**退役理由（实测）。**

1. 渠道块只在翻译前带 `provider_metadata` 的 `/v1/chat/completions` 响应帧里出现；历史上 70 帧的流里只有 1 帧带
   `provider_metadata`（`sudo python3 /tmp/channel_probe.py stream 120 deepseek-flash-1`）。
2. 客户端可见的 `POST /v1/responses` 流里，渠道帧为 0（见上，34 帧 0 命中）——拦截器挂在这一侧，等于在
   生产的主要流量上无效。
3. 生产主机的流量构成决定性地否掉了它：24 小时内 **5,843 条**请求里约 **99%** 是 `POST /v1/responses`。
   也就是说旧设计能记到的只有验收探针自己打的 chat 请求，真实流量几乎全部漏掉。要拿网关渠道，正确的位置是
   CPA 自己看到上游响应的地方，即请求日志（P2），不是翻译之后的流。
4. 它还要为探针假阳性埋单：`provider_metadata` 这个词会出现在模型正文里，当时实测到「假阳性吃掉重试预算 →
   放弃该流 → 真正渠道帧被跳过 → 记录丢失」（`parse_failures: 4`）。当时的修正（假阳性只计 `needle_misses`、
   终止帧当场 `abandon` 计 `unresolved`）随该设计一起移除，记录在此以免重犯。P2 的解析器对同一问题的处理是
   **逐帧 JSON 解码、绝不文本匹配**（§3.5），有测试守着。

**「只记上游模型 slug 等于没有渠道」这条当时同样成立。** 历史数据里 16,396 条 `deepseek-v4.1-flash` 记录中有 4 条
`finalProvider` 是 `alibaba`(3) / `particle`(1)，而 `canonicalSlug` 仍然是 `deepseek/deepseek-v4.1-flash`。
这条证据今天仍然成立：记录里的 `upstream_model` 是**上游模型名**，它替代不了 `finalProvider`；回退到哪个渠道
只有 P2 的 fact 能看到。

### 2.4 选型结论

- **P1（usage 钩子）是记录来源**：全协议覆盖、每请求一次、不回到请求路径、可关闭；回答「CPA 把这条请求交给了哪个凭据」
  以及 TTFT / token 账。
- **P2（CPA 请求日志）是网关渠道来源**：唯一能看到 `provider_metadata.gateway.routing` 的位置，Phase 2 已上线；
  代价是必须打开 CPA 请求日志（明文 prompt 落盘、磁盘增量见 §4.2/§4.3），而且**默认关闭**。
- **两者目前是并列的，不是合并的**：fact 只写进 `channel-log-<UTC 日期>.jsonl` 并通过
  `/channel`、`/health` 的 `channel_log` 段暴露（最新 20 条），页面上的渠道口径仍是 P1 的 CPA 凭据。
  合并（Phase 3：记录改名 `cpa_provider`、`final_provider` 让给网关值、按 join key 并字段）与页面（Phase 4）
  **在本文对应的实测构建（`0.3.0-dev.180`）里尚未实现**，见 §6「仍未做」。
- P3 退役不变：它唯一的优势（网关级 `finalProvider`）不足以抵消「在 ~99% 流量上完全失效」。

## 3. 实现（as-built）

### 3.1 组件

| 位置 | 作用 |
| --- | --- |
| `internal/config/config.go` | 9 个 `channel_*` 键：P1 的 5 个（`channel_observe_enabled` 默认 true、`channel_store_dir` 默认 `/CLIProxyAPI/logs/channel-observation`、`channel_retention_days` 默认 3 上限 30、`channel_max_size_mb` 默认 512 下限 16、`channel_baseline_provider` 默认 `deepseek`）与 P2 的 4 个（`channel_log_enabled` 默认 false、`channel_log_dir` 默认 `/CLIProxyAPI/logs`、`channel_log_delete_after_read` 默认 true、`channel_log_min_age_seconds` 默认 5） |
| `internal/observation/observation.go` | P1 的 `usage.handle` 入口（`HandleUsage`）、载荷结构 `usageWire`、记录映射 `buildRecord`、`Recorder` / `Health`、写入队列与重启回填 |
| `internal/observation/store.go` | 按 UTC 日一文件 `channel-<YYYY-MM-DD>.jsonl`，异步批量写、按保留天数与体积清理、流式扫描、CSV 导出（`WriteCSV`） |
| `internal/observation/aggregate.go` | 内存小时环（`bucketSlots = 24*7`）+ 中位数/分位数 + `Summary(window)` |
| `internal/observation/export.go` | CSV 表头与行（含 `off_baseline` 列，由基线口径算出） |
| `internal/channellog/channellog.go` | P2 的 scanner：轮询、seen 集、健康度、fact 内存环与 fact 文件保留 |
| `internal/channellog/parse.go` | P2 的日志解析：按 `=== NAME ===` 分段、`data:` 帧 JSON 解码、取最后一个成功响应段 |
| `internal/channellog/store.go` | P2 的 fact 落盘：`channel-log-<UTC 日期>.jsonl` 一条一行，open/append/close |
| `internal/plugin/plugin.go` | 声明 `ManagementAPI` + 仅当 `channel_observe_enabled` 时声明 `usage_plugin`；仅当 `channel_log_enabled` 时启动 scanner；方法分发里的 `pluginabi.MethodUsageHandle` case；`LoadConfig`/`Shutdown` 负责启停 |
| `internal/management/channel.go`、`internal/management/index.html` | `GET /channel`（JSON，含 `channel_log` 段）、`GET /channel.csv`（CSV）、页面「渠道」区；`/health` 增加 `channel_observation` 与 `channel_log` |
| `cmd/channel-probe/` | 诊断探针（非发布产物）：把宿主交给插件的原始载荷落盘，用来确认渠道 / 凭据对插件是否可见 |
| `scripts/bench_channel.py` | 计时/CPU 基准脚本，口径仍是旧设计的三态（流式请求的帧间隔 / 解码窗口 / CPA CPU）；现设计的测量需要另写 |

### 3.2 记录映射与热路径（P1）

热路径只有一件事：**一次 JSON 解码 + 一条记录**，由宿主在每个请求结束后触发。

- `HandleUsage(raw)`（`internal/observation/observation.go`）解 `usageWire`；解不开就只计一次
  `decode_failures` 并返回「不改变」信封，**永不返回 error、永不 panic**；
- 无论成功与否，回给宿主的都是预编码好的 `{"ok":true,"result":{}}`（`keepAnswer`）——宿主拿到的 usage 结果为空，
  它不会因为本插件改动任何东西；观测关闭时 `Active()` 返回一个什么都不做的 recorder，仍然回同样的信封；
- 映射规则（`buildRecord`）：

  | 记录键 | 来源 |
  | --- | --- |
  | `time` | `RequestedAt`（RFC3339，解析失败用插件时钟） |
  | `request_id` | `RequestID`，空时退回 `TraceID` |
  | `final_provider` / `resolved_provider` | 同值，都取 `Provider`（**注意：这是 CPA 凭据名，不是网关渠道**；Phase 3 要改名 `cpa_provider`，见 §2.4） |
  | `model` | `Model`（路由名，例如 `cline-pass/deepseek-v4.1-flash`） |
  | `upstream_model` / `canonical_slug` | 同值，都取 `ResponseModel`（上游真实模型，例如 `deepseek/deepseek-v4.1-flash`） |
  | `ttft_ms` | `TTFT` ÷ 1e6（载荷单位是纳秒） |
  | `duration_ms` | `Latency` ÷ 1e6 |
  | `decode_ms` | `duration_ms − ttft_ms`，负数夹到 0（宿主两个计时独立测量） |
  | `tokens_per_second` | `output_tokens ÷ (decode_ms/1000)`，只在 `decode_ms ≥ 50` 且 `output_tokens > 0` 时有值 |
  | `status_code` | 宿主报失败时取 `Failure.StatusCode`（是 0 就保留 0），否则 `200` |
  | `total_tokens` | `Detail.TotalTokens`；为 0 时退回 `input_tokens + output_tokens` |

- 不落盘 `APIKey`、`Source`（凭据哈希）与载荷里的 `BaseURL`；`Detail` 的七个 token 计数全部落盘。

### 3.3 存储、口径与健康度（P1）

- 文件：`<channel_store_dir>/channel-<YYYY-MM-DD>.jsonl`（UTC 日切），一行一条记录，`v` 为 schema 版本（当前 **2**）。
  目录里不属本插件命名的文件**绝不删除**（P2 的 `channel-log-*.jsonl` 正是靠命名前缀被 reader 跳过）。异步写入：
  `queue` → `writeLoop`（`bufio.Writer`，按字节数或定时 flush），队列满只丢弃并计数。
- 保留：先按 `channel_retention_days` 删旧文件，再按 `channel_max_size_mb` 从最旧删到限额；目录打不开只记健康度，不致命。
- 重启回填：启动时回填最近 24 小时的 JSONL（超过 `warmupByteCap = 96 MB` 截断，`/health` 的 `warmup.truncated` 标出来），
  内存小时桶覆盖滚动的 7 天。
- **v1 旧行的读取**：目录里还留着更早的流式分片设计（探针时代）写下的行（`v: 1`），reader 仍然读；v2 不写
  `generation_id`、`original_model_id`、`pinned_provider`、`affinity`、`upstream_request_id`、`fallbacks_available`、
  `model_attempt_count`、`total_provider_attempt_count`、`protocol`、`frames`、`cost_usd`、`is_byok`、`user_agent`、
  `claude_code_version`、`client_app`、`source_format` 这些键，因为 usage 载荷里没有对应字段。CSV 表头保留其中五列
  （`generation_id` / `original_model_id` / `pinned_provider` / `upstream_request_id` / `protocol`），v2 行留空。
  这些字段现在由 P2 的 fact 提供（§3.5），但**还没有并进记录**。
- 页面口径（页面上也写了同样的说明）：
  - **分母是窗口内的记录数**：每条完成的请求一条记录，与协议无关，没有「未识别」这一类（旧设计的
    `resolved` / `unresolved` / `parse_failures` / `needle_misses` / `pending_streams` 随分片探针一起移除）；
  - 小时表保留窗口内**每个**整点刻度（含空小时，显示 0，不插值）。窗口起点落在小时中间，所以 24 小时窗口有
    **25 个刻度**（首尾各覆盖部分小时）；payload 仍然带全部刻度，页面从 **2026-10-03** 起只画**最近 10 个**
    （折线图与小时表同一口径，标题写着「时间线（每小时 · 最近 10 小时）」）。
  - 时间线在 **2026-10-04** 从两张 sparkline 卡片换成**一张三条线的走势图**（请求 / 偏离 / 失败，固定
    `viewBox` + `width:100%` 等比缩放，颜色取 CSS 变量，浅色/深色都跟着主题走）。图上给趋势、下方小时表给数字，
    两边读同一份 `newestChannelHours()`，所以不会出现图上有点、表里少一行。小时表仍然**从近到远排**：最新的整点
    在最上面，表头写着「时间（新 → 旧）」。
  - **视觉基线对齐 CPA Usage Keeper**（2026-10-07 首版，同日按实例 `/cpa/overview`、`/cpa/request-events`、
    `/cpa/analysis`、`/cpa/realtime` 四张线上截图校正）。取值直接读它打包的 CSS，不靠目测：浅色用
    `[data-theme=white]` 那一套——页面与卡片同为 `#fff`、靠 `#e5e5e5` 的 1px 描边分层，交互底色与
    底槽 `#f6f6f6`，正文 `#2d2a26` / `#6d6760` / `#a29c95`（**不是** `:root` 的暖灰 `#faf9f5` /
    `#f0eee8`）；深色用 `[data-theme=dark]` 的 `#151412` / `#1d1b18` / `#262320`。卡片圆角取
    `--keeper-card-radius: 24px`，阴影 `--shadow-lg`。统计卡逐条照 `.statCard`：中性描边 + 顶部一条
    从 12px 起笔、42% 处降到 68% 后渐隐的主题色细线 + 左上角 18% 主题色晕开、右上 34px / 8px 圆角的
    实心图标徽章、底部 `#f6f6f6` 迷你图底槽（**没有数据的卡片也保留空底槽**，与 keeper 的空图一致）、
    数值 28px / 800 + 等宽数字、标题用 `--text-tertiary`（实测 `#a29c95`，不是主题色）。表头是白底
    11px / 600 小字、不用大写；徽标是 999px 胶囊（「基准渠道」用 success 语义色，套餐名、占比这类纯
    标注保持中性）。分段控件（窗口切换）与按钮都是胶囊。图表用 keeper 的蓝 / 红 / 琥珀三色、细实线网格、
    居中圆点图例，以及悬停浮层（取代原来的原生 `<title>`；数值仍是同一份 `newestChannelHours()`）。
    两处刻意偏离 keeper 令牌只为过正文对比度：深色下的状态色取浅一档（keeper 的 `#c65746` 压在
    `#1d1b18` 上只有 4.0:1），主按钮填充比 `#8b8680` 深一档（白字压在 `#8b8680` 上只有 3.6:1）。
    颜色全部走 CSS 变量，浅色/深色由 `prefers-color-scheme` 切换，等价于 keeper 的 Auto 打开时取它的
    white 主题。
  - 页面区块顺序固定为 **Cline 套餐用量（官方接口） → 渠道**。`官方用量明细` 整节、渠道区里的
    `上游推理渠道（官方 usage）` 表与**整个「概览」节**先后在 2026-10-03 / 2026-10-04 按用户要求从页面移除。
    「概览」的三个数字全部来自官方逐条用量（`GET /users/{id}/usages`），而那条路径每次刷新都要按 200 条/页倒着
    翻整个账号历史，所以 `plan_usage_enabled` 也已**默认关闭**：接口仍返回 `official_channels` / `official_usage`
    与 `plan.windows[]` 这几个键，但在默认配置下它们分别是空列表、`enabled: false` 与 `null`。渠道区内部依次是
    渠道卡片（请求数 / 偏离基准渠道 / 缓存命中率，全部本机口径）、`真实渠道` 表、时间线（折线图 + 小时表）、
    `原始记录` 表与它下面的翻页脚注。成本列与成本卡片已随成本字段一起移除；`按模型` 表已删除（2026-10-03，两张渠道表已回答它要回答的问题）；
    `原始记录` 表显示 时间 / 模型 / 渠道 / 凭据（认证 ID + 索引）/ 失败 / TTFT / 解码 t/s / 输入-输出-缓存读 / 状态 / 会话，
    **每页 20 条**：滑到脚注（或点「加载更多」）取下一页，脚注写着还有多少条，表头行写着「已显示 N / 共 M 条（导出 CSV 是完整窗口）」。
    **P2 的 fact 与 `channel_log` 目前不在页面里**（页面还没有渲染这一块，Phase 4 未做）。
  - 「偏离基准渠道」= `final_provider` 与 `channel_baseline_provider` 不区分大小写地不等，**读时**判定
    （`aggregate.go` 里 `provider != "" && baseline != "" && !strings.EqualFold(provider, baseline)`，记录本身不落盘
    `off_baseline`），所以改基准会立刻改变历史窗口的比例。页面上的基线名取自 payload 的 `baseline_provider`，
    **页面里不硬编码渠道名**。注意它比的是记录里的 `final_provider`——**当前是 CPA 凭据名**，不是网关渠道；
    Phase 3 把 `final_provider` 让给网关值之后，这个判定才会落在真渠道上（plan §5）。
  - **当前生产上的口径冲突（未决）**：配置的基准仍是 `deepseek`，而 CPA 侧的渠道名是
    `openai-compatible-cline1/2/3`，两者不可能相等，于是窗口内每一条都算偏离（验收时 1h 窗口 6 条全部
    `off_baseline=yes`）。基准该怎么定（改基准名、还是按 `upstream_model` 重新定义「官渠」）**尚未决定**。
  - **TTFT 与解码速度都取自宿主字段**（`TTFT` / `Latency`，整数纳秒），不再由插件按帧时间戳推算；两个计时由宿主
    独立测量，所以 `decode_ms` 可能为负、记录里夹到 0。宿主这两个计时的起点（是否含「客户端 → CPA 排队 →
    连上游」那段）**本轮没有单独取证**：旧设计曾有「代理侧口径、比客户端少约 0.9 s」的实测，那是分片时间戳得出的，
    不能直接搬到新字段上。
  - **解码速度有下限**：解码窗口（`decode_ms`）短于 `minDecodeWindowMs = 50` 时不记速度（`tokens_per_second: 0`），
    只保留窗口与 token 数。理由：宿主会把短回答一次性投递，实测过「14 token / 4 ms」＝ 2800 t/s，那是批量投递的
    产物；`0` 的样本不进百分位（`appendSample` 丢 0），所以页面上的 `decode_p50_tps` 不会被这类记录拉高。
- `/health` 的 `channel_observation` 字段**恰好**是：`enabled` / `directory` / `events` / `failed_events` /
  `decode_failures` / `dropped` / `written` / `queued` / `write_failures` / `last_record_at` / `last_write_at` /
  `last_error` / `last_error_at` / `last_decode_error` / `last_decode_error_at` / `files` / `bytes` /
  `warmup{records,truncated}`。`events` = 接受的 usage 记录数（每个完成的请求一次，与协议无关）；
  `failed_events` = 其中宿主标为失败的条数；`decode_failures` = 载荷根本解不开的次数（异常信号，应为 0）。
- **失败口径（窗口级）**：`summary.failed_requests` 统计窗口内 `failed=true` 的记录数，并按渠道
  （`providers[].failed`）与小时（`hours[].failed`）拆开。它与 `/health.failed_events`（随插件进程重启归零）
  不是一回事：页面的「失败 N 条（x%）」取窗口口径。生产实测这些失败全部是上游 502。
- **上游推理渠道（官方口径，2026-10-02 新增）**：`/channel` 的 `official_channels[]` 由 Cline 官方
  per-request usage（`GET /api/v1/users/{id}/usages`）聚合，键为 `inference_provider`
  （`aiInferenceProviderName`）与 `model`（`metadata.raw_model`），带请求数、输入/输出/缓存 token 与成本；
  `official_usage` 复用采集器已跟踪的 `oldest/items/truncated/failures/error`。这一维度按**采集器保留窗口**
  统计（不随页面 1h/24h/7d 切换），且官方接口本身会限流（实测 `truncated=true`、`failures=1`、
  `error="upstream status 429"`）。官方只记**成功计费**请求，失败不在其中，因此失败无法归因到上游推理渠道。
  页面从 2026-10-03 起不再画这一维度；**2026-10-04 起 `plan_usage_enabled` 默认 `false`**，这条逐条拉取不再发生，
  所以默认配置下 `official_channels` 是 `[]`、`official_usage.enabled` 是 `false`。要重新取证就在插件配置块里写
  `plan_usage_enabled: true` 再重载；接口的键与结构没有变化。
- CSV：`GET /channel.csv?window=1h|24h|7d`，列序固定，含 `cpa_provider`、`gateway_provider`、
  `gateway_resolved_provider`、`gateway_slug`、`channel_source`、`auth_id`、`auth_index`、`auth_type`、
  `off_baseline` 等列（`final_provider` / `resolved_provider` 两个旧列名已不再出现在表头）；导出失败时把原因作为
  注释行追加进文件（仍返回 200），`X-Record-Count` 头报行数。

### 3.4 fail-open 与回归保护

- 插件返回的信封恒为 `{"ok":true,"result":{}}`：不改写、不阻塞、不参与请求处理；回调发生在请求结束之后。
- 观测层的任何错误都在插件内部消化（`HandleUsage` 永不返回 error）；观测关闭时能力为 nil，宿主根本不注册 adapter。
- P2 的 scanner 同样 fail-open：目录不存在、权限不足、解析失败、落盘失败，一律只记健康度，不删源文件、不影响请求。
- v0.2.x 的页面与接口在测试里仍然被断言（页面不得出现渠道以外的本地统计元素，见
  `internal/management/management_test.go`）。

### 3.5 CPA 请求日志 reader（P2，Phase 2 as-built）

插件构建 `0.3.0-dev.180`；scanner 只在 `channel_log_enabled: true` 时启动（关闭时不启协程、不访问目录）。

配置键：

| 键 | 默认值 | 说明 |
| --- | --- | --- |
| `channel_log_enabled` | `false` | 扫描开关。默认关闭是刻意的：源日志含客户端**明文 prompt**，且只有 CPA 侧同时开了请求日志才会写文件 |
| `channel_log_dir` | `/CLIProxyAPI/logs` | CPA 请求日志目录（容器内路径；宿主是 `/opt/cpa/logs`）。只扫描该目录**顶层**，不递归子目录 |
| `channel_log_delete_after_read` | `true` | fact 落盘后 unlink 源日志；解析失败或落盘失败的文件不删 |
| `channel_log_min_age_seconds` | `5` | 只读 mtime 早于该秒数的文件，避免读到 CPA 正在写的半个请求 |

扫描与解析：

- 每 **2 s** 列一次目录，只认顶层 `*.log`；**跳过 `main.log`**（CPA 自己的进程日志，不是请求日志）；
  子目录一律不进（插件自己的 JSONL 在 `logs/channel-observation/`，正好靠这条隔开）。
- mtime 在最近 5 s 内的文件跳过（`skipped_young`）；读前读后比对大小，变大的文件留到下一轮（`skipped_growing`）。
- 流式读取用 **1 MiB** `bufio.Reader`：实测一个 6,292,038 字节的日志解析期间分配 **1,088,296** 字节，
  单次最大读取 **1,048,576** 字节；`io.ReadAll` 等价物会分配 **14,972,200** 字节。
- 按 `=== NAME ===` 分段；`API RESPONSE n` 段里的 SSE 只取 `data:` 行、逐帧 JSON 解码，读
  `choices[].delta.provider_metadata`，非流式别名 `choices[].message.provider_metadata` 同样取；
  **绝不做文本匹配**——正文里出现 `provider_metadata` 字样必须产出空渠道，有测试守着这条。
- 有 `API ERROR RESPONSE` 段时记 `had_error_response: true`，但仍然取**最后一个成功的** `API RESPONSE` 段；
  尝试次数记在 `attempts_seen`。

fact 落盘：

- 追加到 `<channel_store_dir>/channel-log-<UTC 日期>.jsonl`，一行一条。前缀刻意与记录文件
  `channel-<YYYY-MM-DD>.jsonl` 不同，好让观测 store 的 reader **主动跳过**它们（`store.go` 里写明这是决定而不是巧合）。
- `channel_log_delete_after_read: true` 时，fact 落盘（并 close）之后才 unlink 源日志；落盘失败就保留文件并计数。
- fact 文件按 `channel_retention_days` 同一个时钟过期：文件名里的 UTC 日期早于保留窗口即删。

fact 键（每条都必定出现，日志没给出的字段留空而不是填默认值）：

`time, path, method, session_id, session_uuid, has_session, final_provider, resolved_provider, canonical_slug,
original_model_id, pinned_provider, affinity_outcome, model_attempt_count, total_provider_attempt_count,
fallbacks_available, gateway_cost, frames, attempts_seen, had_error_response, source_file, parsed_at`

健康度（`channel_log.health`，字段恰好是这些）：

`enabled, directory, scanned, parsed, with_channel, without_channel, parse_failures, deleted, deleted_bytes,
skipped_young, skipped_growing, skipped_seen, skip_main_log, pruned, pruned_bytes, last_fact_at, last_error,
last_error_at, pending_files`

实测一轮读 223 个文件：`parsed 223`、`parse_failures 0`、`with_channel 213`、`without_channel 10`。
「没有渠道」是解析器能给出的正常答案（失败或被重试覆盖的请求本来就没有 routing 块），不是错误。

试运行中修掉的缺陷（留档）：

- 没有「已读」集合时，每一趟轮询都会重新解析同一批文件（约 210 个文件产出 **2702** 条 fact），把所有比例成倍放大。
  修法：seen 集以 `name|size|mtime` 为键，命中即跳过（`skipped_seen`）；开启删除时顺带把这些已落盘的文件消费掉。

## 4. 性能数据

### 4.1 P1（usage 钩子）的成本模型 —— 量化实测待做

- 每次**完成的请求**一次 ABI 调用：插件侧一次 JSON 解码 + 一条记录构建 + 一次入队。没有逐帧工作、不读响应字节、
  不克隆请求体、不做探针。
- 调用点在请求结束之后，不在请求处理路径上，因此不影响任何请求的时延与吞吐。
- 观测关闭时不声明 `usage_plugin`，宿主不注册 usage 适配层，这次调用不存在（等价于没装插件）。
- **量化实测未做**：§4.4 的三态数字属于已退役的流式分片设计，不能当作现设计的性能证据。现有的
  `scripts/bench_channel.py` 测的是流式请求的帧间隔 / 解码窗口 / CPA CPU（旧口径），现设计需要一套按**请求**计的
  基准才能量出「每请求一次回调」的成本。

### 4.2 CPA 请求日志的体积（V3，实测）

- 单个请求日志：短请求 **16–47 KB**；**长上下文请求 4.0–5.7 MB**（请求体约 2.5 MB + 上游 SSE 约 3 MB）。
- 实测 10 个文件 = **31.2 MB**，在约 2.5 分钟内产生；真实流量下目录 5 分钟长了约 **40 MB**（约 8 MB/分钟）。
- 按今天的请求构成（**743** 条 input > 100k token）估算：日增量约 **3.5 GB/天**，突发时 **≥10 GB/天**。
  试运行期间生产把 `logs-max-total-size-mb` 设为 **2048**。
- **目录上限与 `main.log` 共享**：CPA 的清理器按 `logs-max-total-size-mb` 清理 `*.log`。这个键的
  **默认值是 0＝不清理**（`internal/config/config_load.go` 里 `cfg.LogsMaxTotalSizeMB = 0`，`config.example.yaml`
  也写明 0 表示 disable——容易跟隔壁 `error-logs-max-files` 的 10 混起来）。上面「几秒内被删掉」发生在
  试运行前这台主机把它设成 **10** 的时候：上限装不下一个请求的日志，文件就会在插件扫到之前被清理器删掉
  （插件只读 mtime 早于 `channel_log_min_age_seconds`＝5 秒的文件）。插件自己 `channel_log_delete_after_read`
  默认 true，fact 落盘即 unlink 源日志，所以常态下目录不会涨；留下的只有解析失败或落盘失败的文件，
  因此仍然建议按 3.5 GB/天 的写入速率给一个够大的兜底上限（试运行用的 2048）。

### 4.3 打开请求日志的代价对照（V4，实测 2026-10-02）

方法：三个阶段各取每分钟一个样本；指标是插件自己的 1 小时窗口聚合加 `docker stats`。

| 阶段 | 时间（UTC） | 样本 | 请求（中位） | 失败 | TTFT p50（中位） | 解码 p50（中位） | 容器 CPU 中位（峰值） | 内存中位（峰值） |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| ① 切换前 | 14:10–14:17 | 7 | 423 | 314（74.2%） | 590.5 ms | 143.0 t/s | 0.01%（5.79%） | 158.4 MiB（179.5 MiB） |
| ② 打开日志 | 14:39–15:08 | 30 | 778 | 494（62.6%） | 1453.5 ms | 177.9 t/s | 0.38%（144.83%） | 154.9 MiB（202.0 MiB） |
| ③ 再次关闭（干净回滚） | 15:12–15:42 | 24（去重后） | 869 | 536（61.7%） | 1690.0 ms | 180.5 t/s | 0.03%（48.67%） | 140.3 MiB（183.1 MiB） |

**怎么读这张表（诚实版）**：① 是另一个流量档位（① 到 ③ 之间流量大约翻了倍），所以有意义的对照是 **② vs ③**：

- 解码 p50 在**打开日志时低 1.4%**（177.9 vs 180.5 t/s）；
- TTFT p50 在打开日志时**更低**（1453.5 vs 1690.0 ms）——在这个分辨率下看不到延迟惩罚；
- 容器 CPU 中位高 **0.35 个百分点**（0.38% vs 0.03%），但峰值高得多（144.83% vs 48.67%）；
- 内存中位高 **14.6 MiB**（154.9 vs 140.3 MiB），峰值 202.0 vs 183.1 MiB；
- 失败率（约 62%）由上游限流驱动，两个阶段一致，**不能归因于日志**（§5.2 的 429 原文与 §3.3 的失败口径）。

**必须一起读的限制**：① 只有 7 个样本（开关在这条序列跑完之前就生效了）；③ 有 24 个有效样本（按分钟去重后；最初误起两个采样进程，同一分钟出现两行）；关闭相位最初还跑着
重复的采样进程（同一分钟的重复行在统计时按每分钟一行去重）；插件的窗口聚合成型慢。所以这张表回答的是
「打开日志大概要付多少代价」这个**数量级**问题，不是精确基准；要精确数字需要 §6「仍未做」里的全长度基准。

### 4.4 旧设计的三态实测（历史，保留）

脚本：`scripts/bench_channel.py`（oracle 上 `sudo python3 /tmp/bench_tristate.py --label NAME --ctx-chars N --max-tokens M --runs R`），
测的是 CPA → `deepseek-flash-1` 的流式链路：usage token 数、SSE 帧分类计数、TTFT、解码窗口吞吐、
相邻文本帧间隔 p50/p90、以及 `cpa` 容器的 CPU（运行中 median/mean/max + 运行前 5 秒空闲 median）。

**§4.4.1 基线（无插件）**

| 场景 | ctx chars | prompt tokens | 总帧 | 文本帧 | 静默帧 | TTFT | 解码窗口 | 吞吐 | 帧间隔 p50 | CPU（空闲 → 运行 median） |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 短 | 4,000 | 800 | 203 | 200 | 2 | 1.21 s | 1.135 s | 176.15 t/s | 0.192 ms | 14.52% → 21.49% |
| 长 | 227,000 | 42,612 | 1,602 | 1,599 | 2 | 1.634 s | 9.358 s | 170.98 t/s | 0.054 ms | 6.15% → 7.92% |

读数要点：

- **口径用解码窗口吞吐（≈171 t/s，800 与 42,612 prompt tokens 两次一致），不要用 1/p50**：
  帧是突发到达的，p50 间隔测到的是同一 TCP 批次内的投递（0.054–0.192 ms），不是生成速度。
- 每次恒定 2 个静默帧（`role_only` 1 + `empty_delta` 1），没有匿名空帧、没有 malformed 帧。
- CPA 自身 CPU 不是瓶颈：1600 token 的流只比空闲高约 2 个百分点。容器是共用的，空闲基线在
  4.78%–14.52% 之间漂移，所以看 CPU 要**减空闲中位数**，不看绝对值。
- 输入侧比例约 0.19 token/char（4,000 chars → 800 tokens；227,000 → 42,612，线性无截断）。

**§4.4.2 观测开启 vs 关闭（旧设计复测）**

**方法**：先做过一轮顺序相位（先关后开，各 1 轮），short 段给出 −6.9% 的吞吐差；但同机负载漂移足以解释它
（空闲 CPU 中位数在同一台机上从 3.7% 漂到 46%；CPU 配对差散布在 −61…+34.6 个百分点之间）。
顺序相位会把「机器变忙/变闲」记到被测变量头上，因此改成**交替 A/B**：driver `/tmp/bench_ab.sh`（oracle），
每个 iteration 先跑 `off` 再跑 `on`，共 5 轮；每臂 `--ctx-chars 227000 --max-tokens 1600 --runs 1`；
日志 oracle `/tmp/bench-ab.log`（本机同一路径留档）。第 5 轮的 `on` 臂撞上上游 Vercel 429
（CPA 返回 502 `stream_initialization_failed`），与本插件无关（该请求没有 routing，本来也只会算 `unresolved`），
故配对样本 n=4。

| 指标 | 关闭观测（off） | 开启观测（on） | 配对差（on−off） |
| --- | --- | --- | --- |
| 吞吐 t/s | 中位 **165.15**（5 run：161.30 / 162.35 / 165.15 / 166.50 / 167.23） | 中位 **161.69**（4 run：157.64 / 160.13 / 163.24 / 164.74） | 均值 −1.56%，中位 **−1.88%**，区间 −3.04%…+0.55% |
| TTFT s | — | — | 均值 −0.04%，中位 **+1.98%**，区间 −7.68%…+3.58% |
| wall s | — | — | 中位 +1.31% |

**当时的判定：通过（当时可留在生产开启）。** 判定线是吞吐 ≥ 基线 95%、TTFT ≤ 基线 110%：
观测开启相对关闭的吞吐中位差 −1.88%（中位比值 0.979），TTFT 中位差 +1.98%，都落在同状态内 run 间离散度之内。
该判定随流式分片设计一起退役；P1 的成本模型见 §4.1，P2 的代价见 §4.3。

读这份表时必须一起看三件事：

1. **同一状态内 run 间离散度就有 ±12%**（off 相位长上下文单 run 曾见 147.22 t/s，而中位是 165.15），
   所以只能按「同样 runs 数比中位数 + 配对差值」判读，不能拿单次结果下结论。
2. **本机 CPU 增量不可判定**：容器是共用的，配对差从 −61 到 +34.6 个百分点，只能说「测不出来」，
   不能拿它当结论。要拿 CPU 数字，必须换独占环境重测。
3. 每次 PATCH 重载都会让 `written` 等计数器归零，`warmup` 则从磁盘回填，所以「计数器归零」不是数据丢失。

### 4.5 真实流量取证：为什么流式分片路径必须退役

旧设计在 A/B 期间暴露了 `/health` 的 `parse_failures: 4`、`pending_streams: 18`、
`last_parse_error: provider_metadata present but no routing block`。追下去发现这不是上游的错，而是两类真实流量：

1. **客户端可见的 Responses 流永远没有渠道帧**：`POST /v1/responses` 是真实的 clinepass 流量（日志里
   `session=codex:se… model=deepseek-flash-2`，每分钟若干条）。对它的流式响应 dump 了 **34 帧，含
   `provider_metadata` 的帧数为 0**，只有 `response.completed` 带 usage。
   **因果更正**：这不是「上游没给渠道块」，而是 CPA 把上游 chat 帧翻译成 Responses 事件时丢掉了它
   （上游 `api.cline.bot` 连 `/responses` 端点都没有，直连 404）；拦截器挂在翻译之后，所以看不到。
2. **needle 假阳性**：`provider_metadata` 这个词也会出现在模型自己的输出文本里；这种帧解不出 routing，
   若按「解析失败 → 重试 → 超预算放弃」处理，会让该流真正的路由帧被永久跳过，**记录直接丢失**。

当时给这两类流量打的补丁是「假阳性只计 `needle_misses`」「终止帧当场 `abandon` 计 `unresolved`」；
它们连同整个分片探针一起被移除。

把第 1 条放到生产规模上看，结论就变了：生产主机 24 小时内 **5,843 条**请求中约 **99%** 是 `POST /v1/responses`。
旧设计在这 99% 的流量上一条都记不到——它记到的只有验收探针自己发的 chat 请求。这就是改用 usage 钩子的直接原因
（改用后的覆盖验证见 §6），也是为什么网关渠道只能走 P2：唯一还留着上游响应原文的位置就是 CPA 的请求日志。

## 5. 部署与回滚

- 目标：`/opt/cpa/plugins/linux/arm64/clinepass-channel-monitor.so`（`abi_version=1`、`schema_version=6`）。
  替换 `.so` 不需要重启容器；只写本插件的 `.so`。
- 前置：备份 `/opt/cpa/config.yaml` 并记录 sha256；配置只做**增量**改动（P1 新增 5 个键、P2 新增 4 个键），
  改动后给出 config diff。
- 启用/重载：换 `.so` 后 `PATCH /v0/management/plugins/clinepass-channel-monitor/enabled {"enabled":false}` 再 `{"enabled":true}`；
  配置改动走 `PATCH …/config`（带完整 config 对象）；开关落在 `config.yaml` 的 `plugins.configs.clinepass-channel-monitor.enabled`。
- **正确顺序（2026-10-02 实测通过）**：装新 `.so`（旧版本继续运行）→ `PATCH enabled` 触发热重载 → 回读
  `GET /v0/management/plugins` 确认 `registered=True enabled=True effective=True` →
  在 `main.log` 里看到 `pluginhost: plugin hot reloaded … active_version=0.3.0-dev.173 retired_version=0.3.0-dev.171 retired_path=…`
  → **确认退休之后**才删旧文件。本次实测就是这样走的，旧文件在重载成功后才被删除。
- **别先删掉正在运行的 `.so`**（实测教训，比上一条更硬）：先 `rm` 活动版本、再装新版本并 `PATCH enabled`，
  宿主会停在 `registered:false / enabled:true / effective_enabled:false`，`/health`、`/channel`、资源页一起 404，
  `main.log` 里连 `pluginhost:` 行都没有；连续 7 次 `enabled:true` 都无效，**只有 `sudo docker restart cpa` 才恢复**
  （重启后不需要再 PATCH，`plugins.configs.<id>.enabled: true` 会在启动扫描时加载）。
  注意进程启动时那次加载**不写** `pluginhost:` 行，判据一律以回读列表为准。
- **`{"status":"ok"}` 不等于加载成功**（实测教训）：连续 `false`→`true` 的两次 PATCH 在配置重载竞态下，
  第二次会被内存态回写覆盖，插件留在 `registered:false / enabled:false`，`/health` 与 `/channel` 直接 404
  （`main.log` 里只有 200 的 PATCH 记录、**没有** `pluginhost: plugin loaded` 行）。
  因此每次换 `.so` 后必须回读 `GET /v0/management/plugins`（返回 `{"plugins_enabled":…,"plugins_dir":…,"plugins":[…]}`，
  要读 `plugins[].registered` 与 `effective_enabled`），为 false 就重发 `enabled:true`。
- **比对「部署的 .so 就是当前源码构建的」**：两边跑同一条命令比哈希，`sort` 必须钉 locale，
  否则 BSD `sort`（macOS）与 glibc `sort`（服务器）排序不同会得到假不一致：
  `find internal cmd -type f \( -name '*.go' -o -name '*.html' \) | LC_ALL=C sort | xargs sha256sum | sha256sum`。
  整树比对还要排掉本地新改、未同步的文件（如 `docs/`、`README.md`）。
- **P2 的前置动作（CPA 侧，必须重启容器）**：在 `/opt/cpa/config.yaml` 里设
  `observability.logs.request-log: true`、`server.commercial-mode: false`，并把 `logs-max-total-size-mb`
  提到试运行所需（本次 2048；这台主机原来是 10，小到装不下一个请求的日志，会让文件在插件扫到之前
  就被清理器删掉）。改完 **`docker restart cpa`**：
  实测 reload 成功但零文件，重启后一分钟内出现两个文件（§2.2）。插件侧再设 `channel_log_enabled: true`。
- 验收口径（本轮实际做到的与未做到的对照见 §6）：面板能回答窗口内的请求数与偏离比例；抽查原始记录与宿主 usage
  数据库对照；CSV 含渠道字段；关掉观测（`channel_observe_enabled=false`）后请求转发照旧；磁盘/写入失败时转发照旧；
  P2 侧：源日志被解析成 fact、fact 落盘后源日志被 unlink、恢复上限后 CPA 把请求日志删掉。
- 回滚：恢复原 `.so` + 把 `channel_observe_enabled` 置 false（或删除新增配置键）→ 重载 → 确认
  `registered: true` 且 `/health` 的 `channel_observation.enabled` 为 false，页面显示「渠道观测未开启」。
  历史 JSONL 不需要动：reader 兼容 `v: 1` 与 `v: 2` 两种行。
  **P2 的回滚分两段**：插件侧 `channel_log_enabled: false`（scanner 停）；CPA 侧把
  `request-log: false`、`logs-max-total-size-mb: 10`（这台主机改动前的值；CPA 默认是 0＝不清理）、
  `commercial-mode: true` 写回配置并**重启容器**。
  只做插件侧不会删掉已经写在盘上的请求日志，只要 CPA 侧还开着，日志就会继续落盘。

### 5.1 2026-10-02 第二轮部署的补充教训

- **同步包必须是本次改动之后的**：复用上一轮留下的 `ccm-sync.tgz` 会构建出不含新字段的 `.so`（回读 `/channel`
  缺 `failed_requests`，白跑一轮）。判据是编译输入哈希（`LC_ALL=C sort` 后再 `sha256sum`）与本地一致；
  哈希没变就是没同步。
- **回读要看 `plugins[].path` 与 `/health.version` 两个值**：列表里的 `path` 读的是磁盘文件，可能已是新版，
  而已加载实例仍是旧版（实测 `path` 指 `0.3.0-dev.177`、`/health` 仍回 `0.3.0-dev.176`，`main.log` 没有对应的
  `plugin hot reloaded` 行）。两者版本号不一致时用 `sudo docker restart cpa` 兜底（本次即如此恢复）。

### 5.2 2026-10-02 请求日志试运行（P2）的事故与教训

- **回滚脚本的 payload 写错，「回滚演练」静默地什么都没做。** 第一版脚本用
  `{"request-log":false}`，而管理接口要求 `{"value":false}`（`updateBoolField` 绑定的是 `{"value": …}`）。
  于是演练看着像成功、实际开关没关；如果按原计划让 90 分钟看门狗去关，结果会是**请求日志留在开启状态、
  明文 prompt 继续落盘**。它是在回读实时配置时被抓到的。
- **修正后的回滚脚本已跑过并核对**：正确 payload + 对 `commercial-mode` 做**不依赖缩进**的改写
  （管理接口 PATCH 会重新序列化 `config.yaml`，把 4 空格缩进规范化成 2 空格，纯文本改法会失效）→
  回读确认 `request-log: false`、`logs-max-total-size-mb: 10`、`commercial-mode: true`、45 s 内没有新日志文件，
  并且被恢复的上限把约 **1 GB** 请求日志清理掉。
- **管理 PATCH 会重写整份 `config.yaml`，而且不落所有键**：`request-log` / `logs-max-total-size-mb`
  必须同时写进文件，否则之后一次重启会把它们悄悄丢掉。比对该用**语义比较**（`yaml.safe_load` + 只比对改动的键），
  不要文本 diff——语义比较还顺带避免把凭据段落打印出来。
- **热重载在这个环境里反复换不上新构建**：插件列表显示的是新文件，而 `/health` 仍报旧版本，连续三次如此；
  每次可靠的路径都是 `docker restart cpa`（与 §5.1 第二条同源）。
- **当天的 502 风暴**：上游限流，原文
  `failed to generate stream from Vercel: … status 429 … Rate limit exceeded for deepseek/deepseek-v4.1-flash: this team's limit of 100000000 input tokens per minute (per region) was reached`，
  CPA 把它以 502 返回给客户端。官方 usage 记录只含**成功计费**请求，所以失败无法归因到某个上游渠道；
  §4.3 里两个阶段的约 62% 失败率就是它，与是否打开请求日志无关。

## 6. 验收清单（2026-10-02 复验）

宿主：生产主机；插件构建 `0.3.0-dev.180`（P2）/ `0.3.0-dev.173`（P1 那轮）；
生产宿主 CPA **v8.0.8**（commit `fd48ea6`，build 2026-10-01）——旧清单里「生产宿主的 CPA 版本号未记录」这一条就此关闭。

（旧设计 2026-09 的那轮 Step 2 验收结论随分片探针一起退役，其性能数字保留在 §4.4；下面是 P1 与 P2 各一轮的复验。）

**P1（usage 钩子，构建 `0.3.0-dev.173`）**

- [x] 部署与热重载：新 `.so` 装上时旧版本继续运行，宿主自行退休旧版本
      （`pluginhost: plugin hot reloaded … active_version=0.3.0-dev.173 retired_version=0.3.0-dev.171`），
      旧文件在确认退休后才删；插件列表回读为 `clinepass-channel-monitor registered=True enabled=True effective=True`。
      反例（先删正在运行的 `.so`）会停在 `registered:false` 且只有 `sudo docker restart cpa` 能恢复，已写入 §5。
- [x] **`/v1/responses` 覆盖（旧设计的能力盲区）**：探针前 1 小时窗口 2 条记录，探针后 6 条——一条
      `POST /v1/responses` 请求确实产生了记录，字段如 `final_provider=openai-compatible-cline2`、
      `auth_id=openai-compatibility:cline2:bcaef0dbf8d3`、`auth_index=7dff1d7b60577eec`、`auth_type=apikey`、
      `model=cline-pass/deepseek-v4.1-flash`、`upstream_model=deepseek/deepseek-v4.1-flash`、`ttft_ms=1448`、
      `duration_ms=3129`、`tokens_per_second=120.76`、`input_tokens=158431`、`output_tokens=203`、
      `cache_read_tokens=158208`、`session_id=codex:179f84c9-…`；同窗口另一条 `session_id=lcp:v1:77c8c3ca…` 为
      `input_tokens=37 output_tokens=13 ttft_ms=707 duration_ms=1288`。
      注意这里的 `final_provider` 装的是 **CPA 凭据名**，不是网关渠道（§2.2、§3.2）。
- [x] 窗口内的渠道分布：`openai-compatible-cline2` 3 条、`openai-compatible-cline1` 2 条、
      `openai-compatible-cline3` 1 条（与上面 6 条一致）。
- [x] CSV 导出：`X-Record-Count: 6`，表头含 `time_utc … final_provider,resolved_provider,pinned_provider,
      auth_id,auth_index,auth_type,executor_type,reasoning_effort,service_tier,upstream_request_id,off_baseline,
      status_code,failed,protocol,ttft_ms,duration_ms,decode_ms,tokens_per_second,input_tokens,output_tokens,
      reasoning_tokens,cached_tokens,cache_read_tokens,cache_creation_tokens,total_tokens`，
      `off_baseline` 取值为 `yes` 或空。
- [x] `/health` 的 `channel_observation` 字段可用：字段集合与 §3.3 列出的**恰好一致**（`events` / `failed_events` /
      `decode_failures` 取代了旧设计的 `resolved` / `unresolved` / `parse_failures` / `needle_misses` /
      `pending_streams` / `last_parse_error*`）。
- [x] 环境对照数：宿主自己的 usage 数据库在同一小时内共 **607** 条（`openai-compatible-cline1` 229、
      `openai-compatible-cline3` 205、`openai-compatible-cline2` 173）。
- [x] 「偏离基准渠道」是读时判定：窗口内 6 条记录全部 `off_baseline=yes`，因为基准仍是 `deepseek`、渠道名是
      `openai-compatible-cline*`（口径冲突本身**未决**，见 §3.3）。

**P2（CPA 请求日志，构建 `0.3.0-dev.180`）**

- [x] **V1 日志真的出来了（含更正）**：两个开关同时满足才写文件；管理接口 reload 成功（`config successfully
      reloaded, triggering client reload`）但**零个** request-log 文件，`docker restart cpa` 后一分钟内出现两个——
      「reload 就够」是错的，已写入 §2.2 / §5。
- [x] **V2 join key 存在**：日志 `Session_id: session-<uuid>` ↔ 记录 `session_id: codex:session-<uuid>`，
      规则是 uuid 命中 **且** `|log.Timestamp − record.time| ≤ 2s`；实测 **12 条里对上 10 条**，偏移 **72–218 ms**。
      未覆盖：不发 `Session_id` 头的客户端（见「仍未做」）。
- [x] **V3 体积与增量**：单文件短 16–47 KB、长上下文 4.0–5.7 MB；10 个文件 31.2 MB / 约 2.5 分钟；
      约 8 MB/分钟；日增量约 3.5 GB（按 743 条 >100k input token 的请求），突发 ≥10 GB。数字见 §4.2。
- [x] **V4 代价对照**：三阶段分钟采样，② vs ③ 解码 p50 −1.4%、TTFT p50 无可见惩罚、CPU 中位 +0.35 个百分点、
      内存中位 +16 MiB；失败率约 62% 由上游限流驱动、两阶段一致，不归因于日志。数字与限制见 §4.3。
- [x] **结构化解析、不误判**：`provider_metadata` 只按 JSON 帧解码取值，正文里出现该字样的请求产出空渠道，
      有测试守着（`internal/channellog/parse_test.go`）。
- [x] **归因正确性（失败请求渠道留空）**：实测一轮读 223 个文件得 `with_channel 213 / without_channel 10`、
      `parse_failures 0`；没有 routing 块就产出空渠道，而不是猜一个渠道名。
- [x] **磁盘不随时长增长**：`channel_log_delete_after_read` 默认 true，fact 落盘后 unlink 源日志；
      恢复 `logs-max-total-size-mb: 10` 后 CPA 把约 1 GB 请求日志清理掉。目录 8 MB/分钟的增速是清理之前的观测值。
- [x] **回滚演练（先失败、修正后通过）**：第一版脚本 payload 错、静默无效（§5.2）；修正后回读
      `request-log: false`、`logs-max-total-size-mb: 10`、`commercial-mode: true`，45 s 内无新日志文件。
- [x] **文档更正**：§2.2 / §2.3 已按 `docs/request-log-channel-plan.md` §8 改写（「死路」与「上游没有渠道块」
      两处），README 同步。

**仍未做（本轮明确未完成）**

- [x] **不带 `Session_id` 的客户端（记录形如 `lcp:v1:<hex>`）的 token/时间兜底 join**：2026-10-03 已实现并上线（`0.3.0-dev.183`）。实测动因：生产上当时的流量**全部**没有 `Session_id` 头（当天 187 条 fact 里 118 条 `has_session=false`），1 小时窗口只能判定 8/134 条；回退上线后同一窗口 72/221。规则：fact 带 `prompt_tokens`/`completion_tokens`，记录带 token 时按「两计数器精确相等 + 候选恰好 1 条」匹配，两条都匹配不上就留空。窗口初版 2 秒，**2026-10-05 放宽到 5 秒**：客户端整段重发上下文时请求体 90 MB 量级，usage 的 `RequestedAt` 比日志到达戳晚 **1.99–2.38 s**（生产 A/B 面 11 条 Cline3 记录，真实渠道全部是 `deepseek`），2 秒窗口正好在边界上把它们丢成「无渠道块」。**限制**：磁盘上早于该构建的 fact 行没有这两个键，回退只对新解析的 fact 生效（随 3 天留存滚动）。
- [x] **合并与页面（Phase 3 / Phase 4）**：构建 `0.3.0-dev.182` 已完成——schema 升 v3，凭据改名 `cpa_provider`，
      网关值占用 `final_provider`/`resolved_provider`（另有 `gateway_slug`/`gateway_attempts`/`gateway_cost`/`channel_source`），
      新增 `summary.unresolved_requests` 与 `summary.cpa_providers[]`，CSV 同步；页面的「真实渠道」表按网关维度、原始记录表
      增加「真实渠道 / 尝试 / CPA 凭据」三列，标题与卡片写明分母是「能判定渠道」的条数。生产实测 24h 窗口：
      `resolved 194 / unresolved 2350`（日志只在 23:00–23:11 开着，所以覆盖率就是这段），网关渠道全部 `deepseek`、
      偏离 0；凭据维度 `cline2 950（失败 420）/ cline3 798（345）/ cline1 772（418）`。
      **join 的关键修正**：最初实现只读扫描器内存里的 fact 环，重启后为空、且只留最新 200 条，24h 窗口判定不出来；
      改为读持久化的 `channel-log-<UTC 日期>.jsonl`（5 秒缓存 + 与内存环按 `source_file|time` 去重）后才对上 194 条。
- [ ] **全长度性能基准**：§4.3 是三阶段分钟采样的数量级结论，不是基准（① 只有 7 个样本、③ 只有 24 个有效样本、
      关闭相位最初跑着重复采样进程、窗口聚合成型慢）。
- [ ] **是否长期开启请求日志**：未决定。它含明文 prompt，日增量约 3.5 GB，且必须重启容器才能生效。

**其余仍未做（本次材料没有触及，保持原状）**

- [ ] **基准语义决定**：`channel_baseline_provider` 该取什么值（渠道名？`upstream_model` 口径？），未决定，
      现象与影响见 §3.3。
- [ ] **pin 验证**：plan §9 要求给出出站 body 里 `providerOptions.gateway.only` 的实际值，本次材料没有这个值。
- [ ] **存储里仍有旧设计的 `v: 1` 行**（探针 / 流式时代写下），reader 仍读、页面按空值显示；是否清理未决定。
- [ ] **插件侧 6 条 vs 宿主 usage DB 607 条的数量级差未解释**：采集窗口起点、观测开启时刻、记录落盘时序等原因
      本轮没有取证。
- [ ] **TTFT / `Latency` 的起点未取证**：不知是否含「客户端 → CPA 排队 → 连上游」那段，因此新旧口径的 TTFT
      不可直接比较。
- [ ] **fail-open 演练与原始记录逐字段抽查未在新设计下重做**：旧设计做过的两类演练（把 `channel_store_dir`
      指向普通文件、逐字段对照原始响应帧）本轮没有重跑。
- [ ] `registry.json` 版本号与 release notes（若发版）——是否发版待确认。

### 6.1 第二轮（2026-10-02 晚）：上游渠道维度 + 失败口径

- [x] 构建 `0.3.0-dev.177` 上线，`/health.version=0.3.0-dev.177`、`channel_observation` 字段集不变；
      上线过程踩到两条已记入 §5.1 的坑（陈旧同步包、热重载未生效需重启）。
- [x] 失败口径：24h 窗口 `resolved=959`、`off_baseline=935`、`failed_requests=428`（44.6%），
      按渠道 `openai-compatible-cline2` 441/187、`cline3` 338/148、`cline1` 156/93，旧 `v: 1` 行聚合出的
      `deepseek` 24 条 0 失败；按小时 17:00 294/166、18:00 641/262。全部失败状态码为 502。
- [x] 上游渠道维度：`official_channels=[{inference_provider:"vercel", model:"deepseek/deepseek-v4.1-flash",
      requests:800, input_tokens:103293902, output_tokens:701740, cached_tokens:99866752,
      cost_usd:123.4713}]`；`/health.plan.official_channels` 同值。「官方用量明细」表新增「上游渠道」列。
- [x] 视觉验收：1920×1080 真实渲染（用真实 `/channel`、`/health` 载荷喂本地 harness）——
      「渠道分布」表含失败列（191/148/93/0）、小时表含失败列、「上游推理渠道（官方 usage）」表与 caption 正常，
      「请求数」卡片显示「失败 432 条（44.58%）」。
- [x] 上游渠道唯一性取证：官方接口连续 12 页共 **2,400** 条（07:15→10:40 UTC）全部
      `aiInferenceProviderName="vercel"`、`raw_model="deepseek/deepseek-v4.1-flash"`，没有第二个渠道。

**6.1 仍未做**：

- [ ] 官方 usage 接口限流导致覆盖不足（`truncated=true`、`upstream status 429`）：长期覆盖率、是否需要改分页节奏
      或缓存策略，未取证。
- [ ] 失败的上游归属：官方只记成功请求、`Failure.Body` 不含上游名，上游错误原文只能在 CPA `main.log` 里；
      是否做日志侧归因（解析 `main.log`）未决定。
- [ ] 「基准渠道」语义仍未决定：现在 24h 窗口 935/959 全部算偏离，因为基准 `deepseek` 与渠道名
      `openai-compatible-cline*` 不可能相等。
- [ ] 现设计性能实测、`v: 1` 旧行清理、TTFT 起点取证、fail-open 演练——见上面两张清单。
