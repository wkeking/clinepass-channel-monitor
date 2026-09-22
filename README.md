# clinepass-channel-monitor

CLIProxyAPI (CPA) 插件：逐请求记录 **Cline 订阅实际服务的上游渠道**，并把用量、成本、缓存命中一起落到本地 JSONL + 内存环形缓冲，在 CPA 管理中心提供一个自带页面查看。

## 它解决什么问题

Cline 的请求打到网关后，由 **Cline 自己**决定这次请求最终落到哪个后端渠道（`deepseek`、`alibaba`……）。这个决策不能靠客户端参数钉住（`payload.params.providerOptions.gateway.only` 已失效），但响应体里仍然带着网关给的渠道元数据。

问题在于：`/v1/responses` 这类端点在 CPA 内部会把上游 openai 协议**翻译**成客户端协议，翻译过程中 `provider_metadata` 被丢弃，普通的响应拦截钩子（`response.intercept_*`）拿到的是翻译后的 body，什么都看不到。

本插件使用 CPA 的 **`response.normalize_before`**（能力 `response_before_translator`）钩子，运行在「翻译之前」，因此对 `/v1/chat/completions`、`/v1/responses` 等端点都能拿到原始的 `provider_metadata.gateway.routing.finalProvider`，再用 `usage.handle` 钩子拿到用量记录，两者关联成一行落盘。

渠道列取值的顺序是 **`finalProvider` → 上游响应的 `provider` 字段 → 空**。走 Cline 自己网关的模型（如 `cline-pass/deepseek-v4.1-flash`、`cline-pass/kimi-k3`）响应里带 `provider_metadata.gateway.routing`；走 OpenRouter 一类后端的模型（如 `cline-pass/glm-5.3-flash`）不带它，只给一个 `provider`（`Relace`、`Photon`、`CoreWeave`……），这些请求同样记录、同样进渠道分布，只是值来自另一个字段。两个字段都没有时该行渠道列留空，页面显示 `—`。**host 命中 `hosts` 就落一行**，不要求必须有渠道证据（`require_routing_marker: true` 可以把记录收窄回严格口径）。

## 能力一览

- 逐请求记录最终渠道：`final_provider`（优先 `finalProvider`，缺失时用上游响应的 `provider` 字段）/ `resolved_provider` / `canonical_slug` / 尝试次数 / 兜底候选数量；
- 记录上游给出的实际成本：`gateway.cost` / `inputInferenceCost` / `outputInferenceCost` / `generationId`；
- 记录 token 与缓存：`input/output/reasoning/total_tokens`、`cached_tokens`、`prompt_cache_hit_tokens` / `prompt_cache_miss_tokens`、`systemFingerprint`；
- 按天切分 JSONL 落盘（默认开启）+ 内存环形缓冲供页面即时查询；
- 管理中心页面：固定五个一行的概览卡片（请求数 / 平均延时 / 生成速度 / 总 Token 数 / 缓存命中率）+ 渠道分布 + 可过滤分页明细表 + CSV 导出；
- **可选接入 Cline 官方用量**：套餐名与月费、5 小时/周/月限额进度、官方 Token 总量/成本/余额，并把概览的请求数、总 Token 数、缓存命中率切到官方口径（延时与生成速度仍为本机口径，官方接口没有这两项）；
- 自诊断：`health` 暴露命中/未命中/解析失败/未关联/orphan/写盘错误等计数器，以及「带渠道证据但 host 不匹配」的样本，避免静默失效；
- **fail-open**：观测钩子永远返回空 body（宿主视为「不修改」），任何解析或写盘异常都不改变响应字节、状态码与时序。

## 环境要求

| 项 | 要求 |
|---|---|
| CPA | **≥ v7.3.8**，且构建带插件支持（响应头 `X-Cpa-Support-Plugin: 1`）。官方带 CGO 的 Linux 构建才有插件支持 |
| 插件 ABI | `abi_version = 1` |
| 插件 schema | `schema_version = 6` |
| 平台 | `linux/amd64`、`linux/arm64` |
| 上游 | 响应里带 `provider_metadata.gateway.routing` 或 `provider` 字段时渠道列有值；两者都没有（失败请求常见）时渠道列留空，页面显示 `—` |

> 版本兼容声明：本插件按 CPA v7.3.8 的 SDK 契约开发，已在 v7.3.10 上核对 `sdk/pluginapi`、`sdk/pluginabi`、`sdk/translator` 与插件宿主适配层均无差异。CPA 大版本升级后请回到本文「排障」一节按表自查。

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
      # ---- 判定规则 ----
      hosts: ["api.cline.bot"]         # 唯一的判据：usage 记录 BaseURL 的 host（支持 ".cline.bot" 后缀写法）
      require_routing_marker: false    # 严格模式（默认关）：开启后只记录响应带 provider_metadata.gateway.routing 的请求
      unmatched_host_samples: 20       # host 未命中而跳过的样本保留条数
      # ---- 存储 ----
      ring_size: 5000                  # 内存环形缓冲条数（页面即时查询用）
      jsonl_enabled: true              # 默认开启 JSONL 落盘
      jsonl_dir: "/var/log/clinepass-channel-monitor"   # 按你的部署改成可写目录
      retention_days: 30               # 过期 JSONL 自动删除
      # ---- 关联 ----
      join_window: 5s                  # 渠道记录与用量记录的关联时间窗
      orphan_ttl: 60s                  # 渠道记录未被消费的判定时长
      # ---- 展示与隐私 ----
      mask_api_key: false              # true 时下游 key 只留前后 4 位
      log_events: false                # true 时每条落盘行额外打一行 CPA 日志
      capture_cost: true
      capture_cache: true
      store_planning_reasoning: false  # 默认只存 planningReasoning 的长度，不存文本
      timezone: "Asia/Shanghai"        # 页面展示时区
      # ---- Cline 官方用量（套餐 / 限额 / 概览口径）----
      plan_enabled: true               # 官方套餐卡片总开关
      plan_api_key: ""                 # 留空则自动发现（不需要手填，见下文「官方用量」）
      plan_config_path: "/CLIProxyAPI/config.yaml"   # 容器内 CPA config.yaml 路径，用于读 Cline 凭据
      plan_refresh: 5m                 # 套餐与限额刷新间隔
      plan_daily_enabled: true         # 官方 Token 总量 / 成本 / 余额（最多每小时一次）
      plan_usage_enabled: true         # 概览的请求数 / 总 Token 数 / 缓存命中率改用官方逐条用量
      plan_usage_refresh: 5m           # 官方逐条用量的增量拉取间隔（最小 1m）
```

改动配置后 CPA 会自动重扫并热加载插件（`reconfigure`）。

### 配置项说明

| 键 | 默认值 | 说明 |
|---|---|---|
| `enabled` | `true` | 关闭后不再注册任何路由、不再写盘、不再计数 |
| `priority` | `1` | 插件优先级 |
| `hosts` | `["api.cline.bot"]` | 唯一的判定项：请求的 host 列表。匹配规则：用 `net/url` 解析 `UsageRecord.BaseURL` 取 host 后**小写精确比较**；以 `.` 开头的项按**域名后缀**匹配（`".cline.bot"` 命中 `api.cline.bot`，不命中 `evil-cline.bot`）。显式留空 `[]` → 不再看 host，所有请求都记录（`health` 里 `mode: "marker-only"`） |
| `require_routing_marker` | `false` | 严格模式。开启后只记录响应里出现过 `provider_metadata.gateway.routing` 的请求；关闭时（默认）host 命中即记录，渠道值按 `finalProvider` → `provider` → 空 的顺序取 |
| `unmatched_host_samples` | `20` | `health` 里保留的 host 未命中样本条数 |
| `ring_size` | `5000` | 内存环形缓冲条数，决定页面能查的最近数据量（JSONL 里保留全量） |
| `jsonl_enabled` | `true` | 是否落盘 |
| `jsonl_dir` | 见配置块 | JSONL 目录。需是 CPA 进程可写目录；默认值按常见部署给出，**请按自己的部署环境确认可写** |
| `retention_days` | `30` | 超过天数的 `channel-monitor-YYYY-MM-DD.jsonl` 会被删除 |
| `join_window` | `5s` | 渠道记录与用量记录的关联时间窗 |
| `orphan_ttl` | `60s` | 渠道记录在该时长内未被任何用量记录消费 → 计入 `orphan_channel`（不落盘） |
| `mask_api_key` | `false` | 掩码下游 key |
| `log_events` | `false` | 额外把落盘行写进 CPA 日志 |
| `capture_cost` / `capture_cache` | `true` | 是否记录成本字段 / 缓存字段 |
| `store_planning_reasoning` | `false` | `false` 时只记 `planningReasoning` 长度，不记文本 |
| `timezone` | `Asia/Shanghai` | 页面与时间戳展示时区 |
| `plan_enabled` | `true` | 开启「Cline 套餐用量」区（套餐名、5 小时/周/月限额、官方 Token 总量）。需要能拿到 Cline API Key |
| `plan_api_key` | 空 | 显式指定一把 Cline API Key。**留空即可**：插件会按 `plan_config_path` → CPA 凭据接口 → 上游请求头自动发现（实测留空时从 CPA 供应商配置的 `api-key-entries` 读到 key）；填了会与自动发现的 key 一起参与轮询（同一把 key 只算一次） |
| `plan_base_url` | `https://api.cline.bot/api/v1` | Cline API 基址 |
| `plan_config_path` | `/CLIProxyAPI/config.yaml` | 容器内 CPA 配置文件路径。Cline 的 key 通常以 `openai-compatibility[].api-key-entries[].api-key` 存在这里 |
| `plan_refresh` | `5m` | 套餐与限额刷新间隔（最小 1 分钟） |
| `plan_daily_enabled` | `true` | 另拉官方 Token 总量 / 成本 / 余额，最多每小时一次 |
| `plan_usage_enabled` | `true` | 概览的请求数 / 总 Token 数 / 缓存命中率改用官方逐条用量口径（延时与生成速度仍是本机口径） |
| `plan_usage_refresh` | `5m` | 官方逐条用量的增量拉取间隔（最小 1 分钟） |

## 官方用量（套餐、限额、概览口径）

开启 `plan_enabled` 后，插件用同一个 Cline API Key 调用 Cline 控制台自己用的接口，页面上多出一块「Cline 套餐用量」，并把概览的部分卡片切到官方口径。

| 接口 | 用途 | 备注 |
|---|---|---|
| `GET /api/v1/users/me` | 账号 id | |
| `GET /api/v1/users/me/plan` | 套餐名与月费 | `pricePerSeatCents` |
| `GET /api/v1/users/me/plan/usage-limits` | 5 小时滚动 / 本周 / 本月已用百分比与重置时间 | 页面顶部三张进度卡 |
| `GET /api/v1/users/{id}/usages/daily?startDate&endDate` | 逐日逐模型的输入/输出 token 与成本 | 单次范围 **≤ 31 天（含端点，所以是今天-30 ~ 今天）**；金额为**微美元**（÷1e6） |
| `GET /api/v1/users/{id}/usages?limit&cursor` | 逐条请求：`totalTokens` / `cachedTokens` / `costUsd` / `createdAt` | 每页上限 **200 条**，按时间**倒序**，**不支持按时间过滤** |
| `GET /api/v1/users/{id}/balance` | 余额 | 微美元 |

**套餐卡片里的数字**：`成本` 是 Cline 按上游 API 单价折算的**参考成本**（ClinePass 是包月，不按这条扣钱），5 小时/周/月限额百分比就是按这个口径算的；`余额` 是账号余额；`官方计费条目` 是逐日逐模型汇总的行数，**不是请求数**——请求数看概览里带「官方」标注的那张卡。

**多个 Cline 条目 / 多把 key**：官方套餐与限额是**按账号**算的，所以插件把每把 key 当成一个账号分别轮询：

- 发现范围：所有 base-url 命中 `hosts` 的 `openai-compatibility` 条目，以及名称是 `Cline` 的条目，取它们的 `api-keys` 与 `api-key-entries`（同一个 key 出现在多处只算一次，最多 8 个）；
- 每把 key 一个账号卡：套餐、5 小时/周/月限额、31 天汇总、7 天汇总、官方逐条用量窗口都是各算各的；页面顶部出现**账号下拉**（≥2 个凭据时），概览里带「官方」标注的数值也跟着下拉切换，选择记在浏览器里；
- 同一账号的两把 key（`/users/me` 返回同一个 user id）会被合并成一条，避免对同一个账号重复拉取；
- 凭据列表每个轮询周期重新解析，所以在 CPA 里新增/删除 Cline key 后不用重启插件；
- 页面标签只显示「条目名 #序号 · sk-…尾4位」，账号显示为 `usr-xxxx…xxxx`，**不会出现完整 key**；
- 被上游拒绝的凭据（401/403，例如误把下游客户端 key 当成 Cline key）不进账号下拉，只在 `/health` 的 `plan_accounts` 里保留（带 `rejected: true`）；
- 上游调用量按账号叠加：账号之间串行并间隔 0.5s，每个账号有自己的 26 小时保留窗口、分页预算与退避。

**凭据发现顺序**：`plan_api_key` → CPA `config.yaml` 里的 `openai-compatibility[].api-keys` / `api-key-entries` → CPA 凭据接口（`host.auth.list` / `host.auth.get`）→ 最近一次上游请求的 `Authorization`（只接受形如 Cline key 的长 `sk_…` 值；下游客户端 key 是 20 字符的 `sk-…`，会被忽略，避免多出一个永远不可用的账号）。多数部署走到第三步就够了：CPA 会把你在供应商配置里填的 key 持久化到 `openai-compatibility[].api-key-entries[].api-key`，所以 `plan_api_key` 可以一直留空（少一份密钥副本），只有把配置文件放到容器外读不到时才需要显式填。key 只留在内存，不落盘、不打日志、不返回给页面。

**概览五块**（顺序：请求数 · 总 Token 数 · 缓存命中率 · 平均延时 · 生成速度，固定一行不换行）：

- **总 Token 数**是官方**累计**（近 31 天逐日汇总，与套餐区同一口径），不是当前时间窗的量；副行给出官方累计区间、官方输入/输出，以及本机同一时间窗的数值；
- **请求数 / 缓存命中率**在近 1 小时、近 24 小时窗口取官方逐条计费记录（整个账号，含 Cline IDE 等其他客户端）；近 7 天窗口官方没有明细，改为本机口径，且 Token 卡仍显示官方累计；
- **平均延时 / 生成速度**没有官方字段，永远是本机口径（只统计经过本 CPA 的 Cline 请求）；
- 页面在「概览」标题下写明当前口径；官方记录覆盖不到窗口起点时，会标出「官方数值偏低」；
- 大数单位是 K / M / B / T（B = billion，十亿），例如 `1.77B token`。

**上游调用量与限流**：逐条明细接口不能按时间过滤，窗口内有多少条记录就要翻多少页（实测近 24 小时约 3500 条 ≈ 17 页）。因此插件在内存里保留最近 **26 小时**的记录，稳态下每次只翻到已见过的记录为止（通常 1 页）；首次回填或覆盖不足时按 300ms/页 节流，最多 60 页，遇到 429 等错误会指数退避（上限 30 分钟），所以覆盖范围会在几个刷新周期内长满，而不是一次打满。近 7 天要多翻上百页，所以这个窗口的明细不取官方，改用 `/usages/daily` 一次请求拿到的逐日汇总（token 与成本）。

插件重载后需要重新回填；采集状态（条数、覆盖起点、是否截断、失败次数、下次重试时间）见 `/health` 的 `plan_usage`。想完全避免这部分上游调用，可以设 `plan_usage_enabled: false`（概览回到本机口径）或 `plan_enabled: false`（整块官方数据关闭）。

## 适配你自己的 Cline 条目

插件**不依赖**你在 CPA 里给上游条目起的名字（`openai-compatibility[].name` 派生的内部 provider key 只作为诊断字段落盘，不参与任何判定），因此改名、换模型别名都不影响记录。

判定只看一件事：**usage 记录的 base_url host 在 `hosts` 里**，命中即记录一行。行里的渠道值按 **`finalProvider` → 上游响应的 `provider` 字段 → 空** 的顺序取，两个都没有时留空、页面显示 `—`。想要「只记走 Cline 网关的请求」就打开 `require_routing_marker: true`。

### 形态 1：直连 `api.cline.bot`

默认配置即可，无需改动：

```yaml
hosts: ["api.cline.bot"]
require_routing_marker: false
```

### 形态 2：自建反代 / 中转 / 自建 PaaS（host 不是 `api.cline.bot`）

两种做法，任选其一：

- 把自己的域名加进列表（推荐，保留 host 判据）：

  ```yaml
  hosts: ["api.cline.bot", "cline-proxy.example.com", ".example.com"]
  ```

  `".example.com"` 这种写法会命中该域名下的所有子域。
- 或者直接不看 host：`hosts: []`。此时所有经过 CPA 的请求都会记录（含非 Cline 流量），`health` 会显示 `mode: "marker-only"`。适合无法确定 host 或中转层会改写 base_url 的场景。

### 形态 3：条目名不是 `Cline`（例如叫 `ClinePass`、`CP`）

**不需要任何改动**。判定与条目名无关；落盘里的 `provider` 字段只是用来排障时对账「这条走的是哪个配置条目」。

### 怎么确认自己配对了

先发一发真实的 Cline 请求，然后看 `health`：

```bash
curl -s -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" \
  http://127.0.0.1:8317/v0/management/plugins/clinepass-channel-monitor/health
```

- `skipped_unmatched_host > 0` 且 `unmatched_host_samples` 非空 → 有请求走了 `hosts` 之外的 host 而被跳过。样本里给出 `host`/`provider`/`model`/时间，把那串 host 加进 `hosts`（或把 `hosts` 清空）即可；
- `requests` 与 `recorded` 基本相等 → 正常。只有开了 `require_routing_marker: true` 才会出现 `marker_missing`（响应里没有 `provider_metadata.gateway.routing` 而被跳过）；
- 明细里渠道列是 `—` → 该行上游既没给 `provider_metadata.gateway.routing` 也没给 `provider` 字段（失败请求常见），`channel_missing` 计数与之对应。

## 页面与接口

页面路径：管理中心的「插件 → Cline 渠道监控」，对应资源路由

```
/v0/resource/plugins/clinepass-channel-monitor/index.html
```

该资源路由**不含任何数据**，只返回静态页面壳。页面里有一个「管理密钥」输入框，密钥存在浏览器 `localStorage`，由前端带着 `Authorization` 头去调下面的管理接口渲染数据。页面为单文件静态 HTML，**不引用任何 CDN 或外网资源**，内网环境可用。

数据接口（都需要 `Authorization: Bearer <management-key>`，未带密钥返回 401/403）：

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/v0/management/plugins/clinepass-channel-monitor/stats?window=1h\|24h\|7d` | 聚合：按渠道/模型/来源，附带 `plan`（官方套餐、限额、`windows` 三个时间窗的官方口径） |
| GET | `/v0/management/plugins/clinepass-channel-monitor/events?window=1h&limit=200&offset=0&channel=&model=&source=&result=` | 明细（分页 + 过滤） |
| GET | `/v0/management/plugins/clinepass-channel-monitor/health` | 计数器与自诊断样本（含 `plan_usage`：官方明细条数、覆盖起点、是否截断、最近一次错误；`plan_accounts`：每个 Cline 凭据的标签、账号、可用性与错误） |
| GET | `/v0/management/plugins/clinepass-channel-monitor/export?window=24h` | 当前筛选条件的 CSV |

## 字段说明

JSONL 每行一个 JSON 对象，按天切分：`<jsonl_dir>/channel-monitor-YYYY-MM-DD.jsonl`（文件权限 0644，追加写，行尾 `\n`）。

页面列：

| 页面列 | 字段 | 来源 | 说明 |
|---|---|---|---|
| 时间 | `timestamp` | usage 记录 `RequestedAt` | 按 `timezone` 展示 |
| 来源 | `api_key` | usage 记录 `APIKey` | 下游 key，可掩码 |
| 模型 | `model_alias` / `model` | usage 记录 | 别名与实际模型都记 |
| 上游地址 | `base_url` | usage 记录 `BaseURL` | 判定用 host 的来源，也方便自查 |
| 推理强度 | `reasoning_effort` | usage 记录 | |
| 结果 | `failed` / `status_code` / `error` | usage 记录 | 失败请求没有渠道，`channel_missing=true` 属正常 |
| 延时 | `latency_ms` | usage 记录 `Latency` | |
| 生成速度 | `tokens_per_second` / `tokens_per_second_after_ttft` | 计算 | 后者用 `output_tokens / ((latency - ttft)/1000)` |
| TTFT | `ttft_ms` | usage 记录 `TTFT` | |
| token | `input_tokens` / `output_tokens` / `reasoning_tokens` / `total_tokens` | usage 记录 `Detail` | |
| 缓存 | `cached_tokens` / `cache_read_tokens` / `cache_creation_tokens` | usage 记录 `Detail` | CPA 侧统计 |
| 缓存（渠道） | `prompt_cache_hit_tokens` / `prompt_cache_miss_tokens` / `system_fingerprint` | 渠道元数据 | 上游侧统计，与上一组来源不同 |
| 渠道 | `final_provider` / `resolved_provider` / `canonical_slug` / `original_model_id` | 渠道元数据 | 本次实际服务的上游渠道（`final_provider` 优先取 `finalProvider`，缺失时取上游响应的 `provider`） |
| 成本 | `cost` / `input_cost` / `output_cost` / `generation_id` | 渠道元数据 `gateway.*` | 上游按请求给出的实际美元成本 |
| 渠道尝试 | `model_attempt_count` / `total_provider_attempt_count` / `fallbacks_available_count` | 渠道元数据 | 用来看是否发生兜底；只记候选数量，不记候选全量 |
| 协议 | `client_protocol` / `upstream_protocol` / `stream` | 渠道钩子 | 例如 `openai-response` / `openai` |
| 其它 | `service_tier` / `endpoint` | usage 记录 | `endpoint` 拿不到时留空并计数 |

仅落盘/内存保留、不出现在页面上（用于排障对账）：`schema`、`event_id`（幂等去重键）、`session_id`、`parent_session_id`、`auth_index`、`auth_type`、`provider`（CPA 内部 provider key，如 `openai-compatible-<你的条目名>`）、`channel_missing`、`planning_reasoning`（按配置只存长度）。

JSONL 行示例：

```json
{"schema":1,"event_id":"…","timestamp":"2026-09-21T15:54:50+08:00","provider":"openai-compatible-cline",
 "base_url":"https://api.cline.bot/api/v1",
 "model":"cline-pass/deepseek-v4.1-flash","model_alias":"deepseek-flash","api_key":"sk-…","session_id":"…",
 "failed":false,"status_code":0,"error":"","latency_ms":2380,"ttft_ms":410,"tokens_per_second":16.4,
 "tokens_per_second_after_ttft":19.8,"input_tokens":333738,"output_tokens":859,"reasoning_tokens":603,
 "total_tokens":334597,"cached_tokens":333568,"cache_read_tokens":333568,"cache_creation_tokens":0,
 "prompt_cache_hit_tokens":0,"prompt_cache_miss_tokens":33,
 "final_provider":"deepseek","resolved_provider":"deepseek","canonical_slug":"deepseek/deepseek-v4.1-flash",
 "cost":"0.0000447","input_cost":"0.0000099","output_cost":"0.0000348","generation_id":"gen_…",
 "model_attempt_count":1,"total_provider_attempt_count":1,"fallbacks_available_count":15,
 "client_protocol":"openai-response","upstream_protocol":"openai","stream":true,"reasoning_effort":"high",
 "service_tier":"","channel_missing":false}
```

### `health` 计数器

| 字段 | 含义 |
|---|---|
| `mode` | `host+marker`（`hosts` 非空）或 `marker-only`（`hosts: []`） |
| `recorded` | 已落盘的请求数（host 命中即落盘，`requests - recorded` 就是被跳过的数量） |
| `skipped_unmatched_host` | host 未命中而跳过的请求数（配错 `hosts` 的信号） |
| `unmatched_host_samples` | 上述跳过的最近样本（host/provider/model/时间） |
| `marker_missing` | 严格模式（`require_routing_marker: true`）下因响应缺少 `gateway.routing` 而跳过的次数 |
| `parse_error` | 渠道元数据解析失败次数（不影响响应） |
| `orphan_channel` | 渠道记录在 `orphan_ttl` 内未被任何用量记录消费的次数 |
| `channel_missing` | 落盘行里渠道列为空的行数（上游两个字段都没有，或请求失败） |
| `write_error` | JSONL 写入失败次数 |
| `fused` | 插件是否被宿主 fuse（插件 panic 后宿主会禁用它，CPA 日志有 error 记录） |

## 隐私

- 默认**不落任何 prompt / 响应正文**，只记元数据（渠道、用量、成本、缓存）；
- `planningReasoning` 是上游网关的规划文本，默认**只记长度**（`store_planning_reasoning: false`），页面里折叠展示，开启后才会落盘文本；
- `api_key` 是**下游**（调用 CPA 的）key。默认与 CPA 用量记录保持一致原样落盘，可用 `mask_api_key: true` 只留前后 4 位；
- 插件**不会**打印或返回 CPA 管理密钥、上游 API key 或 auth 文件内容；
- 数据只出现在两个地方：鉴权过的管理接口，以及你配置的 `jsonl_dir` 下的 JSONL 文件。资源页面路由是静态壳，**不含任何数据**。

## 排障

| 现象 | 可能原因与处理 |
|---|---|
| `GET /v0/management/plugins` 里本插件 `registered: false`、`path: ""` | `.so` 没被扫描到：确认文件名是 `clinepass-channel-monitor.so` 或 `clinepass-channel-monitor-v<version>.so`，且位于 `<plugins.dir>/<goos>/<goarch>/` 或 `<plugins.dir>/` 下；确认 CPA 配置里 `plugins.enabled: true`，且 `plugins.configs` 的键名与插件 id 完全一致 |
| 加载失败、日志提示 ABI 不符 | 需要 CPA ≥ v7.3.8 的**带插件支持**构建（`X-Cpa-Support-Plugin: 1`）；插件声明 `abi_version = 1`、`schema_version = 6` |
| 插件在 `plugins` 列表里但页面 404 | 检查 CPA 版本是否满足；改一次配置触发重扫；确认资源路由路径为 `/v0/resource/plugins/clinepass-channel-monitor/index.html` |
| 页面能开但一直空 | 页面里的管理密钥没填或填错（管理接口会返回 401/403）；或窗口内确实没有命中记录，先看 `health` |
| 有请求但一条都没记录 | 看 `health`：`skipped_unmatched_host > 0` → host 不匹配，按「适配你自己的 Cline 条目」处理；开了 `require_routing_marker` 且 `marker_missing` 在涨 → 这些响应没有 `provider_metadata.gateway.routing`，关掉严格模式即可 |
| `final_provider` 一直是空 | 上游响应里既没有 `provider_metadata.gateway.routing` 也没有 `provider` 字段（失败请求常见）；该行渠道列显示 `—`，并在 `channel_missing` 里计数 |
| 渠道列有值但用量/缓存列是 0 | 关联失败或该请求确实没有 token 统计；看 `channel_missing` 与 CPA 侧用量记录对账 |
| JSONL 没有生成 | `jsonl_enabled: false`、`jsonl_dir` 不可写（看 `health.write_error`）、或宿主与容器目录映射不一致 |
| 插件突然不出数据了 | 看 `health.fused`；插件 panic 会被宿主 fuse，CPA 日志里会有对应 error |
| 升级 CPA 后行为变化 | 回到本文「环境要求」，核对 `X-Cpa-Support-Plugin` 头、`abi_version`/`schema_version`，并重新跑一次发请求→看 `health`→看 JSONL 的链路 |

常用命令（管理密钥用环境变量传入，不要写进脚本或文档）：

```bash
export CPA_MANAGEMENT_KEY='<your-management-key>'
curl -s -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" http://127.0.0.1:8317/v0/management/plugins
curl -s -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" http://127.0.0.1:8317/v0/management/plugins/clinepass-channel-monitor/health
curl -s -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" http://127.0.0.1:8317/v0/management/plugins/clinepass-channel-monitor/stats?window=24h
```

> 管理接口有暴力破解防护：**只打确定存在的路径 + 正确密钥**，错误探测会累计失败并临时封禁来源 IP。

## 升级与回滚

- **升级**：用新版本 `.so` 覆盖旧文件（文件名带版本号时删掉旧的），改一次 CPA 配置触发重扫，然后在 `plugins` 列表确认 `registered: true`、版本正确；
- **回滚**：把 `plugins.configs.clinepass-channel-monitor.enabled` 设为 `false`（插件不再注册路由、不再写盘），或直接删掉 `.so` 后触发重扫；
- **卸载残留**：插件本身不在 CPA 配置之外写任何文件。需要彻底清理时删除：① 插件配置块，② `.so` 文件，③ `jsonl_dir` 下的 `channel-monitor-*.jsonl`（这一步是删数据，按需保留备份）。

## 目录结构

```
cmd/clinepass-channel-monitor/   入口：C ABI 的四个 //export 符号、信封编解码、panic 兜底
  cdecl.h                        C 侧类型声明（cgo 前置用）
internal/abi/                    插件 ABI 信封与观测钩子的「不改动」空响应
internal/buildinfo/              插件 id / 名称 / 作者 / 版本（版本由 -ldflags 注入）
internal/hostapi/                宿主回调桥：日志与 host.* 数据接口
internal/config/                 配置解析与归一化（plugins.configs.<id> 契约）
internal/store/                  环形缓冲、计数器、聚合、事件模型、JSONL 落盘与保留期
internal/metadata/               provider_metadata 解析（渠道证据从哪来）
internal/plan/                   Cline 官方用量：套餐、限额、31 天汇总、逐条记录采集
internal/hooks/                  三个观测钩子 + 请求关联表（identity）
internal/state/                  运行时状态（配置 / 存储 / 用量轮询器）的发布与读取
internal/management/             管理接口与内嵌页面 index.html
internal/plugin/                 注册、生命周期与方法分发（把上面这些接起来）
```

依赖是单向的：`plugin → hooks / management → store / plan → config`；`hooks`、`management`、`plugin` 通过 `state` 读取运行时状态，而 `state` 不反向依赖它们，所以没有任何 import 环。

## 构建与开发

```bash
make build       # 构建本机架构的 .so（CGO，-buildmode=c-shared）到 dist/
make test        # 单元测试（解析器/关联器/环形缓冲，含真实响应片段 fixture）
make bench       # 请求路径开销基准（钩子载荷解码、请求指纹、事件序列化）
make install     # 安装到本地 CPA 插件目录（路径可通过变量覆盖）
make tools       # 安装固定版本的 Go 工具链（1.27.1）到 .toolchain/go，无需 root
make clean-cache # 清空 Go 构建缓存（.toolchain/gocache、gotmp）
make clean       # 删掉构建产物 dist/
```

工具链版本由 `Makefile` 的 `GO_VERSION` 固定为 `1.27.1`（`scripts/install-go.sh` 的默认值与之相同），和 `go.mod` 里 `go 1.26.0` 的语言下限是两件事。工具链、模块缓存、构建缓存都在 `.toolchain/` 下并且已 gitignore：`make clean-cache` 只清构建缓存与临时目录，工具链和模块缓存保持不动，所以清完仍能离线构建（第一次构建是冷编译，会慢一些，缓存会重新长出来）；连工具链一起删就直接删掉 `.toolchain/`，之后需要 `make tools` + `go mod download` 重新拉取。

跨平台产物由 GitHub Actions 在 tag 推送时构建：`linux/amd64`、`linux/arm64` 各打一个 zip，zip 内文件名固定为 `clinepass-channel-monitor.so`，并附 `checksums.txt`。

## 许可证

MIT，见 [LICENSE](LICENSE)。
