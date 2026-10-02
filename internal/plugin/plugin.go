// Package plugin wires the plugin together: it parses the configuration the host hands
// over, publishes the runtime state, and dispatches every ABI method CPA calls.
//
// The plugin declares ManagementAPI, and — only while channel observation is switched on —
// the stream chunk interceptor that reports which upstream channel each response came from.
// It declares no request-side capability and no response translator: the host strips the
// request bodies and the recent-chunk history from the payload chunks of a schema v6
// plugin, so what crosses the ABI per frame is the frame itself.
package plugin

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/wkeking/clinepass-channel-monitor/internal/abi"
	"github.com/wkeking/clinepass-channel-monitor/internal/buildinfo"
	"github.com/wkeking/clinepass-channel-monitor/internal/config"
	"github.com/wkeking/clinepass-channel-monitor/internal/hostapi"
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
	UsagePlugin              bool `json:"usage_plugin"`
	CommandLinePlugin        bool `json:"command_line_plugin"`
	ManagementAPI            bool `json:"management_api"`
	// StreamChunkInterceptor is the only hook on the response path this plugin ever
	// declares, and only while channel observation is on. With it switched off the host
	// reports no stream interceptor at all, which is what keeps the per-frame cost at zero.
	StreamChunkInterceptor bool `json:"response_stream_interceptor"`
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
	state.SetConfig(cfg)
	if cfg.PlanEnabled {
		state.SetPlan(plan.Start(cfg))
	}
	startObservation(cfg)
	hostapi.LogAsync("info", buildinfo.ID+": configured", map[string]string{
		"plan_enabled":      strconv.FormatBool(cfg.PlanEnabled),
		"plan_refresh":      cfg.PlanRefresh.Or(config.DefaultPlanRefresh).String(),
		"plan_usage":        strconv.FormatBool(cfg.PlanUsageEnabled),
		"plan_config_path":  cfg.PlanConfigPath,
		"channel_observe":   strconv.FormatBool(cfg.ChannelObserveEnabled),
		"channel_store_dir": cfg.ChannelStoreDir,
		"channel_baseline":  cfg.ChannelBaselineProvider,
	})
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
	})
	recorder.Start()
	observationMu.Lock()
	observationRecorder = recorder
	observationMu.Unlock()
	state.SetObservation(recorder)
	observation.SetActive(recorder)
}

// Shutdown stops the background work.
func Shutdown() {
	stopObservation()
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
		return abi.OK(nil)
	case pluginabi.MethodManagementRegister:
		return abi.OK(buildManagementRegistration())
	case pluginabi.MethodManagementHandle:
		return management.Handle(request)
	case pluginabi.MethodResponseInterceptStreamChunk:
		return observation.Handle(request)
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
				{Name: "timezone", Type: pluginapi.ConfigFieldTypeString, Description: "展示时区。官方接口返回的是绝对时间，页面按浏览器本地时区渲染，所以本版本没有代码读取它；保留是为了兼容既有配置块。"},
				{Name: "plan_config_path", Type: pluginapi.ConfigFieldTypeString, Description: "容器内 CPA config.yaml 的路径，用于读取 Cline 凭据；留空时按 /CLIProxyAPI/config.yaml → /app/config.yaml 自动探测。"},
				{Name: "plan_refresh", Type: pluginapi.ConfigFieldTypeString, Description: "官方套餐、限额与官方用量的轮询周期（例如 5m）。留空即用默认 5m，最小 1 分钟。"},
				{Name: "channel_observe_enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "逐请求渠道观测开关（默认 true）。开启时插件声明 response_stream_interceptor，对每个流式分片做一次字节扫描，把上游渠道记进 JSONL 并聚合成「渠道」视图；关闭时不声明该能力，请求路径上零开销，但页面不再有新数据。"},
				{Name: "channel_store_dir", Type: pluginapi.ConfigFieldTypeString, Description: "渠道记录的存放目录（默认 /CLIProxyAPI/logs/channel-observation），按天一个 channel-<date>.jsonl。"},
				{Name: "channel_retention_days", Type: pluginapi.ConfigFieldTypeNumber, Description: "渠道记录保留天数（默认 3，上限 30）。"},
				{Name: "channel_max_size_mb", Type: pluginapi.ConfigFieldTypeNumber, Description: "渠道记录目录的总大小上限，单位 MB（默认 512，下限 16）；超出后从最旧的文件开始删。"},
				{Name: "channel_baseline_provider", Type: pluginapi.ConfigFieldTypeString, Description: "基准渠道名（默认 deepseek）。finalProvider/resolvedProvider 不等于它的请求计入「未落在基准渠道」。"},
			},
		},
		Capabilities: registrationCapability{
			ManagementAPI: true,
			// Only declared while the collector runs: an undeclared capability costs
			// nothing, a declared one costs one ABI call per streamed frame.
			StreamChunkInterceptor: cfg.ChannelObserveEnabled,
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
