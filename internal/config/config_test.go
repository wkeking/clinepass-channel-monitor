package config

import (
	"testing"
	"time"
)

// TestFixedDefaults pins the values that are no longer offered in the configuration panel.
// They are behaviour, not preferences: the panel hides them, so a silent change here would
// only be noticed in production.
func TestFixedDefaults(t *testing.T) {
	cfg := Default()
	if cfg.MaskAPIKey {
		t.Error("mask_api_key must default to false")
	}
	if !cfg.LogEvents {
		t.Error("log_events must default to true")
	}
	if cfg.StorePlanningReasoning {
		t.Error("store_planning_reasoning must default to false")
	}
	if !cfg.PlanEnabled || !cfg.PlanDailyEnabled || !cfg.PlanUsageEnabled {
		t.Errorf("plan switches must default to true, got enabled=%v daily=%v usage=%v",
			cfg.PlanEnabled, cfg.PlanDailyEnabled, cfg.PlanUsageEnabled)
	}
	if got := cfg.PlanUsageRefresh.Or(0); got != 10*time.Minute {
		t.Errorf("plan_usage_refresh default = %s, want 10m", got)
	}
	if got := cfg.PlanRefresh.Or(0); got != 5*time.Minute {
		t.Errorf("plan_refresh default = %s, want 5m", got)
	}
}

// TestParseIgnoresRemovedKeys keeps an existing configuration block loadable: a deployment
// that still carries sample_rate (or any other key the plugin no longer reads) must not fail
// to load.
func TestParseIgnoresRemovedKeys(t *testing.T) {
	cfg, errParse := Parse([]byte("log_events: true\nsample_rate: 50\nplan_usage_refresh: 15m\n"))
	if errParse != nil {
		t.Fatalf("Parse() with a removed key: %v", errParse)
	}
	if !cfg.LogEvents {
		t.Error("log_events: true must still be read")
	}
	if got := cfg.PlanUsageRefresh.Or(0); got != 15*time.Minute {
		t.Errorf("plan_usage_refresh = %s, want 15m", got)
	}
}
