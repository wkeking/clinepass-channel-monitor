package plugin

import "testing"

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

// TestDeclaresManagementOnly is the regression guard for the reason this release exists:
// every request-path capability makes CPA clone both request bodies and hand them across
// the plugin ABI for each streamed frame. Declaring none of them keeps the plugin off the
// request path entirely.
func TestDeclaresManagementOnly(t *testing.T) {
	caps := buildRegistration().Capabilities
	if !caps.ManagementAPI {
		t.Error("the plugin must keep its Management API capability")
	}
	for name, declared := range map[string]bool{
		"request_interceptor":        caps.RequestInterceptor,
		"response_before_translator": caps.ResponseBeforeTranslator,
		"usage_plugin":               caps.UsagePlugin,
		"request_normalizer":         caps.RequestNormalizer,
		"response_translator":        caps.ResponseTranslator,
		"response_after_translator":  caps.ResponseAfterTranslator,
		"request_translator":         caps.RequestTranslator,
		"thinking_applier":           caps.ThinkingApplier,
		"executor":                   caps.Executor,
	} {
		if declared {
			t.Errorf("%s must not be declared: it would put the plugin back on the request path", name)
		}
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
