// Package config parses and normalizes the plugin's configuration block.
//
// The host hands the block over as YAML (or JSON) through the register/reconfigure ABI
// call; the tags below are that contract, so they are treated as public API.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// DefaultPlanBaseURL is Cline's public API. It is a default, never a hard requirement.
	DefaultPlanBaseURL = "https://api.cline.bot/api/v1"
	// DefaultChannelStoreDir is where the per-request channel records are appended. It is a
	// directory of its own on purpose: v0.1.x wrote a different schema into
	// /CLIProxyAPI/logs/channel-monitor, which stays untouched and out of the size budget.
	DefaultChannelStoreDir = "/CLIProxyAPI/logs/channel-observation"
	// DefaultChannelBaselineProvider is the provider every other one is compared against
	// when the page answers "how much of the traffic did not land on the expected
	// channel". ClinePass answers "deepseek" today; it is configuration, not a constant,
	// because the expected channel is a property of the subscription, not of Cline.
	DefaultChannelBaselineProvider = "deepseek"
	// DefaultChannelLogDir is where CPA writes its per-request debug logs: /opt/cpa/logs on the
	// host, mounted at this path inside the container. It is the directory the plugin's own
	// store lives in a SUBDIRECTORY of, which is why the scanner never recurses.
	DefaultChannelLogDir = "/CLIProxyAPI/logs"
	// DefaultChannelLogMinAgeSeconds is how long a log file must have been untouched before the
	// scanner reads it. CPA appends to a file while the request is running, and a file read
	// half-way yields a fact that describes no request.
	DefaultChannelLogMinAgeSeconds = 5
	// DefaultChannelRetentionDays and DefaultChannelMaxSizeMB bound the on-disk history.
	// Retention is the primary limit; the size cap is the safety net for a traffic burst
	// that makes a day of records far larger than usual.
	DefaultChannelRetentionDays = 3
	DefaultChannelMaxSizeMB     = 512
	// maxChannelRetentionDays and minChannelMaxSizeMB bound the two knobs the same way
	// normalize() bounds the polling intervals: a value that cannot work is replaced by
	// one that can, instead of silently producing an empty page.
	maxChannelRetentionDays = 30
	minChannelMaxSizeMB     = 16
	// DefaultPlanRefresh is how often the subscription quota is refreshed, and
	// DefaultPlanUsageRefresh how often the official per-request records are paged. The
	// usage interval is the longer one on purpose: the quota calls are cheap, while the
	// per-request endpoint is cursor paginated and rate limited.
	DefaultPlanRefresh      = 5 * time.Minute
	DefaultPlanUsageRefresh = 10 * time.Minute
	defaultPlanRefresh      = DefaultPlanRefresh
	defaultPlanUsageRefresh = DefaultPlanUsageRefresh
	defaultTimezone         = "Asia/Shanghai"
)

// defaultHosts is what tells credential discovery which entries of CPA's own configuration
// belong to Cline. It is a default, never a hard requirement: an empty hosts list falls back
// to matching the entry name.
var defaultHosts = []string{"api.cline.bot"}

// Config mirrors the plugins.configs.<id> block. The host hands that block to the
// plugin as YAML (see pluginhost.runtimeConfigYAML), so the tags below are the
// public configuration contract.
type Config struct {
	Enabled  bool     `yaml:"enabled"`
	Priority int      `yaml:"priority"`
	Hosts    []string `yaml:"hosts"`

	// Timezone is the deployment's display timezone. Nothing reads it any more: it used to
	// bucket the local per-request records by day, and those are gone. It stays because an
	// existing configuration block still carries it.
	Timezone string `yaml:"timezone"`

	// ---- Cline 订阅用量（官方接口）----
	// The card is always on and the credential is discovered automatically, so PlanEnabled,
	// PlanAPIKey, PlanBaseURL, PlanDailyEnabled, PlanUsageEnabled and PlanUsageRefresh keep
	// their defaults and are not shown in the configuration panel. Only the two knobs a
	// deployment may genuinely need to move are exposed: where to read CPA's config from,
	// and how often to poll it.
	PlanEnabled      bool     `yaml:"plan_enabled"`
	PlanAPIKey       string   `yaml:"plan_api_key"`
	PlanBaseURL      string   `yaml:"plan_base_url"`
	PlanConfigPath   string   `yaml:"plan_config_path"`
	PlanRefresh      Duration `yaml:"plan_refresh"`
	PlanDailyEnabled bool     `yaml:"plan_daily_enabled"`
	// PlanUsageEnabled switches the official per-request collection behind the account
	// windows (request count, tokens, cache hit ratio) fetched from /users/{id}/usages.
	PlanUsageEnabled bool     `yaml:"plan_usage_enabled"`
	PlanUsageRefresh Duration `yaml:"plan_usage_refresh"`

	// ---- 逐请求渠道观测 ----
	// ChannelObserveEnabled decides whether the plugin declares the stream chunk
	// interceptor at all. Off means the capability is not advertised, so the host never
	// builds a chunk payload for this plugin: it is the emergency switch and the
	// "capability off" arm of the benchmark.
	ChannelObserveEnabled bool `yaml:"channel_observe_enabled"`
	// ChannelStoreDir is the directory the JSONL files are appended to.
	ChannelStoreDir string `yaml:"channel_store_dir"`
	// ChannelRetentionDays and ChannelMaxSizeMB bound that directory.
	ChannelRetentionDays int `yaml:"channel_retention_days"`
	ChannelMaxSizeMB     int `yaml:"channel_max_size_mb"`
	// ChannelBaselineProvider is the provider the "off-channel" ratio is measured against.
	ChannelBaselineProvider string `yaml:"channel_baseline_provider"`

	// ---- CPA 请求日志渠道补全 ----
	// ChannelLogEnabled switches the CPA request-log scanner on. It is off by default, and the
	// default matters: CPA only writes those files while observability.logs.request-log is on
	// AND server.commercial-mode is off, they carry the client's plaintext prompt, and the
	// scanner can only replace what the usage hook is missing — the gateway channel block on
	// Responses traffic, which the translation step drops.
	ChannelLogEnabled bool `yaml:"channel_log_enabled"`
	// ChannelLogDir is the directory CPA writes the request logs into, and the directory the
	// scanner lists: one level only, `.log` files only, never main.log.
	ChannelLogDir string `yaml:"channel_log_dir"`
	// ChannelLogDeleteAfterRead unlinks a log file once its fact has been stored. It is on by
	// default: the files hold a plaintext prompt, and the fact is what the plugin keeps.
	ChannelLogDeleteAfterRead bool `yaml:"channel_log_delete_after_read"`
	// ChannelLogMinAgeSeconds is how old a file must be before it is read.
	ChannelLogMinAgeSeconds int `yaml:"channel_log_min_age_seconds"`
}

// Duration accepts both Go duration strings ("5s", "1m30s") and plain numbers,
// which are read as seconds. Unparseable values fall back to the caller default.
type Duration struct {
	Value time.Duration
	Set   bool
}

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	if value == nil {
		return nil
	}
	raw := strings.TrimSpace(value.Value)
	if raw == "" || raw == "0" {
		return nil
	}
	if parsed, errParse := time.ParseDuration(raw); errParse == nil {
		d.Value = parsed
		d.Set = true
		return nil
	}
	var seconds float64
	if _, errScan := fmt.Sscanf(raw, "%f", &seconds); errScan == nil {
		d.Value = time.Duration(seconds * float64(time.Second))
		d.Set = true
	}
	return nil
}

func (d Duration) Or(def time.Duration) time.Duration {
	if d.Set && d.Value > 0 {
		return d.Value
	}
	return def
}

// Default returns the configuration used when the host passes no YAML at all.
func Default() Config {
	return Config{
		Enabled:          true,
		Priority:         1,
		Hosts:            append([]string(nil), defaultHosts...),
		Timezone:         defaultTimezone,
		PlanEnabled:      true,
		PlanBaseURL:      DefaultPlanBaseURL,
		PlanRefresh:      Duration{Value: defaultPlanRefresh, Set: true},
		PlanDailyEnabled: true,
		PlanUsageEnabled: true,
		PlanUsageRefresh: Duration{Value: defaultPlanUsageRefresh, Set: true},

		ChannelObserveEnabled:   true,
		ChannelStoreDir:         DefaultChannelStoreDir,
		ChannelRetentionDays:    DefaultChannelRetentionDays,
		ChannelMaxSizeMB:        DefaultChannelMaxSizeMB,
		ChannelBaselineProvider: DefaultChannelBaselineProvider,

		ChannelLogEnabled:         false,
		ChannelLogDir:             DefaultChannelLogDir,
		ChannelLogDeleteAfterRead: true,
		ChannelLogMinAgeSeconds:   DefaultChannelLogMinAgeSeconds,
	}
}

// Parse decodes the host-provided YAML block and normalizes it.
//
// The distinction between "hosts absent" and "hosts: []" is deliberate:
// absent keeps the Cline default, an explicit empty list means "match Cline entries by
// name only" instead of by base URL host.
func Parse(raw []byte) (Config, error) {
	cfg := Default()
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return cfg, nil
	}
	if bytes.Contains(trimmed, []byte{'\n'}) || bytes.Contains(trimmed, []byte{':'}) {
		// YAML block from the host.
		probe := struct {
			Hosts *[]string `yaml:"hosts"`
		}{}
		if errProbe := yaml.Unmarshal(trimmed, &probe); errProbe == nil && probe.Hosts != nil {
			cfg.Hosts = append([]string(nil), (*probe.Hosts)...)
		}
		if errUnmarshal := yaml.Unmarshal(trimmed, &cfg); errUnmarshal != nil {
			return cfg, fmt.Errorf("decode plugin config: %w", errUnmarshal)
		}
	} else if errJSON := decodeJSONConfig(trimmed, &cfg); errJSON != nil {
		return cfg, errJSON
	}
	normalize(&cfg)
	return cfg, nil
}

// decodeJSONConfig supports a raw JSON object, which is what arrives when the host
// itself was configured through a JSON document.
func decodeJSONConfig(raw []byte, cfg *Config) error {
	probe := struct {
		Hosts *[]string `json:"hosts"`
	}{}
	if errProbe := json.Unmarshal(raw, &probe); errProbe == nil && probe.Hosts != nil {
		cfg.Hosts = append([]string(nil), (*probe.Hosts)...)
	}
	if errUnmarshal := json.Unmarshal(raw, cfg); errUnmarshal != nil {
		return fmt.Errorf("decode plugin config: %w", errUnmarshal)
	}
	return nil
}

// HostFromBaseURL returns the lower-cased host of a base URL, or "" when it cannot
// be parsed. It is the only place a base URL is interpreted.
func HostFromBaseURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	parsed, errParse := url.Parse(trimmed)
	if errParse != nil {
		return ""
	}
	return strings.ToLower(parsed.Hostname())
}

func normalize(cfg *Config) {
	for i := range cfg.Hosts {
		cfg.Hosts[i] = strings.ToLower(strings.TrimSpace(cfg.Hosts[i]))
	}
	if strings.TrimSpace(cfg.Timezone) == "" {
		cfg.Timezone = defaultTimezone
	}
	if strings.TrimSpace(cfg.PlanBaseURL) == "" {
		cfg.PlanBaseURL = DefaultPlanBaseURL
	}
	if cfg.PlanUsageRefresh.Set && cfg.PlanUsageRefresh.Value < time.Minute {
		// The official per-request endpoint is paginated; a sub-minute interval would mean
		// dozens of upstream calls per cycle for no visible gain.
		cfg.PlanUsageRefresh.Value = time.Minute
	}
	if strings.TrimSpace(cfg.ChannelStoreDir) == "" {
		cfg.ChannelStoreDir = DefaultChannelStoreDir
	}
	if cfg.ChannelRetentionDays <= 0 {
		cfg.ChannelRetentionDays = DefaultChannelRetentionDays
	}
	if cfg.ChannelRetentionDays > maxChannelRetentionDays {
		cfg.ChannelRetentionDays = maxChannelRetentionDays
	}
	if cfg.ChannelMaxSizeMB <= 0 {
		cfg.ChannelMaxSizeMB = DefaultChannelMaxSizeMB
	}
	if cfg.ChannelMaxSizeMB < minChannelMaxSizeMB {
		// Below this a single busy day would not fit, and the cleaner would delete the
		// history it is supposed to keep.
		cfg.ChannelMaxSizeMB = minChannelMaxSizeMB
	}
	if strings.TrimSpace(cfg.ChannelBaselineProvider) == "" {
		cfg.ChannelBaselineProvider = DefaultChannelBaselineProvider
	}
	cfg.ChannelBaselineProvider = strings.ToLower(strings.TrimSpace(cfg.ChannelBaselineProvider))
	if strings.TrimSpace(cfg.ChannelLogDir) == "" {
		cfg.ChannelLogDir = DefaultChannelLogDir
	}
	if cfg.ChannelLogMinAgeSeconds < 0 {
		// A negative age would mean "read a file before it exists"; zero is allowed and means
		// "read it as soon as it appears", which is what a short-lived debug window wants.
		cfg.ChannelLogMinAgeSeconds = DefaultChannelLogMinAgeSeconds
	}
}

// HostMatched reports whether a Cline entry's base_url host is one of the configured hosts.
//
// Matching parses the URL and compares hosts case-insensitively; entries that start
// with "." match a domain suffix. A prefix comparison would let
// "https://api.cline.bot.example.com" pass, so hosts are never compared as substrings.
func (c Config) HostMatched(baseURL string) (string, bool) {
	host := HostFromBaseURL(baseURL)
	if host == "" {
		return "", false
	}
	for _, candidate := range c.Hosts {
		if candidate == "" {
			continue
		}
		if strings.HasPrefix(candidate, ".") {
			if strings.HasSuffix(host, candidate) && len(host) > len(candidate) {
				return host, true
			}
			continue
		}
		if host == candidate {
			return host, true
		}
	}
	return host, false
}
