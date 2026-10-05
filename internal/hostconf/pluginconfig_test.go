package hostconf

import (
	"errors"
	"strings"
	"testing"
)

// The two keys a caller would add to the fixture below. The literals are what yaml.Marshal
// produces for a string and an int, which is what the plugin hands over.
var pluginDefaultsFixture = []PluginDefault{
	{Key: "channel_store_dir", Literal: "/CLIProxyAPI/logs/channel-observation"},
	{Key: "account_guard_threshold", Literal: "3"},
}

// The exact lines the fixture should grow, at the block's own indentation (12 spaces, matching
// `channel_log_enabled` above them).
const pluginDefaultsInserted = "            channel_store_dir: /CLIProxyAPI/logs/channel-observation\n" +
	"            account_guard_threshold: 3\n"

func TestPluginConfigDefaultsAddsOnlyTheMissingKeys(t *testing.T) {
	out, changed, errFill := PluginConfigDefaults([]byte(v8BlockFixture), "clinepass-channel-monitor", pluginDefaultsFixture)
	if errFill != nil {
		t.Fatalf("PluginConfigDefaults: %v", errFill)
	}
	if !changed {
		t.Fatal("changed = false, want true: both keys are missing from the fixture")
	}
	text := string(out)
	for _, line := range []string{
		"            channel_store_dir: /CLIProxyAPI/logs/channel-observation\n",
		"            account_guard_threshold: 3\n",
	} {
		if !strings.Contains(text, line) {
			t.Errorf("added configuration is missing %q, got:\n%s", line, text)
		}
	}
	// Everything else must survive byte for byte: the comment on the operator's own key, the
	// entries below the plugin block, the top-of-file comment.
	if removed := strings.Replace(text, pluginDefaultsInserted, "", 1); removed != v8BlockFixture {
		t.Errorf("the file changed outside the added keys:\n--- want ---\n%s\n--- got ---\n%s", v8BlockFixture, removed)
	}
	if !strings.Contains(text, "channel_log_enabled: true   # 守卫要保护的就是这一行\n") {
		t.Error("the operator's own key was rewritten; the writer must only add")
	}
}

func TestPluginConfigDefaultsIsIdempotent(t *testing.T) {
	first, changed, errFill := PluginConfigDefaults([]byte(v8BlockFixture), "clinepass-channel-monitor", pluginDefaultsFixture)
	if errFill != nil || !changed {
		t.Fatalf("first pass: changed=%v err=%v", changed, errFill)
	}
	second, changedAgain, errAgain := PluginConfigDefaults(first, "clinepass-channel-monitor", pluginDefaultsFixture)
	if errAgain != nil {
		t.Fatalf("second pass: %v", errAgain)
	}
	if changedAgain {
		t.Error("second pass reports a change; the writer has to leave a filled block alone")
	}
	if string(second) != string(first) {
		t.Error("second pass rewrote the bytes")
	}
}

func TestPluginConfigDefaultsNeverOverwritesAnExistingKey(t *testing.T) {
	// The operator's own value must win even when the plugin would default to something else.
	out, changed, errFill := PluginConfigDefaults([]byte(v8BlockFixture), "clinepass-channel-monitor", []PluginDefault{
		{Key: "channel_log_enabled", Literal: "false"},
	})
	if errFill != nil {
		t.Fatalf("PluginConfigDefaults: %v", errFill)
	}
	if changed {
		t.Error("changed = true, want false: the only key asked for is already in the block")
	}
	if string(out) != v8BlockFixture {
		t.Error("bytes changed even though nothing had to be added")
	}
}

func TestPluginConfigDefaultsRefusesAMissingBlock(t *testing.T) {
	raw := []byte("plugins:\n    enabled: true\n    configs: {}\nobservability:\n    logs:\n        request-log: true\n")
	_, changed, errFill := PluginConfigDefaults(raw, "clinepass-channel-monitor", pluginDefaultsFixture)
	if !errors.Is(errFill, ErrPluginConfigMissing) {
		t.Fatalf("err = %v, want ErrPluginConfigMissing", errFill)
	}
	if changed {
		t.Error("changed = true on the error path")
	}
}

// CPA writes this block itself after a management save, and it writes flow style: one `{...}`
// line, quoted keys, and a nested `store` mapping. That is the shape a real install has, so the
// writer has to add keys to it without reflowing anything.
func TestPluginConfigDefaultsInsertsIntoAFlowBlock(t *testing.T) {
	const raw = `plugins:
    enabled: true
    dir: "plugins"
    configs:
        clinepass-channel-monitor: {"channel_log_enabled": true, "enabled": true, "store": {"version": "0.3.1", "tags": ["a", "b"]}}
config-version: 8
`
	const want = `plugins:
    enabled: true
    dir: "plugins"
    configs:
        clinepass-channel-monitor: {"channel_log_enabled": true, "enabled": true, "store": {"version": "0.3.1", "tags": ["a", "b"]}, "channel_store_dir": /CLIProxyAPI/logs/channel-observation, "account_guard_threshold": 3}
config-version: 8
`
	out, changed, errFill := PluginConfigDefaults([]byte(raw), "clinepass-channel-monitor", pluginDefaultsFixture)
	if errFill != nil {
		t.Fatalf("PluginConfigDefaults: %v", errFill)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	if string(out) != want {
		t.Errorf("flow block:\n--- want ---\n%s\n--- got ---\n%s", want, out)
	}
	again, changedAgain, errAgain := PluginConfigDefaults(out, "clinepass-channel-monitor", pluginDefaultsFixture)
	if errAgain != nil || changedAgain {
		t.Fatalf("second pass: changed=%v err=%v", changedAgain, errAgain)
	}
	if string(again) != want {
		t.Error("second pass rewrote the flow block")
	}
}

func TestPluginConfigDefaultsHandlesAnEmptyFlowBlock(t *testing.T) {
	raw := []byte("plugins:\n    configs:\n        clinepass-channel-monitor: {}\n")
	out, changed, errFill := PluginConfigDefaults(raw, "clinepass-channel-monitor", []PluginDefault{
		{Key: "channel_log_enabled", Literal: "false"},
	})
	if errFill != nil {
		t.Fatalf("PluginConfigDefaults: %v", errFill)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	if want := "plugins:\n    configs:\n        clinepass-channel-monitor: {\"channel_log_enabled\": false}\n"; string(out) != want {
		t.Errorf("empty flow block:\n--- want ---\n%s\n--- got ---\n%s", want, out)
	}
}

func TestPluginConfigDefaultsHandlesAFlowBlockEndingInAComma(t *testing.T) {
	raw := []byte("plugins:\n    configs:\n        clinepass-channel-monitor: {enabled: true,}\n")
	out, changed, errFill := PluginConfigDefaults(raw, "clinepass-channel-monitor", []PluginDefault{
		{Key: "channel_log_enabled", Literal: "false"},
	})
	if errFill != nil {
		t.Fatalf("PluginConfigDefaults: %v", errFill)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	if want := "plugins:\n    configs:\n        clinepass-channel-monitor: {enabled: true, \"channel_log_enabled\": false}\n"; string(out) != want {
		t.Errorf("trailing comma:\n--- want ---\n%s\n--- got ---\n%s", want, out)
	}
}

// A caller that cannot encode a value hands over an empty literal; that key is skipped rather
// than written as a bare `key:` that YAML would read as null.
func TestPluginConfigDefaultsSkipsAnEmptyLiteral(t *testing.T) {
	raw := []byte("plugins:\n    configs:\n        clinepass-channel-monitor:\n            enabled: true\n")
	out, changed, errFill := PluginConfigDefaults(raw, "clinepass-channel-monitor", []PluginDefault{
		{Key: "plan_config_path", Literal: ""},
	})
	if errFill != nil {
		t.Fatalf("PluginConfigDefaults: %v", errFill)
	}
	if changed {
		t.Error("changed = true, want false: an empty literal means 'leave this key alone'")
	}
	if strings.Contains(string(out), "plan_config_path") {
		t.Error("a key with an empty literal was written anyway")
	}
}
