# 渠道观测（逐请求渠道分布）—— Step 0 判定与实现说明

本文回答一个问题，并记录它的取证过程：**过去 24 小时里，clinepass 的请求有多少比例没有落在 deepseek 官渠**，
以及这些请求的 TTFT 与解码速度是多少。

本文的判定先于实现：`§2` 的三路径结论是在写第一行实现代码之前、用可复现命令在 oracle 上取证得出的（命令与原始输出见各小节）。
`§4` 的性能数字分两批：`§4.1` 是 Step 0 的基线（无插件），`§4.2` 是 Step 2 的三态复测（无插件 / 新方案开启 / 新方案关闭）。
**新方案开启时必须与基线同结论（同量级 t/s、同量级 CPU），否则不合并**：不达标就退回「旁路采样探针」方案，本文与 README 同步改写。

## 1. 目标与硬约束

目标（不可退让的部分）：

- 面板能回答「过去 24h 有多少条 / 多大比例的 clinepass 请求没有落在官渠」，并且能被抽查核对；
- 同时给出这些请求的 TTFT 与解码速度；
- 口径是**全量采集**（每一条 clinepass 请求都要进去，比例必须是精确值，不是抽样估计）；
- 记录保留 3 天、磁盘上限 512 MB。

硬约束：

1. **不许回到请求路径上**。v0.2.0 删掉逐请求统计的原因就是 `ResponseBeforeTranslator` 会让宿主为每一帧
   clone 并向插件 JSON 序列化两份请求体（当时 7–16 ms/帧、cpa CPU 106–192%、357 → ~100 t/s）。
   因此：除 `UsagePlugin` 外，**不声明任何 ResponseNormalizer 能力**（`ResponseBeforeTranslator` /
   `ResponseAfterTranslator` / `ResponseTranslator` 都不声明），除非 §4 的实测证明代价可接受，并把数字写进 README。
2. 插件**永不阻塞**请求：任何失败都必须 fail-open（宿主捕获 panic 并熔断插件，见 §3.3）。
3. v0.2.x 的能力不得回退：套餐/额度/官方用量、凭据自动发现、`/health` 自诊断都必须照旧。
4. 不改 CPA 源码、不改写请求或响应、不干预路由与固定（pinning）。

## 2. Step 0：三条路径的判定

渠道信息**只存在于上游响应体里**：Cline 网关在响应的某一帧给出
`choices[0].delta.provider_metadata.gateway.routing`，其中 `finalProvider` / `resolvedProvider` /
`pinnedProvider` / `affinity` / `canonicalSlug` / `modelAttempts[].providerAttempts[]` 才是「这条请求最后落在哪个渠道」。
实测（`sudo python3 /tmp/channel_probe.py stream 120 deepseek-flash-1`）：70 帧里有且仅有 1 帧带 `provider_metadata`。

为什么不能只记 `canonicalSlug`：渠道与 `canonicalSlug` 是两个独立字段。历史数据里 16,396 条
`deepseek-v4.1-flash` 记录中有 4 条 `finalProvider` 是 `alibaba`(3) / `particle`(1)，而 `canonicalSlug` 仍然是
`deepseek/deepseek-v4.1-flash` —— **只记 canonical slug 等于没有渠道**。

| 路径 | 能否拿到渠道 | 能否拿到 TTFT/解码 | 结论 |
| --- | --- | --- | --- |
| P1 只声明 `UsagePlugin` | **不能** | 能 | 不满足需求，不作为渠道来源 |
| P2 解析 CPA 自身 request-log | 能（原始报文里有） | 能（需要自己解析） | 本部署下是死路：要关 `commercial-mode`，代价与约束 1 矛盾 |
| P3 响应侧拦截器（`response_stream_interceptor`） | **能** | 能（同一帧里有 usage） | **选定路径** |

### 2.1 P1：`UsagePlugin` 拿不到渠道（不满足）

- `sdk/pluginapi/types.go:1314-1317`：`type UsagePlugin interface { HandleUsage(context.Context, UsageRecord) }`，
  每个请求结束调用一次。
- `UsageRecord` 的全部字段（`sdk/pluginapi/types.go:1471-1528`）：`RequestID:1474`、`TraceID:1476`、`Provider:1478`、
  `BaseURL:1480`、`ExecutorType:1482`、`Model:1484`、`Alias:1486`、`APIKey:1488`、`SessionID:1490`、`AuthID:1494`、
  `AuthIndex:1496`、`AuthType:1498`、`ResponseModel:1508`、`RequestedAt:1515`、`Latency:1517`、`TTFT:1519`、
  `Failed:1521`、`Failure:1523`、`Detail:1525`、`ResponseHeaders:1527`。
  **没有响应体字段**，唯一的「上游物证」是 `ResponseHeaders`（来源链：
  `internal/runtime/executor/helps/usage_helpers.go:595` → `logging_helpers.go:191-193` → `internal/logging/requestmeta.go:115`）。
- 装配点：`internal/pluginhost/adapters_usage_translation.go:19-33 RegisterUsagePlugins()`（仅当
  `Capabilities.UsagePlugin != nil`；调用点 `sdk/cliproxy/service_plugins.go:127`）→ `:132 HandleUsage` →
  `:170-209` 组装 `pluginapi.UsageRecord`（`:208 ResponseHeaders: cloneHeader(record.ResponseHeaders)`）。
- 结论：`TTFT` / `Latency` / `Detail` 都在，所以**延时与吞吐是 P1 能解的**；渠道不是。P1 只能作为「延时口径的第二来源」，
  不能作为渠道来源。

P1 的代价（顺带取证，用于比较）：只声明 `UsagePlugin` 时，逐帧 clone + JSON 的关口
（`adapters_usage_translation.go:264-276 / :278-289 / :291-303`）都会因为能力为 nil 而 `continue`，
只留下 `bytes.Clone(body)`；但只要有插件存在，宿主就会 `sdk/cliproxy/service_plugins.go:128 sdktranslator.SetPluginHooks(s.pluginHost)`，
于是 `sdk/translator/registry.go:260-262 / :296-298` 每帧仍然做一次 `bytes.Clone`（纯 memcpy，无 JSON、无 cgo）。

### 2.2 P2：解析 CPA 自己的 request-log —— 本部署下是死路

取证过程：

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

### 2.3 P3：响应侧拦截器 —— 唯一可行，选定

上游响应字节只有这些入口（除三个被禁的 Response*Translator 之外）：

| 钩子 | 粒度 | 适用 |
| --- | --- | --- |
| `ResponseBeforeTranslator` | 每响应 | **禁止**（约束 1，最贵） |
| `ResponseTranslator` / `ResponseAfterTranslator` | 每响应（非流式） | 不用：流式响应不走它，且属于 ResponseNormalizer |
| `Capabilities.ResponseInterceptor`（`ResponseInterceptRequest` `sdk/pluginapi/types.go:1190-1204`） | 每个非流式响应 | 覆盖不了流式（流式在这里没有完整响应体） |
| `Capabilities.StreamChunkInterceptor`（`StreamChunkInterceptRequest` `sdk/pluginapi/types.go:1216-1247`） | **每个 SSE 分片** | **选用**：渠道帧与 usage 帧都在分片里 |
| `WebSocketResponseObserver`（`WebSocketResponseEvent:1265`） | 每个 ws 事件 | 只对上游 ws executor 生效，本场景用不上 |

选定 `response_stream_interceptor`（能力 JSON 字段名 `internal/pluginhost/rpc_schema.go:42`，
方法名 `sdk/pluginabi/types.go:75 MethodResponseInterceptStreamChunk = "response.intercept_stream_chunk"`）。

宿主行为（决定为什么它是安全的）：

- **零开销开关**：`internal/pluginhost/rpc_client.go:150-151` 只在 `resp.Capabilities.StreamChunkInterceptor`
  为真时挂 adapter；`internal/pluginhost/adapters_interceptors.go:303 HasStreamInterceptors()` 在能力为 nil 时返回 false，
  于是 `sdk/api/handlers/handlers_stream.go:96 streamInterceptorsActive := streamInterceptorsEnabled(interceptorHost)`
  为假 → **不启用观测时请求路径不付出任何代价**（这也是「新方案关闭」态可以等价于「无插件」态的原因）。
- **fail-open**：`internal/pluginhost/adapters_interceptors.go:56-72 callStreamChunkInterceptor` 捕获 panic 并
  `fusePlugin`；插件返回错误只会让 `ok=false`，分片照原样转发。
- **分片载荷**：`internal/pluginhost/adapters_interceptors.go:257 InterceptStreamChunkExcept` 每片 `bytes.Clone(req.Body)`；
  schema ≥3 剥掉 `OriginalRequest` / `RequestBody`，schema ≥5 剥掉 `HistoryChunks` —— 所以逐分片不会重复搬运请求体。
- **响应结构**：SDK 结构体没有 json tag，JSON 键就是 Go 字段名（`RequestID` / `Body` / `ChunkIndex` …），
  `[]byte Body` 走 Go 默认 base64（`internal/pluginhost/rpc_client.go:209-247 sanitizePluginRequest` 不清 body）；
  信封是 `sdk/pluginabi/types.go:113-117 Envelope{OK, Result, Error}`。
- 流式请求头只在 header-init（`ChunkIndex = -1`，`const StreamChunkHeaderInitIndex = -1`）携带一次：
  `sdk/api/handlers/handlers_stream.go:103-104 streamRequestHeaders = cloneHeader(opts.Headers)`（**客户端请求头**，`:117` 传给插件），
  `:220-240` 逐分片复用；`opts.Metadata` 只带 `coreexecutor.RequestPathMetadataKey`，**不含 session / 客户端 IP**。
- 已知缺口（写在这里以免以后再踩）：分片钩子的 `RequestID` 与 `UsageRecord.RequestID` 是**两个不同的 UUID**，
  唯一的公共键是 `TraceID`（= `logging.GetRequestID(ctx)`，8 位 hex，出现在 `UsageRecord:1476`、
  `RequestInterceptRequest:1121`、`RequestCompletion:1177`、`WebSocketResponseEvent:1265`），
  但 **`StreamChunkInterceptRequest`（`:1216-1247`）没有 TraceID 字段** → 渠道记录无法与 P1 的用量记录按键直接 join，
  只能靠 session/model/时间窗对齐。这是选择「渠道信息只从响应帧自给自足地取全」的直接原因：
  每条记录里的 session、token、cost 都来自那一帧，不依赖 P1。

### 2.4 选型结论

- 渠道只能从响应体来 ⇒ P1 出局；P2 要动 CPA 的 commercial-mode 且与日志实现耦合 ⇒ 放弃；
  **P3 是唯一既能拿到渠道、又能保持「不声明 ResponseNormalizer、可关闭、fail-open」的路径**。
- P3 的代价用「每分片两次 `bytes.Contains` 探针 + 全流 1–2 次 JSON 解码」把它压到最低（§3.2），
  并用 §4.2 的三态实测验证。

## 3. 实现（as-built）

### 3.1 组件

| 位置 | 作用 |
| --- | --- |
| `internal/config/config.go` | 新增 5 个键：`channel_observe_enabled`(bool, 默认 true)、`channel_store_dir`(默认 `/CLIProxyAPI/logs/channel-observation`)、`channel_retention_days`(默认 3，上限 30)、`channel_max_size_mb`(默认 512，下限 16)、`channel_baseline_provider`(默认 `deepseek`，小写化) |
| `internal/observation/observation.go` | 分片观测热路径、流内状态机、`Recorder`、`Handle`/`SetActive`/`Active` |
| `internal/observation/store.go` | 按 UTC 日一文件 `channel-<YYYY-MM-DD>.jsonl`，异步批量写、按保留天数与体积清理、流式扫描、CSV 导出（`WriteCSV`） |
| `internal/observation/aggregate.go` | 内存小时环（`bucketSlots = 24*7`）+ 中位数/分位数 + `Summary(window)` |
| `internal/observation/export.go` | CSV 表头与行（含 `off_baseline` 列，由基线口径算出） |
| `internal/plugin/plugin.go` | 声明 `ManagementAPI` + 仅当 `channel_observe_enabled` 时声明 `response_stream_interceptor`；`LoadConfig`/`Shutdown` 负责启停 recorder |
| `internal/management/channel.go`、`internal/management/index.html` | `GET /channel`（JSON）、`GET /channel.csv`（CSV）、页面「渠道」区；`/health` 增加 `channel_observation` |
| `scripts/bench_channel.py` | 可运行的计时/CPU 基准脚本（sha256 `ac7f34cfca90d1c7f6e9d06de2b03a36e704562d0aad9379033f8b00cad45ed5`） |

### 3.2 热路径设计（为什么它不该影响转发）

每个分片只做常数级工作，**不等于「每分片解码 JSON」**：

1. 两次 `bytes.Contains` 探针：`needleRouting = []byte("provider_metadata")`、`needleUsage = []byte("\"usage\"")`；
   未定首 token 时再加一次宽松文本探针（`content` / `reasoning` / `reasoning_content` / `text`）。
2. 探针命中才 `json.Unmarshal`（`json.Decoder` + `UseNumber()`，只解第一个 JSON 值，因此 `data:` 前缀与尾随 SSE 文本无害）；
   一次流通常只有 1–2 帧命中（渠道帧、usage 帧）。
3. 跨分片防御：保留上一分片 `previous`（上限 `maxPreviousChunk = 1<<20`），解码失败时 `prev+body` 重试一次，
   用 `retryRouting` / `retryUsage` 把「探针命中但本片解不开」的状态带到下一片（上限 `maxDecodeRetries = 3`，之后放弃并计数，
   **不会**让后续每个分片都走解码）。
   回退解码拿到的帧**不能**当作本分片的证据：解码器只读第一个完整 JSON 值，而它可能是上一分片自己的帧
   （`usedPrevious` 标志就是为此存在，否则跨分片重组会误判、把记录丢掉）。
4. 探针假阳性不等于解析失败：`provider_metadata` 这个词也会出现在模型的输出文本里。若整帧用 `findObject` 递归都找不到
   `provider_metadata` 对象，就按「这只是一个词」处理——不重试、不消耗预算、不放弃该流，只计 `needle_misses`。
   （反例是 2026-10-02 实测到的真实故障：假阳性吃掉重试预算 → 放弃该流 → 真正的渠道帧被永久跳过 → 记录丢失。）
5. 渠道帧即结束帧：解出 routing 就 `finish()` 建记录入队，然后从 `pending` 移除；后续帧不会重复建流。
6. 没有可观测内容的帧（例如 `data: [DONE]`）在流已结束时被忽略，**不会**造出一条假请求。
7. 终止帧当场结账：流一直没拿到 routing 时，遇到终止帧（`[DONE]`、`response.completed`、`response.incomplete`、
   `response.failed`）立即 `abandon` 并计入 `unresolved`，不等 15 分钟的 janitor——否则计数只在事后 15 分钟才动，
   没法用来盯采集是否正常。
8. 写入是异步的：`queue`（4096）→ `writeLoop`（`bufio.Writer`，`flushBytes = 64<<10` 或 2 秒定时 flush）。
   队列满则丢弃并计数，绝不阻塞转发。`Flush()` 仅供测试与「点开就必须看到最新」的读路径使用。
9. 有响应但没有渠道帧的流（非 Cline 流量、或上游没回报）只计入 `unresolved`，**不落盘**：
   该能力对所有上游生效，不该让非本场景流量污染数据集。

### 3.3 存储、口径与健康度

- 文件：`<channel_store_dir>/channel-<YYYY-MM-DD>.jsonl`（UTC 日切），一行一条记录，`v` 为 schema 版本（当前 1）。
  目录里不属本插件命名的文件**绝不删除**。
- 保留：先按 `channel_retention_days` 删旧文件，再按 `channel_max_size_mb` 从最旧删到限额；目录打不开只记健康度，不致命。
- 页面口径（页面上也写了同样的说明）：
  - 分母是**已识别渠道**的请求数（上游回报了 `provider_metadata.gateway.routing` 的那些），不是全部请求；
    未识别的条数单独显示，不进分母。
    **`POST /v1/responses` 的请求永远拿不到渠道**（实测 34 帧里 0 帧含 `provider_metadata`，见 §4.3），
    所以它只会累积在 `unresolved` 里；要回答「非官渠比例」时，分母天然只含能看见渠道的那部分流量。
  - 小时表保留窗口内**每个**整点刻度（含空小时，显示 0，不插值）。因为窗口起点落在小时中间，
    24 小时窗口会有 **25 个刻度**（首尾各覆盖部分小时）；桶本身是整点桶，所以「过去 24 小时」在旧边缘最多含
    1 小时的桶级余量。
  - 小时表**从近到远排**：最新的整点在最上面，向下回溯历史（表头写着「时间（新 → 旧）」）。
    上方三张 sparkline 卡片不受影响，仍按时间正序画（左旧右新），趋势方向不变。
  - 页面区块顺序固定为 **Cline 套餐用量（官方接口） → 概览 → 官方用量明细 → 渠道**：官方口径在前，
    渠道观测在后。渠道区内部依次是概览卡片、渠道分布、按模型、时间线（卡片 + 小时表）、原始记录。
  - 「偏离官渠」= `final_provider`（缺失时退回 `resolved_provider`）与 `channel_baseline_provider` 不区分大小写地不等。
    页面上的基线名取自 payload 的 `baseline_provider`，**页面里不硬编码渠道名**。
  - **TTFT 是代理侧口径**：起点是 CPA 开始把上游响应转成流的那一刻（分片拦截器的首片），
    不含「客户端 → CPA 排队 → 连上上游」这段。实测同一批请求客户端测到 1274 / 1369 / 1332 ms，
    插件记录 352 / 417 / 387 ms，差距稳定在 ~0.9 s。所以这个 TTFT 用来横向比较不同请求/不同渠道是有效的，
    但不要拿它当客户端体验的绝对延迟；解码速度（`tokens_per_second`）在同一窗口内计算，不受这个偏移影响。
  - **解码速度有下限**：解码窗口（`decode_ms`）短于 `minDecodeWindowMs = 50` 时不记速度（`tokens_per_second: 0`），
    只保留窗口与 token 数。理由：宿主会把一个短回答一次性交给插件——实测到「14 token / 4 ms」＝ 2800 t/s，
    那是批量投递的产物而不是速度；`0` 的样本不进百分位（`appendSample` 丢 0），所以页面上的
    `decode_p50_tps` 不会被这类记录拉高。
- `/health` 增加 `channel_observation`：`enabled` / `directory` / `resolved` / `unresolved` / `parse_failures` /
  `needle_misses` / `dropped` / `written` / `queued` / `write_failures` / `pending_streams` / 最后记录时间 /
  最后写入时间 / 最后错误 / 文件数与体积 / 重启回填（`warmup`）。
  `parse_failures` = 「上游确实回报了渠道，但我们没解开」；`needle_misses` = 「探针命中的其实是正文里的一个词」，
  是健康计数（见 §4.3）；`unresolved` = 有响应但始终没有渠道回报（Responses API 与所有非 Cline 流量都归到这里）。
- CSV：`GET /channel.csv?window=1h|24h|7d`，含 `final_provider`、`resolved_provider`、`pinned_provider`、
  `canonical_slug`、`off_baseline` 等渠道列；导出失败时把原因作为注释行追加进文件（仍返回 200），
  `X-Record-Count` 头报行数。

### 3.4 fail-open 与回归保护

- 插件返回的信封恒为 `{"ok":true,"result":{}}`：头的增删、body 的替换、丢弃分片全部不发生（恒等响应）。
- 观测层的任何错误都在插件内部消化（`Handle` 永不返回 error）；观测关闭时能力为 nil，宿主根本不挂 adapter。
- v0.2.x 的页面与接口在测试里仍然被断言（页面不得出现渠道以外的本地统计元素，见
  `internal/management/management_test.go`）。

## 4. 性能数据

脚本：`scripts/bench_channel.py`（oracle 上 `sudo python3 /tmp/bench_tristate.py --label NAME --ctx-chars N --max-tokens M --runs R`），
测的是 CPA → `deepseek-flash-1` 的流式链路：usage token 数、SSE 帧分类计数、TTFT、解码窗口吞吐、
相邻文本帧间隔 p50/p90、以及 `cpa` 容器的 CPU（运行中 median/mean/max + 运行前 5 秒空闲 median）。

### 4.1 基线（无插件）

| 场景 | ctx chars | prompt tokens | 总帧 | 文本帧 | 静默帧 | TTFT | 解码窗口 | 吞吐 | 帧间隔 p50 | CPU（空闲 → 运行 median） |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 短 | 4,000 | 800 | 203 | 200 | 2 | 1.21 s | 1.135 s | 176.15 t/s | 0.192 ms | 14.52% → 21.49% |
| 长 | 227,000 | 42,612 | 1,602 | 1,599 | 2 | 1.634 s | 9.358 s | 170.98 t/s | 0.054 ms | 6.15% → 7.92% |

读数要点（写 README 时同样引用这些结论）：

- **口径用解码窗口吞吐（≈171 t/s，800 与 42,612 prompt tokens 两次一致），不要用 1/p50**：
  帧是突发到达的，p50 间隔测到的是同一 TCP 批次内的投递（0.054–0.192 ms），不是生成速度。
- 每次恒定 2 个静默帧（`role_only` 1 + `empty_delta` 1），没有匿名空帧、没有 malformed 帧。
- CPA 自身 CPU 不是瓶颈：1600 token 的流只比空闲高约 2 个百分点。容器是共用的，空闲基线在
  4.78%–14.52% 之间漂移，所以看 CPU 要**减空闲中位数**，不看绝对值。
- 输入侧比例约 0.19 token/char（4,000 chars → 800 tokens；227,000 → 42,612，线性无截断）。

### 4.2 观测开启 vs 关闭（Step 2 复测）

**方法**：先做过一轮顺序相位（先关后开，各 1 轮），short 段给出 −6.9% 的吞吐差；但同机负载漂移足以解释它
（空闲 CPU 中位数在同一台机上从 3.7% 漂到 46%；CPU 配对差散布在 −61…+34.6 个百分点之间）。
顺序相位会把「机器变忙/变闲」记到被测变量头上，因此改成**交替 A/B**：driver `/tmp/bench_ab.sh`（oracle），
每个 iteration 先跑 `off` 再跑 `on`，共 5 轮；每臂 `--ctx-chars 227000 --max-tokens 1600 --runs 1`；
日志 oracle `/tmp/bench-ab.log`（本机同一路径留档）。第 5 轮的 `on` 臂撞上上游 Vercel 429
（CPA 返回 502 `stream_initialization_failed`），与本插件无关（该请求没有 routing，本来也只会算 unresolved），
故配对样本 n=4。

| 指标 | 关闭观测（off） | 开启观测（on） | 配对差（on−off） |
| --- | --- | --- | --- |
| 吞吐 t/s | 中位 **165.15**（5 run：161.30 / 162.35 / 165.15 / 166.50 / 167.23） | 中位 **161.69**（4 run：157.64 / 160.13 / 163.24 / 164.74） | 均值 −1.56%，中位 **−1.88%**，区间 −3.04%…+0.55% |
| TTFT s | — | — | 均值 −0.04%，中位 **+1.98%**，区间 −7.68%…+3.58% |
| wall s | — | — | 中位 +1.31% |

**判定：通过（观测可留在生产开启）。** 判定线是吞吐 ≥ 基线 95%、TTFT ≤ 基线 110%：
观测开启相对关闭的吞吐中位差 −1.88%（中位比值 0.979），TTFT 中位差 +1.98%，都落在同状态内 run 间离散度之内。

读这份表时必须一起看三件事：

1. **同一状态内 run 间离散度就有 ±12%**（off 相位长上下文单 run 曾见 147.22 t/s，而中位是 165.15），
   所以只能按「同样 runs 数比中位数 + 配对差值」判读，不能拿单次结果下结论。
2. **本机 CPU 增量不可判定**：容器是共用的，配对差从 −61 到 +34.6 个百分点，只能说「测不出来」，
   不能拿它当结论。要拿 CPU 数字，必须换独占环境重测。
3. 每次 PATCH 重载都会让 `written` 等计数器归零，`warmup` 则从磁盘回填（本次 9 条记录），
   所以「计数器归零」不是数据丢失。

### 4.3 真实流量下的自证（顺带查到的两类流量）

A/B 期间 `/health` 出现 `parse_failures: 4`、`pending_streams: 18`、
`last_parse_error: provider_metadata present but no routing block`。追下去发现这不是上游的错，而是两类真实流量：

1. **Responses API 永远没有渠道**：`POST /v1/responses` 是真实的 clinepass 流量（日志里
   `session=codex:se… model=deepseek-flash-2`，每分钟若干条）。`/tmp/responses_dump.py` 对它的流式响应 dump 了
   34 帧，**含 `provider_metadata` 的帧数为 0**，只有 `response.completed` 带 `usage`
   （`input_tokens` / `input_tokens_details.cached_tokens` / `output_tokens` /
   `output_tokens_details.reasoning_tokens` / `total_tokens`）。⇒ 这类请求永远只能计 `unresolved`，
   不可能进「非官渠比例」的分子或分母。这是本方案的能力边界，不是 bug。
2. **needle 假阳性**：`provider_metadata` 这个词也会出现在模型自己的输出文本里（本仓库的 agent 当时正在讨论这个插件）。
   这种帧解不出 routing；若按「解析失败 → 重试 → 超预算放弃」处理，会让该流真正的路由帧被永久跳过，
   **记录直接丢失**（这正是 `parse_failures: 4` 的来源）。

两处对应修正已在代码里（见 §3.2 第 4、7 条）：探针命中但整帧递归找不到 `provider_metadata` 对象时，
不计解析失败、不消耗重试预算，只计 `needle_misses`；流出现终止帧而始终没有 routing 时当场 `abandon` 计入
`unresolved`。因此判读 `/health` 时：`needle_misses` 高（真实流量下每几分钟就出现一次）是**健康的**，
`parse_failures` 才是要看的异常信号。

## 5. 部署与回滚

- 目标：`/opt/cpa/plugins/linux/arm64/clinepass-channel-monitor.so`（`abi_version=1`、`schema_version=6`）。
  替换 `.so` 不需要重启容器；只写本插件的 `.so`。
- 前置：备份 `/opt/cpa/config.yaml` 并记录 sha256；配置只做**增量**改动（新增 5 个 `channel_*` 键），改动后给出 config diff。
- 启用/重载：换 `.so` 后 `PATCH /v0/management/plugins/clinepass-channel-monitor/enabled {"enabled":false}` 再 `{"enabled":true}`；
  配置改动走 `PATCH …/config`（带完整 config 对象）；开关落在 `config.yaml` 的 `plugins.configs.clinepass-channel-monitor.enabled`。
- **`{"status":"ok"}` 不等于加载成功**（实测教训）：连续 `false`→`true` 的两次 PATCH 在配置重载竞态下，
  第二次会被内存态回写覆盖，插件留在 `registered:false / enabled:false`，`/health` 与 `/channel` 直接 404
  （`main.log` 里只有 200 的 PATCH 记录、**没有** `pluginhost: plugin loaded` 行）。
  因此每次换 `.so` 后必须回读 `GET /v0/management/plugins`（返回 `{"plugins_enabled":…,"plugins_dir":…,"plugins":[…]}`，
  要读 `plugins[0].registered` 与 `effective_enabled`），为 false 就重发 `enabled:true`；
  成功时 `main.log` 会出现 `pluginhost: plugin loaded/registered/hot reloaded … version=<new>`。
- **别先删掉正在运行的 `.so`**（实测教训，比上一条更硬）：先 `rm` 活动版本、再装新版本并 `PATCH enabled`，
  宿主会停在 `registered:false / enabled:true / effective_enabled:false`，`/health`、`/channel`、资源页一起 404，
  `main.log` 里连 `pluginhost:` 行都没有；连续 7 次 `enabled:true` 都无效，**只有 `sudo docker restart cpa` 才恢复**
  （重启后不需要再 PATCH，`plugins.configs.<id>.enabled: true` 会在启动扫描时加载）。正确顺序：装新 `.so` → 重载 → 回读确认 →
  看到 `plugin hot reloaded … retired_version=<旧版本>` 之后再删旧文件。
  注意进程启动时那次加载**不写** `pluginhost:` 行，判据一律以回读列表为准。
- **比对「部署的 .so 就是当前源码构建的」**：两边跑同一条命令比哈希，`sort` 必须钉 locale，
  否则 BSD `sort`（macOS）与 glibc `sort`（服务器）排序不同会得到假不一致（本项目的 §6 双证就是这么踩过的）：
  `find internal cmd -type f \( -name '*.go' -o -name '*.html' \) | LC_ALL=C sort | xargs sha256sum | sha256sum`。
  整树比对还要排掉本地新改、未同步的文件（如 `docs/`、`README.md`）。
- 验收（Step 2）：面板能回答 24h 的偏离条数与比例；抽查 3 条原始记录与原始响应帧对上；CSV 含渠道字段；
  关掉观测（`channel_observe_enabled=false`）后请求转发照旧；磁盘/写入失败（把记录目录指向不可写路径）时转发照旧。
- 回滚：恢复原 `.so` + 把 `channel_observe_enabled` 置 false（或删除新增配置键）→ 重载 → 确认
  `registered: true` 且 `/health` 的 `channel_observation.enabled` 为 false，页面显示「渠道观测未开启」。

## 6. 验收清单（Step 2 / Step 3）

Step 2 已在生产（`0.3.0-dev.169`）逐条复现，括号里是当时的实测值：

- [x] 面板给出过去 24h 的「已识别请求数 / 偏离条数 / 偏离比例」，并说明分母口径；
      常态 `resolved=18 off=0 ratio=0 records_total=18`；把 `channel_baseline_provider` 临时改成没人用的名字后
      `off=18 ratio=1`（模型行 `off_baseline` 同步变 18），改回 `deepseek` 后回到 `off=0`（分母口径见 §3.3）。
- [x] 抽查 3 条原始记录（页面「原始记录」表）与 oracle 上的原始响应帧/JSONL 行逐字段对上；
      `spot_probe.py deepseek-flash-1 4000 200` 连跑 3 次均 `SPOTCHECK ok=18 mismatch=0`，
      且 `client_frames == stored_frames`（23/23、14/14、37/37）。
- [x] CSV 导出包含渠道字段，行数与页面窗口一致；
      `X-Record-Count: 22` 与 `records_total=22` 一致，表头含 `final_provider / resolved_provider / pinned_provider /
      canonical_slug / off_baseline`；`off_baseline` 取值是 `yes` 或空（基准翻转时 18 行全 `yes`，恢复基准后 22 行全空）。
- [x] 网络/存储失败演练：转发不受影响（fail-open 复现记录）；
      把 `channel_store_dir` 指向普通文件 `/CLIProxyAPI/logs/not-a-dir`：两次真实请求都 `status=200`、流完整
      （47 / 61 帧、`done=True`、ttft 1.75 / 1.72 s），同一时刻 `/health` 报
      `write_failures=2 queued=2 written=0 last_error='read store directory: open /CLIProxyAPI/logs/not-a-dir: not a directory'`，
      `/channel` 仍 200（旧数据照常可读）；目录改回后 `write_failures=0 last_error=None files=1`。
- [x] §4.2 三态数字写入 README，且判定通过；
- [x] README 更新：新能力、口径、代价、排障、升级/回滚；
- [x] `/health` 的采集健康度字段可用（`resolved / written / unresolved / parse_failures / needle_misses /
      write_failures / queued / files / bytes / last_error`）。
      注：「偏离」是**读时**按当前基准算的（`aggregate.go` 里 `provider != "" && baseline != "" &&
      !strings.EqualFold(provider, baseline)`，记录本身不落盘 `off_baseline`），所以改基准会立刻改变历史窗口的比例。
- [ ] `registry.json` 版本号与 release notes（若发版）——是否发版待确认。
