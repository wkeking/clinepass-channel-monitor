// Package plugin wires the plugin together: it parses the configuration the host hands
// over, publishes the runtime state, and dispatches every ABI method CPA calls.
package plugin

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/wkeking/clinepass-channel-monitor/internal/abi"
	"github.com/wkeking/clinepass-channel-monitor/internal/buildinfo"
	"github.com/wkeking/clinepass-channel-monitor/internal/config"
	"github.com/wkeking/clinepass-channel-monitor/internal/hooks"
	"github.com/wkeking/clinepass-channel-monitor/internal/hostapi"
	"github.com/wkeking/clinepass-channel-monitor/internal/management"
	"github.com/wkeking/clinepass-channel-monitor/internal/plan"
	"github.com/wkeking/clinepass-channel-monitor/internal/state"
	"github.com/wkeking/clinepass-channel-monitor/internal/store"
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
// records. A reconfigure also starts a fresh in-memory view: the ring buffer and the counters
// describe this instance only, while the JSONL files keep the full history.
func LoadConfig(raw []byte) {
	cfg, errParse := config.Parse(raw)
	if errParse != nil {
		hostapi.LogAsync("warn", buildinfo.ID+": falling back to default config", map[string]string{
			"error": errParse.Error(),
		})
		cfg = config.Default()
	}
	if existing := state.Store(); existing == nil {
		state.SetStore(store.New(cfg))
	} else {
		existing.Reconfigure(cfg)
	}
	state.Store().Reset()
	hooks.Reset()
	if poller := state.Plan(); poller != nil {
		poller.Stop()
		state.SetPlan(nil)
	}
	state.SetConfig(cfg)
	if cfg.PlanEnabled {
		state.SetPlan(plan.Start(cfg))
	}
	hostapi.LogAsync("info", buildinfo.ID+": configured", map[string]string{
		"mode":          cfg.MatchMode(),
		"hosts":         strings.Join(cfg.Hosts, ","),
		"jsonl_enabled": strconv.FormatBool(cfg.JSONLEnabled),
		"jsonl_dir":     cfg.JSONLDir,
	})
}

// Shutdown flushes the JSONL queue and stops the background work.
func Shutdown() {
	store.Shutdown()
	if poller := state.Plan(); poller != nil {
		poller.Stop()
	}
	if st := state.Store(); st != nil {
		st.Close()
	}
}

// handleMethod dispatches one ABI call. Every registered capability must have a case here.
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
	case pluginabi.MethodResponseNormalizeBefore:
		return hooks.ResponseNormalizeBefore(request)
	case pluginabi.MethodRequestInterceptBefore:
		return hooks.RequestInterceptBefore(request)
	case pluginabi.MethodRequestInterceptAfter:
		return hooks.RequestInterceptAfter(request)
	case pluginabi.MethodRequestComplete:
		return hooks.RequestComplete(request)
	case pluginabi.MethodUsageHandle:
		return hooks.Usage(request)
	case pluginabi.MethodManagementRegister:
		return abi.OK(buildManagementRegistration())
	case pluginabi.MethodManagementHandle:
		return management.Handle(request)
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
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             buildinfo.Name,
			Version:          buildinfo.Version,
			Author:           buildinfo.Author,
			GitHubRepository: buildinfo.Repository,
			ConfigFields: []pluginapi.ConfigField{
				{Name: "hosts", Type: pluginapi.ConfigFieldTypeArray, Description: "需要记录的请求所属的主机名（Base URL 的 host）。以 . 开头表示匹配域名后缀；留空表示记录所有携带网关路由元数据的请求。"},
				{Name: "require_routing_marker", Type: pluginapi.ConfigFieldTypeBoolean, Description: "严格模式：只有响应里出现过 provider_metadata.gateway.routing 的请求才记录。默认关闭，此时所有命中 hosts 的请求都会记录，渠道值优先取 finalProvider，其次取上游响应的 provider 字段，两者都没有时留空。"},
				{Name: "unmatched_host_samples", Type: pluginapi.ConfigFieldTypeInteger, Description: "保留多少条主机名不匹配的样本，用于自诊断。"},
				{Name: "ring_size", Type: pluginapi.ConfigFieldTypeInteger, Description: "管理页使用的内存环形缓冲区条数上限。"},
				{Name: "jsonl_enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "为每条已记录的请求追加一行 JSON 到按天切分的 JSONL 文件。"},
				{Name: "jsonl_dir", Type: pluginapi.ConfigFieldTypeString, Description: "JSONL 文件所在目录。相对路径相对于 CPA 的工作目录解析。"},
				{Name: "retention_days", Type: pluginapi.ConfigFieldTypeInteger, Description: "超过该天数的 JSONL 文件会被删除。"},
				{Name: "join_window", Type: pluginapi.ConfigFieldTypeString, Description: "把渠道观测与用量记录关联起来的时间窗（例如 5s）。"},
				{Name: "orphan_ttl", Type: pluginapi.ConfigFieldTypeString, Description: "未被消费的渠道观测保留多久后算作孤儿（例如 60s）。"},
				{Name: "capture_cost", Type: pluginapi.ConfigFieldTypeBoolean, Description: "记录上游网关返回的成本字段。"},
				{Name: "capture_cache", Type: pluginapi.ConfigFieldTypeBoolean, Description: "记录缓存计数。"},
				{Name: "timezone", Type: pluginapi.ConfigFieldTypeString, Description: "展示与时间戳使用的时区。"},
				{Name: "plan_config_path", Type: pluginapi.ConfigFieldTypeString, Description: "容器内 CPA config.yaml 的路径，用于读取 Cline 凭据。"},
				{Name: "plan_refresh", Type: pluginapi.ConfigFieldTypeString, Description: "官方套餐、限额与官方用量的轮询周期（例如 5m）。留空即用默认 5m，最小 1 分钟。"},
			},
		},
		Capabilities: registrationCapability{
			RequestInterceptor:       true,
			ResponseBeforeTranslator: true,
			UsagePlugin:              true,
			ManagementAPI:            true,
		},
	}
}

func buildManagementRegistration() pluginapi.ManagementRegistrationResponse {
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: management.BasePath + "/stats"},
			{Method: http.MethodGet, Path: management.BasePath + "/events"},
			{Method: http.MethodGet, Path: management.BasePath + "/health"},
			{Method: http.MethodGet, Path: management.BasePath + "/export"},
		},
		Resources: []pluginapi.ResourceRoute{
			{Path: "/index.html", Menu: buildinfo.Name, Description: buildinfo.Description},
		},
	}
}
