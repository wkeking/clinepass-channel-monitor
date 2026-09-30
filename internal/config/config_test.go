package config

import (
	"testing"
	"time"
)

// TestPlanDefaults pins the values that are no longer offered in the configuration panel.
// They are behaviour, not preferences: the panel hides them, so a silent change here would
// only be noticed in production.
func TestPlanDefaults(t *testing.T) {
	cfg := Default()
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
	if cfg.PlanBaseURL != DefaultPlanBaseURL {
		t.Errorf("plan_base_url default = %q, want %q", cfg.PlanBaseURL, DefaultPlanBaseURL)
	}
}

// TestHostsDefaultDrivesCredentialDiscovery pins the one host assumption credential
// discovery relies on: the plan poller collects the api keys of every CPA
// openai-compatibility entry whose base_url host is in this list.
func TestHostsDefaultDrivesCredentialDiscovery(t *testing.T) {
	cfg := Default()
	host, matched := cfg.HostMatched("https://api.cline.bot/api/v1")
	if !matched || host != "api.cline.bot" {
		t.Errorf("HostMatched(api.cline.bot) = (%q, %v), want (api.cline.bot, true)", host, matched)
	}
	if _, matched := cfg.HostMatched("https://api.deepseek.com"); matched {
		t.Error("a non-Cline base URL must not match")
	}
	// An explicit empty hosts list means "match Cline entries by name only", which is why
	// Parse has to keep the difference between absent and empty.
	empty, errParse := Parse([]byte("hosts: []\n"))
	if errParse != nil {
		t.Fatalf("Parse(): %v", errParse)
	}
	if len(empty.Hosts) != 0 {
		t.Errorf("hosts = %v, want an explicit empty list", empty.Hosts)
	}
}

// TestParseIgnoresRemovedKeys keeps an existing v0.1.x configuration block loadable: a
// deployment that still carries the per-request statistics keys must not fail to load.
func TestParseIgnoresRemovedKeys(t *testing.T) {
	cfg, errParse := Parse([]byte("jsonl_enabled: true\nring_size: 5000\nsample_rate: 50\nplan_usage_refresh: 15m\n"))
	if errParse != nil {
		t.Fatalf("Parse() with a removed key: %v", errParse)
	}
	if got := cfg.PlanUsageRefresh.Or(0); got != 15*time.Minute {
		t.Errorf("plan_usage_refresh = %s, want 15m", got)
	}
}
