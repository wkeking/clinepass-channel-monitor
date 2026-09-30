# clinepass-channel-monitor

CLIProxyAPI (CPA) 插件：在管理中心展示 **Cline 订阅的套餐、限额与官方用量**，并自动从 CPA 自己的 Cline 凭据里发现 API Key。

> **v0.2.0 是一次职责收窄**：v0.1.x 的「逐请求渠道/用量/成本统计」（JSONL 落盘、渠道分布、明细表、CSV 导出、`/stats`、`/events`、`/export`）已经**整体移除**，本插件不再是请求路径上的插件。原因见下一节。历史 JSONL 文件不受影响，但插件不再写入新的文件。

## 为什么去掉逐请求统计

v0.1.x 注册了 `response_before_translator` 钩子。CPA 在每个流式帧都会调用它，而调用前宿主会把**整个客户端请求体**和**整个上游请求体**各 clone 一份、JSON 化后跨插件 ABI 传给插件。实测（同一 prompt，ctx≈56.7k token，输出约 1200 token）：

| 状态 | delta 间隔 p50 | 解码 t/s | cpa CPU |
|---|---|---|---|
| v0.1.1 插件开启 | 7–16 ms | ~100 | 106–192% |
| 插件关闭 | 0.0–0.1 ms | 357（官方同条件 345） | 0–7% |

插件自己在该钩子里只做一次 `bytes.Contains` + 一次 sha256（基准实测 sha256 只占 910,671 ns/op 里的 35,561 ns），**主要成本是宿主侧的载荷搬运**。CPA 是第三方开源项目，不能改它的源码让宿主只在首帧传完整请求体，所以唯一的解法是把插件从请求路径上完全摘掉：v0.2.0 只声明 `ManagementAPI`，不再声明任何请求/响应/用量能力。

代价只有一条：凭据发现的第 4 顺位（"最近一次被拦截请求上的 bearer"）没有了。前三个顺位（`plan_api_key` → `plan_config_path` 指向的 CPA `config.yaml` → 宿主 auth 回调）保持原样，实测部署走第 2 顺位即可，且第 4 顺位本来就基本无效——下游客户端给 CPA 的是 20 字符的 `sk-…`，会被 `looksLikeClineKey` 过滤掉。

## 能力一览

- 管理页展示：套餐名与月费、5 小时 / 每周 / 每月限额进度与重置时间、近 31 天官方 Token 总量（输入/输出）、参考成本、余额、官方计费条目数；
- 概览卡片（官方口径）：近 1 小时 / 近 24 小时的官方计费请求数、总 Token 数、缓存命中率，以及近 7 天的官方逐日汇总（只有 token 与成本）；官方记录覆盖不到窗口起点时页面会标注「官方数值偏低」；
- 多凭据分别轮询：一个 CPA 里配置多个 Cline 条目或多把 key 时，每把 key 一个账号卡，页面顶部出现账号下拉（≥2 个凭据时）；
- 凭据自动发现：读 CPA 自己的 `config.yaml`，通常不需要手填任何 key；key 只留在内存，不落盘、不打日志、不返回给页面；
- 自诊断：`/health` 暴露 `plan`（完整套餐快照）、`plan_usage`（官方逐条用量的采集状态）、`plan_accounts`（每个凭据的来源、账号、可用性与错误）；
- **不在请求路径上**：不声明任何请求/响应/用量能力，不 clone、不改写、不阻塞任何请求，因此对解码速度与宿主 CPU 零影响；
- **fail-open**：配置解析失败时回落到默认值，插件照常加载并照常提供套餐视图。

## 环境要求

| 项 | 要求 |
|---|---|
| CPA | **≥ v7.3.8**，且构建带插件支持（响应头 `X-Cpa-Support-Plugin: 1`）。官方带 CGO 的 Linux 构建才有插件支持 |
| 插件 ABI | `abi_version = 1` |
| 插件 schema | `schema_version = 6` |
| 平台 | `linux/amd64`、`linux/arm64` |
| 外网 | 需要能访问 Cline 的 API（默认 `https://api.cline.bot/api/v1`）。插件只读套餐与用量，不代理任何流量 |

> 版本兼容声明：本插件按 CPA v7.3.8 的 SDK 契约开发，已在 v7.3.10 上核对 `sdk/pluginapi`、`sdk/pluginabi`、`sdk/translator` 与插件宿主适配层均无差异；v0.2.0 的加载与热重载另在 **v8.0.4** 宿主上实测通过。CPA 大版本升级后请回到本文「排障」一节按表自查。

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
      plan_config_path: "/CLIProxyAPI/config.yaml"   # 容器内 CPA config.yaml 路径，用于读 Cline 凭据
      plan_refresh: 5m                 # 套餐与限额刷新间隔
```

改动配置后 CPA 会自动重扫并热加载插件（`reconfigure`）。

### 配置项说明

| 键 | 默认值 | 说明 |
|---|---|---|
| `enabled` | `true` | 关闭后不再注册路由、不再轮询官方用量 |
| `priority` | `1` | 插件优先级 |
| `hosts` | `["api.cline.bot"]` | **只用于凭据发现**：决定 CPA `openai-compatibility` 里哪些条目算 Cline 条目，进而取它们的 `api-keys` / `api-key-entries` 来轮询官方套餐。匹配规则：用 `net/url` 解析条目的 `base-url` 取 host 后**小写精确比较**；以 `.` 开头的项按**域名后缀**匹配（`".cline.bot"` 命中 `api.cline.bot`，不命中 `evil-cline.bot`）。显式留空 `[]` → 不按 host 匹配，只认条目名恰为 `Cline` 的条目 |
| `timezone` | `Asia/Shanghai` | 展示时区。v0.2.0 起页面按**浏览器本地时区**渲染官方接口返回的绝对时间，所以这个键被保留但**没有代码读取它**（原来的消费方是已移除的本地逐请求统计）；留着是为了兼容既有配置块 |
| `plan_config_path` | `/CLIProxyAPI/config.yaml` | 容器内 CPA 配置文件路径。Cline 的 key 通常以 `openai-compatibility[].api-key-entries[].api-key` 存在这里 |
| `plan_refresh` | `5m` | 套餐、限额与官方用量的轮询周期（最小 1 分钟） |

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

**多个 Cline 条目 / 多把 key**：官方套餐与限额是**按账号**算的，所以插件把每把 key 当成一个账号分别轮询：

- 发现范围：所有 base-url 命中 `hosts` 的 `openai-compatibility` 条目，以及名称恰为 `Cline` 的条目，取它们的 `api-keys` 与 `api-key-entries`（同一个 key 出现在多处只算一次，最多 8 个）；
- 每把 key 一个账号卡：套餐、5 小时/周/月限额、31 天汇总都是各算各的；页面顶部出现**账号下拉**（≥2 个凭据时），选择记在浏览器里；
- 同一账号的两把 key（`/users/me` 返回同一个 user id）会被合并成一条，避免对同一个账号重复拉取；
- 凭据列表每个轮询周期重新解析，所以在 CPA 里新增/删除 Cline key 后不用重启插件；
- 页面标签只显示「条目名 #序号 · sk_…尾4位」，账号显示为 `usr-xxxx…xxxx`，**不会出现完整 key**；
- 被上游拒绝的凭据（401/403，例如误把下游客户端 key 当成 Cline key）不进账号下拉，只在 `/health` 的 `plan_accounts` 里保留（带 `rejected: true`）；
- 上游调用量按账号叠加：账号之间串行并间隔 0.5s，每个账号有自己的 26 小时保留窗口、分页预算与退避。

**凭据发现顺序**：CPA `config.yaml` 里的 `openai-compatibility[].api-keys` / `api-key-entries` → CPA 凭据接口（`host.auth.list` / `host.auth.get`）。多数部署走到第一步就够了：CPA 会把你在供应商配置里填的 key 持久化到 `openai-compatibility[].api-key-entries[].api-key`，不需要手填任何 key（少一份密钥副本）；只有把配置文件放到容器外读不到时才需要手工写 `plan_api_key`。key 只留在内存，不落盘、不打日志、不返回给页面。

**上游调用量与限流**：逐条明细接口不能按时间过滤，窗口内有多少条记录就要翻多少页（实测近 24 小时约 3500 条 ≈ 17 页）。因此插件在内存里保留最近 **26 小时**的记录，稳态下每次只翻到已见过的记录为止（通常 1 页）；首次回填或覆盖不足时按 300ms/页 节流，最多 60 页，遇到 429 等错误会指数退避（上限 30 分钟），所以覆盖范围会在几个刷新周期内长满，而不是一次打满。

插件重载后需要重新回填；采集状态（条数、覆盖起点、是否截断、失败次数、下次重试时间）见 `/health` 的 `plan_usage`。逐条用量的增量拉取默认每 **10 分钟**一次（`plan_usage_refresh`，只有大于 `plan_refresh` 的 5 分钟轮询周期时才起作用），官方 Token 总量与余额最多每小时一次。要彻底停掉这部分上游调用，只能手工在插件配置块里写 `plan_usage_enabled: false` 或 `plan_enabled: false`——这两个键已不在配置面板里。

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
| GET | `/v0/management/plugins/clinepass-channel-monitor/health` | 完整套餐快照 `plan`（账号、限额、31 天汇总、官方窗口），`plan_usage`（官方逐条用量的采集状态），`plan_accounts`（每个 Cline 凭据的标签、账号、可用性与错误） |

v0.1.x 的 `/stats`、`/events`、`/export` 三条路由已移除，请求它们返回 404（属预期）。

## `health` 字段说明

| 字段 | 含义 |
|---|---|
| `plugin` / `version` / `enabled` / `uptime` | 插件标识、版本、配置里的启用开关与本次加载后的运行时长 |
| `plan` | 完整套餐快照，页面直接渲染它：`available`、`source`、`account`、`plan_name`、`plan_price`、`limits[]`（`percent_used` / `resets_at` / `resets_in`）、`tokens`（31 天输入/输出/总量、成本、余额、计费条目数）、`usage`、`fetched_at`、`error`、`accounts[]` |
| `plan_enabled` | 官方套餐轮询是否开启 |
| `plan_usage` | 官方逐条用量的采集状态：`enabled`、`items`、`oldest`、`fetched_at`、`truncated`、`failures`、`retry_at`、`error` |
| `plan_accounts` | 每个 Cline 凭据一行：`id`、`label`（掩码后的 key）、`source`（`config-file` / `host-auth`）、`available`、`rejected`、`account`、`items`、`oldest`、`truncated`、`failures`、`error` |
| `request_header_names` / `request_bearer_len` | 上一次请求路径上看到的 header 名与 bearer 长度。v0.2.0 不声明任何请求能力，所以**恒为空**；保留是因为它们从来只含 header 名与长度，不含凭据值 |

## 隐私

- 插件**不落任何文件**：v0.2.0 没有本地存储，历史 JSONL 由 v0.1.x 写入，本版本不读也不写；
- 不记录 prompt、响应正文、下游 key；
- 插件**不会**打印或返回 CPA 管理密钥、上游 API key 或 auth 文件内容；发现的 Cline key 只留在内存，页面与 `/health` 里只出现掩码（`sk_…尾4位`）与 key 派生的稳定 id；
- 数据只出现在一个地方：鉴权过的管理接口 `/health`。资源页面路由是静态壳，**不含任何数据**；
- 仓库里不出现任何真实凭据或抓包标识：`scripts/check-secrets.sh` 扫描工作区**和整个 git 历史**，只放行 `sk-TESTKEY…` / `gen_FIXTURE…` / `fp_fixture…` / `codex-fixture…` 这类明显合成的值，CI 每次推送都会跑一遍。测试里要造 key 就用这些前缀，别用真 key 的前几位。

## 排障

| 现象 | 可能原因与处理 |
|---|---|
| `GET /v0/management/plugins` 里本插件 `registered: false`、`path: ""` | `.so` 没被扫描到：确认文件名是 `clinepass-channel-monitor.so` 或 `clinepass-channel-monitor-v<version>.so`，且位于 `<plugins.dir>/<goos>/<goarch>/` 或 `<plugins.dir>/` 下；确认 CPA 配置里 `plugins.enabled: true`，且 `plugins.configs` 的键名与插件 id 完全一致 |
| 加载失败、日志提示 ABI 不符 | 需要 CPA ≥ v7.3.8 的**带插件支持**构建（`X-Cpa-Support-Plugin: 1`）；插件声明 `abi_version = 1`、`schema_version = 6` |
| 插件在 `plugins` 列表里但页面 404 | 检查 CPA 版本是否满足；改一次配置触发重扫；确认资源路由路径为 `/v0/resource/plugins/clinepass-channel-monitor/index.html` |
| 页面能开但一直空 | 页面里的管理密钥没填或填错（管理接口会返回 401/403）；或 `plan_accounts` 为空（凭据没被发现） |
| 套餐卡片提示「插件拿不到 Cline API Key」 | 插件读的是 CPA 自己的 Cline 凭据：确认 CPA 里有指向 `api.cline.bot` 的 `openai-compatibility` 条目（或条目名恰为 `Cline`），且 `plan_config_path` 指向容器内可读的 `config.yaml`；`/health` 的 `plan_accounts` 会列出每个凭据的来源、可用性与错误 |
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

- **从 v0.1.x 升级**：用新版本 `.so` 覆盖旧文件（文件名带版本号时删掉旧的），改一次 CPA 配置触发重扫，然后在 `plugins` 列表确认 `registered: true`、版本是 `0.2.0`。配置块可以原样保留：v0.1.x 的统计键会被忽略，页面与 `/health` 的套餐部分不变；
- **回滚到 v0.1.1**：把旧的 `.so` 装回去并触发重扫即可，配置块新旧版本都能读。注意回滚等于恢复逐帧开销——回滚前先确认性能可以接受；
- **历史 JSONL 不受影响**：v0.2.0 不读也不写 `/opt/cpa/logs/channel-monitor/*.jsonl`，需要清理的话自行删除；
- **卸载残留**：插件本身不在 CPA 配置之外写任何文件。需要彻底清理时删除：① 插件配置块，② `.so` 文件。

## 目录结构

```
cmd/clinepass-channel-monitor/   入口：C ABI 的四个 //export 符号、信封编解码、panic 兜底
  cdecl.h                        C 侧类型声明（cgo 前置用）
internal/abi/                    插件 ABI 信封
internal/buildinfo/              插件 id / 名称 / 作者 / 版本（版本由 -ldflags 注入）
internal/hostapi/                宿主回调桥：日志与 host.* 数据接口
internal/config/                 配置解析与归一化（plugins.configs.<id> 契约）
internal/plan/                   Cline 官方用量：套餐、限额、31 天汇总、逐条记录采集、凭据发现
internal/state/                  运行时状态（配置 / 用量轮询器）的发布与读取
internal/management/             管理接口与内嵌页面 index.html
internal/plugin/                 注册、生命周期与方法分发（把上面这些接起来）
```

依赖是单向的：`plugin → management / plan → config`；`management` 与 `plugin` 通过 `state` 读取运行时状态，而 `state` 不反向依赖它们，所以没有任何 import 环。

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

**版本只有一个来源**：`internal/buildinfo/buildinfo.go` 里的 `Version`。release tag 必须是 `v<version>`（例如 `v0.2.0`），本地 `make build` 自动加 `-dev.N` 后缀，所以每次构建的版本都不同、CPA 一定会热加载新库而不是继续用旧的。

**本地打包**（产物落在 `release/`，已 gitignore）：

```bash
make tools                               # 固定版本 Go 工具链，无需 root
scripts/release.sh 0.2.0                 # 只打本机平台
scripts/release.sh 0.2.0 linux/arm64 linux/amd64
```

脚本按插件商店的硬性规则产出并自检：`release/clinepass-channel-monitor_<version>_<goos>_<goarch>.zip`（zip 根目录里只有 `clinepass-channel-monitor.so`）、`release/checksums.txt`（sha256sum 格式）。CGO 交叉编译需要目标平台的 C 工具链：Linux 上会识别 `x86_64-linux-gnu-gcc` / `aarch64-linux-gnu-gcc`，windows/amd64 识别 `x86_64-w64-mingw32-gcc`（`gcc-mingw-w64-x86-64`），没有就直接报错让你装；darwin 产物必须在 macOS 上构建（CGO 链接要用 macOS SDK）。

**CI 发布**：推 tag 触发 `.github/workflows/release.yml`，五平台矩阵（`linux/amd64`、`linux/arm64`、`windows/amd64`、`darwin/arm64`、`darwin/amd64`）各自构建、合并 `checksums.txt`、创建 GitHub Release。插件商店的审核要求这五个平台齐全（缺 `darwin_amd64` 或 `windows_amd64` 会被判 Platform Support Violation），所以矩阵不要再删项：

```bash
git tag v0.2.0 && git push origin v0.2.0
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
  "http://127.0.0.1:8317/v0/management/plugin-store/clinepass-channel-monitor/install?version=0.2.0"
```

也可以在管理中心的「插件商店」里点安装。装好的库落在 `<plugins.dir>/<goos>/<goarch>/clinepass-channel-monitor-v<version>.so`，之后还需要 `plugins.configs.clinepass-channel-monitor` 配置块并触发一次重载才会生效。覆盖当前正在加载的同版本文件会被 CPA 拒绝（`ErrLoadedPluginLocked`）：要么发一个新版本号，要么先重启 CPA。

两个容易踩的点：① tag 带 `v`、资产名不带 `v`，CPA 逐字符匹配资产名（`<id>_<version>_<goos>_<goarch>.zip`），少一个下划线就是 `release asset ... not found`；② 同一个插件 id 同时出现在多个源时，手工安装的副本会变成 `unknown` 来源、不再提示更新——自建源和官方商店只登记一处，或统一走商店安装。

## 许可证

MIT，见 [LICENSE](LICENSE)。
