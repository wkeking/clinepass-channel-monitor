# clinepass-channel-monitor

CLIProxyAPI (CPA) 插件：在管理中心展示 **Cline 订阅的套餐、限额与官方用量**，并自动从 CPA 自己的 Cline 凭据里发现 API Key；v0.3.0 起另有**渠道观测**——回答「过去 24 小时里 clinepass 的请求有多少比例没有落在官方 deepseek 渠道，以及这些请求的 TTFT / 解码速度是多少」。

> **v0.2.0 是一次职责收窄；v0.3.0 把「逐请求渠道观测」以不回到请求路径上的方式加了回来**：v0.1.x 的「逐请求渠道/用量/成本统计」（JSONL 落盘、渠道分布、明细表、CSV 导出、`/stats`、`/events`、`/export`）已在 v0.2.0 **整体移除**，原因见下一节。v0.3.0 只重新声明一个响应侧分片拦截能力，且**只在观测开启时声明**：关闭时该能力为 nil，请求路径上没有任何插件代码在跑。记录写在**新目录** `<channel_store_dir>/channel-<YYYY-MM-DD>.jsonl`（默认 `/CLIProxyAPI/logs/channel-observation`），与 v0.1.x 的历史 JSONL 不同名、不同目录；旧文件本版本不读也不写。

## 为什么去掉逐请求统计

v0.1.x 注册了 `response_before_translator` 钩子。CPA 在每个流式帧都会调用它，而调用前宿主会把**整个客户端请求体**和**整个上游请求体**各 clone 一份、JSON 化后跨插件 ABI 传给插件。实测（同一 prompt，ctx≈56.7k token，输出约 1200 token）：

| 状态 | delta 间隔 p50 | 解码 t/s | cpa CPU |
|---|---|---|---|
| v0.1.1 插件开启 | 7–16 ms | ~100 | 106–192% |
| 插件关闭 | 0.0–0.1 ms | 357（官方同条件 345） | 0–7% |

插件自己在该钩子里只做一次 `bytes.Contains` + 一次 sha256（基准实测 sha256 只占 910,671 ns/op 里的 35,561 ns），**主要成本是宿主侧的载荷搬运**。CPA 是第三方开源项目，不能改它的源码让宿主只在首帧传完整请求体，所以唯一的解法是把插件从请求路径上完全摘掉：v0.2.0 只声明 `ManagementAPI`，不再声明任何请求/响应/用量能力。

v0.3.0 只把其中一个口子重新打开：CPA 的 `response_stream_interceptor` 能力（能力 JSON 字段名 `response_stream_interceptor`，Go 字段 `StreamChunkInterceptor`，ABI 方法 `response.intercept_stream_chunk`），而且**只在观测开启时声明**。关闭时能力为 nil，CPA 侧 `HasStreamInterceptors()` 返回 false，请求路径上既不 clone 分片也不走插件 ABI——在宿主看来「观测关闭」等价于「没装插件」。开启时每个流式分片只多一次 `bytes.Contains` 探测（在分片里找 `provider_metadata` 或 `"usage"`），探针命中才解 JSON（一次流通常 1–3 帧命中），并从帧时间戳取 TTFT 与解码窗口。实测数字见下文「[性能](#性能)」。

代价只有一条：凭据发现的第 4 顺位（"最近一次被拦截请求上的 bearer"）没有了。前三个顺位（`plan_api_key` → `plan_config_path` 指向的 CPA `config.yaml` → 宿主 auth 回调）保持原样，实测部署走第 2 顺位即可，且第 4 顺位本来就基本无效——下游客户端给 CPA 的是 20 字符的 `sk-…`，会被 `looksLikeClineKey` 过滤掉。

## 能力一览

- 管理页展示：套餐名与月费、套餐说明与权益清单、订阅周期与取消状态、5 小时 / 每周 / 每月限额进度与重置时间、近 31 天官方 Token 总量（输入/输出）、参考成本、余额、官方计费条目数；
- 概览卡片（官方口径）：近 1 小时 / 近 24 小时的官方计费请求数、总 Token 数、缓存命中率，以及近 7 天的官方逐日汇总（只有 token 与成本）；官方记录覆盖不到窗口起点时页面会标注「官方数值偏低」；
- 官方用量明细：按模型拆开当前窗口（请求数、输入/输出 token、缓存命中率、参考成本、扣减 credits），并给出流式 / BYOK 条数；这份拆分来自已经拉到的记录，不产生额外上游调用；
- 多凭据分别轮询：一个 CPA 里配置多个 Cline 条目或多把 key 时，每把 key 一个账号卡，页面顶部出现账号下拉（≥2 个凭据时）；
- 凭据自动发现：读 CPA 自己的 `config.yaml`，通常不需要手填任何 key；key 只留在内存，不落盘、不打日志、不返回给页面；
- 自诊断：`/health` 暴露 `plan`（完整套餐快照）、`plan_usage`（官方逐条用量的采集状态）、`plan_accounts`（每个凭据的来源、账号、可用性与错误）；
- **渠道观测（v0.3.0 新增）**：过去 1 小时 / 近 24 小时 / 近 7 天窗口里，已识别渠道的 clinepass 请求有多少条、多少比例没有落在基准渠道（默认 `deepseek`，即被认为「官渠」的那个渠道名），并给出这些请求的 TTFT 与解码速度；页面有「渠道」区，可导出 CSV；
- **请求路径零成本开关**：除 `ManagementAPI` 外只声明 `response_stream_interceptor`，且**只在 `channel_observe_enabled: true` 时声明**；关闭时能力为 nil，宿主不挂载分片拦截适配层，请求路径上没有任何插件代码在跑（不 clone 分片、不走 ABI）；
- **不改写、不阻塞任何请求**：不声明任何 translator / normalizer / 请求侧拦截能力，不 clone、不改写请求或响应，不干预上游路由与固定；分片按原样转发（恒等响应）；
- **fail-open**：配置解析失败时回落到默认值，插件照常加载并照常提供套餐视图；观测层自身的错误（解析失败、队列满、目录不可写）也只在插件内部消化，分片照原样转发。

## 环境要求

| 项 | 要求 |
|---|---|
| CPA | **≥ v7.3.8**，且构建带插件支持（响应头 `X-Cpa-Support-Plugin: 1`）。官方带 CGO 的 Linux 构建才有插件支持 |
| 插件 ABI | `abi_version = 1` |
| 插件 schema | `schema_version = 6` |
| 平台 | `linux/amd64`、`linux/arm64` |
| 外网 | 需要能访问 Cline 的 API（默认 `https://api.cline.bot/api/v1`）。插件只读套餐与用量，不代理任何流量 |

> 版本兼容声明：本插件按 CPA v7.3.8 的 SDK 契约开发，已在 v7.3.10 上核对 `sdk/pluginapi`、`sdk/pluginabi`、`sdk/translator` 与插件宿主适配层均无差异；v0.2.0 的加载与热重载另在 **v8.0.4** 宿主上实测通过，v0.3.0 的渠道观测（`response_stream_interceptor`）另在 **v8.0.8** 宿主上实测通过。CPA 大版本升级后请回到本文「排障」一节按表自查。

## 安装

### 方式 A：手动放置 `.so`

1. 从 [Releases](https://github.com/wkeking/clinepass-channel-monitor/releases) 下载对应架构的压缩包（`linux_amd64` / `linux_arm64`），解压得到 `clinepass-channel-monitor.so`；
2. 放进 CPA 的插件目录：`<plugins.dir>/<goos>/<goarch>/clinepass-channel-monitor.so`
   （`plugins.dir` 由 CPA 配置里的 `plugins.dir` 决定，默认 `plugins`，相对 CPA 工作目录；也支持直接放 `<plugins.dir>/clinepass-channel-monitor.so`）；
3. 在 CPA 配置里加上配置块（见下节），或改一次配置文件触发重扫。**换 `.so` 不需要重启容器**：CPA 在配置变更时会重新扫描插件目录并热加载。
4. 用管理接口确认加载成功：

   ```bash
   curl -s -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" \
     http://127.0.0.1:8317/v0/management/plugins
   ```

   看到本插件 `"registered": true` 即为加载成功。

### 方式 B：插件商店

如果 CPA 配置了插件商店源（`plugins.store-sources`），把本仓库的 `registry.json` 地址加进去，即可在管理中心「插件商店」里一键安装，或直接调用：

```bash
curl -s -X POST -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" \
  http://127.0.0.1:8317/v0/management/plugin-store/clinepass-channel-monitor/install
```

> ⚠️ 商店安装的 `.so` 落在容器内插件目录，**容器重建会丢失**。想持久化，请给插件目录挂一个宿主卷（bind mount），或重建后重装一次。方式 A 放在挂载卷里则不受重建影响。

## 配置

配置写在 CPA 配置文件的 `plugins.configs.clinepass-channel-monitor` 下：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    clinepass-channel-monitor:
      enabled: true
      priority: 1
      # ---- 凭据发现 ----
      hosts: ["api.cline.bot"]         # 哪些 openai-compatibility 条目算 Cline（支持 ".cline.bot" 后缀写法）
      timezone: "Asia/Shanghai"
      # ---- Cline 官方用量（套餐 / 限额 / 官方用量，恒定开启）----
      plan_config_path: "/CLIProxyAPI/config.yaml"   # 容器内 CPA config.yaml 路径，用于读 Cline 凭据（留空则自动探测）
      plan_refresh: 5m                 # 套餐与限额刷新间隔
      # ---- 逐请求渠道观测（v0.3.0）----
      channel_observe_enabled: true    # 观测总开关；false 时不声明 response_stream_interceptor，请求路径零开销
      channel_store_dir: "/CLIProxyAPI/logs/channel-observation"   # JSONL 目录，按 UTC 日一天一个文件
      channel_retention_days: 3        # 保留天数（上限 30）
      channel_max_size_mb: 512         # 目录总大小上限（下限 16），超出先从最旧的文件删
      channel_baseline_provider: "deepseek"   # 基准渠道名：最终渠道不等于它的请求算「未落在官渠」
```

改动配置后 CPA 会自动重扫并热加载插件（`reconfigure`）。

### 配置项说明

| 键 | 默认值 | 说明 |
|---|---|---|
| `enabled` | `true` | 关闭后不再注册路由、不再轮询官方用量 |
| `priority` | `1` | 插件优先级 |
| `hosts` | `["api.cline.bot"]` | **只用于凭据发现**：决定 CPA `openai-compatibility` 里哪些条目算 Cline 条目，进而取它们的 `api-keys` / `api-key-entries` 来轮询官方套餐。匹配规则：用 `net/url` 解析条目的 `base-url` 取 host 后**小写精确比较**；以 `.` 开头的项按**域名后缀**匹配（`".cline.bot"` 命中 `api.cline.bot`，不命中 `evil-cline.bot`）。显式留空 `[]` → 不按 host 匹配，只认条目名恰为 `Cline` 的条目 |
| `timezone` | `Asia/Shanghai` | 展示时区。v0.2.0 起页面按**浏览器本地时区**渲染官方接口返回的绝对时间，所以这个键被保留但**没有代码读取它**（原来的消费方是已移除的本地逐请求统计）；留着是为了兼容既有配置块 |
| `plan_config_path` | 空（自动探测） | 容器内 CPA 配置文件路径，用于读 Cline 凭据。留空时按 `/CLIProxyAPI/config.yaml`（容器内挂载点）→ `/app/config.yaml` 依次尝试。Cline 的 key 通常以 `openai-compatibility[].api-key-entries[].api-key` 存在这里 |
| `plan_refresh` | `5m` | 套餐、限额与官方用量的轮询周期（最小 1 分钟） |
| `channel_observe_enabled` | `true` | 渠道观测总开关。关闭时插件**不声明** `response_stream_interceptor` 能力，CPA 侧 `HasStreamInterceptors()` 为 false，请求路径上没有任何插件开销，页面「渠道」区显示「渠道观测未开启」 |
| `channel_store_dir` | `/CLIProxyAPI/logs/channel-observation` | 渠道记录的 JSONL 目录，按 UTC 日一个文件 `channel-<YYYY-MM-DD>.jsonl` |
| `channel_retention_days` | `3` | 保留天数，上限 30（写更大也会被夹到 30） |
| `channel_max_size_mb` | `512` | 该目录的总大小上限（MB），下限 16。目录同时受天数与体积约束：**先按天数删旧文件，再按体积从最旧删到限额** |
| `channel_baseline_provider` | `deepseek` | 基准渠道名（被认为「官渠」的那个渠道）。记录的 `final_provider`（缺失时退回 `resolved_provider`）与它**不区分大小写**地不等即算「未落在官渠」 |

### 固定值（不在插件配置面板里显示）

这些键在插件配置面板里不显示，值由插件固定给默认值；确实需要改时直接写进 CPA `config.yaml` 的插件配置块再重载即可（YAML 键仍然有效）。

| 键 | 固定值 | 说明 |
|---|---|---|
| `plan_enabled` | `true` | 「Cline 套餐用量」区始终开启；设为 `false` 可整块关掉官方数据 |
| `plan_api_key` | 空 | 不手填；插件自动发现 Cline 凭据（见下文「凭据发现顺序」） |
| `plan_base_url` | `https://api.cline.bot/api/v1` | 只有走代理 / 自建 Cline API 时才需要改 |
| `plan_daily_enabled` | `true` | 另拉官方 Token 总量 / 成本 / 余额，最多每小时一次 |
| `plan_usage_enabled` | `true` | 拉官方逐条用量（账号窗口的请求数 / token / 缓存命中率口径） |
| `plan_usage_refresh` | `10m` | 官方逐条用量的增量拉取间隔（最小 1 分钟；只有大于 `plan_refresh` 时才起作用） |

v0.1.x 的统计专用键（`require_routing_marker`、`unmatched_host_samples`、`ring_size`、`jsonl_enabled`、`jsonl_dir`、`retention_days`、`join_window`、`orphan_ttl`、`capture_cost`、`capture_cache`、`store_planning_reasoning`、`mask_api_key`、`log_events`）已从插件中删除。**留着它们不会导致加载失败**：未知键被忽略，新旧配置块都能读。

## 官方用量（套餐、限额、官方用量）

插件用 CPA 自己的 Cline API Key 调用 Cline 控制台自己用的接口。这块数据始终开启，不需要配置。

| 接口 | 用途 | 备注 |
|---|---|---|
| `GET /api/v1/users/me` | 账号 id | |
| `GET /api/v1/users/me/plan` | 套餐名与月费 | `pricePerSeatCents` |
| `GET /api/v1/users/me/plan/usage-limits` | 5 小时滚动 / 本周 / 本月已用百分比与重置时间 | 页面顶部进度卡 |
| `GET /api/v1/users/{id}/usages/daily?startDate&endDate` | 逐日逐模型的输入/输出 token 与成本 | 单次范围 **≤ 31 天（含端点，所以是今天-30 ~ 今天）**；金额为**微美元**（÷1e6） |
| `GET /api/v1/users/{id}/usages?limit&cursor` | 逐条请求：`totalTokens` / `cachedTokens` / `costUsd` / `createdAt` | 每页上限 **200 条**，按时间**倒序**，**不支持按时间过滤** |
| `GET /api/v1/users/{id}/balance` | 余额 | 微美元 |

**套餐卡片里的数字**：`成本` 是 Cline 按上游 API 单价折算的**参考成本**（ClinePass 是包月，不按这条扣钱），5 小时/周/月限额百分比就是按这个口径算的；`余额` 是账号余额；`官方计费条目` 是逐日逐模型汇总的行数，**不是请求数**。

**概览三块**（顺序：请求数 · 总 Token 数 · 缓存命中率，固定一行不换行，右上角切换近 1 小时 / 近 24 小时 / 近 7 天）：

- **请求数 / 缓存命中率**在近 1 小时、近 24 小时窗口取官方逐条计费记录（整个账号，含 Cline IDE 等其他客户端）；
- **总 Token 数**是官方**累计**（近 31 天逐日汇总，与套餐区同一口径），不是当前时间窗的量；副行给出本窗口的官方输入/输出与成本；
- 近 7 天窗口来自官方逐日汇总，官方没有这个窗口的逐条明细，所以请求数与缓存命中率显示 `—`，只有 token 与成本；
- 官方记录覆盖不到窗口起点时，标题下方会标出「官方数值偏低」；页面「概览」标题旁的感叹号里写明当前口径；
- v0.1.x 的「平均延时 / 生成速度」两块是本机口径，数据来自已被移除的逐请求统计，v0.2.0 起不再显示；大数单位是 K / M / B / T（B = billion，十亿），例如 `1.77B token`。

**套餐详情与订阅周期**：同一条 `/users/me/plan` 响应里还有套餐说明、权益清单（`features.included`）、计费周期（`interval`、`type`）与订阅周期（`currentPeriodStart` ~ `currentPeriodEnd`）。`canceledAt` 有值表示这个订阅**已取消但当前周期结束前仍然有效**，页面顶部会标出「已取消，<到期日> 到期」。这些字段一直都在同一条响应里，显示它们不需要任何新请求。

**按模型明细**：同一个窗口可以按模型拆开。近 1 小时 / 近 24 小时取记录里的 `metadata.raw_model`（**真正跑的模型**，例如 `deepseek/deepseek-v4.1-flash`），并给出请求数、缓存命中率、参考成本与扣减的 credits；近 7 天窗口来自官方逐日汇总，官方只给**路由名**（例如 `cline-pass/deepseek-v4.1-flash`），也拿不到请求数与缓存列，所以那两列显示 `—`。这份拆分同样是从已经拉到的记录里算出来的，不产生额外上游调用。

**官方记录里没有的东西**：最终上游渠道（`finalProvider`）、平均延时、TTFT、生成速度、失败状态码。渠道只出现在响应体的 `provider_metadata.gateway.routing` 里，官方接口不会回报它——这正是 v0.2.0 为性能移除、又在 v0.3.0 用响应侧分片拦截单独补回来的部分（见下文「渠道观测」）。这一节的数字仍然只来自官方接口，渠道与 TTFT / 解码速度在「渠道」区单独给。

**多个 Cline 条目 / 多把 key**：官方套餐与限额是**按账号**算的，所以插件把每把 key 当成一个账号分别轮询：

- 发现范围：所有 base-url 命中 `hosts` 的 `openai-compatibility` 条目，以及名称恰为 `Cline` 的条目，取它们的 `api-keys` 与 `api-key-entries`（同一个 key 出现在多处只算一次，最多 8 个）；
- 每把 key 一个账号卡：套餐、5 小时/周/月限额、31 天汇总都是各算各的；页面顶部出现**账号下拉**（≥2 个凭据时），选择记在浏览器里；
- 同一账号的两把 key（`/users/me` 返回同一个 user id）会被合并成一条，避免对同一个账号重复拉取；
- 凭据列表每个轮询周期重新解析，所以在 CPA 里新增/删除 Cline key 后不用重启插件；
- 页面标签只显示「条目名 #序号 · sk_…尾4位」，账号显示为 `usr-xxxx…xxxx`，**不会出现完整 key**；
- 被上游拒绝的凭据（401/403，例如误把下游客户端 key 当成 Cline key）不进账号下拉，只在 `/health` 的 `plan_accounts` 里保留（带 `rejected: true`）；
- 上游调用量按账号叠加：账号之间串行并间隔 0.5s，每个账号有自己的 26 小时保留窗口、分页预算与退避。

**凭据发现顺序**：插件配置里的 `plan_api_key`（`source: plugin-config`，通常留空）→ CPA `config.yaml` 里的 Cline 供应商条目（`source: config-file`）→ CPA 凭据接口（`host.auth.list` / `host.auth.get`，`source: host-auth`）。多数部署走到第二步就够了：CPA 会把你在供应商配置里填的 key 持久化到自己的配置文件里，不需要手填任何 key（少一份密钥副本）；只有把配置文件放到容器外读不到时才需要手工写 `plan_api_key`。key 只留在内存，不落盘、不打日志、不返回给页面。

**两种配置文件布局都支持**：CPA ≤ v7 把供应商放在顶层 `openai-compatibility:`、key 放在 `api-key-entries[].api-key`；v8 的配置 schema（`config-version: 8`）把它们挪到 `api-keys.openai-compatibility:` 下、key 改名成 `keys[].api-key`。插件从 **0.2.1** 起两种都会读（0.2.0 只认旧布局，CPA 改写配置文件后会读不到凭据），平铺的 `api-keys: ["sk_…"]` 字符串写法也一并支持。

**上游调用量与限流**：逐条明细接口不能按时间过滤，窗口内有多少条记录就要翻多少页（实测近 24 小时约 3500 条 ≈ 17 页）。因此插件在内存里保留最近 **26 小时**的记录，稳态下每次只翻到已见过的记录为止（通常 1 页）；首次回填或覆盖不足时按 300ms/页 节流，最多 60 页，遇到 429 等错误会指数退避（上限 30 分钟），所以覆盖范围会在几个刷新周期内长满，而不是一次打满。

插件重载后需要重新回填；采集状态（条数、覆盖起点、是否截断、失败次数、下次重试时间）见 `/health` 的 `plan_usage`。逐条用量的增量拉取默认每 **10 分钟**一次（`plan_usage_refresh`，只有大于 `plan_refresh` 的 5 分钟轮询周期时才起作用），官方 Token 总量与余额最多每小时一次。要彻底停掉这部分上游调用，只能手工在插件配置块里写 `plan_usage_enabled: false` 或 `plan_enabled: false`——这两个键已不在配置面板里。

## 渠道观测（逐请求渠道分布）

这一块回答一个问题：**过去 24 小时里，clinepass 的请求有多少条、多大比例没有落在基准渠道（默认 `deepseek`，即「官渠」那个渠道名）**，以及这些请求的 TTFT 与解码速度是多少。数据是**全量采集**，不是抽样估计：每条带渠道信息的 clinepass 请求都会进 JSONL，比例是精确值。

### 数据从哪来

Cline 网关只在**响应体**里回报请求最后落在哪个渠道：`choices[0].delta.provider_metadata.gateway.routing`（`finalProvider`、`resolvedProvider`、`pinnedProvider`、`canonicalSlug`、`originalModelId`、`generationId`、`clientSessionId`、`affinity.outcome`、`fallbacksAvailable[]`、`modelAttemptCount`、`totalProviderAttemptCount`、`modelAttempts[].providerAttempts[].{providerRequestId,statusCode}`），同一帧里还有 `usage`（`prompt_tokens`、`completion_tokens`、`total_tokens`、`cost`、`is_byok`、`prompt_tokens_details.cached_tokens`、`completion_tokens_details.reasoning_tokens`、`cache_creation_input_tokens`）。

渠道只存在于响应帧里，所以插件声明 CPA 的 `response_stream_interceptor` 能力（Go 字段 `StreamChunkInterceptor`，ABI 方法 `response.intercept_stream_chunk`）：

- **只在 `channel_observe_enabled: true` 时声明**。关闭时能力为 nil，宿主侧 `HasStreamInterceptors()` 返回 false，请求路径上不会为这个插件 clone 分片、也不会发起 ABI 调用；
- 每个流式分片只做一次 `bytes.Contains` 探测（找 `provider_metadata` 或 `"usage"`），探针命中才解 JSON（一次流通常 1–3 帧命中）。携带 routing 的收尾帧同时也是流的最后一帧，所以它一到就直接定稿；
- TTFT 取第一个文本帧的时间戳，解码窗口与解码速度由帧时间戳算出。两个测量口径要知道：**TTFT 是代理侧口径**（起点是 CPA 开始转发上游响应的那一刻，不含客户端→CPA 排队与连上游那段，实测比客户端测到的少约 0.9 s，横向比较有效、但不是客户端体验的绝对延迟）；**解码窗口短于 50 ms 时不记速度**（宿主会把短回答一次性投递，实测出现过「14 token / 4 ms」＝ 2800 t/s，那是批量投递的产物；这种记录 `tokens_per_second` 为 0，且不进百分位）；
- 不 clone 请求体（schema ≥3 的载荷里已剥掉 `OriginalRequest` / `RequestBody`）、不改写请求或响应、不阻塞请求、不干预上游路由与固定；插件返回恒为「分片原样转发」。

### 数据存在哪、留多久

- 目录 `channel_store_dir`（默认 `/CLIProxyAPI/logs/channel-observation`），按 **UTC 日**一天一个文件 `channel-<YYYY-MM-DD>.jsonl`，一行一条记录，`v` 是 schema 版本（当前 `1`）；不是本插件命名的文件**绝不删除**；
- 记录字段：`v, time, request_id, session_id, generation_id, model, upstream_model, canonical_slug, original_model_id, final_provider, resolved_provider, pinned_provider, affinity, upstream_request_id, fallbacks_available, model_attempt_count, total_provider_attempt_count, protocol, stream, status_code, ttft_ms, duration_ms, decode_ms, tokens_per_second, frames, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, cost_usd, is_byok, user_agent, claude_code_version, client_app, source_format`；
- 写入是异步的（内存队列 → 批量 flush），队列满只丢弃并计数，**绝不阻塞转发**；
- 保留：先按 `channel_retention_days`（默认 3，上限 30）删旧文件，再按 `channel_max_size_mb`（默认 512）从最旧的文件删到限额；
- 聚合在内存里按小时分桶、覆盖**滚动的 7 天**，所以插件重启后近 24 小时的渠道视图仍在：启动时会回填最近 24 小时的 JSONL（超过 96 MB 就截断，`/health` 的 `warmup.truncated` 会标出来）；
- 目录不可写不致命：插件照常转发、照常在内存里聚合，失败暴露在 `/health` 的 `channel_observation.last_error` 与 `write_failures` 里。

### 两个口径

- **未识别（unresolved）**：流里有响应、但没有任何 `provider_metadata` 渠道帧的请求（即不是 clinepass 的流量，或上游没回报）。这些**不落盘、也不计入比例分母**，只计入 `/health` 的 `channel_observation.unresolved`。**分母只包含插件能归因到渠道的请求**，所以比例不会被别的上游流量稀释；
- **未落在官渠（off baseline）**：记录的 `final_provider`（缺失时退回 `resolved_provider`）与 `channel_baseline_provider` 不区分大小写地不等。页面上的基线名取自接口返回的 `baseline_provider`，页面里不硬编码渠道名；
- 窗口是**滚动窗口**：`24h` 指「此刻往前 24 小时」，起点落在小时中间，所以小时分布表会有 **25 个整点刻度**（首尾各覆盖部分小时；空小时显示 0，不插值）。

### 接口

管理密钥与既有接口相同（`Authorization: Bearer <management-key>`）：插件只声明 `/health`、`/channel`、`/channel.csv` 三条路由，CPA 只转发已声明的路径。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/v0/management/plugins/clinepass-channel-monitor/channel?window=1h\|24h\|7d` | 渠道视图 JSON：`enabled` 开关、`summary`（窗口 `from`/`to`、`baseline_provider`、已识别请求数 `resolved_requests`、偏离数 `off_baseline_requests` 与比例 `off_baseline_ratio`、换过渠道的请求数 `fallback_requests`、渠道数、TTFT p50/p90、解码 p50 tps、token 与成本合计、按渠道与按模型的拆分、小时分布），最新 20 条原始记录（`records`，另有 `records_total`）以及 `health`。`window` 非法返回 **400 + `invalid_window`** |
| GET | `/v0/management/plugins/clinepass-channel-monitor/channel.csv?window=1h\|24h\|7d` | 同一窗口的 CSV 导出：一行一条记录，含 `off_baseline` 列（以及 `final_provider` / `resolved_provider` / `pinned_provider` / `canonical_slug` 等渠道列），以附件形式下载（`Content-Disposition: attachment`），响应头 `X-Record-Count` 报行数，表头行恒在 |

> 面板页面在「渠道」区把这些数字画出来：窗口选择 1h / 24h / 7d、偏离官渠的条数与比例、（按渠道 / 按模型 / 按小时）三张表和原始记录表，右上角是「导出 CSV」按钮。

## 适配你自己的 Cline 条目

插件**不依赖**你在 CPA 里给上游条目起的名字来决定展示什么；`hosts` 只影响**凭据发现**：哪些 `openai-compatibility` 条目里的 key 会被拿去轮询官方套餐。

同一条目有两种被识别的方式：

- base-url 的 host 命中 `hosts`（**推荐**，与条目名无关）；
- 条目名恰好是 `Cline`（大小写不敏感；`Cline1`、`ClinePass` 这类名字**不**算命中）。

> 走宿主 auth 回调那条顺位时会宽松一些：provider 或条目名里包含 `cline` 即可。

### 形态 1：直连 `api.cline.bot`

默认配置即可，无需改动：

```yaml
hosts: ["api.cline.bot"]
```

### 形态 2：自建反代 / 中转 / 自建 PaaS（host 不是 `api.cline.bot`）

把自己的域名加进列表（推荐，保留 host 判据）：

```yaml
hosts: ["api.cline.bot", "cline-proxy.example.com", ".example.com"]
```

`".example.com"` 这种写法会命中该域名下的所有子域。

### 形态 3：条目名不是 `Cline`（例如叫 `ClinePass`、`CP`）

如果 base-url 也不是 `api.cline.bot`（例如自建代理），那么两种识别方式都不命中，凭据不会被发现。此时任选其一：把它的 host 加进 `hosts`，或把条目名改成 `Cline`。

### 怎么确认自己配对了

```bash
curl -s -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" \
  http://127.0.0.1:8317/v0/management/plugins/clinepass-channel-monitor/health
```

- `plan_accounts` 非空，且每条的 `available: true`、`items` 在增长 → 正常；
- `plan_accounts` 为空或 `available: false` 且 `error` 提到 `no Cline api key` → 凭据没被发现，按上一节检查条目名 / base-url / `plan_config_path`；
- `plan_accounts[].rejected: true` → 该 key 被上游拒绝（401/403），通常是错的 key。

## 页面与接口

页面路径：管理中心的「插件 → Cline 渠道监控」，对应资源路由

```
/v0/resource/plugins/clinepass-channel-monitor/index.html
```

该资源路由**不含任何数据**，只返回静态页面壳。页面里有一个「管理密钥」输入框，密钥存在浏览器 `localStorage`，由前端带着 `Authorization` 头去调下面的管理接口渲染数据。页面为单文件静态 HTML，**不引用任何 CDN 或外网资源**，内网环境可用。

数据接口（需要 `Authorization: Bearer <management-key>`，未带密钥返回 401/403）：

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/v0/management/plugins/clinepass-channel-monitor/health` | 完整套餐快照 `plan`（账号、限额、31 天汇总、官方窗口），`plan_usage`（官方逐条用量的采集状态），`plan_accounts`（每个 Cline 凭据的标签、账号、可用性与错误），以及 `channel_observation`（渠道采集的健康度与计数） |
| GET | `/v0/management/plugins/clinepass-channel-monitor/channel?window=1h\|24h\|7d` | 渠道视图（v0.3.0）：偏离官渠的条数与比例、按渠道 / 按模型 / 按小时的拆分、最新 20 条原始记录。`window` 非法返回 400 + `invalid_window` |
| GET | `/v0/management/plugins/clinepass-channel-monitor/channel.csv?window=1h\|24h\|7d` | 同一窗口的 CSV 导出（v0.3.0）：一行一条记录、含 `off_baseline` 列，附件下载，`X-Record-Count` 报行数 |

页面里除了套餐区还有「渠道」区（窗口 1h / 24h / 7d、偏离官渠的头条数字与比例、按渠道 / 按模型 / 按小时的表格、原始记录表和「导出 CSV」按钮）。

v0.1.x 的 `/stats`、`/events`、`/export` 三条路由已移除，请求它们返回 404（属预期）。注意**插件只声明 `/health`、`/channel`、`/channel.csv`**：CPA 只转发已声明的路径，所以一个旧版本生成的页面 / 接口清单里不会有后两条，请求它们就是 404（见「排障」）。

## `health` 字段说明

| 字段 | 含义 |
|---|---|
| `plugin` / `version` / `enabled` / `uptime` | 插件标识、版本、配置里的启用开关与本次加载后的运行时长 |
| `plan` | 完整套餐快照，页面直接渲染它：`available`、`source`、`account`、`plan_name`、`plan_price`、`plan_description`、`plan_interval` / `plan_type` / `plan_active`、`plan_benefits[]`、`plan_period_start` / `plan_period_end` / `plan_canceled_at`、`limits[]`（`percent_used` / `resets_at` / `resets_in`）、`tokens`（31 天输入/输出/总量、成本、余额、计费条目数）、`usage`、`fetched_at`、`error`、`accounts[]`；`accounts[].windows[1h\|24h\|7d]` 里另有 `models[]`（按模型拆分：`model` / `requests` / `input_tokens` / `output_tokens` / `cached_tokens` / `cache_ratio` / `cost_usd` / `credits_used`）、`stream_requests`、`byok_requests`、`credits_used` |
| `plan_enabled` | 官方套餐轮询是否开启 |
| `plan_usage` | 官方逐条用量的采集状态：`enabled`、`items`、`oldest`、`fetched_at`、`truncated`、`failures`、`retry_at`、`error` |
| `plan_accounts` | 每个 Cline 凭据一行：`id`、`label`（掩码后的 key）、`source`（`plugin-config` / `config-file` / `host-auth`）、`available`、`rejected`、`account`、`items`、`oldest`、`truncated`、`failures`、`error` |
| `request_header_names` / `request_bearer_len` | 上一次请求路径上看到的 header 名与 bearer 长度。v0.2.0 起不声明任何**请求侧**能力（v0.3.0 只新增响应分片拦截），所以**恒为空**；保留是因为它们从来只含 header 名与长度，不含凭据值 |
| `channel_observation` | v0.3.0 新增的渠道采集健康度：`enabled`（观测总开关）、`directory`（JSONL 目录）、`resolved`（已归因到渠道的请求数）、`unresolved`（有响应但没有渠道帧、因而**未落盘也未计入比例分母**的请求数）、`parse_failures`、`needle_misses`、`dropped`（队列满丢弃）、`written`、`queued`、`write_failures`、`pending_streams`、`last_record_at`、`last_routing_at`、`last_write_at`、`last_error` / `last_error_at`、`last_parse_error` / `last_parse_error_at`、`files`、`bytes`、`warmup{records,truncated}`（启动回填近 24 小时 JSONL 的结果，回填超过 96 MB 时 `truncated: true`） |
| `channel_observation.parse_failures` vs `needle_misses` | `parse_failures`：上游**确实回报了**渠道、但插件没解开（异常信号，应为 0）。`needle_misses`：分片里出现了 `provider_metadata` 这个词，但整帧递归找不到渠道对象——真实流量里模型正文也会写出这个词，所以**它在涨是健康的**，只说明探针命中了正文 |

## 隐私

- 插件**只写渠道观测的 JSONL**（v0.3.0 起，默认 `/CLIProxyAPI/logs/channel-observation/channel-<YYYY-MM-DD>.jsonl`）；关掉 `channel_observe_enabled` 后不声明分片能力，就完全不写了。v0.1.x 写的 `/opt/cpa/logs/channel-monitor/*.jsonl` 本版本不读也不写；
- 不读、不写任何其他文件，不改写请求或响应，不克隆请求体；
- 不记录 prompt、响应正文、下游 key；
- 插件**不会**打印或返回 CPA 管理密钥、上游 API key 或 auth 文件内容；发现的 Cline key 只留在内存，页面与 `/health` 里只出现掩码（`sk_…尾4位`）与 key 派生的稳定 id；
- 数据只出现在两个地方：鉴权过的管理接口（`/health`、`/channel`、`/channel.csv`）和上一条说的本地 JSONL 目录。资源页面路由是静态壳，**不含任何数据**；
- 仓库里不出现任何真实凭据或抓包标识：`scripts/check-secrets.sh` 扫描工作区**和整个 git 历史**，只放行 `sk-TESTKEY…` / `gen_FIXTURE…` / `fp_fixture…` / `codex-fixture…` 这类明显合成的值，CI 每次推送都会跑一遍。测试里要造 key 就用这些前缀，别用真 key 的前几位。

## 排障

| 现象 | 可能原因与处理 |
|---|---|
| `GET /v0/management/plugins` 里本插件 `registered: false`、`path: ""` | `.so` 没被扫描到：确认文件名是 `clinepass-channel-monitor.so` 或 `clinepass-channel-monitor-v<version>.so`，且位于 `<plugins.dir>/<goos>/<goarch>/` 或 `<plugins.dir>/` 下；确认 CPA 配置里 `plugins.enabled: true`，且 `plugins.configs` 的键名与插件 id 完全一致 |
| 加载失败、日志提示 ABI 不符 | 需要 CPA ≥ v7.3.8 的**带插件支持**构建（`X-Cpa-Support-Plugin: 1`）；插件声明 `abi_version = 1`、`schema_version = 6` |
| 插件在 `plugins` 列表里但页面 404 | 检查 CPA 版本是否满足；改一次配置触发重扫；确认资源路由路径为 `/v0/resource/plugins/clinepass-channel-monitor/index.html` |
| `/channel` 或 `/channel.csv` 返回 404 | 两种情况：① 装的是**没声明这两条路由的旧构建**（v0.2.0 及更早只有 `/health`）——换成 v0.3.0 的 `.so`；② 插件被禁用（`enabled: false`）。插件只声明 `/health`、`/channel`、`/channel.csv`，CPA 只转发已声明的路径，旧版本的页面 / 接口清单里不会有后两条 |
| 「渠道」区显示「渠道观测未开启」 | 采集器在配置里被关掉了（`channel_observe_enabled: false`）。这是开关状态，不是故障；要数据就把它设回 `true` 并热重载插件（注意：改这个键必须真的改变值才触发重载） |
| 「渠道」区有数字但记录表一直是空的 / 比例的分母一直是 0 | 只有**带 `provider_metadata` 渠道帧的 clinepass 流量**才会被记录：别的上游流量只计入 `unresolved`，既不落盘也不进分母。先看 `/health` 里 `channel_observation.unresolved` 是否在涨——在涨说明有流量但没渠道信息（不是 clinepass 或上游没回报），不在涨说明请求压根没走到这个插件（对 `/health` 以外的路径看不到它，或 `hosts` 没命中你的条目） |
| 目录不可写 / 记录数不涨 | 看 `/health` 的 `channel_observation.last_error` 与 `write_failures`：目录权限、挂载或磁盘满都会记在这里。这时插件照常转发、照常在内存里聚合，只是不落盘；修好目录后（或换 `channel_store_dir`）新记录会继续写，但内存聚合在插件重启后会丢失 |
| `POST /v1/responses` 的请求从不出现（`unresolved` 在涨） | 能力边界：Responses 协议的流式响应里**没有** `provider_metadata` 渠道帧（实测 34 帧里 0 帧带渠道），只有 `response.completed` 带 usage。这类请求永远只能计 `unresolved`，不会进比例的分母。要逐请求渠道，请让客户端走 chat/completions |
| 页面能开但一直空 | 页面里的管理密钥没填或填错（管理接口会返回 401/403）；或 `plan_accounts` 为空（凭据没被发现） |
| 套餐卡片提示「插件拿不到 Cline API Key」 | 插件读的是 CPA 自己的 Cline 凭据：确认 CPA 里有指向 `api.cline.bot` 的 `openai-compatibility` 条目（或条目名恰为 `Cline`），且 `plan_config_path` 指向容器内可读的 `config.yaml`；`/health` 的 `plan_accounts` 会列出每个凭据的来源、可用性与错误 |
| 同上，但 CPA 是 v8 且配置文件里有 `config-version: 8` | 插件 0.2.0 只认顶层 `openai-compatibility:`，而 v8 把它挪进了 `api-keys.openai-compatibility:`，于是读不到 key。升级到 **0.3.0** 即可；升级前可临时在插件配置里写 `plan_api_key` 顶上 |
| `plan_accounts[].rejected: true` | 该 key 被上游拒绝（401/403）。检查条目里的 key 是否真的是 Cline key（形如 `sk_…`、长度 ≥ 32），不要填下游客户端 key |
| 官方逐条用量的 `items` 一直不涨 | 看 `plan_usage.error` / `retry_at`：429 会指数退避（上限 30 分钟）；`plan_usage_enabled: false` 时不会采集 |
| 你还在请求 `/stats`、`/events`、`/export` | v0.2.0 已移除，返回 404 属预期。要看历史逐请求数据，用 v0.1.x 写入的 JSONL 文件，或回滚到 v0.1.1 |
| 升级 CPA 后行为变化 | 回到本文「环境要求」，核对 `X-Cpa-Support-Plugin` 头、`abi_version`/`schema_version` |

常用命令（管理密钥用环境变量传入，不要写进脚本或文档）：

```bash
export CPA_MANAGEMENT_KEY='<your-management-key>'
curl -s -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" http://127.0.0.1:8317/v0/management/plugins
curl -s -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" http://127.0.0.1:8317/v0/management/plugins/clinepass-channel-monitor/health
```

> 管理接口有暴力破解防护：**只打确定存在的路径 + 正确密钥**，错误探测会累计失败并临时封禁来源 IP。

## 升级与回滚

### 升级到 v0.3.0

```bash
make build                                   # 产出 dist/clinepass-channel-monitor.so
make install INSTALL_DIR=/opt/cpa/plugins/<goos>/<goarch>
```

1. `make build` 构建本机架构的 `.so`；
2. `make install INSTALL_DIR=/opt/cpa/plugins/<goos>/<goarch>` 把它拷进插件目录（`<goos>`/`<goarch>` 例如 `linux/amd64`）。用户级安装是默认值 `/opt/cpa/plugins/$(GOOS)/$(GOARCH)`；
3. **删掉上一版本的 `.so`**：文件名带版本号时（形如 `clinepass-channel-monitor-v<版本>.so`）新旧会同时被扫描到，先移走旧的再装新的，别让两个版本共存；
4. **重载插件**：对插件配置做一次 `PATCH` 把 `enabled` 关掉再打开（`false` → 停 → `true`），让 CPA 重新注册这个插件。**配置 `PUT` 里值没变不会触发重载**，改配置值也算；
5. 确认：`GET /v0/management/plugins` 里本插件 `registered: true`、`effective_enabled: true`、版本是 `0.3.0`。

**`{"status":"ok"}` 不代表加载成功**（实测踩过）：两次连续的 `enabled` PATCH 撞上配置重载时，第二次可能被内存态回写覆盖，插件停在 `registered:false`，页面与 `/health` 直接 404，而 `main.log` 里只有 200 的 PATCH 记录、**没有** `pluginhost: plugin loaded`。所以别信 PATCH 的回执，回读列表，是 `false` 就再发一次 `enabled:true`；成功时 `main.log` 会打印 `pluginhost: plugin loaded / registered / hot reloaded … version=<新版本>`。

```bash
export CPA_MANAGEMENT_KEY='<your-management-key>'
curl -s -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" http://127.0.0.1:8317/v0/management/plugins
```

升级本身不动数据：v0.2.x → v0.3.0 只是多声明一个能力、多一个 JSONL 目录，配置块可以原样保留；v0.3.0 新增的 5 个键不写就用默认值。

### 回滚

三种粒度，按需要选最轻的一种：

- **只是不想承担分片开销**：把 `channel_observe_enabled` 设为 `false` 并热重载。插件不再声明 `response_stream_interceptor` 能力，**请求路径上的逐帧成本彻底消失**（宿主侧 `HasStreamInterceptors()` 为 false，不 clone 分片、不走 ABI）；套餐、限额、官方用量视图完全不受影响，历史数据留在磁盘上；
- **想回到 v0.2.x**：把上一版的 `.so` 装回去（删掉 v0.3.0 的 `.so`），按上面的方式重载插件并确认 `registered: true`、版本变回 `0.2.x`。回滚后渠道接口自然 404（旧构建没声明那条路由），磁盘上的 JSONL 不会被读也不会被删；
- **想整块停掉**：把插件配置里 `enabled` 设为 `false`。路由不再注册，页面与接口都不可用。

数据侧：渠道记录就是 `channel_store_dir` 下的 `channel-<YYYY-MM-DD>.jsonl`，**那整个目录可以直接删**（只删插件自己命名的文件；插件启动时会重建目录）。内存聚合不落盘，插件重启后会从 JSONL 回填最近 24 小时。

历史版本相关的几条：

- **从 v0.1.x 升级**：直接用新版本 `.so` 覆盖旧文件（文件名带版本号时删掉旧的），触发重扫，然后在 `plugins` 列表确认 `registered: true`、版本是 `0.3.0`。配置块可以原样保留：v0.1.x 的统计键会被忽略，页面与 `/health` 的套餐部分不变；
- **回滚到 v0.1.1**：把旧的 `.so` 装回去并触发重扫即可，配置块新旧版本都能读。注意回滚等于恢复逐帧开销——回滚前先确认性能可以接受；
- **历史 JSONL 不受影响**：v0.3.0 不读也不写 v0.1.x 写的 `/opt/cpa/logs/channel-monitor/*.jsonl`，需要清理的话自行删除；
- **卸载残留**：删掉插件配置块、`.so` 文件，以及（可选）`channel_store_dir` 目录。

## 目录结构

```
cmd/clinepass-channel-monitor/   入口：C ABI 的四个 //export 符号、信封编解码、panic 兜底
  cdecl.h                        C 侧类型声明（cgo 前置用）
internal/abi/                    插件 ABI 信封
internal/buildinfo/              插件 id / 名称 / 作者 / 版本（版本由 -ldflags 注入）
internal/hostapi/                宿主回调桥：日志与 host.* 数据接口
internal/config/                 配置解析与归一化（plugins.configs.<id> 契约）
internal/plan/                   Cline 官方用量：套餐、限额、31 天汇总、逐条记录采集、凭据发现
internal/observation/            渠道观测（v0.3.0）：分片探针与解析、JSONL 存储与保留策略、内存小时桶聚合
internal/state/                  运行时状态（配置 / 用量轮询器 / 渠道存储）的发布与读取
internal/management/             管理接口与内嵌页面 index.html
internal/plugin/                 注册、生命周期与方法分发（把上面这些接起来）
```

依赖是单向的：`plugin → management / plan / observation`，`plan / observation / config` 是叶子包；`management` 与 `plugin` 通过 `state` 读取运行时状态，而 `state` 不反向依赖它们，所以没有任何 import 环。

## 性能

渠道观测是 v0.1.x 那个「逐帧 clone」问题的低配版解法，所以它的开销必须能被量出来、也必须能被关掉。下面三态用同一个脚本、同一台宿主机、同一段时间测：

- **基线**：不装插件；
- **装了插件 + 观测关闭**：`channel_observe_enabled: false`（不声明分片能力）；
- **装了插件 + 观测开启**：`channel_observe_enabled: true`。

### 测量方法

```bash
python3 scripts/bench_channel.py --label <name> --ctx-chars N --max-tokens M --runs R
```

脚本（`scripts/bench_channel.py`）对 CPA 发流式 chat 请求并统计：提示词 / 补齐 token 数、SSE 帧数、TTFT、解码窗口、解码速度（token/s）、帧间隔 p50/p90、输出字符数，以及 `cpa` 容器的 CPU 中位数 / 均值 / 最大值；另在开跑前先采一段空闲期的 CPU 中位数。参数 `--ctx-chars` 是提示词字符数、`--max-tokens` 是补齐上限、`--runs` 是重复次数，脚本对多次运行取中位数。

**验收阈值**：吞吐 ≥ 基线的 **95%**、TTFT ≤ 基线的 **110%**。必须**同一台机器、同一次会话内**测三态——跑与跑之间的离散度约 **±12%**，跨机器或跨时段比数字没有意义。

### 基线（未安装插件）

| 样例 | prompt / 补齐 | 运行次数 | 解码速度 | TTFT | 端到端 | 解码窗口 | 帧间隔 p50 / p90 | cpa CPU（空闲 / 运行中位数 / 最大） | token（提示 / 补齐） |
|---|---|---|---|---|---|---|---|---|---|
| 长提示 | 227000 字符 / 1600 | 2 | **151.47 t/s** | **1.878 s** | 12.452 s | 10.572 s | 0.341 ms / 13.594 ms | 7.89% / 12.75% / 21.24% | 42612 / 1600 |
| 短提示 | 4000 字符 / 200 | 3 | **178.89 t/s** | **1.224 s** | 2.28 s | 1.118 s | 0.198 ms / 12.668 ms | 15.79% / 15.0% / 17.88% | 800 / 200 |

短提示那组另有：每次运行 2 个静默帧（无内容的 SSE 帧），**0** 个畸形帧。

### 三态对比

顺序相位（先关后开各一轮）先给出 short 段 −6.9% 的吞吐差，但同一台机器的空闲 CPU 中位数会从 3.7% 漂到 46%，顺序相位把「机器变忙变闲」记到了被测变量头上，因此改用**交替 A/B**：每轮先关后开，共 5 轮，每臂长提示（227000 字符 / 1600 补齐 / 1 run）。第 5 轮的开启臂撞上上游 Vercel 429（CPA 返回 502 `stream_initialization_failed`，与该请求无关，它本来也只会算 unresolved），故配对样本 n=4。

| 状态 | 解码速度（中位） | TTFT | 相对关闭 | 结论 |
|---|---|---|---|---|
| 基线（未装插件） | 151.47 t/s（2 run） | 1.878 s | — | 与下面两行分属不同时段，只当数量级看 |
| 装了插件 + 观测关闭 | **165.15 t/s**（5 run：161.30 / 162.35 / 165.15 / 166.50 / 167.23） | 配对基准 | — | `channel_observe_enabled: false`，不声明分片能力，请求路径零开销 |
| 装了插件 + 观测开启 | **161.69 t/s**（4 run：157.64 / 160.13 / 163.24 / 164.74） | — | 吞吐中位 **−1.88%**（均值 −1.56%，区间 −3.04%…+0.55%）；TTFT 中位 **+1.98%**（均值 −0.04%，区间 −7.68%…+3.58%）；端到端 +1.31% | `channel_observe_enabled: true`，每帧一次 `bytes.Contains`，命中才解 JSON |

> **判定：通过，观测可以留在生产开启。** 观测开启相对关闭的吞吐中位差 −1.88%（比值 0.979），TTFT 中位差 +1.98%，都落在同一状态内 run 间 ±12% 的离散度之内，也都在验收阈值（吞吐 ≥ 95%、TTFT ≤ 110%）之内。
>
> 读这份表必须一起看三件事：①同一状态内 run 间离散度就有 ±12%（关闭相位长提示单 run 曾见 147.22 t/s，而中位是 165.15），只能按「同样运行次数比中位数 + 配对差值」判读；②**本机的 CPU 增量测不出来**（容器共用，配对差从 −61 到 +34.6 个百分点），要 CPU 数字必须换独占环境重测；③每次 PATCH 重载都会让 `written` 等计数器归零，`warmup` 会从磁盘回填，这不是数据丢失。
>
> 真实流量下的两类边界（Responses API 永远不带 `provider_metadata`、模型正文里出现 `provider_metadata` 一词）与相应修正见 [docs/channel-observation.md](docs/channel-observation.md) §4.3。

## 构建与开发

```bash
make build       # 构建本机架构的 .so（CGO，-buildmode=c-shared）到 dist/
make test        # 单元测试（配置解析、套餐快照渲染、管理路由分发）
make install     # 安装到本地 CPA 插件目录（路径可通过变量覆盖）
make tools       # 安装固定版本的 Go 工具链（1.27.1）到 .toolchain/go，无需 root
make clean-cache # 清空 Go 构建缓存（.toolchain/gocache、gotmp）
make clean       # 删掉构建产物 dist/
```

工具链版本由 `Makefile` 的 `GO_VERSION` 固定为 `1.27.1`（`scripts/install-go.sh` 的默认值与之相同），和 `go.mod` 里 `go 1.26.0` 的语言下限是两件事。工具链、模块缓存、构建缓存都在 `.toolchain/` 下并且已 gitignore：`make clean-cache` 只清构建缓存与临时目录，工具链和模块缓存保持不动，所以清完仍能离线构建（第一次构建是冷编译，会慢一些，缓存会重新长出来）；连工具链一起删就直接删掉 `.toolchain/`，之后需要 `make tools` + `go mod download` 重新拉取。

跨平台产物由 GitHub Actions 在 tag 推送时构建，见下文「发布与插件商店」。

## 发布与插件商店

**版本只有一个来源**：`internal/buildinfo/buildinfo.go` 里的 `Version`。release tag 必须是 `v<version>`（例如 `v0.3.0`），本地 `make build` 自动加 `-dev.N` 后缀，所以每次构建的版本都不同、CPA 一定会热加载新库而不是继续用旧的。

**本地打包**（产物落在 `release/`，已 gitignore）：

```bash
make tools                               # 固定版本 Go 工具链，无需 root
scripts/release.sh 0.3.0                 # 只打本机平台
scripts/release.sh 0.3.0 linux/arm64 linux/amd64
```

脚本按插件商店的硬性规则产出并自检：`release/clinepass-channel-monitor_<version>_<goos>_<goarch>.zip`（zip 根目录里只有 `clinepass-channel-monitor.so`）、`release/checksums.txt`（sha256sum 格式）。CGO 交叉编译需要目标平台的 C 工具链：Linux 上会识别 `x86_64-linux-gnu-gcc` / `aarch64-linux-gnu-gcc`，windows/amd64 识别 `x86_64-w64-mingw32-gcc`（`gcc-mingw-w64-x86-64`），没有就直接报错让你装；darwin 产物必须在 macOS 上构建（CGO 链接要用 macOS SDK）。

**CI 发布**：推 tag 触发 `.github/workflows/release.yml`，五平台矩阵（`linux/amd64`、`linux/arm64`、`windows/amd64`、`darwin/arm64`、`darwin/amd64`）各自构建、合并 `checksums.txt`、创建 GitHub Release。插件商店的审核要求这五个平台齐全（缺 `darwin_amd64` 或 `windows_amd64` 会被判 Platform Support Violation），所以矩阵不要再删项：

```bash
git tag v0.3.0 && git push origin v0.3.0
```

不想为一个新平台先发版本的话，先干跑一次：把改动推到一个名为 `release-dryrun` 的分支（或直接在 Actions 里手动跑 `release`），它只构建并上传五个 zip 作为 artifacts 供你核对，不会创建 release——publish job 用 `if: startsWith(github.ref, 'refs/tags/')` 卡掉了非 tag 事件。脚本还会读产物头部校验容器格式与架构（ELF / Mach-O / PE 的 machine 字段），所以 `darwin/amd64` 那一条不会悄悄出一个 arm64 库。

**登记到插件商店**：仓库根目录的 `registry.json` 就是登记文件（`schema_version: 1`，`github-release` 类型，资产取自你的 GitHub Release）。两种用法：

1. **自建源**（立刻可用，不需要别人合并）：把仓库推上 GitHub 后，在 CPA 配置里加这个文件的 raw 地址：

   ```yaml
   plugins:
     store-sources:
       - "https://raw.githubusercontent.com/wkeking/clinepass-channel-monitor/main/registry.json"
   ```

   CPA 始终加载官方源，第三方源只是追加；源 id 是 URL 的 sha256 前 12 位，名称取 host，私有源另配 `plugins.store-auth`。
2. **官方商店**：向 `router-for-me/CLIProxyAPI-Plugins-Store` 提 PR，只在它的 `registry.json` 里加一条（必填 `id`/`name`/`description`/`author`/`repository`，且 `repository` 必须正好是 `https://github.com/{owner}/{repo}`），并附最新 tag 与 release 资产的证据。合入后所有人默认可见。你的项目本身**不需要**推进那个仓库——它只放登记表，二进制始终留在你自己的仓库。

**安装与验证**：

```bash
curl -s -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" \
  http://127.0.0.1:8317/v0/management/plugin-store          # 各源条目、installed_version、update_available
curl -s -X POST -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" \
  "http://127.0.0.1:8317/v0/management/plugin-store/clinepass-channel-monitor/install?version=0.3.0"
```

也可以在管理中心的「插件商店」里点安装。装好的库落在 `<plugins.dir>/<goos>/<goarch>/clinepass-channel-monitor-v<version>.so`，之后还需要 `plugins.configs.clinepass-channel-monitor` 配置块并触发一次重载才会生效。覆盖当前正在加载的同版本文件会被 CPA 拒绝（`ErrLoadedPluginLocked`）：要么发一个新版本号，要么先重启 CPA。

两个容易踩的点：① tag 带 `v`、资产名不带 `v`，CPA 逐字符匹配资产名（`<id>_<version>_<goos>_<goarch>.zip`），少一个下划线就是 `release asset ... not found`；② 同一个插件 id 同时出现在多个源时，手工安装的副本会变成 `unknown` 来源、不再提示更新——自建源和官方商店只登记一处，或统一走商店安装。

## 许可证

MIT，见 [LICENSE](LICENSE)。
