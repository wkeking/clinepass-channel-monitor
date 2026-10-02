# 实施清单：用 CPA request-log 采集「每个请求的真实渠道」

> 状态：待实施（Step 0 未开始）
> 日期：2026-10-02
> 关联：`docs/channel-observation.md`（现有 usage 钩子方案）、`README.md`「渠道观测」一节
> 本文是工作单：先做 Step 0 的四项验证，任一项不过则停并走 §7 退路。

## 0. 目标与成功标准

**要交付的唯一形态**：页面上一行一条请求 —— 时间 / **真实渠道（`finalProvider`）** / `resolvedProvider` /
尝试次数 / 输入-输出-缓存 token / TTFT，再配合 Cline 套餐的 5 小时·周·月额度和已用 token。
其他统计不是本次范围。

成功标准（可当场演示）：

1. 随便指一条真实业务请求，能给出它对应的 `provider_metadata.gateway.routing.finalProvider`，
   并同时给出**产生该值的那段日志原文**；
2. 同一份日志的 `=== API REQUEST ===` 段能证明出站 body 里确实带了
   `providerOptions.gateway.only: ["deepseek"]`（这是最初问题的另一半）；
3. 开启后 CPA 的内存/磁盘/延迟增量有实测数字，且磁盘不随时长增长。

## 1. 已知事实（直接引用，不要重新论证）

**拓扑**：harness → sub2api（Responses）→ CPA（入口 responses）→ CPA 内部转 chat →
`api.cline.bot/api/v1` → Cline 网关（Vercel 管道）→ 模型渠道。
**渠道块在上游 chat 响应里，CPA 转 responses 时丢掉。**

**为什么其它路不通（已核对源码，见 `.reference/CLIProxyAPI`）**：

- `ResponseBeforeTranslator`（`sdk/pluginapi/types.go:107-108`）：唯一能看上游字节的插件钩子，但
  `internal/pluginhost/adapters_usage_translation.go:364-372` **无条件 clone 两份请求体**
  （`OriginalRequest` / `TranslatedRequest`），且 `sdk/translator/registry.go:262` 在 `TranslateStream`
  里**每帧**调用它。**没有 schema 开关**（对比 `StreamChunkInterceptRequest` 的 schema≥3 剥离）。
  v0.1.x 实测解码 357→100 t/s。→ 只能做有界诊断窗口，不能常开。
- `StreamChunkInterceptor` / `ResponseInterceptor`：在 `sdk/api/handlers/`（客户端侧），注释写明
  "before downstream delivery"，**在翻译之后**，拿不到。
- `UsagePlugin`（每请求一次，便宜）：`UsageRecord` 无响应体字段。
- `RequestLifecyclePlugin`（每请求一次，异步）：`RequestCompletion`（`:1175-1188`）只有 `Metadata` 快照，
  无响应体。
- Cline 官方 usage API 的 `aiInferenceProviderName`：生产 2400 条全是 `vercel`（平台层），不是模型渠道。

**`commercial-mode` 的爆炸半径（已核对全部引用点，只有 5 处）**：

```
internal/config/config.go:50   // CommercialMode disables high-overhead request logging and HTTP
                               // middleware features to minimize per-request memory usage.
internal/runtime/executor/helps/logging_helpers.go:61  requestLogCaptureEnabled = cfg.RequestLog && !cfg.CommercialMode
internal/runtime/executor/helps/logging_helpers.go:66  if cfg.CommercialMode { return }
internal/api/server.go:156                              if !cfg.CommercialMode { RequestLoggingMiddleware }
internal/api/server_options.go:51                       if cfg.CommercialMode { sdkCfg.RequestLog = false }
```

即：关掉它只会**重新打开请求日志与 HTTP 中间件**，没有别的行为变化。

**日志文件（`internal/logging/request_logger_format.go:59-65`）里正好有两段关键内容**：

```
=== API REQUEST ===     ← CPA 发出的上游 chat 请求体（验证 pin 注入）
=== API RESPONSE ===    ← 上游响应原文（provider_metadata.gateway.routing 在这里）
```

命名形如 `v1_chat_completions-2026-09-23T120000-00000000.log`
（`<路径下划线化>-<时间戳>-<shortID>.log`，同名冲突会加 `_1`）。
`internal/logging/request_logger_body_source.go` 负责落盘与清理；CPA 侧清理是 `find … -mtime +1 -delete`。

## 2. Step 0：四项前置验证

必须先做，任何一项不过就走 §7。用**限时窗口**做（建议开 1 小时，把上限刻在时间上），不要一上来常开。

- [ ] **V1 日志真的出来了**：改 `/opt/cpa/config.yaml`（`server.commercial-mode: false` +
      `observability.logs.request-log: true`；改前 `cp -a` 备份并记 sha256），reload/重启后打一条真实流式请求，
      确认 `/CLIProxyAPI/logs/` 出现新 `.log` 且含 `=== API RESPONSE ===` 与 `provider_metadata`。
      **若 `commercial-mode` 能走管理接口改，优先用接口**（`request-log` 有读写接口；`commercial-mode` 待确认），
      避免手改文件。
- [ ] **V2 join key 存在**：确认日志文件名里的 short id 与 usage 回调的 `RequestID`（UUID）能否对应。
      不能对应就用 `(时间 ±2s, input_tokens, output_tokens)` 对齐，并把这条降级写进文档。
- [ ] **V3 体积与增量**：用一条**长上下文**请求（输入 30–40 万 token）量单文件体积，乘以真实日请求数
      （今日 1074 条）给出日增量估算。
- [ ] **V4 代价对照**：开关前后各取 30 分钟，用**已有的 usage 采集**对比 TTFT / 解码 t/s / 失败率；
      并记录 `docker stats` 的内存与 CPU。结论写进文档。

**任何一项不过 → 停在这里，走 §7，不要把开关留在生产。**

## 3. Phase 1：CPA 侧开关（可回滚）

- [ ] 备份 `/opt/cpa/config.yaml`，记录 sha256；把改动写成最小 diff（只加两个键）。
- [ ] 明确回滚步骤（改回 + reload）并**实际演练一次**。
- [ ] 记录生产宿主 CPA 版本号（`X-CPA-VERSION` 头或管理接口）——日志格式随版本变，这个必须留档。
- [ ] 记录当前插件的处理位置：日志目录 `/CLIProxyAPI/logs` 与插件自己的 `channel_store_dir`
      默认也是 `/CLIProxyAPI/logs/channel-observation`，**扫描器必须只认 CPA 的 `*.log`**，
      不要把自己的 JSONL 扫进去。

## 4. Phase 2：旁路 reader（新增，不进请求路径）

- [ ] 目录扫描：轮询 `/CLIProxyAPI/logs/*.log`（间隔 1–5s 即可）。
- [ ] **只处理写完的文件**：`mtime` 早于 N 秒（建议 5s）或文件尾出现结束标记；流式写入中的文件跳过。
- [ ] 解析：按 `=== API RESPONSE ===` 分段；流式内容是 SSE，需要按 `data:` 行拼接后**逐帧做 JSON 解析**，
      只取 `provider_metadata.gateway.routing` 路径。
      **禁止 `bytes.Contains` 命中即记录**——`provider_metadata` 这个词会出现在模型正文里，
      旧设计因此产生过假阳性（见 `docs/channel-observation.md` §2.3 第 4 条）。
- [ ] 重试文件里有多个 `=== API REQUEST ===` / `=== API RESPONSE ===` 段：取**最后一个成功**的响应段。
- [ ] 字段：`finalProvider` / `resolvedProvider` / `canonicalSlug` / `originalModelId` /
      `modelAttemptCount` / `totalProviderAttemptCount`（`fallbacks_available_count` 若在块里也取）。
- [ ] 同一文件的 `=== API REQUEST ===` 段顺手解析：记录出站 body 里 `providerOptions.gateway.only`
      与 `provider.only` 的实际值（pin 验证用）。
- [ ] **落盘后立即删除**该日志文件（或读完立刻 unlink），把磁盘占用压到"当前窗口内"；
      删除失败只计入健康度，不影响解析。
- [ ] fail-open：解析失败、目录不存在、权限不足，一律只记健康度，不影响任何请求（这层天然在旁路）。
- [ ] `/health` 增加 `channel_log` 段：扫描/命中/解析失败计数、最近成功时间、最近错误、
      已删除文件数与字节数。

## 5. Phase 3：合并与语义改名（必做，否则两套 final_provider 会撞）

- [ ] **改名**：现有记录里的 `final_provider` / `resolved_provider`（装的是 CPA 凭据，
      `internal/observation/observation.go:370-371`、`internal/observation/export.go:27-28`）
      改为 `cpa_provider`（`resolved_provider` 删除或另名），`auth_id` / `auth_index` / `auth_type` 保持。
- [ ] `final_provider` / `resolved_provider` 这两个名字**让给网关值**，只由日志解析写入。
- [ ] schema 升 v3，reader 兼容 v1/v2（v2 行的 `final_provider` 按 `cpa_provider` 解释，
      页面/导出处标注旧行语义不同）。
- [ ] 合并：按 V2 的 join key 把渠道字段并进 usage 记录；无渠道值的行**不猜**，渠道列留空。
- [ ] `off_baseline` 判定改为**只对真渠道**（`final_provider` vs `channel_baseline_provider`），
      默认值 `deepseek` 这次才成立。CPA 凭据维度单独保留一列，不要混进"是否偏离官渠"。

## 6. Phase 4：页面（只保留你要的）

- [ ] 主表：时间 / **真实渠道** / `resolvedProvider` / 尝试次数 / 输入-输出-缓存 token / TTFT /
      CPA 凭据 / 会话。
- [ ] 顶部队列：5 小时、周、月额度与已用 token（沿用现有官方接口，不动）。
- [ ] 概览卡片改成回答原问题：**近 24h 有多少条请求的 `finalProvider != deepseek`**，
      并给出这些请求的 TTFT / 解码速度。
- [ ] **删掉不再需要的部分**（用户明确说其他数值不关注）：探针/诊断相关的展示、
      无来源的 `frames`/`protocol`/`pinned_provider` 等 v1 空列、成本列。
- [ ] 口径文案：写清"渠道来自 Cline 上游响应原文（`API RESPONSE` 段）"，
      并写明失败请求（502/429）在上游响应里可能**没有**渠道块 —— 这类行渠道留空而不是记 0。

## 7. 退路（Step 0 任一验证不过时）

按顺序退：

1. **有界诊断窗口**：定时（例如每小时 3 分钟）声明 `ResponseBeforeTranslator`，逐请求采真渠道，
   到点自动取消声明；窗口内解码会掉到 ~100 t/s 档，页面上必须显著提示"诊断模式"。
2. **抽样探针**：只能回答"此刻有没有回退"，**不得**用于"24 小时回退比例"这类断言；
   已知会漏检短促回退（09-23 那次事件只有 7 分钟、4 条）。

## 8. 必须先纠正的文档陈述

`docs/channel-observation.md` §2.2 现在写"P2 解析 request-log —— 本部署下是死路"，
§2.3 写"渠道块只出现在 `/v1/chat/completions` 的响应帧里 / `/v1/responses` 的流一帧都没有"。
两处都要改：

- §2.2：改成"在不动 `commercial-mode` 的前提下是死路；关掉后是**首选**渠道来源"，
  并写上 §1 的 5 处引用点证据。
- §2.3：把"响应里没有"改成"**客户端可见的流里没有**——根因是 CPA 把上游 chat 帧翻译成
  Responses 事件时丢掉了 `provider_metadata`；上游帧是有的（历史 v0.1.x 在
  `client_protocol=openai-response` 的 16,396 条记录上都取到了 `final_provider`）"。
  这条因果之前写反了，会被后来人引用成错误前提。

## 9. 验收清单

- [ ] 一条真实业务请求：日志原文片段 ↔ 页面行**逐字段对照**（至少 3 条，含 1 条失败请求）。
- [ ] pin 验证：给出该请求 `=== API REQUEST ===` 段里 `providerOptions.gateway.only` 的实际值。
- [ ] 归因正确性：挑一条 `failed=true`（502）的请求，确认它渠道列为空、且不是被误记成 `deepseek`。
- [ ] 结构化解析证据：构造一条正文含 `provider_metadata` 字样的响应，证明不会被误判。
- [ ] 磁盘：连续运行 ≥1 小时，`logs/` 目录大小**不随时长增长**；给出删除计数。
- [ ] 代价：开关前后 30 分钟的 TTFT / 解码 t/s / 失败率 / 内存对照，数字写进文档。
- [ ] 回归：JSONL 新旧行都能读、CSV 表头正确、`off_baseline` 只对真渠道生效。
- [ ] 文档：§2.2 / §2.3 已按 §8 更正；新增一节记录本次渠道来源（含 CPA 版本号、日志格式快照）。

## 10. 非目标 / 不要做

- 不要为了拿渠道而常开 `ResponseBeforeTranslator`；
- 不要改 CPA 源码、不要用 MITM 代理、不要动 `requests.proxy-url`；
- 不要把抽样结果说成逐请求归属；
- 不要在仓库、日志或文档里出现任何 key；管理 key 只从容器环境读取；
- 不要把明文 prompt 的日志留超过必要时间。

## 11. 需要先确认的

- 是否接受 `commercial-mode: false`（代价：日志里是**明文 prompt**、内存占用上升）；
- 试运行窗口多长、磁盘上限多少；
- 页面上要不要保留 CPA 凭据那一列（建议保留：能看出是哪把 key 在扛 502）。

## 附：为什么这是当前的最优解

| 方案 | 逐请求 | 代价 | 判定 |
| --- | --- | --- | --- |
| **CPA request-log 旁路** | ✅ | 每请求落盘一次 | **最优** |
| `ResponseBeforeTranslator` | ✅ | 每帧 clone 两份请求体，解码 357→100 t/s | 只能做有界诊断 |
| `StreamChunkInterceptor` | ❌ | 便宜 | 块在翻译前，看不见 |
| `UsagePlugin` | ❌ | 便宜 | 载荷无响应体字段 |
| 官方 usage API | ❌ | 免费 | 只有 `vercel`（平台层） |
| MITM 代理（`requests.proxy-url`） | ✅ | 全局 TLS 中间人 + 证书管理 | 更重，不推荐 |
| 抽样探针 | ❌ 抽样 | 便宜 | 只能答"此刻有没有回退" |
