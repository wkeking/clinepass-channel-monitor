// Package plugin wires the plugin together: it parses the configuration the host hands
// over, publishes the runtime state, and dispatches every ABI method CPA calls.
//
// The plugin declares ManagementAPI only: it is not on the request path at all, because
// the host clones both request bodies and hands them across the plugin ABI for every
// streamed frame, which costs far more than the observation was worth.
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
	"github.com/wkeking/clinepass-channel-monitor/internal/hostapi"
	"github.com/wkeking/clinepass-channel-monitor/internal/management"
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
	state.SetConfig(cfg)
	if cfg.PlanEnabled {
		state.SetPlan(plan.Start(cfg))
	}
	hostapi.LogAsync("info", buildinfo.ID+": configured", map[string]string{
		"plan_enabled":     strconv.FormatBool(cfg.PlanEnabled),
		"plan_refresh":     cfg.PlanRefresh.Or(config.DefaultPlanRefresh).String(),
		"plan_usage":       strconv.FormatBool(cfg.PlanUsageEnabled),
		"plan_config_path": cfg.PlanConfigPath,
	})
}

// Shutdown stops the background work.
func Shutdown() {
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
				{Name: "timezone", Type: pluginapi.ConfigFieldTypeString, Description: "展示时区。官方接口返回的是绝对时间，页面按浏览器本地时区渲染，所以本版本没有代码读取它；保留是为了兼容既有配置块。"},
				{Name: "plan_config_path", Type: pluginapi.ConfigFieldTypeString, Description: "容器内 CPA config.yaml 的路径，用于读取 Cline 凭据；留空时按 /CLIProxyAPI/config.yaml → /app/config.yaml 自动探测。"},
				{Name: "plan_refresh", Type: pluginapi.ConfigFieldTypeString, Description: "官方套餐、限额与官方用量的轮询周期（例如 5m）。留空即用默认 5m，最小 1 分钟。"},
			},
		},
		Capabilities: registrationCapability{
			ManagementAPI: true,
		},
	}
}

func buildManagementRegistration() pluginapi.ManagementRegistrationResponse {
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: management.BasePath + "/health"},
		},
		Resources: []pluginapi.ResourceRoute{
			{Path: "/index.html", Menu: buildinfo.Name, Description: buildinfo.Description},
		},
	}
}
