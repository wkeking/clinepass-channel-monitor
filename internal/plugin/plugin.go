// Package plugin wires the plugin together: it parses the configuration the host hands
// over, publishes the runtime state, and dispatches every ABI method CPA calls.
//
// The plugin declares ManagementAPI, and — only while channel observation is switched on —
// the usage plugin hook, which the host calls once per request with the credential the
// request was routed to. That hook is protocol independent: it fires for /v1/responses
// traffic, which is where the retired stream chunk interceptor saw nothing at all.
//
// It declares no request-side capability and no response translator: nothing on the request
// path needs cloning or handing across the ABI.
//
// When CPA's request log is switched on, the plugin also polls it for the gateway channel
// block the Responses translation drops (internal/channellog). That scanner is off by default,
// and it is not on the request path either: it reads files CPA has already finished writing.
package plugin

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/wkeking/clinepass-channel-monitor/internal/abi"
	"github.com/wkeking/clinepass-channel-monitor/internal/buildinfo"
	"github.com/wkeking/clinepass-channel-monitor/internal/channellog"
	"github.com/wkeking/clinepass-channel-monitor/internal/config"
	"github.com/wkeking/clinepass-channel-monitor/internal/hostapi"
	"github.com/wkeking/clinepass-channel-monitor/internal/hostconf"
	"github.com/wkeking/clinepass-channel-monitor/internal/management"
	"github.com/wkeking/clinepass-channel-monitor/internal/observation"
	"github.com/wkeking/clinepass-channel-monitor/internal/plan"
	"github.com/wkeking/clinepass-channel-monitor/internal/state"
)

// registrationCapability mirrors the host capability record. Field names are the
// ABI contract; see sdk/pluginhost rpcCapabilities.
type registrationCapability struct {
	ModelRegistrar           bool `json:"model_registrar"`
	ModelProvider            bool `json:"model_provider"`
	AuthProvider             bool `json:"auth_provider"`
	FrontendAuthProvider     bool `json:"frontend_auth_provider"`
	Executor                 bool `json:"executor"`
	RequestTranslator        bool `json:"request_translator"`
	RequestNormalizer        bool `json:"request_normalizer"`
	RequestInterceptor       bool `json:"request_interceptor"`
	ResponseTranslator       bool `json:"response_translator"`
	ResponseBeforeTranslator bool `json:"response_before_translator"`
	ResponseAfterTranslator  bool `json:"response_after_translator"`
	ThinkingApplier          bool `json:"thinking_applier"`
	// UsagePlugin is the only hook this plugin ever declares, and only while channel
	// observation is on: the host calls it once per request, for every wire protocol, with
	// the credential the request was routed to.
	UsagePlugin       bool `json:"usage_plugin"`
	CommandLinePlugin bool `json:"command_line_plugin"`
	ManagementAPI     bool `json:"management_api"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

// LoadConfig parses the configuration block the host hands over, publishes the new runtime
// state and (re)starts the Cline usage poller.
//
// It fails open: an unparseable block keeps the defaults so the plugin still loads and still
// serves the subscription view.
func LoadConfig(raw []byte) {
	cfg, errParse := config.Parse(raw)
	if errParse != nil {
		hostapi.LogAsync("warn", buildinfo.ID+": falling back to default config", map[string]string{
			"error": errParse.Error(),
		})
		cfg = config.Default()
	}
	if poller := state.Plan(); poller != nil {
		poller.Stop()
		state.SetPlan(nil)
	}
	stopObservation()
	stopChannelLog()
	stopAccountGuard()
	state.SetConfig(cfg)
	// 面板只显示配置块里存了什么；插件声明的字段 schema 没有默认值可显示，所以缺的键在这里
	// 用「当前生效值」补上。只补缺失键，不动任何已有键（见 plugindefaults.go）。
	seedPluginConfigDefaults(cfg)
	if cfg.PlanEnabled {
		state.SetPlan(plan.Start(cfg))
	}
	startObservation(cfg)
	startChannelLog(cfg)
	startAccountGuard(cfg)
	fields := map[string]string{
		"plan_enabled":      strconv.FormatBool(cfg.PlanEnabled),
		"plan_refresh":      cfg.PlanRefresh.Or(config.DefaultPlanRefresh).String(),
		"plan_usage":        strconv.FormatBool(cfg.PlanUsageEnabled),
		"plan_config_path":  cfg.PlanConfigPath,
		"channel_observe":   strconv.FormatBool(cfg.ChannelObserveEnabled),
		"channel_store_dir": cfg.ChannelStoreDir,
		"channel_baseline":  cfg.ChannelBaselineProvider,
		"channel_log":       strconv.FormatBool(cfg.ChannelLogEnabled),
		"channel_log_dir":   cfg.ChannelLogDir,
	}
	for key, value := range accountGuardLogFields(cfg) {
		fields[key] = value
	}
	hostapi.LogAsync("info", buildinfo.ID+": configured", fields)
	// 装完最容易漏的是网关渠道那一套开关（插件侧的 channel_log_enabled 默认 false，CPA 侧
	// 的请求日志也默认关）。加载时把还缺的项说一次，和页面顶部「配置自检」卡片同源。
	if hint := hostconf.GatewayHint(cfg); hint != "" {
		hostapi.LogAsync("info", buildinfo.ID+": "+hint, nil)
	}
}

// observationRecorder is the running collector. It is stopped before a reconfigure starts a
// new one so a reload can never leave two writers on the same directory.
var (
	observationMu       sync.Mutex
	observationRecorder *observation.Recorder
)

// stopObservation flushes and stops the collector, and makes the ABI entry point inert.
func stopObservation() {
	observationMu.Lock()
	recorder := observationRecorder
	observationRecorder = nil
	observationMu.Unlock()
	observation.SetActive(nil)
	state.SetObservation(nil)
	if recorder != nil {
		recorder.Stop()
	}
}

// startObservation starts the collector when the configuration asks for it. The capability
// is declared from the same value, so "off" means the host never calls the hook at all.
func startObservation(cfg config.Config) {
	if !cfg.ChannelObserveEnabled {
		return
	}
	recorder := observation.New(observation.Options{
		Enabled:       true,
		Directory:     cfg.ChannelStoreDir,
		RetentionDays: cfg.ChannelRetentionDays,
		MaxSizeMB:     cfg.ChannelMaxSizeMB,
		Baseline:      cfg.ChannelBaselineProvider,
		// The gateway channel half of every record: the facts the CPA request-log scanner
		// parsed. The live ring alone is not enough — it is empty after a restart and holds only
		// the newest facts — so the source merges it with the persisted fact files, which is
		// what makes a 24-hour window answerable. Resolved at read time because the scanner is
		// published just after this recorder (startChannelLog runs on the next line of
		// LoadConfig).
		Facts: observation.FactsFromStoreAndScanner(cfg.ChannelStoreDir, observation.FactsFromChannelLog(state.ChannelLog)),
	})
	recorder.Start()
	observationMu.Lock()
	observationRecorder = recorder
	observationMu.Unlock()
	state.SetObservation(recorder)
	observation.SetActive(recorder)
}

// channelLogScanner is the running CPA request-log scanner. Like the recorder, it is stopped
// before a reconfigure starts a new one so a reload can never leave two readers on the same
// directory: two scanners would race for the same files.
var (
	channelLogMu      sync.Mutex
	channelLogScanner *channellog.Scanner
)

// stopChannelLog stops the scanner, if one is running, and makes the management view inert.
func stopChannelLog() {
	channelLogMu.Lock()
	scanner := channelLogScanner
	channelLogScanner = nil
	channelLogMu.Unlock()
	state.SetChannelLog(nil)
	if scanner != nil {
		scanner.Stop()
	}
}

// startChannelLog starts the scanner when the configuration asks for it. Nothing happens when
// it does not: no goroutine, no directory access, which is what makes the default (off) state
// free of any cost at all.
//
// The facts are appended to the channel store directory the observation collector also uses.
// That directory is a SUBDIRECTORY of the directory being scanned, which is one of the reasons
// the scan never recurses.
func startChannelLog(cfg config.Config) {
	if !cfg.ChannelLogEnabled {
		return
	}
	scanner := channellog.New(channellog.Options{
		Enabled:         true,
		Dir:             cfg.ChannelLogDir,
		StoreDir:        cfg.ChannelStoreDir,
		MinAge:          time.Duration(cfg.ChannelLogMinAgeSeconds) * time.Second,
		DeleteAfterRead: cfg.ChannelLogDeleteAfterRead,
		// The fact files are a sidecar of the observation records: they expire on the same
		// clock, so channel_retention_days configures both.
		RetentionDays: cfg.ChannelRetentionDays,
	})
	scanner.Start()
	channelLogMu.Lock()
	channelLogScanner = scanner
	channelLogMu.Unlock()
	state.SetChannelLog(scanner)
}

// Shutdown stops the background work.
func Shutdown() {
	stopObservation()
	stopChannelLog()
	if poller := state.Plan(); poller != nil {
		poller.Stop()
	}
}

// HandleMethod dispatches one ABI call. Every registered capability must have a case here.
func HandleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		LoadConfig(configYAMLFromLifecycleRequest(request))
		if method == pluginabi.MethodPluginRegister {
			// A single line on load makes it obvious which build CPA is running, which
			// matters because CPA replaces a plugin by file name rather than content.
			hostapi.LogAsync("info", buildinfo.ID+": loaded", map[string]string{
				"version": buildinfo.Version,
			})
		}
		return abi.OK(buildRegistration())
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		// The scanner is a background poller with no way to be useful while the plugin is out
		// of service, so it goes down with the lifecycle call. The published configuration and
		// the usage collector stay as they are: a quiesce can be followed by a resume rather
		// than by a reload, and this call carries no new configuration to act on.
		stopChannelLog()
		return abi.OK(nil)
	case pluginabi.MethodManagementRegister:
		return abi.OK(buildManagementRegistration())
	case pluginabi.MethodManagementHandle:
		return management.Handle(request)
	case pluginabi.MethodUsageHandle:
		return observation.HandleUsage(request)
	default:
		return abi.Failure("unknown_method", "unknown method: "+method), nil
	}
}

// lifecycleRequest is the payload of plugin.register / plugin.reconfigure.
type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

func configYAMLFromLifecycleRequest(raw []byte) []byte {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil
	}
	var req lifecycleRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		// The host may also hand over the raw config block itself.
		return raw
	}
	return req.ConfigYAML
}

func buildRegistration() registration {
	cfg := state.Config()
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             buildinfo.Name,
			Version:          buildinfo.Version,
			Author:           buildinfo.Author,
			GitHubRepository: buildinfo.Repository,
			ConfigFields: []pluginapi.ConfigField{
				{Name: "plan_config_path", Type: pluginapi.ConfigFieldTypeString, Description: "默认：留空（自动探测 /CLIProxyAPI/config.yaml → /app/config.yaml）。容器内 CPA config.yaml 的路径，插件从中读取 Cline 凭据；只有自动探测选错文件时才需要填。"},
				{Name: "plan_refresh", Type: pluginapi.ConfigFieldTypeString, Description: "默认 5m（最小 1 分钟）。官方套餐、限额与官方用量的轮询周期。"},
				{Name: "channel_observe_enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "默认 true。逐请求渠道观测开关。开启时插件声明 usage_plugin 能力，宿主每完成一个请求回调一次并带上所落渠道（无论客户端说哪种协议），记录写入 JSONL 并聚合成「渠道」视图；关闭时不声明该能力，请求路径上零开销，但页面不再有新数据。"},
				{Name: "channel_store_dir", Type: pluginapi.ConfigFieldTypeString, Description: "默认 /CLIProxyAPI/logs/channel-observation。渠道记录的存放目录，按天一个 channel-<date>.jsonl。"},
				{Name: "channel_retention_days", Type: pluginapi.ConfigFieldTypeNumber, Description: "默认 3（上限 30）。渠道记录保留天数。"},
				{Name: "channel_max_size_mb", Type: pluginapi.ConfigFieldTypeNumber, Description: "默认 512（下限 16）。渠道记录目录的总大小上限，单位 MB；超出后从最旧的文件开始删。"},
				{Name: "channel_baseline_provider", Type: pluginapi.ConfigFieldTypeString, Description: "默认 deepseek。基准渠道名：finalProvider/resolvedProvider 不等于它的请求计入「未落在基准渠道」。取值以页面「真实渠道」表里出现的名字为准，不一致时偏离比例会失真——管理页顶部的「配置自检」会把窗口内实际出现的渠道名列出来。"},
				{Name: "channel_log_enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "默认 false。CPA 请求日志扫描开关。CPA 在 observability.logs.request-log 打开且 server.commercial-mode 关闭时，会把每个请求的完整调试日志写进 channel_log_dir，而上游 chat 响应原文里的 gateway.routing 渠道块只有这些日志能看到（CPA 翻译成 Responses 时丢掉了它）。开启后插件在旁路轮询该目录、解析 .log、把结论写入 channel_store_dir；关闭时不启协程、不访问目录。打开前请先确认 CPA 侧 request-log 已开、commercial-mode 已关并**重启过容器**；管理页顶部的「配置自检」会按当前状态列出还缺哪一项。"},
				{Name: "channel_log_dir", Type: pluginapi.ConfigFieldTypeString, Description: "默认 /CLIProxyAPI/logs（容器内路径）。CPA 请求日志目录。只扫描该目录顶层的 *.log，不递归子目录，且跳过 main.log。"},
				{Name: "channel_log_delete_after_read", Type: pluginapi.ConfigFieldTypeBoolean, Description: "默认 true。解析并落盘后删除日志文件。这些文件含明文 prompt，读完即 unlink，磁盘占用只与一个轮询窗口有关；解析失败的文件不删。"},
				{Name: "channel_log_min_age_seconds", Type: pluginapi.ConfigFieldTypeNumber, Description: "默认 5。只读取 mtime 早于该秒数的日志文件，避免读到 CPA 正在写的半个请求；读取前后都比对文件大小，变大的文件留到下一轮。"},
				{Name: "account_guard_enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "默认 false。账号守卫总开关。打开后，某个账号的真实渠道连续 N 次不是基准渠道（channel_baseline_provider）就把该 openai-compatibility 条目的 disabled 置 true，再按 account_guard_reenable_minutes 起、每次翻倍的退避自动放回。它只守 **Cline 条目**（base-url 的 host 命中 hosts，或条目名恰为 Cline）；同一个列表里别的供应商不进判定、不会被关。它会**写 CPA 的 config.yaml**（只改那一个标量，改完由 CPA 自己的 watcher 热加载），所以默认关闭；第一次打开请让 account_guard_dry_run 保持 true。没有真实渠道数据时（channel_log_enabled=false 或渠道没 join 上）守卫不判定，也不会误关账号。"},
				{Name: "account_guard_dry_run", Type: pluginapi.ConfigFieldTypeBoolean, Description: "默认 true。只算不动：守卫照常算连击、写审计和页面状态，但不改 config.yaml，只在宿主日志里写 account guard (dry run)。先这样观察一两天，确认没有误报再改成 false。"},
				{Name: "account_guard_threshold", Type: pluginapi.ConfigFieldTypeNumber, Description: "默认 3。连续多少次非基准渠道就关这个账号。没有渠道块、没 join 上的请求既不计数也不清零；基准渠道命中一次清零。"},
				{Name: "account_guard_min_enabled", Type: pluginapi.ConfigFieldTypeNumber, Description: "默认 1（0 或负数按 1 处理）。至少保留几个可用账号：守卫不会把账号全部关掉。"},
				{Name: "account_guard_scope_names", Type: pluginapi.ConfigFieldTypeString, Description: "默认：留空（守所有 Cline 条目）。在 Cline 条目之上再缩小范围：只守这些条目名（不区分大小写；YAML 列表或逗号分隔）。第一次上线建议只写一个账号名试。"},
				{Name: "account_guard_reenable_minutes", Type: pluginapi.ConfigFieldTypeNumber, Description: "默认 5。被守卫关掉的账号过多少分钟自动放回；每次再犯翻倍，直到 account_guard_max_disable_minutes。0 = 永不自动放回，只能人工改回。"},
				{Name: "account_guard_max_disable_minutes", Type: pluginapi.ConfigFieldTypeNumber, Description: "默认 360 分钟。退避上限：账号被关得越频繁，放回前等得越久，但不会超过这个值。"},
			},
		},
		Capabilities: registrationCapability{
			ManagementAPI: true,
			// Only declared while the collector runs: an undeclared capability costs
			// nothing, a declared one costs one ABI call per request.
			UsagePlugin: cfg.ChannelObserveEnabled,
		},
	}
}

func buildManagementRegistration() pluginapi.ManagementRegistrationResponse {
	return pluginapi.ManagementRegistrationResponse{
		// The host only forwards the paths declared here (internal/pluginhost/management.go:57
		// registers an exact match per declared route), so every management endpoint the
		// plugin serves must be listed or the host's own router answers 404.
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: management.BasePath + "/health"},
			{Method: http.MethodGet, Path: management.BasePath + "/channel"},
			{Method: http.MethodGet, Path: management.BasePath + "/channel.csv"},
		},
		Resources: []pluginapi.ResourceRoute{
			{Path: "/index.html", Menu: buildinfo.Name, Description: buildinfo.Description},
		},
	}
}
