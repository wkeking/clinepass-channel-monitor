# 渠道观测（逐请求渠道分布）—— Step 0 判定与实现说明

本文回答一个问题，并记录它的取证过程：**过去 24 小时里，clinepass 的请求有多少条 / 多大比例没有落在基准渠道**，
以及这些请求的 TTFT 与解码速度是多少。

数据来源是**宿主的 usage 钩子**（能力键 `usage_plugin`，ABI 方法 `usage.handle`）：宿主在**每个请求结束后**调用一次，
载荷里带 CPA 为这条请求选中的凭据。它与客户端说什么协议无关，所以 `POST /v1/responses`（生产上约 99% 的流量）也在覆盖范围内。

本文的判定先于实现：`§2` 的三路径结论是在写第一行实现代码之前、用可复现命令在 oracle 上取证得出的（命令与原始输出见各小节）。
`§2.3` 记录的流式分片路径（`response_stream_interceptor`）**曾选定、后被退役**：它只能看到 OpenAI
`/v1/chat/completions` 响应里的 `provider_metadata.gateway.routing`，在 `/v1/responses` 的流上实测 34 帧里 0 帧命中，
而生产主机 24 小时 5,843 条请求里约 **99%** 是 `POST /v1/responses`——即旧设计只能记到验收探针，记不到真实流量。
退役理由与数字见 §2.3、§4.3。
`§4` 的性能数字分两批：`§4.1` 是现设计（usage 钩子）的成本模型（量化实测**待做**），`§4.2` 是为**退役设计**测的三态数字
（保留，但必须当历史读）。

宿主侧源码引用（`sdk/...`、`internal/pluginhost/...` 的行号）来自本仓库 `.reference/CLIProxyAPI` 的 **v8.0.8** 检出；
本插件自身依赖的是 `CLIProxyAPI/v7 v7.3.8`（`go.mod`）。

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
   `StreamChunkInterceptor` 全都不声明）。
2. 插件**永不阻塞**请求：任何失败都必须 fail-open（宿主捕获 panic 并熔断插件，见 §3.4）。
3. v0.2.x 的能力不得回退：套餐/额度/官方用量、凭据自动发现、`/health` 自诊断都必须照旧。
4. 不改 CPA 源码、不改写请求或响应、不干预路由与固定（pinning）。
5. **接受信号降级**（本轮确立的边界）：usage 载荷里**没有** Cline 网关级的 `provider_metadata.gateway.routing.finalProvider`。
   渠道改用 CPA 侧凭据（`Provider` / `AuthID` / `AuthIndex` / `AuthType`）与上游模型 slug（`upstream_model`）表达。
   网关内部是否发生回退（例如这条请求落到了 `alibaba` / `particle` 而不是 `deepseek`）现在**看不到**；
   这是已知代价，不是待修的 bug。

## 2. Step 0：三条路径的判定

渠道信息在 CPA 内部有两处：上游响应体里的网关路由块，与宿主自己的 usage 记录。判定如下：

| 路径 | 能否拿到渠道 | 覆盖哪些流量 | 结论 |
| --- | --- | --- | --- |
| P1 usage 钩子（`UsagePlugin` / `usage.handle`） | 能：CPA 为这条请求**选中的凭据**（`Provider` + `AuthID` / `AuthIndex` / `AuthType`） | **所有客户端协议**，每请求一次 | **选定** |
| P2 解析 CPA 自身 request-log | 能（原始报文里有网关路由块） | 全量，但必须先开 request-log | 本部署下是死路：要关 `commercial-mode`，代价与约束 1 矛盾 |
| P3 响应侧拦截器（`response_stream_interceptor`） | 能：网关级 `finalProvider` | 只有带 `provider_metadata` 帧的响应；实测只在 `/v1/chat/completions` 上出现 | **曾选定，已退役** |

### 2.1 P1：usage 钩子 —— 现在的渠道来源（选定）

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
- **它拿不到什么**：网关级 `finalProvider`（回退可见性，见 §1 硬约束 5）、成本、`cost_usd`、原始响应帧相关字段
  （`frames` / `protocol` / `pinned_provider` / `affinity` 等）。
- **它的代价**：宿主每完成一个请求调用一次插件（一次 JSON 解码 + 一条记录构建）。没有逐帧工作、不读响应字节、
  不克隆请求体；关闭观测时不声明该能力，这次调用不存在。

### 2.2 P2：解析 CPA 自己的 request-log —— 本部署下是死路

取证过程（结论不随 P1/P3 改变，保留原文）：

1. 打开 request-log：`PUT /v0/management/request-log`，body `{"value":true}`
   （处理函数 `internal/api/handlers/management/handler.go:426-436 updateBoolField` 只读 `Value *bool`）；
   reload 日志确认 `request-log: false -> true`。
2. 打一条真实流式请求（37 帧，含 `provider_metadata`）。
3. 3 分钟内在 `/opt/cpa/logs` 里**除 `main.log` 外没有出现任何新文件**。

根因（源码）：

- `internal/api/server.go:156 if !cfg.CommercialMode {` —— 非商业模式下才安装 `RequestLoggingMiddleware`；
  当前部署 `server.commercial-mode: True`。
- `internal/api/server_options.go:51-52 if cfg.CommercialMode { sdkCfg.RequestLog = false }` —— 商业模式下强制关闭。

也就是说 **P2 要生效必须先关 `commercial-mode`**，那会同时打开一套高开销中间件，与约束 1 的目标相反；
而且「寄生地 tail 别人的请求日志」把解析逻辑绑在 CPA 的日志实现上（字段、轮转、清理策略一变就烂），
可靠性风险高于收益。已回滚（`PUT` `false`）。

参考事实（若将来 P2 复活）：`internal/config/sdk_config.go:51 RequestLog bool "yaml:\"request-log\""`；
v8 的嵌套键是 `observability.logs.request-log`（映射表 `internal/config/config_v8.go:59`，示例
`config.example.yaml:1079`）；文件出现在 `/CLIProxyAPI/logs/`，命名 `<unix>-<requestid>.log`，
内容保留原始请求/响应（含 `provider_metadata`）；日志清理是 `find … -mtime +1 -delete`。

### 2.3 P3：响应侧拦截器 —— 曾选定，已退役（保留取证）

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
  `Provider` 与 `Detail`）。

当时还证明过「只记上游模型 slug 等于没有渠道」：历史数据里 16,396 条 `deepseek-v4.1-flash` 记录中有 4 条
`finalProvider` 是 `alibaba`(3) / `particle`(1)，而 `canonicalSlug` 仍然是 `deepseek/deepseek-v4.1-flash`。
这条证据今天同样成立：现设计里的 `upstream_model` 是**上游模型名**，它替代不了 `finalProvider`，
回退到哪个渠道仍然不可见（§1 约束 5）。

**退役理由（实测）。**

1. 渠道块只出现在 OpenAI `/v1/chat/completions` 的响应帧里。历史上 70 帧的流里只有 1 帧带 `provider_metadata`
   （`sudo python3 /tmp/channel_probe.py stream 120 deepseek-flash-1`）。
2. `POST /v1/responses` 的流式响应**一帧都没有** `provider_metadata`：对它的流 dump 了 **34 帧，含渠道帧为 0**，
   只有 `response.completed` 带 `usage`。
3. 生产主机的流量构成决定性地否掉了它：24 小时内 **5,843 条**请求里约 **99%** 是 `POST /v1/responses`。
   也就是说旧设计能记到的只有验收探针自己打的 chat 请求，真实流量几乎全部漏掉。
4. 它还要为探针假阳性埋单：`provider_metadata` 这个词会出现在模型正文里，当时实测到「假阳性吃掉重试预算 →
   放弃该流 → 真正渠道帧被跳过 → 记录丢失」（`parse_failures: 4`）。当时的修正（假阳性只计 `needle_misses`、
   终止帧当场 `abandon` 计 `unresolved`）随该设计一起移除，记录在此以免重犯。

### 2.4 选型结论

- 渠道信号在 CPA 侧就能拿到，不必读响应体：**usage 钩子（P1）是唯一同时满足「全协议覆盖 / 每请求一次 /
  不回到请求路径 / 可关闭」的来源**，作为唯一渠道来源。
- 代价是信号降级：网关级 `finalProvider` 不再可见（§1 约束 5）。替代信号是凭据（`Provider` / `AuthID` /
  `AuthIndex` / `AuthType`）与上游模型 slug（`upstream_model`）。
- P2 保持放弃：要动 CPA 的 `commercial-mode`，且与日志实现耦合。
- P3 退役：它唯一的优势（网关级 `finalProvider`）不足以抵消「在 ~99% 流量上完全失效」。

## 3. 实现（as-built）

### 3.1 组件

| 位置 | 作用 |
| --- | --- |
| `internal/config/config.go` | 5 个键：`channel_observe_enabled`(bool, 默认 true)、`channel_store_dir`(默认 `/CLIProxyAPI/logs/channel-observation`)、`channel_retention_days`(默认 3，上限 30)、`channel_max_size_mb`(默认 512，下限 16)、`channel_baseline_provider`(默认 `deepseek`) |
| `internal/observation/observation.go` | `usage.handle` 入口（`HandleUsage`）、载荷结构 `usageWire`、记录映射 `buildRecord`、`Recorder` / `Health`、写入队列与重启回填 |
| `internal/observation/store.go` | 按 UTC 日一文件 `channel-<YYYY-MM-DD>.jsonl`，异步批量写、按保留天数与体积清理、流式扫描、CSV 导出（`WriteCSV`） |
| `internal/observation/aggregate.go` | 内存小时环（`bucketSlots = 24*7`）+ 中位数/分位数 + `Summary(window)` |
| `internal/observation/export.go` | CSV 表头与行（含 `off_baseline` 列，由基线口径算出） |
| `internal/plugin/plugin.go` | 声明 `ManagementAPI` + 仅当 `channel_observe_enabled` 时声明 `usage_plugin`；方法分发里的 `pluginabi.MethodUsageHandle` case；`LoadConfig`/`Shutdown` 负责启停 recorder |
| `internal/management/channel.go`、`internal/management/index.html` | `GET /channel`（JSON）、`GET /channel.csv`（CSV）、页面「渠道」区；`/health` 增加 `channel_observation` |
| `cmd/channel-probe/` | 诊断探针（非发布产物）：把宿主交给插件的原始载荷落盘，用来确认渠道 / 凭据对插件是否可见 |
| `scripts/bench_channel.py` | 计时/CPU 基准脚本，口径仍是旧设计的三态（流式请求的帧间隔 / 解码窗口 / CPA CPU）；现设计的测量需要另写 |

### 3.2 记录映射与热路径

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
  | `final_provider` / `resolved_provider` | 同值，都取 `Provider` |
  | `model` | `Model`（路由名，例如 `cline-pass/deepseek-v4.1-flash`） |
  | `upstream_model` / `canonical_slug` | 同值，都取 `ResponseModel`（上游真实模型，例如 `deepseek/deepseek-v4.1-flash`） |
  | `ttft_ms` | `TTFT` ÷ 1e6（载荷单位是纳秒） |
  | `duration_ms` | `Latency` ÷ 1e6 |
  | `decode_ms` | `duration_ms − ttft_ms`，负数夹到 0（宿主两个计时独立测量） |
  | `tokens_per_second` | `output_tokens ÷ (decode_ms/1000)`，只在 `decode_ms ≥ 50` 且 `output_tokens > 0` 时有值 |
  | `status_code` | 宿主报失败时取 `Failure.StatusCode`（是 0 就保留 0），否则 `200` |
  | `total_tokens` | `Detail.TotalTokens`；为 0 时退回 `input_tokens + output_tokens` |

- 不落盘 `APIKey`、`Source`（凭据哈希）与载荷里的 `BaseURL`；`Detail` 的七个 token 计数全部落盘。

### 3.3 存储、口径与健康度

- 文件：`<channel_store_dir>/channel-<YYYY-MM-DD>.jsonl`（UTC 日切），一行一条记录，`v` 为 schema 版本（当前 **2**）。
  目录里不属本插件命名的文件**绝不删除**。异步写入：`queue` → `writeLoop`（`bufio.Writer`，按字节数或定时 flush），
  队列满只丢弃并计数。
- 保留：先按 `channel_retention_days` 删旧文件，再按 `channel_max_size_mb` 从最旧删到限额；目录打不开只记健康度，不致命。
- 重启回填：启动时回填最近 24 小时的 JSONL（超过 `warmupByteCap = 96 MB` 截断，`/health` 的 `warmup.truncated` 标出来），
  内存小时桶覆盖滚动的 7 天。
- **v1 旧行的读取**：目录里还留着更早的流式分片设计（探针时代）写下的行（`v: 1`），reader 仍然读；v2 不写
  `generation_id`、`original_model_id`、`pinned_provider`、`affinity`、`upstream_request_id`、`fallbacks_available`、
  `model_attempt_count`、`total_provider_attempt_count`、`protocol`、`frames`、`cost_usd`、`is_byok`、`user_agent`、
  `claude_code_version`、`client_app`、`source_format` 这些键，因为 usage 载荷里没有对应字段。CSV 表头保留其中五列
  （`generation_id` / `original_model_id` / `pinned_provider` / `upstream_request_id` / `protocol`），v2 行留空。
- 页面口径（页面上也写了同样的说明）：
  - **分母是窗口内的记录数**：每条完成的请求一条记录，与协议无关，没有「未识别」这一类（旧设计的
    `resolved` / `unresolved` / `parse_failures` / `needle_misses` / `pending_streams` 随分片探针一起移除）；
  - 小时表保留窗口内**每个**整点刻度（含空小时，显示 0，不插值）。窗口起点落在小时中间，所以 24 小时窗口有
    **25 个刻度**（首尾各覆盖部分小时）。
  - 小时表**从近到远排**：最新的整点在最上面，表头写着「时间（新 → 旧）」；上方三张 sparkline 卡片不受影响，
    仍按时间正序画（左旧右新）。
  - 页面区块顺序固定为 **Cline 套餐用量（官方接口） → 概览 → 官方用量明细 → 渠道**，渠道在最后。渠道区内部依次是
    概览卡片、`渠道分布`、`按模型`、时间线（卡片 + 小时表）、`原始记录` 表。成本列与成本卡片已随成本字段一起移除；
    `原始记录` 表显示 时间 / 模型 / 渠道 / 凭据（认证 ID + 索引）/ 失败 / TTFT / 解码 t/s / 输入-输出-缓存读 / 状态 / 会话。
  - 「偏离基准渠道」= `final_provider` 与 `channel_baseline_provider` 不区分大小写地不等，**读时**判定
    （`aggregate.go` 里 `provider != "" && baseline != "" && !strings.EqualFold(provider, baseline)`，记录本身不落盘
    `off_baseline`），所以改基准会立刻改变历史窗口的比例。页面上的基线名取自 payload 的 `baseline_provider`，
    **页面里不硬编码渠道名**。
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
- CSV：`GET /channel.csv?window=1h|24h|7d`，列序固定，含 `final_provider`、`resolved_provider`、`auth_id`、
  `auth_index`、`auth_type`、`off_baseline` 等列；导出失败时把原因作为注释行追加进文件（仍返回 200），
  `X-Record-Count` 头报行数。

### 3.4 fail-open 与回归保护

- 插件返回的信封恒为 `{"ok":true,"result":{}}`：不改写、不阻塞、不参与请求处理；回调发生在请求结束之后。
- 观测层的任何错误都在插件内部消化（`HandleUsage` 永不返回 error）；观测关闭时能力为 nil，宿主根本不注册 adapter。
- v0.2.x 的页面与接口在测试里仍然被断言（页面不得出现渠道以外的本地统计元素，见
  `internal/management/management_test.go`）。

## 4. 性能数据

### 4.1 现设计（usage 钩子）的成本模型 —— 实测待做

- 每次**完成的请求**一次 ABI 调用：插件侧一次 JSON 解码 + 一条记录构建 + 一次入队。没有逐帧工作、不读响应字节、
  不克隆请求体、不做探针。
- 调用点在请求结束之后，不在请求处理路径上，因此不影响任何请求的时延与吞吐。
- 观测关闭时不声明 `usage_plugin`，宿主不注册 usage 适配层，这次调用不存在（等价于没装插件）。
- **量化实测未做**：下面 §4.2 的三态数字属于已退役的流式分片设计，不能当作现设计的性能证据。现有的
  `scripts/bench_channel.py` 测的是流式请求的帧间隔 / 解码窗口 / CPA CPU（旧口径），现设计需要一套按**请求**计的
  基准才能量出「每请求一次回调」的成本。

### 4.2 旧设计的三态实测（历史，保留）

脚本：`scripts/bench_channel.py`（oracle 上 `sudo python3 /tmp/bench_tristate.py --label NAME --ctx-chars N --max-tokens M --runs R`），
测的是 CPA → `deepseek-flash-1` 的流式链路：usage token 数、SSE 帧分类计数、TTFT、解码窗口吞吐、
相邻文本帧间隔 p50/p90、以及 `cpa` 容器的 CPU（运行中 median/mean/max + 运行前 5 秒空闲 median）。

**§4.2.1 基线（无插件）**

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

**§4.2.2 观测开启 vs 关闭（旧设计复测）**

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
该判定随流式分片设计一起退役；现设计的成本模型见 §4.1。

读这份表时必须一起看三件事：

1. **同一状态内 run 间离散度就有 ±12%**（off 相位长上下文单 run 曾见 147.22 t/s，而中位是 165.15），
   所以只能按「同样 runs 数比中位数 + 配对差值」判读，不能拿单次结果下结论。
2. **本机 CPU 增量不可判定**：容器是共用的，配对差从 −61 到 +34.6 个百分点，只能说「测不出来」，
   不能拿它当结论。要拿 CPU 数字，必须换独占环境重测。
3. 每次 PATCH 重载都会让 `written` 等计数器归零，`warmup` 则从磁盘回填，所以「计数器归零」不是数据丢失。

### 4.3 真实流量取证：为什么流式分片路径必须退役

旧设计在 A/B 期间暴露了 `/health` 的 `parse_failures: 4`、`pending_streams: 18`、
`last_parse_error: provider_metadata present but no routing block`。追下去发现这不是上游的错，而是两类真实流量：

1. **Responses API 永远没有渠道帧**：`POST /v1/responses` 是真实的 clinepass 流量（日志里
   `session=codex:se… model=deepseek-flash-2`，每分钟若干条）。对它的流式响应 dump 了 **34 帧，含
   `provider_metadata` 的帧数为 0**，只有 `response.completed` 带 usage。
2. **needle 假阳性**：`provider_metadata` 这个词也会出现在模型自己的输出文本里；这种帧解不出 routing，
   若按「解析失败 → 重试 → 超预算放弃」处理，会让该流真正的路由帧被永久跳过，**记录直接丢失**。

当时给这两类流量打的补丁是「假阳性只计 `needle_misses`」「终止帧当场 `abandon` 计 `unresolved`」；
它们连同整个分片探针一起被移除。

把第 1 条放到生产规模上看，结论就变了：生产主机 24 小时内 **5,843 条**请求中约 **99%** 是 `POST /v1/responses`。
旧设计在这 99% 的流量上一条都记不到——它记到的只有验收探针自己发的 chat 请求。这就是改用 usage 钩子的直接原因
（改用后的覆盖验证见 §6）。

## 5. 部署与回滚

- 目标：`/opt/cpa/plugins/linux/arm64/clinepass-channel-monitor.so`（`abi_version=1`、`schema_version=6`）。
  替换 `.so` 不需要重启容器；只写本插件的 `.so`。
- 前置：备份 `/opt/cpa/config.yaml` 并记录 sha256；配置只做**增量**改动（新增 5 个 `channel_*` 键），改动后给出 config diff。
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
- 验收口径（本轮实际做到的与未做到的对照见 §6）：面板能回答窗口内的请求数与偏离比例；抽查原始记录与宿主 usage
  数据库对照；CSV 含渠道字段；关掉观测（`channel_observe_enabled=false`）后请求转发照旧；磁盘/写入失败时转发照旧。
- 回滚：恢复原 `.so` + 把 `channel_observe_enabled` 置 false（或删除新增配置键）→ 重载 → 确认
  `registered: true` 且 `/health` 的 `channel_observation.enabled` 为 false，页面显示「渠道观测未开启」。
  历史 JSONL 不需要动：reader 兼容 `v: 1` 与 `v: 2` 两种行。

### 5.1 2026-10-02 第二轮部署的补充教训

- **同步包必须是本次改动之后的**：复用上一轮留下的 `ccm-sync.tgz` 会构建出不含新字段的 `.so`（回读 `/channel`
  缺 `failed_requests`，白跑一轮）。判据是编译输入哈希（`LC_ALL=C sort` 后再 `sha256sum`）与本地一致；
  哈希没变就是没同步。
- **回读要看 `plugins[].path` 与 `/health.version` 两个值**：列表里的 `path` 读的是磁盘文件，可能已是新版，
  而已加载实例仍是旧版（实测 `path` 指 `0.3.0-dev.177`、`/health` 仍回 `0.3.0-dev.176`，`main.log` 没有对应的
  `plugin hot reloaded` 行）。两者版本号不一致时用 `sudo docker restart cpa` 兜底（本次即如此恢复）。

## 6. 验收清单（2026-10-02 复验）

宿主：生产主机；插件构建 `0.3.0-dev.173`；旧版本 `0.3.0-dev.171`。未记录该生产主机的 CPA 版本号。

（旧设计 2026-09 的那轮 Step 2 验收结论随分片探针一起退役，其性能数字保留在 §4.2；下面是 usage 钩子这一版的复验。）

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
- [x] README 更新：能力、口径、代价、`/health` 字段、排障、升级/回滚、性能（旧数字标注为历史）。

**本轮仍未做（不算通过）**：

- [ ] **现设计（usage 钩子）的性能实测**：§4.2 的三态数字属于退役的流式分片设计，现设计只有成本模型（§4.1），
      没有数字；`scripts/bench_channel.py` 的口径也要改。
- [ ] **基准语义决定**：`channel_baseline_provider` 该取什么值（渠道名？`upstream_model` 口径？），未决定，
      现象与影响见 §3.3。
- [ ] **存储里仍有旧设计的 `v: 1` 行**（探针 / 流式时代写下），reader 仍读、页面按空值显示；是否清理未决定，
      也解释了 CSV 里那几列 v1 数据为空的原因。
- [ ] **插件侧 6 条 vs 宿主 usage DB 607 条的数量级差未解释**：采集窗口起点、观测开启时刻、记录落盘时序等原因
      本轮没有取证。
- [ ] **TTFT / `Latency` 的起点未取证**：不知是否含「客户端 → CPA 排队 → 连上游」那段，因此新旧口径的 TTFT
      不可直接比较。
- [ ] **fail-open 演练与原始记录逐字段抽查未在新设计下重做**：旧设计做过的两类演练（把 `channel_store_dir`
      指向普通文件、逐字段对照原始响应帧）本轮没有重跑。
- [ ] **生产宿主的 CPA 版本号未记录**，因此无法声明新设计在哪些宿主版本上受支持。
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
- [ ] 现设计性能实测、`v: 1` 旧行清理、TTFT 起点取证、fail-open 演练、生产宿主 CPA 版本号——见上一条清单。
