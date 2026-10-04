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
	if !cfg.PlanEnabled || !cfg.PlanDailyEnabled {
		t.Errorf("plan and daily must default to true, got enabled=%v daily=%v",
			cfg.PlanEnabled, cfg.PlanDailyEnabled)
	}
	// The per-request pull is off by default: the page no longer renders the official windows,
	// so the walk would only cost upstream requests. The switch still works when set.
	if cfg.PlanUsageEnabled {
		t.Error("plan_usage_enabled must default to false (the page no longer reads the official windows)")
	}
	reEnabled, errParse := Parse([]byte("plan_usage_enabled: true\n"))
	if errParse != nil {
		t.Fatalf("Parse(plan_usage_enabled: true): %v", errParse)
	}
	if !reEnabled.PlanUsageEnabled {
		t.Error("plan_usage_enabled: true must still turn the per-request pull back on")
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

// TestChannelLogDefaults pins the four request-log scanner knobs. The switch being off is
// behaviour, not a preference: the files the scanner reads carry the client's plaintext
// prompt, and CPA only writes them while its own request-log flag is on.
func TestChannelLogDefaults(t *testing.T) {
	cfg := Default()
	if cfg.ChannelLogEnabled {
		t.Error("channel_log_enabled must default to false")
	}
	if cfg.ChannelLogDir != DefaultChannelLogDir {
		t.Errorf("channel_log_dir default = %q, want %q", cfg.ChannelLogDir, DefaultChannelLogDir)
	}
	if !cfg.ChannelLogDeleteAfterRead {
		t.Error("channel_log_delete_after_read must default to true")
	}
	if cfg.ChannelLogMinAgeSeconds != DefaultChannelLogMinAgeSeconds {
		t.Errorf("channel_log_min_age_seconds default = %d, want %d",
			cfg.ChannelLogMinAgeSeconds, DefaultChannelLogMinAgeSeconds)
	}
}

// TestParseChannelLogKeys covers both directions of the block: the keys a deployment sets, and
// the defaults an older block keeps.
func TestParseChannelLogKeys(t *testing.T) {
	cfg, errParse := Parse([]byte("channel_log_enabled: true\nchannel_log_dir: /tmp/cpa-logs\n" +
		"channel_log_delete_after_read: false\nchannel_log_min_age_seconds: 30\n"))
	if errParse != nil {
		t.Fatalf("Parse(): %v", errParse)
	}
	if !cfg.ChannelLogEnabled {
		t.Error("channel_log_enabled = false, want the block's true")
	}
	if cfg.ChannelLogDir != "/tmp/cpa-logs" {
		t.Errorf("channel_log_dir = %q, want /tmp/cpa-logs", cfg.ChannelLogDir)
	}
	if cfg.ChannelLogDeleteAfterRead {
		t.Error("an explicit false must override the default true")
	}
	if cfg.ChannelLogMinAgeSeconds != 30 {
		t.Errorf("channel_log_min_age_seconds = %d, want 30", cfg.ChannelLogMinAgeSeconds)
	}

	// A block that predates the feature: everything keeps its default.
	older, errParse := Parse([]byte("timezone: UTC\nchannel_observe_enabled: true\n"))
	if errParse != nil {
		t.Fatalf("Parse() with an older block: %v", errParse)
	}
	if older.ChannelLogEnabled || older.ChannelLogDir != DefaultChannelLogDir ||
		!older.ChannelLogDeleteAfterRead || older.ChannelLogMinAgeSeconds != DefaultChannelLogMinAgeSeconds {
		t.Errorf("an older block must keep every scanner default: %+v", older)
	}

	// Values that cannot work are replaced by ones that can, instead of disabling the scanner
	// silently: an empty directory and a negative age.
	normalized, errParse := Parse([]byte("channel_log_dir: \"   \"\nchannel_log_min_age_seconds: -3\n"))
	if errParse != nil {
		t.Fatalf("Parse(): %v", errParse)
	}
	if normalized.ChannelLogDir != DefaultChannelLogDir {
		t.Errorf("channel_log_dir = %q, want the default for a blank value", normalized.ChannelLogDir)
	}
	if normalized.ChannelLogMinAgeSeconds != DefaultChannelLogMinAgeSeconds {
		t.Errorf("channel_log_min_age_seconds = %d, want the default for a negative value",
			normalized.ChannelLogMinAgeSeconds)
	}

	// Zero is a value, not a missing one: it means "read the file as soon as it appears".
	immediate, errParse := Parse([]byte("channel_log_min_age_seconds: 0\n"))
	if errParse != nil {
		t.Fatalf("Parse(): %v", errParse)
	}
	if immediate.ChannelLogMinAgeSeconds != 0 {
		t.Errorf("channel_log_min_age_seconds = %d, want the explicit 0", immediate.ChannelLogMinAgeSeconds)
	}
}
