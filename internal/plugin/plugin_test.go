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
	for _, name := range []string{"timezone", "plan_config_path", "plan_refresh"} {
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

// TestManagementRoutesAreHealthOnly pins the served routes: the three statistics endpoints
// are gone, and 404 is the expected answer for them.
func TestManagementRoutesAreHealthOnly(t *testing.T) {
	routes := buildManagementRegistration().Routes
	if len(routes) != 1 || routes[0].Path != "/v0/management/plugins/clinepass-channel-monitor/health" {
		t.Fatalf("routes = %+v, want /health only", routes)
	}
}
