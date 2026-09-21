package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	pluginID          = "clinepass-channel-monitor"
	pluginName        = "Channel Monitor"
	pluginAuthor      = "wkeking"
	pluginRepository  = "https://github.com/wkeking/clinepass-channel-monitor"
	pluginDescription = "Per-request Cline channel, usage, and cost monitoring with a management page."
)

// pluginVersion is a variable so release builds can stamp it with -ldflags.
var pluginVersion = "0.1.0"

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

var (
	stateMu sync.RWMutex
	state   = pluginState{
		Config:     defaultConfig(),
		Identities: newIdentityTable(),
	}
)

// pluginState holds everything shared between the hook callbacks.
type pluginState struct {
	Config config
	Store  *store
	// Identities correlates the request, response and usage hooks of one request.
	Identities *identityTable
}

func loadConfig(raw []byte) {
	cfg, errParse := parseConfig(raw)
	if errParse != nil {
		// Fail open: keep defaults so the plugin still loads and still records.
		hostLogAsync("warn", "clinepass-channel-monitor: falling back to default config", map[string]string{
			"error": errParse.Error(),
		})
		cfg = defaultConfig()
	}
	stateMu.Lock()
	state.Config = cfg
	if state.Store == nil {
		state.Store = newStore(cfg)
	} else {
		state.Store.reconfigure(cfg)
	}
	state.Identities = newIdentityTable()
	stateMu.Unlock()
	hostLogAsync("info", "clinepass-channel-monitor: configured", map[string]string{
		"mode":          cfg.matchMode(),
		"hosts":         strings.Join(cfg.Hosts, ","),
		"jsonl_enabled": strconv.FormatBool(cfg.JSONLEnabled),
		"jsonl_dir":     cfg.JSONLDir,
	})
}

func currentConfig() config {
	stateMu.RLock()
	defer stateMu.RUnlock()
	return state.Config
}

func currentStore() *store {
	stateMu.RLock()
	defer stateMu.RUnlock()
	return state.Store
}

func currentIdentities() *identityTable {
	stateMu.RLock()
	defer stateMu.RUnlock()
	if state.Identities == nil {
		return newIdentityTable()
	}
	return state.Identities
}

func shutdown() {
	sinkShutdown()
	stateMu.Lock()
	defer stateMu.Unlock()
	if state.Store != nil {
		state.Store.close()
	}
}

// handleMethod dispatches one ABI call. Every registered capability must have a case here.
func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		loadConfig(configYAMLFromLifecycleRequest(request))
		if method == pluginabi.MethodPluginRegister {
			// A single line on load makes it obvious which build CPA is running, which
			// matters because CPA replaces a plugin by file name rather than content.
			hostLogAsync("info", "clinepass-channel-monitor: loaded", map[string]string{
				"version": pluginVersion,
			})
		}
		return okEnvelope(buildRegistration())
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		return okEnvelope(nil)
	case pluginabi.MethodResponseNormalizeBefore:
		return handleResponseNormalizeBefore(request)
	case pluginabi.MethodRequestInterceptBefore:
		return handleRequestInterceptBefore(request)
	case pluginabi.MethodRequestInterceptAfter:
		return handleRequestInterceptAfter(request)
	case pluginabi.MethodRequestComplete:
		return handleRequestComplete(request)
	case pluginabi.MethodUsageHandle:
		return handleUsage(request)
	case pluginabi.MethodManagementRegister:
		return okEnvelope(buildManagementRegistration())
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
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
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           pluginAuthor,
			GitHubRepository: pluginRepository,
			ConfigFields: []pluginapi.ConfigField{
				{Name: "hosts", Type: pluginapi.ConfigFieldTypeArray, Description: "Base URL hosts whose requests are recorded. Entries starting with '.' match a domain suffix; an empty list records every request that carries gateway routing metadata."},
				{Name: "require_routing_marker", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Require provider_metadata.gateway.routing to be present in the upstream response before a request is recorded."},
				{Name: "unmatched_host_samples", Type: pluginapi.ConfigFieldTypeInteger, Description: "How many host-mismatch samples to keep for self-diagnosis."},
				{Name: "ring_size", Type: pluginapi.ConfigFieldTypeInteger, Description: "In-memory ring buffer size for the management page."},
				{Name: "jsonl_enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Append one JSON object per recorded request to a daily JSONL file."},
				{Name: "jsonl_dir", Type: pluginapi.ConfigFieldTypeString, Description: "Directory for JSONL files. Relative paths resolve against the CPA working directory."},
				{Name: "retention_days", Type: pluginapi.ConfigFieldTypeInteger, Description: "Delete JSONL files older than this many days."},
				{Name: "join_window", Type: pluginapi.ConfigFieldTypeString, Description: "Time window used to join a channel observation with its usage record (for example 5s)."},
				{Name: "orphan_ttl", Type: pluginapi.ConfigFieldTypeString, Description: "How long an unconsumed channel observation is kept before it counts as an orphan (for example 60s)."},
				{Name: "mask_api_key", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Store only the first and last four characters of the client API key."},
				{Name: "log_events", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Also write one CPA log line per recorded request."},
				{Name: "capture_cost", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Record gateway cost fields."},
				{Name: "capture_cache", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Record cache counters."},
				{Name: "store_planning_reasoning", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Store the gateway planning text. When false only its length is stored."},
				{Name: "timezone", Type: pluginapi.ConfigFieldTypeString, Description: "Timezone used for display and timestamps."},
				{Name: "sample_rate", Type: pluginapi.ConfigFieldTypeInteger, Description: "Record every Nth request (1-100)."},
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
			{Method: http.MethodGet, Path: managementBasePath + "/stats"},
			{Method: http.MethodGet, Path: managementBasePath + "/events"},
			{Method: http.MethodGet, Path: managementBasePath + "/health"},
			{Method: http.MethodGet, Path: managementBasePath + "/export"},
		},
		Resources: []pluginapi.ResourceRoute{
			{Path: "/index.html", Menu: pluginName, Description: pluginDescription},
		},
	}
}
