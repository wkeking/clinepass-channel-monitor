package plugin

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/wkeking/clinepass-channel-monitor/internal/buildinfo"
	"github.com/wkeking/clinepass-channel-monitor/internal/config"
	"github.com/wkeking/clinepass-channel-monitor/internal/hostapi"
	"github.com/wkeking/clinepass-channel-monitor/internal/hostconf"
)

// This file keeps the host's configuration panel honest.
//
// The panel shows whatever sits in plugins.configs.<id>; it has no notion of a default and the
// plugin's own field schema cannot carry one (sdk/pluginapi.ConfigField is Name/Type/EnumValues/
// Description, and the store manifest has no config block either). So a fresh install - which
// writes `enabled` and `store` and nothing else - renders as a form of empty boxes even though
// every one of those keys works without being set. The only lever the plugin has is the file
// itself, so on every load it adds the keys that are missing, each with the value it is using
// right now. Existing keys are never touched, so an operator's choice always wins; the cost is
// that clearing a field does not mean "use the default", because the next load puts the value
// back.

// pluginConfigDefaults is the panel's fields with the value each one has right now. Every key
// declared in buildRegistration appears here, with two deliberate omissions:
//
//   - plan_config_path defaults to empty, which means "probe /CLIProxyAPI/config.yaml then
//     /app/config.yaml"; the writer cannot express a fallback list, so guessing a path here would
//     silently remove the fallback;
//   - account_guard_scope_names defaults to empty, which means "every openai-compatibility
//     entry"; there is no honest non-empty encoding of that.
//
// Both keep their "leave it blank" descriptions, and blank is their default.
func pluginConfigDefaults(cfg config.Config) []hostconf.PluginDefault {
	return []hostconf.PluginDefault{
		{Key: "plan_refresh", Literal: yamlLiteral(durationLiteral(cfg.PlanRefresh.Or(config.DefaultPlanRefresh)))},
		{Key: "channel_observe_enabled", Literal: yamlLiteral(cfg.ChannelObserveEnabled)},
		{Key: "channel_store_dir", Literal: yamlLiteral(cfg.ChannelStoreDir)},
		{Key: "channel_retention_days", Literal: yamlLiteral(cfg.ChannelRetentionDays)},
		{Key: "channel_max_size_mb", Literal: yamlLiteral(cfg.ChannelMaxSizeMB)},
		{Key: "channel_baseline_provider", Literal: yamlLiteral(cfg.ChannelBaselineProvider)},
		{Key: "channel_log_enabled", Literal: yamlLiteral(cfg.ChannelLogEnabled)},
		{Key: "channel_log_dir", Literal: yamlLiteral(cfg.ChannelLogDir)},
		{Key: "channel_log_delete_after_read", Literal: yamlLiteral(cfg.ChannelLogDeleteAfterRead)},
		{Key: "channel_log_min_age_seconds", Literal: yamlLiteral(cfg.ChannelLogMinAgeSeconds)},
		{Key: "account_guard_enabled", Literal: yamlLiteral(cfg.AccountGuardEnabled)},
		{Key: "account_guard_dry_run", Literal: yamlLiteral(cfg.AccountGuardDryRun)},
		{Key: "account_guard_threshold", Literal: yamlLiteral(cfg.AccountGuardThreshold)},
		{Key: "account_guard_min_enabled", Literal: yamlLiteral(cfg.AccountGuardMinEnabled)},
		{Key: "account_guard_reenable_minutes", Literal: yamlLiteral(cfg.AccountGuardReenableMinutes)},
		{Key: "account_guard_max_disable_minutes", Literal: yamlLiteral(cfg.AccountGuardMaxDisableMinutes)},
	}
}

// yamlLiteral renders one value as the YAML token that goes after `key:`, so quoting and boolean
// spelling come from the encoder rather than from string concatenation here. An empty result
// tells the writer to skip the key.
func yamlLiteral(value any) string {
	encoded, errMarshal := yaml.Marshal(value)
	if errMarshal != nil {
		return ""
	}
	return strings.TrimRight(string(encoded), "\n")
}

// durationLiteral writes a whole number of minutes the way the configuration examples do ("5m"
// rather than Go's "5m0s"), and leaves the rest to time.Duration's own spelling ("1m30s").
func durationLiteral(value time.Duration) string {
	if value > 0 && value%time.Minute == 0 {
		return strconv.FormatInt(int64(value/time.Minute), 10) + "m"
	}
	return value.String()
}

// seedPluginConfigDefaults adds the missing panel keys to the plugin's own configuration block.
//
// It is fail-open by construction: whatever goes wrong - no configuration file, no plugin block,
// a flow-style block, an unwritable directory - only costs the panel its values, never the
// plugin, and is reported once in the host log.
func seedPluginConfigDefaults(cfg config.Config) {
	path, raw, errRead := hostconf.ReadRaw(cfg)
	if errRead != nil {
		return
	}
	updated, changed, errFill := hostconf.PluginConfigDefaults(raw, buildinfo.ID, pluginConfigDefaults(cfg))
	switch {
	case errors.Is(errFill, hostconf.ErrPluginConfigMissing), errors.Is(errFill, hostconf.ErrPluginConfigNotEditable):
		// Nothing to fill in, or a shape this writer refuses to guess at. Either way an operator
		// can add the keys by hand; saying so on every load would only be noise.
		return
	case errFill != nil:
		hostapi.LogAsync("warn", buildinfo.ID+": could not fill in the configuration defaults", map[string]string{
			"path":  path,
			"error": errFill.Error(),
		})
		return
	}
	if !changed {
		return
	}
	if errWrite := writeConfigAtomic(path, updated); errWrite != nil {
		hostapi.LogAsync("warn", buildinfo.ID+": could not write the configuration defaults", map[string]string{
			"path":  path,
			"error": errWrite.Error(),
		})
		return
	}
	hostapi.LogAsync("info", buildinfo.ID+": wrote the configuration defaults the panel was missing", map[string]string{
		"path": path,
	})
}
