package plugin

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/wkeking/clinepass-channel-monitor/internal/config"
)

// seedFixture is what a store install leaves behind: the plugin block carries `enabled` and the
// store record plus one key the operator set, and nothing else. That is exactly the state that
// renders as a panel of empty boxes.
const seedFixture = `# 顶部注释，不能被改写
plugins:
    enabled: true
    configs:
        clinepass-channel-monitor:
            enabled: true
            channel_log_enabled: true   # 操作者自己开的，不能被覆盖
            store:
                id: clinepass-channel-monitor
                version: 0.3.1
api-keys:
    openai-compatibility: []
`

func seedTestConfig(t *testing.T, body string) (config.Config, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(path, []byte(body), 0o600); errWrite != nil {
		t.Fatalf("write configuration: %v", errWrite)
	}
	cfg := config.Default()
	cfg.PlanConfigPath = path
	return cfg, path
}

// TestSeedPluginConfigDefaultsFillsThePanel pins the contract this file exists for: the keys the
// panel renders are written with the values the plugin is actually using, the operator's own key
// and the store record survive, and the two keys whose default is "empty" stay out.
func TestSeedPluginConfigDefaultsFillsThePanel(t *testing.T) {
	cfg, path := seedTestConfig(t, seedFixture)
	seedPluginConfigDefaults(cfg)
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read back: %v", errRead)
	}
	text := string(raw)
	for _, needle := range []string{
		"            plan_refresh: 5m\n",
		"            channel_observe_enabled: true\n",
		"            channel_store_dir: /CLIProxyAPI/logs/channel-observation\n",
		"            channel_retention_days: 3\n",
		"            channel_max_size_mb: 512\n",
		"            channel_baseline_provider: deepseek\n",
		"            channel_log_dir: /CLIProxyAPI/logs\n",
		"            channel_log_delete_after_read: true\n",
		"            channel_log_min_age_seconds: 5\n",
		"            account_guard_enabled: false\n",
		"            account_guard_dry_run: true\n",
		"            account_guard_threshold: 3\n",
		"            account_guard_min_enabled: 1\n",
		"            account_guard_reenable_minutes: 30\n",
		"            account_guard_max_disable_minutes: 360\n",
	} {
		if !strings.Contains(text, needle) {
			t.Errorf("the panel would still show %q as blank; file is:\n%s", strings.TrimSpace(needle), text)
		}
	}
	if !strings.Contains(text, "channel_log_enabled: true   # 操作者自己开的，不能被覆盖\n") {
		t.Error("the operator's own value was rewritten")
	}
	if !strings.Contains(text, "# 顶部注释，不能被改写\n") || !strings.Contains(text, "                version: 0.3.1\n") {
		t.Error("content outside the added keys was not preserved")
	}
	// "Leave it blank" is the default for these two, so writing anything would be wrong.
	for _, absent := range []string{"plan_config_path", "account_guard_scope_names"} {
		if strings.Contains(text, absent) {
			t.Errorf("%s was written; its default is an empty value", absent)
		}
	}
	leftovers, errGlob := filepath.Glob(filepath.Join(filepath.Dir(path), ".clinepass-*.tmp"))
	if errGlob != nil {
		t.Fatalf("glob: %v", errGlob)
	}
	if len(leftovers) != 0 {
		t.Errorf("temporary files left behind: %v", leftovers)
	}
}

// TestSeedPluginConfigDefaultsIsIdempotent keeps the write from turning into a reconfigure loop:
// once the keys are there, the second load must leave the file alone.
func TestSeedPluginConfigDefaultsIsIdempotent(t *testing.T) {
	cfg, path := seedTestConfig(t, seedFixture)
	seedPluginConfigDefaults(cfg)
	first, errFirst := os.ReadFile(path)
	if errFirst != nil {
		t.Fatalf("read after first pass: %v", errFirst)
	}
	seedPluginConfigDefaults(cfg)
	second, errSecond := os.ReadFile(path)
	if errSecond != nil {
		t.Fatalf("read after second pass: %v", errSecond)
	}
	if string(first) != string(second) {
		t.Errorf("the second load rewrote the file:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

// CPA writes the block itself after a management save, in flow style on one line. That is the
// shape the panel leaves behind in production, so the seeder has to fill it too.
func TestSeedPluginConfigDefaultsFillsAFlowBlock(t *testing.T) {
	const flow = `plugins:
    enabled: true
    dir: "plugins"
    configs:
        clinepass-channel-monitor: {"channel_log_enabled": true, "enabled": true, "store": {"version": "0.3.1"}, "account_guard_enabled": true}
config-version: 8
`
	cfg, path := seedTestConfig(t, flow)
	seedPluginConfigDefaults(cfg)
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read back: %v", errRead)
	}
	text := string(raw)
	for _, needle := range []string{
		`"channel_log_enabled": true`,
		`"account_guard_enabled": true`,
		`"store": {"version": "0.3.1"}`,
		`"plan_refresh": 5m`,
		`"channel_store_dir": /CLIProxyAPI/logs/channel-observation`,
		`"account_guard_threshold": 3`,
		"config-version: 8",
	} {
		if !strings.Contains(text, needle) {
			t.Errorf("the flow block lost %q:\n%s", needle, text)
		}
	}
	if strings.Contains(text, "plan_config_path") || strings.Contains(text, "account_guard_scope_names") {
		t.Errorf("a key whose default is empty was written:\n%s", text)
	}
	if !strings.HasSuffix(text, "config-version: 8\n") {
		t.Error("content below the plugin block was disturbed")
	}
}

// durationLiteral is what keeps the panel showing "5m" instead of Go's "5m0s".
func TestDurationLiteralMatchesTheDocumentedSpelling(t *testing.T) {
	cases := map[string]string{
		"5m0s":  "5m",
		"10m0s": "10m",
		"90s":   "1m30s",
		"1m30s": "1m30s",
	}
	for input, want := range cases {
		parsed, errParse := time.ParseDuration(input)
		if errParse != nil {
			t.Fatalf("parse %q: %v", input, errParse)
		}
		if got := durationLiteral(parsed); got != want {
			t.Errorf("durationLiteral(%s) = %q, want %q", input, got, want)
		}
	}
}

// This is the shape a real install has on 2026-10-05: CPA wrote the whole block itself as one
// flow line, with the store manifest inlined - including a description that carries commas,
// parentheses, a slash and Chinese punctuation. The writer has to add keys to that exact line
// without breaking it, so the test parses the result back and compares the document rather than
// only grepping text.
func TestSeedPluginConfigDefaultsHandlesTheRealStoreBlockShape(t *testing.T) {
	const productionShape = `plugins:
    enabled: true
    dir: "plugins"
    configs:
        clinepass-channel-monitor: {"channel_log_enabled": true, "enabled": true, "store": {"author": "wkeking", "description": "在管理中心展示 Cline 套餐、限额与官方用量（含订阅周期与按模型明细）。不注册任何请求钩子，因此不在请求路径上。 / Shows the Cline plan, rolling limits and official usage (per-model breakdown, billing period) in the Management Center, and declares no request hook, so it stays off the request path.", "homepage": "https://github.com/wkeking/clinepass-channel-monitor", "id": "clinepass-channel-monitor", "install": {"type": "github-release"}, "license": "MIT", "name": "Cline 渠道监控", "release-tag": "v0.3.1", "repository": "https://github.com/wkeking/clinepass-channel-monitor", "source-id": "official", "source-name": "Official", "source-url": "https://raw.githubusercontent.com/router-for-me/CLIProxyAPI-Plugins-Store/main/registry.json", "tags": ["Management", "Cline", "Usage", "Subscription"], "version": "0.3.1"}, "account_guard_enabled": true}
config-version: 8
`
	cfg, path := seedTestConfig(t, productionShape)
	seedPluginConfigDefaults(cfg)
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read back: %v", errRead)
	}
	var document map[string]any
	if errUnmarshal := yaml.Unmarshal(raw, &document); errUnmarshal != nil {
		t.Fatalf("the rewritten configuration is not valid YAML: %v\n%s", errUnmarshal, raw)
	}
	block := pluginBlockForTest(t, document)
	// What the operator and the store had must survive.
	if block["enabled"] != true || block["channel_log_enabled"] != true || block["account_guard_enabled"] != true {
		t.Errorf("an existing key changed: %#v", block)
	}
	store, okStore := block["store"].(map[string]any)
	if !okStore {
		t.Fatalf("the store record is gone: %#v", block["store"])
	}
	if store["version"] != "0.3.1" || !strings.Contains(asString(store["description"]), "不注册任何请求钩子") {
		t.Errorf("the store record was damaged: %#v", store)
	}
	// What the panel was missing must be there, with the value the plugin uses.
	want := map[string]any{
		"plan_refresh":                      "5m",
		"channel_observe_enabled":           true,
		"channel_store_dir":                 "/CLIProxyAPI/logs/channel-observation",
		"channel_retention_days":            3,
		"channel_max_size_mb":               512,
		"channel_baseline_provider":         "deepseek",
		"channel_log_dir":                   "/CLIProxyAPI/logs",
		"channel_log_delete_after_read":     true,
		"channel_log_min_age_seconds":       5,
		"account_guard_dry_run":             true,
		"account_guard_threshold":           3,
		"account_guard_min_enabled":         1,
		"account_guard_reenable_minutes":    30,
		"account_guard_max_disable_minutes": 360,
	}
	for key, expected := range want {
		if got := block[key]; !reflect.DeepEqual(got, expected) {
			t.Errorf("%s = %#v, want %#v", key, got, expected)
		}
	}
	// config-version and everything below the block survive untouched.
	if document["config-version"] != 8 {
		t.Errorf("config-version = %#v, want 8", document["config-version"])
	}
}

// pluginBlockForTest digs plugins.configs.<id> out of a decoded document.
func pluginBlockForTest(t *testing.T, document map[string]any) map[string]any {
	t.Helper()
	plugins, okPlugins := document["plugins"].(map[string]any)
	if !okPlugins {
		t.Fatalf("no plugins mapping: %#v", document)
	}
	configs, okConfigs := plugins["configs"].(map[string]any)
	if !okConfigs {
		t.Fatalf("no plugins.configs mapping: %#v", plugins)
	}
	block, okBlock := configs["clinepass-channel-monitor"].(map[string]any)
	if !okBlock {
		t.Fatalf("no plugin block: %#v", configs)
	}
	return block
}

func asString(value any) string {
	text, _ := value.(string)
	return text
}
