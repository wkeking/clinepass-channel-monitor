# 账号守卫：连续 N 次非基准渠道 → 禁用该账号（可行性 + 方案）

需求：CPA 里有多条 Cline 账号（`openai-compatibility` 条目），当某个账号的**真实渠道**
（请求日志里的 `finalProvider`）连续 3 次不是官方渠道（不是 `deepseek`）时，把该账号
`disabled` 改成 `true`。

本文只做调研与方案，不含实现。证据分三档标注：**实测**（本次在生产 CPA v8.0.13 / 插件
0.3.1 上跑出来的）、**代码**（读 `.reference/CLIProxyAPI` 检出）、**未验证**。

## 0. 结论

可实施，但有三件事必须一起接受，否则这个功能会帮倒忙：

1. **判据成立**：账号与真实渠道能按请求配起来，实测最近 200 条记录里 197 条（98.5%）拿到了
   渠道；剩下 3 条没有渠道块（失败或客户端没送 `Session_id`），它们**不计数**。
2. **动作生效**：把条目 `disabled` 置 true 后，CPA 不再为该条目合成凭据
   （`synthesizeOpenAICompat` 直接 `continue`），即账号退出路由。生产是「一个账号一条
   `openai-compatibility` 条目、一条一个 key」，所以条目级 `disabled` 正好等于「关掉这个账号」。
3. **副作用很硬**：三个账号分别绑定三个 alias（`deepseek-flash-1/2/3` → Cline1/2/3，实测
   `/v1/models` 与记录里的 `alias` 都是 1:1）。**关掉账号 = 该 alias 不再有模型**，用这个 alias
   的客户端会直接失败，而不是自动切到另一个账号。要透明切换，得先把三个条目改成共用一个 alias。

再加上一条实现层面的取舍：插件写 CPA 配置有两条路（管理接口 / 直接最小节点编辑），两条都
实测过，见 §2.3，需要你选一条。

## 1. 判据：账号 × 真实渠道，能不能按请求算出来

### 1.1 数据从哪来

- **账号**来自 usage 钩子写下的记录：`cpa_provider` = `openai-compatible-<条目名小写>`
  （内部键由 `util.OpenAICompatibleProviderKey` 生成，`internal/util/provider.go:18`），
  记录里还带 `auth_id`（形如 `openai-compatibility:cline1:<12 位哈希>`）、`auth_index`、
  `auth_type=apikey`。**实测**最近 200 条记录的取值：

  | `cpa_provider` | 条数 | `auth_id` |
  |---|---|---|
  | `openai-compatible-cline1` | 146 | `openai-compatibility:cline1:…` |
  | `openai-compatible-cline2` | 52 | `openai-compatibility:cline2:…` |
  | `openai-compatible-cline3` | 2 | `openai-compatibility:cline3:…` |

- **真实渠道**来自 CPA 请求日志解析出的 fact（`final_provider`，`channel_source: "log"`），
  由插件在**读时**与记录 join（`internal/observation/join.go`：先按 `Session_id` 的 uuid，
  再回退到「2 秒窗口 + token 精确相等且候选唯一」）。
- join 的覆盖：**实测** 197/200 = 98.5%。

### 1.2 这条规则在真实流量上会命中什么

把最近 200 条按时间排序、每账号独立记连击（未 join 的请求跳过，既不 +1 也不清零），
**实测**得到：

```
10:38:20  cline3  openai-compatible-private   连击 1
10:46:47  cline1  fireworks                   连击 1
10:47:32  cline1  fireworks                   连击 1   ← 中间夹了一条 deepseek，清零
10:48:54  cline1  fireworks                   连击 2
10:49:08  cline1  fireworks                   连击 3   ← 阈值（3）在这里到
10:49:21 … 10:54:01  cline1  fireworks        连击 4…15
11:03:42  cline3  openai-compatible-private   连击 2
```

也就是说：这段流量里 cline1 有过一次持续 15 条的 `fireworks` 回退，规则会在第 3 条
（10:49:08，距开始 2 分钟）就把它关掉；cline3 在窗口末尾也已经 2 连击。这不是假想场景。

同时它也暴露了阈值 3 的敏感度：cline1 的第 3 条才发生在 2 分钟内。要不要 3 次、要不要再加一个
「至少持续 X 秒」的条件，见 §6。

## 2. 动作：把 disabled 置 true，会发生什么

### 2.1 语义（代码）

- `OpenAICompatibility.Disabled` 在 `synthesizeOpenAICompat` 里被最先判断，true 就 `continue`
  （`internal/watcher/synthesizer/config.go:298`），该条目**不产生任何 Auth**，也就不会被选中；
  `internal/watcher/clients.go:420` 统计凭据数时同样跳过 disabled 条目。
- 客户端仍然能看到条目名、key 与模型定义（它们还在配置里），只是不参与路由。
- **副作用（强推断）**：条目 disabled 后它提供的 alias 不再可用。旁证是 **实测** `/v1/models`
  只有 `deepseek-flash-1/2/3`，没有已 disabled 的 DeepSeek 条目的 `deepseek-flash`。同一份
  实测里启用的 UGQ DS 条目（alias `deepseek-flash-t`）也没出现，说明这个列表还受别的因素影响，
  所以这条标 **强推断**，落地前值得在测试环境确认一次。

### 2.2 生产结构（实测）

`api-keys.openai-compatibility` 共 5 条，每条恰好 1 个 key：

| # | 条目名 | 现在 | alias |
|---|---|---|---|
| 0 | `DeepSeek` | `disabled: true` | `deepseek-flash` |
| 1 | `UGQ DS` | `disabled: false` | `deepseek-flash-t` |
| 2 | `Cline1` | `disabled: false` | `deepseek-flash-1` |
| 3 | `Cline2` | `disabled: false` | `deepseek-flash-2` |
| 4 | `Cline3` | `disabled: false` | `deepseek-flash-3` |

每条一目的一条 key，所以「关账号」= 改这一个条目的 `disabled`，不用碰 key 级字段。

### 2.3 写配置的两条路（都实测过）

**路 A：CPA 自己的管理接口**

```
PATCH /v0/management/openai-compatibility
{"name":"Cline1","value":{"disabled":true}}
```

（`PatchOpenAICompat`，`internal/api/handlers/management/config_lists.go:901`；按 `name` 或
`index` 定位，成功后 `persistLocked` → `SaveConfigPreserveComments`。）

- **实测**（2026-10-05，对本来就是 `disabled: true` 的 `DeepSeek` 做同值 PATCH）：返回
  `{"status":"ok"}`；语义快照前后**完全一致**（条目/disabled/插件块键名/request-log/
  commercial-mode/access/management/oauth/routing 全部不变）。
- 但也有两个硬约束：
  1. **整篇重写 `config.yaml`**：实测把原本紧凑的单行列表展开成多行、键加引号、注释重新缩进。
     语义没问题，但手工维护的排版（和按行改配置的脚本）会失效；
  2. **需要明文管理密钥**：`Authorization: Bearer <management.secret-key>`。生产上
     `management.secret-key` 是 bcrypt 哈希（`$2a$…`，60 字符），插件**拿不到明文**，所以走这条路
     要操作者额外在插件配置里放一把可用的明文密钥（新键，见 §4.1）。

**路 B：插件直接做最小 YAML 节点编辑**

- 只改 `api-keys.openai-compatibility[i].disabled` 这一个标量，**其余字节不动**（注释、排版、
  同一文件里的其它凭据都保持原样）；
- 写盘用同目录 `tmp` + `rename` 原子替换；写前用「刚才读到的原始字节」做一次 CAS（内容变了
  就放弃本轮），把与操作者/CPA 自身写冲突的窗口压到微秒级；
- CPA 用 fsnotify 监听配置文件并处理 `Write|Create|Rename`
  （`internal/watcher/events.go:67`），改完自动 reload，**不需要重启**；
- 不需要任何新凭据；代价是「插件改宿主的配置文件」这件事本身，必须由用户显式开启、
  且默认 dry-run。

**不适用的一条（代码）**：CPA 自带的「禁用某个凭据」入口 `PATCH /v0/management/auth-files/status`
对 `openai-compatibility` 无效——它走的 `toggleConfigAPIKeyExcludedAll`
（`internal/api/handlers/management/config_apikey_disable.go:33`）只遍历
gemini/interactions/claude/codex/xai/meta/vertex 的 key 列表，不含 `OpenAICompatibility`，
必然返回 404。所以别在这条上做设计。

## 3. 必须一起解决的边界

1. **alias 绑定**（§0.3）：关掉 Cline1，`deepseek-flash-1` 就没人服务。三种选择：
   (a) 把三个条目改成共用一个 alias（例如都叫 `deepseek-flash`），让 CPA 在三个凭据间轮转——
   这样关掉一个对客户端透明，守卫才真正有用；(b) 保留 1:1 alias，接受「关了就得改客户端配置」；
   (c) 不关，只报警。**这是要先拍板的第一个点。**
2. **关掉之后必须能回来**：账号一旦 disabled 就不再产生流量，连击永远停在阈值上，自己不
   恢复就永久损失一个账号。方案里带 `reenable` 定时 + 指数退避（§4.2）。
3. **兜底**：`min_enabled`——无论如何至少保留 N 个可用账号（默认 1），否则整条链路会断。
4. **什么不算连击**：没有渠道块 / 没 join 上 / 上游失败的请求**既不 +1 也不清零**；只有拿到
   `final_provider` 且它 ≠ 基准名的请求才 +1，等于基准名才清零。否则失败率一高就会误伤。
5. **人为干预优先**：如果操作者自己把条目改回 enabled（或自己禁用），守卫要认输并重置自己的
   状态——实现方式是记住「上次是我把它写成什么」，与当前文件值不一致就当作操作者意图。
6. **没有数据就说没有数据**：`channel_log_enabled=false` 或 `channel_observe_enabled=false`
   时守卫必须 inert，并在页面/日志里说「守卫不可用」，而不是安静地什么都不做。
7. **基准名**：只认一个基准（复用 `channel_baseline_provider`，默认 `deepseek`）。要放行
   `fireworks` 之外的更多渠道，就把基准改成白名单语义（本期不做）。

## 4. 方案

### 4.1 配置键（默认全关，加在插件自己的块里）

```yaml
account_guard_enabled: false          # 总开关，默认关
account_guard_dry_run: true           # 只算并记录，不改配置（默认 true）
account_guard_threshold: 3            # 连续 N 次非基准渠道
account_guard_min_enabled: 1          # 至少保留几个可用账号，达不到就不动手
account_guard_scope_names: []         # 只守这些条目名（空 = 所有 openai-compatibility 条目）
account_guard_reenable_minutes: 30    # 多久后自动恢复；0 = 不自动恢复
account_guard_max_disable_minutes: 360
account_guard_management_key: ""      # 留空 = 走「直接最小节点编辑」；填了 = 走管理接口
```

`account_guard_min_enabled` 只数「当前 enabled 的、非 disabled 的 openai-compatibility 条目」。

### 4.2 状态机（每 5 秒一轮，在插件后台跑）

1. 取 `recorder.ChannelRecordsSince(Window1h)`（`internal/observation/observation.go:775`）——
   就是页面用的那份「记录 + 已经 join 好的渠道」，不另写一套匹配逻辑，避免守卫与页面口径不一致。
2. 按 `cpa_provider` 分组、按时间升序，从**上次处理到的位置**往后推（高水位用「时间 + 请求 id」
   去重，抗重启、抗重复处理）。
3. 每个账号维护：`streak`、`last_channel`、`last_seen`、`disabled_by_guard`（上次自己写的值）、
   `disable_count`、`next_retry_at`。
4. `streak >= threshold` 且该账号当前是 enabled 且 enabled 数 > `min_enabled` 时：
   - `dry_run` → 只写审计 + 宿主日志 + 页面标记「本应禁用」；
   - 否则 → 走 §2.3 选定的写入路径，写成功再更新 `disabled_by_guard=true`、
     `next_retry_at = now + min(reenable_minutes × 2^disable_count, max_disable_minutes)`。
5. 到 `next_retry_at` 且当前 disabled 且是我禁的 → 写回 `disabled: false`，`streak` 清零；
   再犯就连击更快（退避翻倍）。
6. 读到的条目状态与我上次写的不一致 → 认输：清掉 `disabled_by_guard`、`streak` 与退避。
7. 状态落盘到 `<channel_store_dir>/account-guard.json`（原子写），审计追加到
   `account-guard.jsonl`：一行一次动作，带账号名、连击数、触发它的那几条请求的时间与渠道、
   动作、结果（写后回读值）。

### 4.3 写入器

- 路 B 的具体做法：把配置读成 `yaml.Node`，定位 `api-keys.openai-compatibility[i].disabled`
  （v7 布局是顶层 `openai-compatibility[i].disabled`），只改这个标量的 `Value`，
  序列化后与「读到的原始字节」逐字节比对，确认只有预期的那一处变化，再 `rename` 落盘。
- 无论哪条路，**写后都要回读确认**（路 A 读接口 / 路 B 重新解析文件），并把「请求写的值 /
  实际读到的值」写进审计；失败就下一轮重试，不重复写。

### 4.4 可观测

- `/health` 增 `account_guard`：`enabled` / `dry_run` / `write_path` / `last_action_at` /
  `accounts[]`（`name`、`disabled`、`by_guard`、`streak`、`last_channel`、`next_retry_at`、
  `disable_count`）。字段里只有账号名、渠道名、计数与时间，没有 key。
- 页面：在现有「真实渠道」表下面加一块可折叠的**账号守卫**（默认收起），列出每个账号当前连击、
  最近几次渠道、是否被守卫关过、下次重试时间；`dry_run` 时明确写「只记录不改配置」。
- 每次动作往宿主日志打一行（`hostapi.LogAsync`），和「配置自检」的写法一致。

## 5. 分阶段落地

| 阶段 | 做什么 | 验证 | 回滚 |
|---|---|---|---|
| 0 | 确认 alias 决策（§6.2）；如有测试机，跑一次「同值 PATCH / 最小编辑」验证写路径与 reload | 配置语义快照前后一致；`/health` 的 `channel_log.enabled` 不变 | 用写前的 `config.yaml` 备份还原 |
| 1 | 实现状态机 + 审计 + `/health` + 页面，`account_guard_enabled: true` + `dry_run: true` | 用 §1.2 那段历史（放进单测夹具）验证连击判定；线上观察 1–2 天，看是否误报 | 关总开关 |
| 2 | `dry_run: false`，只守一个账号（`scope_names: ["Cline1"]`） | 真禁用一次后：`/health` 显示 `by_guard`、`/v1/models` 少了对应 alias、`disabled` 在文件里为 true；到期自动恢复 | 手动把该条目改回 `disabled: false`，守卫会认输（§4.2.6） |
| 3 | 放开到全部 Cline 账号 + 页面文案定稿 | 同上 | 同阶段 2 |

单测要覆盖：连击只在「拿到渠道且 ≠ 基准」时 +1、基准清零、无渠道块跳过、阈值命中一次只动作
一次、`min_enabled` 拦截、退避翻倍、重启后高水位不重复计数、写后回读不一致不算成功、
节点编辑只动一个标量（对着 v7/v8 两种布局各一份夹具）。

> ⚠️ 验证配置改动时**只比对语义快照**（`yaml.safe_load` 后比字段），不要 `diff` 全文：
> 管理接口写完会把整段配置重新格式化，`diff` 会把 `keys[].api-key` 这些凭据行整行打印出来
> （这次调研就踩到了，谁在跑这类验证都注意别把输出贴出去）。

## 6. 需要你先拍板的四点

**已定（2026-10-05）**：

1. 写路径取 **路 B**：插件直接做最小 YAML 节点编辑，不改用管理接口、不引入明文管理密钥。
2. alias **保持 1:1**：接受「关掉账号 = 对应 alias 暂时不可用」，不改成共用 alias。
3. 阈值 **3 次**（`account_guard_threshold: 3`）。
4. **自动恢复**：按方案默认 30 分钟起、指数退避（`account_guard_reenable_minutes: 30`、
   `account_guard_max_disable_minutes: 360`）。

下面是最初的四个问题，保留原文以便对照：

1. **写路径**：路 A（官方管理接口，需要你在插件配置里放一把明文管理密钥，且每次动作整篇重写
   `config.yaml`）还是路 B（插件只改那一个标量，保持文件排版，不需要新密钥）？我倾向 **路 B**：
   生产 `management.secret-key` 是 bcrypt，路 A 要额外引入一份明文密钥，而收益只是「用官方接口写」。
2. **alias 怎么处理**：接受「关了账号，对应 alias 暂时不可用」，还是先把 Cline1/2/3 改成共用
   一个 alias（客户端不用改，守卫才能真正无感）？这决定了功能的价值上限。
3. **阈值**：确认 3 次（实测里 cline1 的 3 连击发生在 2 分钟内），还是 5 次 / 加「至少持续
   60 秒」这类条件？
4. **自动恢复**：默认 30 分钟起、翻倍退避（上限 6 小时），还是干脆不自动恢复、只在页面和
   日志里提示人工处理？

## 7. 实施结果（2026-10-05）

按上面定下来的四条实现完毕（写路径 B、alias 保持 1:1、阈值 3、自动恢复）。落地位置：

| 位置 | 内容 |
|---|---|
| `internal/guard/` | 纯逻辑 + 状态机 + 状态/审计持久化（`guard.go`、`store.go`、`status.go`）。样本按 `provider|RequestID` 去重（重启安全），连击只在拿到渠道且 ≠ 基准时 +1，无渠道块跳过，命中基准清零；`MinEnabled` 用「enabled 数 − 本轮禁用数 > 1」判；退避 30 → 60 → 120 → 240 → 360 封顶 |
| `internal/hostconf/accounts.go` | 读两种布局的 `openai-compatibility` 条目（名称 / disabled / provider key / alias），以及**只改一个标量**的 `SetEntryDisabled`：YAML 节点定位 + 文本级替换（或 block 式插入），v7/v8 两种排版都支持；flow 式且缺 `disabled` 时返回 `ErrFlowStyle` 让操作者补一行，而不是重排整个文件 |
| `internal/plugin/accountguard.go` | 每 5 秒一轮的 runner：读配置拿账号、读 `ChannelRecordsSince(1h)` 拿（记录+渠道）、喂给状态机、执行决策（审计 → 状态落盘 → `tmp+rename` 写配置 → 回读确认）、发布 `/health` 快照。写失败或回读不一致时把「想要的值」挂进 `pending`，下一轮仍然按「已关」告诉状态机，避免把「还没读到」当成操作者放回 |
| `internal/management/index.html` | 「渠道」区「真实渠道」表下面新增**账号守卫**表（账号 / 当前渠道 / 连击 / 状态 / 下次重试 / 累计关闭）+ 一行规则说明 + 最近一次动作；`dry_run`、无渠道数据、写入待确认、人工关闭都有各自文案 |

验证：

- `go build ./...`、`go vet ./...`、`go test ./...`、`gofmt -l internal cmd` 全绿；
- `hostconf`：v7 block / v7 flow / v8 block 三种排版的单测断言「只有目标那一行变了」，另外对**真实生产配置**做过逐条目「翻转再翻回」往返，输出与输入逐字节相同；
- `plugin`：写成功 / 写失败保持 pending / 已经是想要的值就不写 / `pending` 覆盖读到的状态 / start-stop 发布快照 / 原子写保留权限且不留临时文件，各有单测；
- 真实渲染（1920×1080，浅色 + 深色）：试运行混合态、正式运行、无渠道数据、老版本无 `account_guard` 四种，`bodyWidth = docWidth = 1905`，控制台无错误；视觉验收见下一条；
- 注意：本轮的 `go test ./...` 第一次是**红的**，原因不在本功能：`internal/channellog` 有两个测试把 `channel-log-2026-10-02.jsonl` 当成「今天」，到了 10-05 就被 3 天保留期删掉了。已把它们改成用足够宽的保留窗口（这两个测试测的是扫描/去重，不是保留），保留策略本身仍由 `TestExpiredFactFilesArePruned` 覆盖。

### 7.1 上线后修掉的三处（2026-10-05，都在生产上实测到）

| 问题 | 症状 | 修法 |
|---|---|---|
| 去重标在了「还没拿到渠道」的样本上 | 某个账号一小时里有 4 条偏离渠道，连击却一直是 1：这 4 条的 token 全在去重表里，但一条都没计过 | `State.count` 把 `markSeen` 移到「账号在名单里 + 渠道非空」之后。渠道来自 CPA 请求日志，比记录本身晚几秒落地，所以「没有渠道」多数是「fact 还没到」；在那里消费掉这一天就永远补不回来 |
| `rename` 写不动 bind mount 上的 config.yaml | 面板默认值一条没写进去；账号守卫真去关账号时同样会失败（之前一直 dry-run 没暴露）。实测报错 `rename /CLIProxyAPI/.clinepass-*.tmp /CLIProxyAPI/config.yaml: device or resource busy` | `writeConfigAtomic` 在 `rename` 失败后退回原地覆写（`O_TRUNC` + write + fsync），和 CPA 自己保存配置的做法一致（`internal/config/config_yaml.go` 用 `os.WriteFile`） |
| 守卫会去看非 Cline 的条目 | 生产列表里有官方 DeepSeek 与 ugq.ai 两条无关账号，也在「账号守卫」表里 | 守卫先用凭据发现那条规则过滤：`base-url` host 命中 `hosts`，或条目名恰为 `Cline`；其余条目完全不进判定（`config.Config.IsClineEntry`）。生产上 `entries` 从 5 降到 3 |
| dry-run 只在「连击正好等于阈值」那一轮报告 | 修好上一条之后，一个账号的连击在一轮里从 2 直接跳到 5（渠道块成批落地 / 重启后重读整个窗口），于是什么也没报——而 dry-run 的意义正是让人看到「本应关谁」 | Rule 5 改成「每个连击在第一次 ≥ 阈值的那一轮报一次」，`AccountState.DryRunReported` 记住本连击已报过，连击被基准渠道/人工放回/外部禁用清零时复位 |
| 页面「最近一次动作」在插件重载后变空 | 状态文件里连击还在（表格照常显示 6/3），审计文件里也躺着那条 dry-run 决策，但 `/health` 的 `recent` 是空的：它是「做出决策的那个 runner」的内存，重载会换一个 runner | `/health` 在快照没有 `recent` 时读审计文件尾部（`guard.Store.RecentAudit`，只看最后 10 条、跳过坏行）。审计文件是每个进程共享的持久记录，因此页面永远能解释它显示的连击 |
