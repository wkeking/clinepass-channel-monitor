# clinepass-channel-monitor

CLIProxyAPI (CPA) 插件：在管理中心展示 **Cline 订阅的套餐、限额与官方用量**，并自动从 CPA 自己的 Cline 凭据里发现 API Key；v0.3.0 起另有**渠道观测**——回答「过去 24 小时里 clinepass 的请求有多少比例没有落在基准渠道，以及这些请求的 TTFT / 解码速度是多少」。渠道默认取自宿主在每个请求结束后的 usage 回调（CPA 为这条请求选中的凭据），**与客户端协议无关**，`POST /v1/responses` 同样覆盖；Cline 网关级的最终上游渠道另有一条**默认关闭**的旁路来源——解析 CPA 请求日志（`channel_log_enabled`），因为上游响应里一直有这个块，是 CPA 把它翻译成 Responses 时丢掉的。

> **v0.2.0 是一次职责收窄；v0.3.0 把「逐请求渠道观测」以不回到请求路径上的方式加了回来**：v0.1.x 的「逐请求渠道/用量/成本统计」（JSONL 落盘、渠道分布、明细表、CSV 导出、`/stats`、`/events`、`/export`）已在 v0.2.0 **整体移除**，原因见下一节。v0.3.0 只在插件注册里多声明一个 **usage 能力**（能力 JSON 键 `usage_plugin`，ABI 方法 `usage.handle`），且**只在观测开启时声明**：关闭时该能力为 nil，宿主不注册 usage 适配层，每个完成的请求都不会发起插件 ABI 调用。记录写在**新目录** `<channel_store_dir>/channel-<YYYY-MM-DD>.jsonl`（默认 `/CLIProxyAPI/logs/channel-observation`），与 v0.1.x 的历史 JSONL 不同名、不同目录；旧文件本版本不读也不写。`channel_log_enabled` 打开时另有旁路 scanner 读 CPA 请求日志（同样的「不回到请求路径上」：它只读文件）。
>
> v0.3.0 的**初版**不是这样：它声明 CPA 的 `response_stream_interceptor` 能力，从 SSE 分片里抠 `provider_metadata.gateway.routing`。该字段在**上游**（CPA → `api.cline.bot` 的 `chat/completions`）响应里有，但 CPA 把它翻译成 Responses 事件时丢掉了，所以它只出现在翻译前的 `/v1/chat/completions` 响应里；而生产主机 24 小时内 5,843 条真实请求中约 **99%** 是 `POST /v1/responses`（clinepass 上游根本没有 `/responses` 端点，直连返回 404）——也就是说旧设计只能记到验收探针，几乎记不到真实流量（实测一个 `/v1/responses` 流 34 帧里 0 帧带 `provider_metadata`）。现在改用 usage 钩子：每请求一次、全协议覆盖、请求结束后才被调用；网关级渠道另由 `channel_log_enabled` 从 CPA 请求日志补（见下文「渠道观测」）。判定过程与因果更正见 [docs/channel-observation.md](docs/channel-observation.md) §2。

## 为什么去掉逐请求统计，以及 v0.3.0 用什么方式加回观测

v0.1.x 注册了 `response_before_translator` 钩子。CPA 在每个流式帧都会调用它，而调用前宿主会把**整个客户端请求体**和**整个上游请求体**各 clone 一份、JSON 化后跨插件 ABI 传给插件。实测（同一 prompt，ctx≈56.7k token，输出约 1200 token）：

| 状态 | delta 间隔 p50 | 解码 t/s | cpa CPU |
|---|---|---|---|
| v0.1.1 插件开启 | 7–16 ms | ~100 | 106–192% |
| 插件关闭 | 0.0–0.1 ms | 357（官方同条件 345） | 0–7% |

插件自己在该钩子里只做一次 `bytes.Contains` + 一次 sha256（基准实测 sha256 只占 910,671 ns/op 里的 35,561 ns），**主要成本是宿主侧的载荷搬运**。CPA 是第三方开源项目，不能改它的源码让宿主只在首帧传完整请求体，所以唯一的解法是把插件从请求路径上完全摘掉：v0.2.0 只声明 `ManagementAPI`，不再声明任何请求/响应/用量能力。

v0.3.0 重新打开的口子不是那个逐帧钩子，而是 CPA 的 **usage 钩子**：插件在注册里声明 `usage_plugin`（Go 字段 `UsagePlugin`，ABI 方法 `usage.handle`），宿主在**每个请求结束后**调用一次，载荷是 `UsageRecord`——里面有 CPA 为这条请求选中的凭据（`Provider` / `AuthID` / `AuthIndex` / `AuthType`）、上游模型（`ResponseModel`）、宿主自己的 token 账（`Detail`）与 `TTFT` / `Latency`。它每请求只调用一次、与客户端协议无关（`/v1/chat/completions`、`/v1/responses` 都覆盖），并且**只在观测开启时声明**：关闭时能力为 nil，宿主不注册 usage 适配层，请求路径上没有任何插件代码在跑——在宿主看来「观测关闭」等价于「没装插件」。插件对每次回调只回一个「不改变」信封 `{"ok":true,"result":{}}`，只负责记录。成本模型与旧设计的历史数字见下文「[性能](#性能)」（P1 的量化测量待做；P2 请求日志那条来源有 §4.2 / §4.3 的实测）。

代价有两条。一条是凭据发现的第 4 顺位（"最近一次被拦截请求上的 bearer"）没有了：v0.1.x 的逐帧钩子顺带能看到客户端 bearer，现在插件不声明任何请求侧能力，自然看不到。前三个顺位（`plan_api_key` → `plan_config_path` 指向的 CPA `config.yaml` → 宿主 auth 回调）保持原样，实测部署走第 2 顺位即可，且第 4 顺位本来就基本无效——下游客户端给 CPA 的是 20 字符的 `sk-…`，会被 `looksLikeClineKey` 过滤掉。

另一条更要紧：**usage 载荷里没有 Cline 网关级的最终上游渠道**。`provider_metadata.gateway.routing.finalProvider`（例如这条 cline-pass 请求有没有回退到 `alibaba` / `particle` 而不是 `deepseek`）在 usage 载荷里没有对应字段，所以记录能给出的最接近信号是 **CPA 侧的凭据**（`Provider`、`AuthID`、`AuthIndex`、`AuthType`）和**上游模型 slug**（`upstream_model`，例如 `deepseek/deepseek-v4.1-flash`）。也就是说页面上的「渠道」是「CPA 把这条请求交给了哪个凭据」，不是「Cline 网关最后选了哪个上游」。网关渠道另有来源：`channel_log_enabled`（默认关闭）解析 **CPA 请求日志**——那是唯一还留着上游响应原文的地方——代价是 CPA 侧必须同时打开请求日志（`observability.logs.request-log: true` 且 `server.commercial-mode: false`，且**要重启容器**才生效），而日志里是**明文 prompt**、长上下文请求单文件 4.0–5.7 MB、实测日增量约 3.5 GB（[docs/channel-observation.md](docs/channel-observation.md) §2.2 / §4.2 / §4.3）。这些 fact 目前还没并进记录、也没画在页面上。

## 能力一览

- 管理页展示：套餐名与月费、套餐说明与权益清单、订阅周期与取消状态、5 小时 / 每周 / 每月限额进度与重置时间、近 31 天官方 Token 总量（输入/输出）、参考成本、余额、官方计费条目数；
- 概览卡片（官方口径）：近 1 小时 / 近 24 小时的官方计费请求数、总 Token 数、缓存命中率，以及近 7 天的官方逐日汇总（只有 token 与成本）；官方记录覆盖不到窗口起点时页面会标注「官方数值偏低」；
- 官方用量明细：按模型拆开当前窗口（请求数、输入/输出 token、缓存命中率、参考成本、扣减 credits），并给出流式 / BYOK 条数；这份拆分来自已经拉到的记录，不产生额外上游调用；
- 多凭据分别轮询：一个 CPA 里配置多个 Cline 条目或多把 key 时，每把 key 一个账号卡，页面顶部出现账号下拉（≥2 个凭据时）；
- 凭据自动发现：读 CPA 自己的 `config.yaml`，通常不需要手填任何 key；key 只留在内存，不落盘、不打日志、不返回给页面；
- 自诊断：`/health` 暴露 `plan`（完整套餐快照）、`plan_usage`（官方逐条用量的采集状态）、`plan_accounts`（每个凭据的来源、账号、可用性与错误）；
- **渠道观测（v0.3.0 新增）**：过去 1 小时 / 近 24 小时 / 近 7 天窗口里，clinepass 请求有多少条、多大比例没有落在基准渠道（默认 `deepseek`），并给出这些请求的 TTFT 与解码速度；「渠道」是 **CPA 为这条请求选中的凭据**（`Provider` + `AuthID` / `AuthIndex` / `AuthType`），由宿主每请求一次 usage 回调给出，因此对**所有客户端协议**都成立（含 `POST /v1/responses`）；页面有「渠道」区，可导出 CSV。注意当前生产上渠道名是 `openai-compatible-cline*`、基准仍是 `deepseek`，于是每条都算偏离——基准语义**未决**，见下文「渠道观测 → 两个口径」；网关级渠道另由 `channel_log_enabled`（默认关闭）从 CPA 请求日志取，并与记录**读时合并**：页面的「真实渠道」表按网关维度、原始记录表分列「真实渠道 / 尝试 / CPA 凭据」，`off_baseline` 只对真渠道判定（没有渠道块的请求记「无渠道块」，不计入比例）；
- **请求路径零成本开关**：除 `ManagementAPI` 外只声明 `usage_plugin`，且**只在 `channel_observe_enabled: true` 时声明**；关闭时能力为 nil，宿主不注册 usage 适配层，请求路径上没有任何插件代码在跑（不拦分片、不走 ABI、不做探针）；
- **不改写、不阻塞任何请求**：不声明任何 translator / normalizer / 请求侧或响应侧拦截能力，不 clone、不改写请求或响应，不干预上游路由与固定；对每次 usage 回调只回一个「不改变」信封 `{"ok":true,"result":{}}`，回调发生在请求**结束之后**，与请求处理无关；
- **fail-open**：配置解析失败时回落到默认值，插件照常加载并照常提供套餐视图；观测层自身的错误（载荷解不开、队列满、目录不可写）也只在插件内部消化，宿主请求流程完全不受影响。

## 环境要求

| 项 | 要求 |
|---|---|
| CPA | **≥ v7.3.8**，且构建带插件支持（响应头 `X-Cpa-Support-Plugin: 1`）。官方带 CGO 的 Linux 构建才有插件支持 |
| 插件 ABI | `abi_version = 1` |
| 插件 schema | `schema_version = 6` |
| 平台 | `linux/amd64`、`linux/arm64` |
| 外网 | 需要能访问 Cline 的 API（默认 `https://api.cline.bot/api/v1`）。插件只读套餐与用量，不代理任何流量 |

> 版本兼容声明：本插件按 CPA v7.3.8 的 SDK 契约开发（`go.mod` 依赖 `CLIProxyAPI/v7 v7.3.8`），宿主侧源码（`internal/pluginhost`、`sdk/*`）的核对用的是本仓库 `.reference/CLIProxyAPI` 的 **v8.0.8** 检出；v0.2.0 的加载与热重载另在 **v8.0.4** 宿主上实测通过；v0.3.0 的渠道观测（usage 钩子，能力键 `usage_plugin`）在**生产宿主**上完成验收（插件构建 `0.3.0-dev.173`，2026-10-02；该生产宿主是 CPA **v8.0.8**，commit `fd48ea6`，build 2026-10-01，容器 `eceasy/cli-proxy-api:latest`，验收结果见 [docs/channel-observation.md](docs/channel-observation.md) §6）。CPA 大版本升级后请回到本文「排障」一节按表自查。

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
      channel_observe_enabled: true    # 观测总开关；false 时不声明 usage 钩子，请求路径零开销
      channel_store_dir: "/CLIProxyAPI/logs/channel-observation"   # JSONL 目录，按 UTC 日一天一个文件
      channel_retention_days: 3        # 保留天数（上限 30）
      channel_max_size_mb: 512         # 目录总大小上限（下限 16），超出先从最旧的文件删
      channel_baseline_provider: "deepseek"   # 基准渠道名：渠道不等于它的请求算「未落在基准渠道」
      # ---- CPA 请求日志渠道补全（默认关闭；需 CPA 侧同时打开请求日志）----
      channel_log_enabled: false       # 扫描 CPA 请求日志，取网关级 finalProvider；源日志含明文 prompt
      channel_log_dir: "/CLIProxyAPI/logs"          # CPA 请求日志目录（容器内路径；宿主是 /opt/cpa/logs）
      channel_log_delete_after_read: true           # fact 落盘后 unlink 源日志
      channel_log_min_age_seconds: 5                # 只读 mtime 早于该秒数的文件，避免读到半个请求
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
| `channel_observe_enabled` | `true` | 渠道观测总开关。关闭时插件**不声明** `usage_plugin` 能力，宿主不注册 usage 适配层，请求路径上没有任何插件开销，页面「渠道」区显示「渠道观测未开启」 |
| `channel_store_dir` | `/CLIProxyAPI/logs/channel-observation` | 渠道记录的 JSONL 目录，按 UTC 日一个文件 `channel-<YYYY-MM-DD>.jsonl` |
| `channel_retention_days` | `3` | 保留天数，上限 30（写更大也会被夹到 30） |
| `channel_max_size_mb` | `512` | 该目录的总大小上限（MB），下限 16。目录同时受天数与体积约束：**先按天数删旧文件，再按体积从最旧删到限额** |
| `channel_baseline_provider` | `deepseek` | 基准渠道名。记录的 `final_provider`（与 `resolved_provider` 同值）与它**不区分大小写**地不等即算「未落在基准渠道」，判定发生在**读时**。注意记录里的 `final_provider` 当前装的是 **CPA 凭据名**（`openai-compatible-cline1/2/3`）、不是网关渠道，与默认基准 `deepseek` 不可能相等，于是窗口内每一条都算偏离；基准该怎么定还没决定，见「渠道观测 → 两个口径」 |
| `channel_log_enabled` | `false` | 扫描 **CPA 请求日志**（默认关闭）。开启后插件在旁路轮询 `channel_log_dir`，从上游响应原文里取网关级 `finalProvider` / `resolvedProvider` / 尝试次数 / 网关成本，写成 fact 落进 `channel_store_dir`。默认关闭是刻意的：源日志含客户端**明文 prompt**，且只有 CPA 侧同时打开请求日志（`observability.logs.request-log: true` 且 `server.commercial-mode: false`，**要重启容器**）才会写文件。实测代价与体积见 [docs/channel-observation.md](docs/channel-observation.md) §4.2 / §4.3 |
| `channel_log_dir` | `/CLIProxyAPI/logs` | CPA 请求日志目录（容器内路径；宿主是 `/opt/cpa/logs`）。只扫描该目录**顶层**的 `*.log`，不递归子目录，且跳过 `main.log` |
| `channel_log_delete_after_read` | `true` | fact 落盘后 unlink 源日志。解析失败或落盘失败的文件不删，计数进 `/health` 的 `channel_log` |
| `channel_log_min_age_seconds` | `5` | 只读 mtime 早于该秒数的日志文件，避免读到 CPA 正在写的半个请求；读前读后都比对文件大小，变大的留到下一轮 |

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

**官方记录里有什么、没有什么**：每条记录带 `aiInferenceProviderName`（**上游推理渠道**，生产实测恒为 `vercel`）与 `metadata.raw_model`（真正跑的模型，如 `deepseek/deepseek-v4.1-flash`），但不带延时、TTFT、生成速度与失败状态码。v0.3.0 用宿主的 usage 回调补回来的是 **CPA 侧凭据**（`Provider` / `AuthID` / `AuthIndex` / `AuthType`）与宿主自己测的 TTFT / 总耗时，**不是** Cline 网关级的 `finalProvider`（网关内部最终选了哪个上游，例如 `deepseek` 之外的回退目标）——后者在**上游** `chat/completions` 响应体的 `provider_metadata.gateway.routing` 里，CPA 把它翻译成 Responses 事件时丢掉了，只有 CPA 请求日志还留着它（`channel_log_enabled`，默认关闭，见下文「渠道观测」）。于是页面上有**两个并列的渠道维度**：CPA 凭据（来自插件记录）与上游推理渠道（来自官方记录）。

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

这一块回答一个问题：**过去 24 小时里，clinepass 的请求有多少条、多大比例没有落在基准渠道**，以及这些请求的 TTFT 与解码速度是多少。数据是**全量采集**：宿主每完成一个请求回调插件一次，一条记录进 JSONL，比例是精确值、不是抽样估计。

### 数据从哪来

渠道来自**宿主的 usage 钩子**，不是响应体。插件在注册里声明能力 JSON 键 `usage_plugin`（Go 字段 `UsagePlugin`；SDK 常量 `pluginabi.MethodUsageHandle`，方法名 `usage.handle`），宿主在**每个请求结束后**调用一次，载荷是 SDK 的 `UsageRecord`：

- **只在 `channel_observe_enabled: true` 时声明**。关闭时能力为 nil，宿主不注册 usage 适配层，每个完成的请求都不会发起插件 ABI 调用——这是「观测关闭」等价于「没装插件」的原因；
- **与客户端协议无关**：`/v1/chat/completions`、`/v1/responses` 以及其它 wire protocol 都走同一次回调，所以 `POST /v1/responses` 的流量现在能记账，而旧设计在这类流量上一无所获；
- 载荷字段（SDK 结构体没有 json tag，**JSON 键就是 Go 字段名**）：`Provider`、`BaseURL`、`ExecutorType`、`Model`、`Alias`、`APIKey`、`RequestID`、`TraceID`、`SessionID`、`ParentSessionID`、`AuthID`、`AuthIndex`、`AuthType`、`Source`、`ReasoningEffort`、`ServiceTier`、`ResponseServiceTier`、`ResponseModel`、`Generate`、`Stream`、`Failed`、`Failure{StatusCode,Body}`、`Detail{InputTokens,OutputTokens,ReasoningTokens,CachedTokens,CacheReadTokens,CacheCreationTokens,TotalTokens}`、`RequestedAt`（RFC3339）、`Latency`（整数纳秒）、`TTFT`（整数纳秒）；
- 插件**不落盘**载荷里的 `APIKey` / `Source`（都是凭据哈希）与 `BaseURL`；
- 回调是**只读**的：插件每次只回答「不改变」信封 `{"ok":true,"result":{}}`，不在请求或响应上写任何东西，也不参与请求处理。

**第二个来源（默认关闭）：CPA 请求日志。** 网关级的最终上游渠道（`provider_metadata.gateway.routing.finalProvider` / `resolvedProvider` / 尝试次数 / 网关成本）在**上游** `chat/completions` 响应里有，但 CPA 把它翻译成 Responses 事件时丢掉了，所以 usage 载荷里没有；唯一还留着它的地方是 **CPA 自己的请求日志**。把 `channel_log_enabled` 设为 `true` 后，插件在旁路每 2 s 轮询 `channel_log_dir`（只扫顶层 `*.log`、跳过 `main.log`）、逐帧 JSON 解码（**不做文本匹配**，正文里出现 `provider_metadata` 字样只会得到空渠道）、把结论写成 fact 追加到 `channel-log-<UTC 日期>.jsonl`，并在 fact 落盘后 unlink 源日志（`channel_log_delete_after_read`，默认 `true`——源文件含**明文 prompt**）。这条来源要求 CPA 侧同时打开请求日志（`observability.logs.request-log: true` 且 `server.commercial-mode: false`，**改完要重启容器**，reload 不够），代价见 [docs/channel-observation.md](docs/channel-observation.md) §4.2 / §4.3。

**两个维度（必须一起理解）**：页面把两者分列，不能互相替代——「CPA 凭据」是 `Provider` 给出的凭据名（例如 `openai-compatible-cline2`）加 `AuthID` / `AuthIndex` / `AuthType`（记录键 `cpa_provider`）；「真实渠道」是 Cline 网关实际服务的渠道 `finalProvider`（记录键 `final_provider` / `gateway_resolved_provider` / `gateway_slug` / `gateway_attempts`，`channel_source: "log"` 标明来自请求日志）。记录与 fact 的 join 有两条路：先按日志 `Session_id: session-<uuid>` 与记录 `session_id` 里的同一个 uuid（配合 2 秒窗口）；没有该头时回退到「2 秒窗口 + `prompt_tokens`/`completion_tokens` 精确相等 + 候选唯一」，两条都匹配不上就留空（页面显示「无渠道块」，不计入偏离比例）。

### 数据存在哪、留多久

- 目录 `channel_store_dir`（默认 `/CLIProxyAPI/logs/channel-observation`），按 **UTC 日**一天一个文件 `channel-<YYYY-MM-DD>.jsonl`，一行一条记录，`v` 是 schema 版本（当前 `2`）；不是本插件命名的文件**绝不删除**；
- **`channel_log_enabled: true` 时**，同一目录下另有 fact 文件 `channel-log-<UTC 日期>.jsonl`（一行一条 fact，前缀刻意与记录文件不同，好让记录 reader 主动跳过它）；它按 `channel_retention_days` 同一个时钟过期，源日志则在 fact 落盘后被 unlink（`channel_log_delete_after_read`）；
- v2 写入的键：`v, time, request_id, session_id, model, alias, upstream_model, canonical_slug, final_provider, resolved_provider, auth_id, auth_index, auth_type, executor_type, reasoning_effort, service_tier, stream, failed, status_code, ttft_ms, duration_ms, decode_ms, tokens_per_second, input_tokens, output_tokens, reasoning_tokens, cached_tokens, cache_read_tokens, cache_creation_tokens, total_tokens`；
- 载荷 → 记录的关键映射（其余键同名直取）：

  | 记录键 | 来源 |
  |---|---|
  | `time` | `RequestedAt`（解析失败时用插件自己的时钟） |
  | `request_id` | `RequestID`，空时退回 `TraceID` |
  | `final_provider` / `resolved_provider` | 同值，都取 `Provider`（**CPA 为这条请求选中的凭据名，不是网关渠道**；Phase 3 计划改名为 `cpa_provider`，把 `final_provider` 让给网关值） |
  | `model` | `Model`，例如 `cline-pass/deepseek-v4.1-flash`（路由名） |
  | `upstream_model` / `canonical_slug` | 同值，都取 `ResponseModel`，例如 `deepseek/deepseek-v4.1-flash`（上游真实模型） |
  | `ttft_ms` | `TTFT` ÷ 1e6 |
  | `duration_ms` | `Latency` ÷ 1e6 |
  | `decode_ms` | `duration_ms − ttft_ms`，负数夹到 0（宿主两个计时独立测量） |
  | `tokens_per_second` | `output_tokens ÷ (decode_ms/1000)`，只在 `decode_ms ≥ 50` 且 `output_tokens > 0` 时有值 |
  | `status_code` | 宿主报失败时取 `Failure.StatusCode`，否则 `200` |

- **旧版本（v1）的行**：目录里可能还有旧设计（流式分片探针）写下的行，reader 仍然读，但 v2 从不写这些键——usage 载荷里没有对应字段：`generation_id, original_model_id, pinned_provider, affinity, upstream_request_id, fallbacks_available, model_attempt_count, total_provider_attempt_count, protocol, frames, cost_usd, is_byok, user_agent, claude_code_version, client_app, source_format`。这些行的 `v` 是 `1`，v2 新增的列在其上为空；
- 写入是异步的（内存队列 → 批量 flush），队列满只丢弃并计数，**绝不阻塞宿主**；
- 保留：先按 `channel_retention_days`（默认 3，上限 30）删旧文件，再按 `channel_max_size_mb`（默认 512）从最旧的文件删到限额；
- 聚合在内存里按小时分桶、覆盖**滚动的 7 天**，所以插件重启后近 24 小时的渠道视图仍在：启动时会回填最近 24 小时的 JSONL（超过 96 MB 就截断，`/health` 的 `warmup.truncated` 会标出来）；
- 目录不可写不致命：插件照常回包、照常在内存里聚合，失败暴露在 `/health` 的 `channel_observation.last_error` 与 `write_failures` 里。

### 两个口径

- **偏离基准渠道（off baseline）**：记录的 `final_provider` 与 `channel_baseline_provider` 不区分大小写地不等。判定发生在**读时**：记录里不落盘 `off_baseline`，所以改基准会立刻重算历史窗口的比例。页面上的基线名取自接口返回的 `baseline_provider`，页面里不硬编码渠道名。注意记录里的 `final_provider` 当前是 **CPA 凭据名**（`openai-compatible-cline*`），不是网关渠道；`channel_log` 的 fact 还没有并进记录，所以这个判定暂时落不到真渠道上；
- **分母就是窗口内的记录数**：每条完成的请求一条记录，没有「未识别」这一类，也没有 `unresolved` 计数器（那是旧设计的）；
- **失败（`failed`）**：宿主在 usage 载荷里判定的失败请求数，落在 `summary.failed_requests`，并按渠道（`providers[].failed`）与小时（`hours[].failed`）拆开。它是**窗口口径**，与 `/health` 里会随插件重启归零的 `failed_events` 不是一回事（页面的「失败 N 条（x%）」取窗口口径）。生产实测这些失败全部是上游 502，来自上游限流；官方用量接口**只记成功计费请求**，所以失败无法归因到上游推理渠道，只能按 CPA 凭据计数；
- **上游推理渠道（官方口径）**：`official_channels[]` 由官方 per-request usage 聚合而成，键是 `inference_provider`（`aiInferenceProviderName`）与 `model`（`metadata.raw_model`），带请求数、输入/输出/缓存 token 与成本。它按采集器的**保留窗口**统计，不随页面 1h/24h/7d 切换；官方接口本身也会限流（实测 `plan.usage.failures=1`、`error="upstream status 429"`），因此覆盖范围可能不足，页面在表下注明覆盖到哪一刻；
- **当前生产上的口径冲突（未决）**：配置的基准仍是 `deepseek`，而 CPA 侧的渠道名是 `openai-compatible-cline1/2/3`，两者不可能相等，于是窗口内每一条都算偏离——验收时 1 小时窗口 6 条记录全部 `off_baseline=yes`。这是配置语义问题，不是采集故障：基准该改成渠道名、还是换判定方式（例如按 `upstream_model` 重新定义「官渠」），**尚未决定**；在决定之前，页面上的偏离比例必然接近 100%；
- **TTFT 与解码速度**都取自宿主字段（`TTFT` / `Latency`，整数纳秒），不再由插件按帧时间戳推算。两者由宿主独立测量，所以 `decode_ms` 可能为负、记录里夹到 0；解码窗口短于 **50 ms** 时不记速度（`tokens_per_second` 为 0，也不进百分位），短窗口测到的是宿主批量投递而不是生成速度。宿主这两个计时的起点（是否含「客户端 → CPA 排队 → 连上游」）**本轮没有单独取证**；
- 窗口是**滚动窗口**：`24h` 指「此刻往前 24 小时」，起点落在小时中间，所以小时分布表会有 **25 个整点刻度**（首尾各覆盖部分小时；空小时显示 0，不插值）。

### 接口

管理密钥与既有接口相同（`Authorization: Bearer <management-key>`）：插件只声明 `/health`、`/channel`、`/channel.csv` 三条路由，CPA 只转发已声明的路径。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/v0/management/plugins/clinepass-channel-monitor/channel?window=1h\|24h\|7d` | 渠道视图 JSON：`enabled` 开关、`summary`（窗口 `from`/`to`、`baseline_provider`、窗口内请求数 `resolved_requests`、偏离数 `off_baseline_requests` 与比例 `off_baseline_ratio`、渠道数、TTFT p50/p90、解码 p50 tps、token 合计、按渠道 / 按模型 / 按小时的拆分 `providers` / `models` / `hours`），最新 20 条原始记录（`records`，另有 `records_total`）、官方上游渠道维度 `official_channels` / `official_usage`，以及 CPA 请求日志扫描器 `channel_log`（`enabled`、`delete_after_read`、`min_age_seconds`、`health`、最新 20 条事实 `facts` 与 `facts_total`）和 `health`。`window` 非法返回 **400 + `invalid_window`** |
| GET | `/v0/management/plugins/clinepass-channel-monitor/channel.csv?window=1h\|24h\|7d` | 同一窗口的 CSV 导出：一行一条记录，列序固定（见下），以附件形式下载（`Content-Disposition: attachment`），响应头 `X-Record-Count` 报行数，表头行恒在 |

CSV 表头（v2 记录填不满的列留空）：

```
time_utc,request_id,session_id,generation_id,model,alias,upstream_model,canonical_slug,original_model_id,final_provider,resolved_provider,pinned_provider,auth_id,auth_index,auth_type,executor_type,reasoning_effort,service_tier,upstream_request_id,off_baseline,status_code,failed,protocol,ttft_ms,duration_ms,decode_ms,tokens_per_second,input_tokens,output_tokens,reasoning_tokens,cached_tokens,cache_read_tokens,cache_creation_tokens,total_tokens
```

其中 `generation_id`、`original_model_id`、`pinned_provider`、`upstream_request_id`、`protocol` 是 v1 列的存留；`off_baseline` 取值是 `yes` 或空，按**读时**的当前基准算。

> 面板页面把这些数字画在**最后一个**区块「渠道」里：区块顺序固定为 `Cline 套餐用量（官方接口）` → `概览` → `官方用量明细` → `渠道`。渠道区内部依次是概览卡片、`真实渠道` 表（网关 `finalProvider`）、`上游推理渠道（官方 usage）` 表、时间线（卡片 + 小时表；小时表行序是**新 → 旧**，表头写着「时间（新 → 旧）」，含 失败 列）、`原始记录` 表。成本列与成本卡片已随之移除；原始记录表显示 时间 / 模型 / 渠道 / 凭据（认证 ID + 索引）/ 失败 / TTFT / 解码 t/s / 输入-输出-缓存读 / 状态 / 会话。右上角是「导出 CSV」按钮。

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
| GET | `/v0/management/plugins/clinepass-channel-monitor/channel?window=1h\|24h\|7d` | 渠道视图（v0.3.0）：偏离基准渠道的条数与比例、按渠道 / 按模型 / 按小时的拆分、最新 20 条原始记录。`window` 非法返回 400 + `invalid_window` |
| GET | `/v0/management/plugins/clinepass-channel-monitor/channel.csv?window=1h\|24h\|7d` | 同一窗口的 CSV 导出（v0.3.0）：一行一条记录、含 `off_baseline` 列，附件下载，`X-Record-Count` 报行数 |

页面里除了套餐区还有「渠道」区，且**排在最后**（顺序：`Cline 套餐用量（官方接口）` → `概览` → `官方用量明细` → `渠道`）；渠道区内部依次是概览卡片、`真实渠道` 表、`上游推理渠道（官方 usage）` 表、时间线（卡片 + 小时表，小时表新 → 旧，含失败列）、`原始记录` 表和「导出 CSV」按钮。

v0.1.x 的 `/stats`、`/events`、`/export` 三条路由已移除，请求它们返回 404（属预期）。注意**插件只声明 `/health`、`/channel`、`/channel.csv`**：CPA 只转发已声明的路径，所以一个旧版本生成的页面 / 接口清单里不会有后两条，请求它们就是 404（见「排障」）。

## `health` 字段说明

| 字段 | 含义 |
|---|---|
| `plugin` / `version` / `enabled` / `uptime` | 插件标识、版本、配置里的启用开关与本次加载后的运行时长 |
| `plan` | 完整套餐快照，页面直接渲染它：`available`、`source`、`account`、`plan_name`、`plan_price`、`plan_description`、`plan_interval` / `plan_type` / `plan_active`、`plan_benefits[]`、`plan_period_start` / `plan_period_end` / `plan_canceled_at`、`limits[]`（`percent_used` / `resets_at` / `resets_in`）、`tokens`（31 天输入/输出/总量、成本、余额、计费条目数）、`usage`、`fetched_at`、`error`、`accounts[]`；`accounts[].windows[1h\|24h\|7d]` 里另有 `models[]`（按模型拆分：`model` / `requests` / `input_tokens` / `output_tokens` / `cached_tokens` / `cache_ratio` / `cost_usd` / `credits_used`）、`stream_requests`、`byok_requests`、`credits_used` |
| `plan_enabled` | 官方套餐轮询是否开启 |
| `plan_usage` | 官方逐条用量的采集状态：`enabled`、`items`、`oldest`、`fetched_at`、`truncated`、`failures`、`retry_at`、`error` |
| `plan_accounts` | 每个 Cline 凭据一行：`id`、`label`（掩码后的 key）、`source`（`plugin-config` / `config-file` / `host-auth`）、`available`、`rejected`、`account`、`items`、`oldest`、`truncated`、`failures`、`error` |
| `request_header_names` / `request_bearer_len` | 上一次请求路径上看到的 header 名与 bearer 长度。v0.2.0 起不声明任何**请求侧**能力（v0.3.0 只新增 usage 钩子与管理接口），所以**恒为空**；保留是因为它们从来只含 header 名与长度，不含凭据值 |
| `channel_observation` | v0.3.0 新增的渠道采集健康度，字段**恰好**是这些：`enabled`（观测总开关）、`directory`（JSONL 目录）、`events`（接受并处理的 usage 记录数）、`failed_events`（其中宿主标为失败的请求数）、`decode_failures`（载荷根本解不开的次数）、`dropped`（队列满丢弃）、`written`、`queued`、`write_failures`、`last_record_at`、`last_write_at`、`last_error` / `last_error_at`、`last_decode_error` / `last_decode_error_at`、`files`、`bytes`、`warmup{records,truncated}`（启动回填近 24 小时 JSONL 的结果，回填超过 96 MB 时 `truncated: true`） |
| `channel_observation.events` / `failed_events` / `decode_failures` | `events` 是采集到多少条请求（每个完成的请求一次回调，与协议无关，应当跟着真实流量涨）；`failed_events` 是其中宿主报失败的条数（正常会随上游报错起伏）；`decode_failures` 是**载荷解不开**的次数（异常信号，应为 0，解不开时看 `last_decode_error`）。旧设计的 `resolved` / `unresolved` / `parse_failures` / `last_parse_error` / `needle_misses` / `pending_streams` 已随分片探针一起移除 |
| `channel_log` | CPA 请求日志扫描器（`channel_log_enabled`）：`enabled`、`delete_after_read`、`min_age_seconds`、`facts`（最新 20 条 fact，恒为数组）、`facts_total`、`health`。`health` 字段**恰好**是：`enabled`、`directory`、`scanned`、`parsed`、`with_channel`、`without_channel`、`parse_failures`、`deleted`、`deleted_bytes`、`skipped_young`、`skipped_growing`、`skipped_seen`、`skip_main_log`、`pruned`、`pruned_bytes`、`last_fact_at`、`last_error`、`last_error_at`、`pending_files`。fact 的键恒为：`time, path, method, session_id, session_uuid, has_session, final_provider, resolved_provider, canonical_slug, original_model_id, pinned_provider, affinity_outcome, model_attempt_count, total_provider_attempt_count, fallbacks_available, gateway_cost, frames, attempts_seen, had_error_response, source_file, parsed_at`；没有 routing 块的请求渠道字段留空而不是猜值 |

## 隐私

- 插件**只写渠道观测的 JSONL**（v0.3.0 起，默认 `/CLIProxyAPI/logs/channel-observation/channel-<YYYY-MM-DD>.jsonl`；`channel_log_enabled` 打开时另有 fact 文件 `channel-log-<UTC 日期>.jsonl`）；关掉 `channel_observe_enabled` 后不声明 usage 能力，`channel_log_enabled` 为 false 时也不访问 CPA 日志目录。v0.1.x 写的 `/opt/cpa/logs/channel-monitor/*.jsonl` 本版本不读也不写；
- **`channel_log_enabled: true` 时插件会读 CPA 自己的请求日志**，那些文件含客户端**明文 prompt**（CPA 侧写的，单个文件 16–47 KB（短请求）/ 4.0–5.7 MB（长上下文））。插件的处理是：解析后只把结论写进 fact（fact 里没有 prompt 正文），并在 fact 落盘后 unlink 源日志（`channel_log_delete_after_read` 默认 `true`）；解析失败或落盘失败的文件保留，不做删除；
- 除此之外不读、不写任何其他文件，不改写请求或响应，不克隆请求体；
- 渠道记录里不含凭据：usage 载荷里的 `APIKey` / `Source`（凭据哈希）和 `BaseURL` 一律不落盘；`AuthID` / `AuthIndex` 是宿主给出的凭据标识（形如 `openai-compatibility:cline2:bcaef0dbf8d3`），不是密钥本身；
- 记录与 fact 里不含 prompt、响应正文、下游 key（`channel_log_enabled` 时插件会**读取**含明文 prompt 的 CPA 日志文件，但只把结论写进 fact，见上一条）；
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
| 「渠道」区显示「渠道观测未开启」 | 采集器在配置里被关掉了（`channel_observe_enabled: false`），插件因此不声明 usage 钩子。这是开关状态，不是故障；要数据就把它设回 `true` 并热重载插件（注意：改这个键必须真的改变值才触发重载） |
| 「渠道」区一条记录都没有 / 比例的分母是 0 | 新口径下每条**完成的请求**都会记一条，所以先看 `/health` 的 `channel_observation.events`：① `events` 在涨而 `written` 不涨 → 写盘失败，看 `write_failures` / `last_error`；② `events` 完全不动 → 宿主没在调用插件，查 `plugins` 列表里本插件的 `registered` / `effective_enabled`，或观测被关掉了；③ `events` 在涨但当前窗口是空的 → 核对窗口起点，以及记录目录 `channel_store_dir` 是否被换过 |
| 目录不可写 / 记录数不涨 | 看 `/health` 的 `channel_observation.last_error` 与 `write_failures`：目录权限、挂载或磁盘满都会记在这里。这时插件照常回包、照常在内存里聚合，只是不落盘；修好目录后（或换 `channel_store_dir`）新记录会继续写，但内存聚合在插件重启后会丢失 |
| 窗口内**所有**请求都算偏离（页面偏离比例接近 100%） | 基准名与渠道名对不上：`channel_baseline_provider` 默认 `deepseek`，而 CPA 侧的渠道名是 `openai-compatible-cline1/2/3`，不区分大小写也不可能相等（验收时 1 小时窗口 6 条记录全部 `off_baseline=yes`）。这是配置语义问题、不是采集故障；基准该怎么定（改基准名、还是按 `upstream_model` 重新定义「官渠」）**尚未决定** |
| 记录里 `tokens_per_second` 为 0 | 解码窗口短于 50 ms 时不记速度（宿主对短回答批量投递，量出来的是投递不是生成）；这类记录不进解码速度百分位。`ttft_ms` 为 0 表示宿主这次没有报首 token 时间（例如非流式请求或请求失败） |
| 「上游推理渠道（官方 usage）」表为空 | 官方 per-request usage 还没取到：采集器每次刷新才填这张表，插件刚重启或官方接口回 429 时会是空的（页面写「官方用量记录还没有到」，不是 0）。看 `/health` 的 `plan.usage.failures` / `plan.usage.error`；接口限流会退避重试 |
| 官方表的覆盖范围比窗口短 | 表按采集器**保留窗口**（26 小时）统计，且官方接口有限流：实测 `items=800`、`oldest=04:59Z` 时 `truncated=true`。这是上游限额，不是插件故障；历史更长的区间要等采集器补齐或改用官方逐日汇总 |
| 上游返回 502，能否看出是哪个上游渠道 | **不能**。官方用量接口只记成功计费请求（实测 200 条里 0 条零 completion），失败不会出现在里面；CPA 的 usage 载荷里 `Failure.Body` 只有一句 `upstream stream returned an error payload`，不含上游渠道字样。上游错误原文（例如 `failed to generate stream from Vercel: … status 429 … Rate limit exceeded`）只在 CPA 自己的 `main.log` 里。渠道区只能按 CPA 凭据给出失败分布 |
| `POST /v1/responses` 的请求能看到吗 | 能。渠道取自宿主每个请求结束后的 usage 回调，与客户端协议无关；旧的流式分片设计在这里才是盲区，已退役。**网关级渠道**（`finalProvider`）要看 `channel_log_enabled`：它在上游响应里，只有 CPA 请求日志能看到（CPA 翻译成 Responses 时丢了它，clinepass 上游也没有 `/responses` 端点） |
| `channel_log` 一个 fact 都没有 / 目录里没有日志文件 | 按顺序查：① CPA 侧是否同时满足 `observability.logs.request-log: true` 与 `server.commercial-mode: false`（两个都满足才写文件）；② 改完是否**重启过容器**（实测 reload 成功也不出文件，见 [docs/channel-observation.md](docs/channel-observation.md) §2.2）；③ `logs-max-total-size-mb` 是否太小（默认 10 MB，与 `main.log` 共享，日志几秒内就被清理器删掉）；④ 插件侧 `channel_log_enabled` / `channel_log_dir` 是否指对；⑤ 文件 mtime 是否还在 `channel_log_min_age_seconds` 之内。健康度看 `/health` 的 `channel_log.health`（`scanned` / `parsed` / `parse_failures` / `skipped_young` / `last_error`） |
| 打开 `channel_log_enabled` 有没有代价 | 有，且是明确测过的：CPA 请求日志里是**明文 prompt**，单个长上下文请求 4.0–5.7 MB，实测目录增速约 8 MB/分钟、日增量约 3.5 GB；代价对照（解码 p50 −2.4%、CPU 中位 +0.35 个百分点、内存中位 +16 MiB）见 [docs/channel-observation.md](docs/channel-observation.md) §4.2 / §4.3。默认关闭 |
| 记录 / CSV 里有些列一直为空（`generation_id`、`pinned_provider`、`protocol`、`cost_usd` …） | 目录里仍有**旧版本（`v: 1`）写下的行**，CSV 也保留了这些 v1 列。usage 载荷里没有对应字段，所以 v2 不再写这些键；`v: 2` 的行这些列本来就是空的 |
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
3. **重载插件**：对插件配置做一次 `PATCH` 把 `enabled` 关掉再打开（`false` → 停 → `true`），让 CPA 重新注册这个插件。**配置 `PUT` 里值没变不会触发重载**，改配置值也算；
4. 确认：`GET /v0/management/plugins` 里本插件 `registered: true`、`effective_enabled: true`、版本是 `0.3.0`；
5. **最后**才删掉上一版本的 `.so`。文件名带版本号时（形如 `clinepass-channel-monitor-v<版本>.so`）新旧会同时被扫描到，但**不要在装新版本之前删掉正在运行的那个 `.so`**：实测先 `rm` 掉活动版本、再装新版本并 `PATCH enabled`，宿主会停在
   `registered:false, enabled:true, effective_enabled:false`（页面与 `/health` 一起 404，`main.log` 里连 `pluginhost:` 行都没有），连续 7 次 `enabled:true` 都救不回来，只有 `sudo docker restart cpa` 才恢复。
   正常流程下宿主自己会退休旧版本，`main.log` 会打印 `plugin hot reloaded … active_version=<新版本> retired_version=<旧版本> retired_path=<旧文件>`，看到这行再删旧文件最稳（2026-10-02 的部署实测就是这样：新 `.so` 装上、旧版本继续跑、热重载成功后 `active_version=0.3.0-dev.173 retired_version=0.3.0-dev.171`，旧文件在确认退休之后才删）。

**`{"status":"ok"}` 不代表加载成功**（实测踩过两次）：两次连续的 `enabled` PATCH 撞上配置重载时，第二次可能被内存态回写覆盖，插件停在 `registered:false`，页面与 `/health` 直接 404，而 `main.log` 里只有 200 的 PATCH 记录、**没有** `pluginhost: plugin loaded`。所以别信 PATCH 的回执，回读列表，是 `false` 就再发一次 `enabled:true`；热重载成功时 `main.log` 会打印 `pluginhost: plugin loaded / registered / hot reloaded … version=<新版本>`（进程启动时的那次加载不写这些行，判据一律以回读列表为准）。

**卡住了就重启容器**：上面那些 `registered:false` 的场景里，重复 PATCH 无效时用 `sudo docker restart cpa`（约 3 s 端口恢复）。重启后不需要再 PATCH，`plugins.configs.<id>.enabled: true` 会让宿主在启动时按目录重新扫描并加载。

**2026-10-02 第二轮部署教到的两条（都已复现）**：

1. **先确认同步的产物真的包含本次改动**：这一轮我复用了上一轮留下的同步包，构建出的 `.so` 不含新字段（回读 `/channel` 时缺 `failed_requests`），白跑了一轮部署。判据是编译输入哈希（`find internal cmd -type f \( -name '*.go' -o -name '*.html' \) | LC_ALL=C sort | xargs sha256sum | sha256sum`，`sort` 必须钉 `LC_ALL=C`）必须与本地一致；哈希没变就是没同步。
2. **回读要看两处，光看列表会骗人**：`GET /v0/management/plugins` 的 `path` 读的是磁盘上的文件，可能已经是新文件，而**已加载的实例仍是旧的**——这时 `/health` 里的 `version` 还是旧版本号（实测列表 `path` 指到 `0.3.0-dev.177`、`/health` 仍回 `0.3.0-dev.176`，且 `main.log` 没有对应的 `plugin hot reloaded` 行）。判据是 `plugins[].path` 的文件名版本 **与** `/health` 的 `version` 一致，两者不一致就 `sudo docker restart cpa`。

**怎么确认部署的 `.so` 就是当前源码构建的**：两边跑同一条命令再比哈希，注意 `sort` 必须钉住 locale，否则 BSD `sort`（macOS）与 glibc `sort`（服务器）的排序不同，会得到假的不一致：

```bash
find internal cmd -type f \( -name '*.go' -o -name '*.html' \) | LC_ALL=C sort | xargs sha256sum | sha256sum
# 编译输入一致即可认定同一个 .so；整树比对还要排掉本地新改、尚未同步的文件，例如 docs/、README.md
```

```bash
export CPA_MANAGEMENT_KEY='<your-management-key>'
curl -s -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" http://127.0.0.1:8317/v0/management/plugins
```

升级本身不动数据：v0.2.x → v0.3.0 只是多声明一个能力、多一个 JSONL 目录，配置块可以原样保留；v0.3.0 新增的 9 个 `channel_*` 键不写就用默认值（其中 `channel_log_enabled` 默认 `false`，不开就不访问 CPA 请求日志）。目录里由更早的流式分片设计写下的 `v: 1` 行不会被删，reader 仍然读它们（缺 v2 的字段，页面按空值显示）。

### 回滚

三种粒度，按需要选最轻的一种：

- **只是不想承担采集开销**：把 `channel_observe_enabled` 设为 `false` 并热重载。插件不再声明 `usage_plugin` 能力，宿主不注册 usage 适配层，**每个完成的请求不再有这一次 ABI 调用**；套餐、限额、官方用量视图完全不受影响，历史数据留在磁盘上，页面「渠道」区显示「渠道观测未开启」；
- **只想停掉请求日志那条来源**：把 `channel_log_enabled` 设为 `false`（scanner 停、不再访问 CPA 日志目录），并在 CPA 侧把 `request-log: false`、`logs-max-total-size-mb: 10`、`commercial-mode: true` 写回配置文件、**重启容器**。只关插件侧不会删掉盘上已有的请求日志，CPA 侧还开着就还会继续写；回滚脚本的 payload 必须是 `{"value": …}`（写成 `{"request-log": …}` 会静默无效，实测踩过，见 [docs/channel-observation.md](docs/channel-observation.md) §5.2）；
- **想回到 v0.2.x**：把上一版的 `.so` 装回去（删掉 v0.3.0 的 `.so`），按上面的方式重载插件并确认 `registered: true`、版本变回 `0.2.x`。回滚后渠道接口自然 404（旧构建没声明那条路由），磁盘上的 JSONL 不会被读也不会被删；
- **想整块停掉**：把插件配置里 `enabled` 设为 `false`。路由不再注册，页面与接口都不可用。

数据侧：渠道记录就是 `channel_store_dir` 下的 `channel-<YYYY-MM-DD>.jsonl`、fact 是同一目录下的 `channel-log-<UTC 日期>.jsonl`，**那整个目录可以直接删**（只删插件自己命名的文件；插件启动时会重建目录）。内存聚合不落盘，插件重启后会从 JSONL 回填最近 24 小时；旧设计的 `v: 1` 行夹在其中也照常回填，只是没有 v2 的字段。fact 不参与回填（它不并进记录），删掉只影响 `channel_log` 那一段的可见历史。

历史版本相关的几条：

- **从 v0.1.x 升级**：直接用新版本 `.so` 覆盖旧文件（文件名带版本号时删掉旧的），触发重扫，然后在 `plugins` 列表确认 `registered: true`、版本是 `0.3.0`。配置块可以原样保留：v0.1.x 的统计键会被忽略，页面与 `/health` 的套餐部分不变；
- **回滚到 v0.1.1**：把旧的 `.so` 装回去并触发重扫即可，配置块新旧版本都能读。注意回滚等于恢复逐帧开销——回滚前先确认性能可以接受；
- **历史 JSONL 不受影响**：v0.3.0 不读也不写 v0.1.x 写的 `/opt/cpa/logs/channel-monitor/*.jsonl`，需要清理的话自行删除；
- **卸载残留**：删掉插件配置块、`.so` 文件，以及（可选）`channel_store_dir` 目录。

## 目录结构

```
cmd/clinepass-channel-monitor/   入口：C ABI 的四个 //export 符号、信封编解码、panic 兜底
  cdecl.h                        C 侧类型声明（cgo 前置用）
cmd/channel-probe/               诊断探针（非发布产物）：把宿主交给插件的原始载荷落盘，用来确认渠道 / 凭据对插件是否可见
internal/abi/                    插件 ABI 信封
internal/buildinfo/              插件 id / 名称 / 作者 / 版本（版本由 -ldflags 注入）
internal/hostapi/                宿主回调桥：日志与 host.* 数据接口
internal/config/                 配置解析与归一化（plugins.configs.<id> 契约）
internal/plan/                   Cline 官方用量：套餐、限额、31 天汇总、逐条记录采集、凭据发现
internal/observation/            渠道观测（v0.3.0）：usage 载荷解析与记录映射、JSONL 存储与保留策略、内存小时桶聚合与 CSV 导出
internal/channellog/             CPA 请求日志 reader（v0.3.0）：轮询与 seen 集、日志分段与 SSE 帧 JSON 解码、fact 落盘与保留
internal/state/                  运行时状态（配置 / 用量轮询器 / 渠道存储）的发布与读取
internal/management/             管理接口与内嵌页面 index.html
internal/plugin/                 注册、生命周期与方法分发（把上面这些接起来）
```

依赖是单向的：`plugin → management / plan / observation`，`plan / observation / config` 是叶子包；`management` 与 `plugin` 通过 `state` 读取运行时状态，而 `state` 不反向依赖它们，所以没有任何 import 环。

## 性能

渠道观测的开销必须能被量出来、也必须能被关掉。**本节的两张三态表是为已被退役的流式分片设计（`response_stream_interceptor`）测的**，保留它们是为了留住当时的判定依据与测量方法；它们不代表现在这版实现的成本，不要拿这些数字当现版本的性能证据。

新设计的成本模型（定性，量化测量**还没做**）：

- 每个**完成的请求**一次 ABI 调用，插件侧一次 JSON 解码 + 一条记录构建；没有逐帧工作、不读响应字节、不克隆请求体、不做探针；
- 调用发生在请求**结束之后**，不在请求处理路径上，因此不参与任何请求的时延与吞吐；
- 观测关闭时不声明 `usage_plugin`，宿主不注册 usage 适配层，这次调用根本不存在——「关闭」仍然等价于「没装插件」；
- 要量新开销需要一套按**请求**计的基准；`scripts/bench_channel.py` 目前测的是流式请求的帧间隔 / 解码窗口 / CPA CPU（旧设计口径），不能直接用来量这次改动。新数字待测。

`channel_log_enabled` 那条来源有独立代价，已实测（2026-10-02，三阶段分钟采样）：源日志 16–47 KB（短请求）/ 4.0–5.7 MB（长上下文请求），目录增速约 8 MB/分钟、日增量约 3.5 GB；② 打开日志 vs ③ 关闭：解码 p50 **−2.4%**、TTFT p50 无可见惩罚、容器 CPU 中位 **+0.35 个百分点**（峰值 144.83% vs 48.67%）、内存中位 **+16 MiB**（峰值 202 vs 183 MiB）。这是一次数量级测量（① 只有 7 个样本、③ 的样本数未记录），不是基准；数字、读法与限制见 [docs/channel-observation.md](docs/channel-observation.md) §4.2 / §4.3。

以下表格标注为**历史（旧设计）**：

- **基线**：不装插件；
- **装了插件 + 观测关闭**：`channel_observe_enabled: false`（旧设计下不声明分片能力）；
- **装了插件 + 观测开启**：`channel_observe_enabled: true`（旧设计下每帧一次 `bytes.Contains`）。

### 测量方法（旧设计口径，保留）

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

### 三态对比（旧设计的历史数字）

> 下面全部是**旧设计**（逐帧探针）时期的实测，用来说明测量方法与当时的判定，不是现版本的性能证据。

顺序相位（先关后开各一轮）先给出 short 段 −6.9% 的吞吐差，但同一台机器的空闲 CPU 中位数会从 3.7% 漂到 46%，顺序相位把「机器变忙变闲」记到了被测变量头上，因此改用**交替 A/B**：每轮先关后开，共 5 轮，每臂长提示（227000 字符 / 1600 补齐 / 1 run）。第 5 轮的开启臂撞上上游 Vercel 429（CPA 返回 502 `stream_initialization_failed`，与本插件无关），故配对样本 n=4。

| 状态 | 解码速度（中位） | TTFT | 相对关闭 | 结论 |
|---|---|---|---|---|
| 基线（未装插件） | 151.47 t/s（2 run） | 1.878 s | — | 与下面两行分属不同时段，只当数量级看 |
| 装了插件 + 观测关闭 | **165.15 t/s**（5 run：161.30 / 162.35 / 165.15 / 166.50 / 167.23） | 配对基准 | — | 旧设计：`channel_observe_enabled: false`，不声明分片能力，请求路径零开销 |
| 装了插件 + 观测开启 | **161.69 t/s**（4 run：157.64 / 160.13 / 163.24 / 164.74） | — | 吞吐中位 **−1.88%**（均值 −1.56%，区间 −3.04%…+0.55%）；TTFT 中位 **+1.98%**（均值 −0.04%，区间 −7.68%…+3.58%）；端到端 +1.31% | 旧设计：`channel_observe_enabled: true`，每帧一次 `bytes.Contains`，命中才解 JSON |

> **旧设计的判定：通过，观测当时可以留在生产开启。** 观测开启相对关闭的吞吐中位差 −1.88%（比值 0.979），TTFT 中位差 +1.98%，都落在同一状态内 run 间 ±12% 的离散度之内，也都在验收阈值（吞吐 ≥ 95%、TTFT ≤ 110%）之内。该判定随流式分片设计一起退役，现版本的成本模型见本节开头。
>
> 读这份表必须一起看三件事：①同一状态内 run 间离散度就有 ±12%（关闭相位长提示单 run 曾见 147.22 t/s，而中位是 165.15），只能按「同样运行次数比中位数 + 配对差值」判读；②**本机的 CPU 增量测不出来**（容器共用，配对差从 −61 到 +34.6 个百分点），要 CPU 数字必须换独占环境重测；③每次 PATCH 重载都会让 `written` 等计数器归零，`warmup` 会从磁盘回填，这不是数据丢失。
>
> 旧设计被退役的过程与它暴露的两类边界（`/v1/responses` 永远不带 `provider_metadata`、模型正文里出现 `provider_metadata` 一词造成探针假阳性）见 [docs/channel-observation.md](docs/channel-observation.md) §2.3 与 §4.3。

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
