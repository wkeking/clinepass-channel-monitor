package plugin

import "testing"

// TestPanelHidesFixedDefaults pins the configuration panel contract: the keys a deployment
// should not have to think about are not offered there, while the ones that describe the
// deployment (judgement, storage, credential location, poll interval) stay.
func TestPanelHidesFixedDefaults(t *testing.T) {
	fields := map[string]bool{}
	for _, field := range buildRegistration().Metadata.ConfigFields {
		fields[field.Name] = true
	}
	for _, name := range []string{
		"mask_api_key", "log_events", "store_planning_reasoning", "sample_rate",
		"plan_enabled", "plan_api_key", "plan_base_url", "plan_daily_enabled",
		"plan_usage_enabled", "plan_usage_refresh",
	} {
		if fields[name] {
			t.Errorf("%s must not be exposed in the configuration panel", name)
		}
	}
	for _, name := range []string{
		"hosts", "require_routing_marker", "unmatched_host_samples", "ring_size",
		"jsonl_enabled", "jsonl_dir", "retention_days", "join_window", "orphan_ttl",
		"capture_cost", "capture_cache", "timezone", "plan_config_path", "plan_refresh",
	} {
		if !fields[name] {
			t.Errorf("%s must stay in the configuration panel", name)
		}
	}
}
