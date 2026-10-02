package plugin

import (
	"encoding/json"
	"testing"

	"github.com/wkeking/clinepass-channel-monitor/internal/state"
)

// capabilitiesWithObservation renders the declared capabilities the way the host receives
// them, with channel observation forced on or off. The JSON is what the host reads, so
// asserting on it is also what catches a capability that is declared by accident: a field
// that does not exist in the struct cannot appear in the object at all.
func capabilitiesWithObservation(t *testing.T, enabled bool) map[string]bool {
	t.Helper()
	previous := state.Config()
	defer state.SetConfig(previous)
	cfg := previous
	cfg.ChannelObserveEnabled = enabled
	state.SetConfig(cfg)

	raw, errMarshal := json.Marshal(buildRegistration().Capabilities)
	if errMarshal != nil {
		t.Fatalf("marshal capabilities: %v", errMarshal)
	}
	declared := map[string]bool{}
	if errUnmarshal := json.Unmarshal(raw, &declared); errUnmarshal != nil {
		t.Fatalf("capabilities are not a JSON object: %v (%s)", errUnmarshal, raw)
	}
	return declared
}

// TestPanelHidesFixedDefaults pins the configuration panel contract: only the knobs a
// deployment genuinely has to move are offered there, while the fixed defaults stay out.
func TestPanelHidesFixedDefaults(t *testing.T) {
	fields := map[string]bool{}
	for _, field := range buildRegistration().Metadata.ConfigFields {
		fields[field.Name] = true
	}
	for _, name := range []string{
		"mask_api_key", "log_events", "store_planning_reasoning", "sample_rate",
		"plan_enabled", "plan_api_key", "plan_base_url", "plan_daily_enabled",
		"plan_usage_enabled", "plan_usage_refresh",
		// The per-request statistics keys are gone from the plugin entirely.
		"hosts", "require_routing_marker", "unmatched_host_samples", "ring_size",
		"jsonl_enabled", "jsonl_dir", "retention_days", "join_window", "orphan_ttl",
		"capture_cost", "capture_cache",
	} {
		if fields[name] {
			t.Errorf("%s must not be exposed in the configuration panel", name)
		}
	}
	for _, name := range []string{
		"timezone", "plan_config_path", "plan_refresh",
		// The channel observation knobs must stay reachable from the panel: turning the
		// collector off is the documented rollback for the per-frame scan cost.
		"channel_observe_enabled", "channel_store_dir", "channel_retention_days",
		"channel_max_size_mb", "channel_baseline_provider",
	} {
		if !fields[name] {
			t.Errorf("%s must stay in the configuration panel", name)
		}
	}
}

// TestDeclaresManagementAndUsageOnly is the regression guard for the reason this release
// exists: every request-path capability makes CPA clone request bodies and hand them across
// the plugin ABI, and the old response_stream_interceptor only ever saw OpenAI chat traffic.
// The usage hook is the one capability this plugin declares, and only while the collector
// runs; nothing may come back that puts the plugin on the request path.
func TestDeclaresManagementAndUsageOnly(t *testing.T) {
	declared := capabilitiesWithObservation(t, true)
	if !declared["management_api"] {
		t.Error("the plugin must keep its Management API capability")
	}
	if !declared["usage_plugin"] {
		t.Error("usage_plugin must be declared while channel observation is on")
	}
	for _, name := range []string{
		"request_interceptor",
		"request_translator",
		"request_normalizer",
		"response_translator",
		"response_before_translator",
		"response_after_translator",
		"response_stream_interceptor",
		"thinking_applier",
		"executor",
		"model_registrar",
		"model_provider",
		"auth_provider",
		"frontend_auth_provider",
		"command_line_plugin",
	} {
		if declared[name] {
			t.Errorf("%s must not be declared: it would put the plugin back on the request path", name)
		}
	}
}

// TestUsageCapabilityFollowsObservation pins the switch: with observation off the host must
// not call usage.handle at all, which is what keeps the disabled state free of any per-request
// work.
func TestUsageCapabilityFollowsObservation(t *testing.T) {
	if declared := capabilitiesWithObservation(t, false); declared["usage_plugin"] {
		t.Error("usage_plugin must not be declared while channel observation is off")
	}
	if declared := capabilitiesWithObservation(t, true); !declared["usage_plugin"] || !declared["management_api"] {
		t.Error("turning observation back on must declare usage_plugin and keep the Management API capability")
	}
}

// TestManagementRoutesAreDataEndpoints pins the served routes: the v0.1.x statistics
// endpoints stay gone, while the channel view endpoints must be declared here because the
// host only forwards declared paths (an undeclared one is answered by the host's own 404).
func TestManagementRoutesAreDataEndpoints(t *testing.T) {
	const base = "/v0/management/plugins/clinepass-channel-monitor"
	want := []string{base + "/health", base + "/channel", base + "/channel.csv"}
	routes := buildManagementRegistration().Routes
	if len(routes) != len(want) {
		t.Fatalf("routes = %+v, want %v", routes, want)
	}
	for i, path := range want {
		if routes[i].Path != path {
			t.Errorf("routes[%d] = %q, want %q", i, routes[i].Path, path)
		}
	}
	for _, gone := range []string{base + "/stats", base + "/events", base + "/export"} {
		for _, route := range routes {
			if route.Path == gone {
				t.Errorf("route %q must not exist: the v0.1.x statistics endpoints stay gone", route.Path)
			}
		}
	}
}
