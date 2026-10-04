package hostconf

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wkeking/clinepass-channel-monitor/internal/config"
)

// writeConfig writes one fixture file and returns a config that points at it.
func writeConfig(t *testing.T, contents string) config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(path, []byte(contents), 0o600); errWrite != nil {
		t.Fatalf("write fixture: %v", errWrite)
	}
	return config.Config{PlanConfigPath: path}
}

// TestReadKeyValuesPresent covers the readable shape with every key the card shows.
func TestReadKeyValuesPresent(t *testing.T) {
	cfg := writeConfig(t, `
plugins:
  enabled: true
  dir: plugins
observability:
  logs:
    request-log: true
    logs-max-total-size-mb: 2048
server:
  commercial-mode: false
`)
	snapshot := Read(cfg)
	if !snapshot.Readable {
		t.Fatalf("readable = false, error = %q", snapshot.Error)
	}
	if snapshot.Path != cfg.PlanConfigPath {
		t.Errorf("path = %q, want %q", snapshot.Path, cfg.PlanConfigPath)
	}
	if snapshot.PluginsEnabled == nil || *snapshot.PluginsEnabled != true {
		t.Errorf("plugins enabled = %v, want true", snapshot.PluginsEnabled)
	}
	if snapshot.PluginsDir != "plugins" {
		t.Errorf("plugins dir = %q, want %q", snapshot.PluginsDir, "plugins")
	}
	if snapshot.RequestLog == nil || *snapshot.RequestLog != true {
		t.Errorf("request-log = %v, want true", snapshot.RequestLog)
	}
	if snapshot.LogsMaxTotalSizeMB == nil || *snapshot.LogsMaxTotalSizeMB != 2048 {
		t.Errorf("logs-max-total-size-mb = %v, want 2048", snapshot.LogsMaxTotalSizeMB)
	}
	// false is a real value: the key is present, so the pointer must be set.
	if snapshot.CommercialMode == nil || *snapshot.CommercialMode != false {
		t.Errorf("commercial-mode = %v, want a pointer to false", snapshot.CommercialMode)
	}
}

// TestReadMissingKeysStayNil guards the "do not fake a zero value" rule: a missing key
// must be nil, while a present zero value (0, false) must be a set pointer.
func TestReadMissingKeysStayNil(t *testing.T) {
	cfg := writeConfig(t, `
plugins:
  dir: plugins
server:
  commercial-mode: false
observability:
  logs:
    logs-max-total-size-mb: 0
`)
	snapshot := Read(cfg)
	if !snapshot.Readable {
		t.Fatalf("readable = false, error = %q", snapshot.Error)
	}
	if snapshot.PluginsEnabled != nil {
		t.Errorf("plugins enabled = %v, want nil", *snapshot.PluginsEnabled)
	}
	if snapshot.RequestLog != nil {
		t.Errorf("request-log = %v, want nil", *snapshot.RequestLog)
	}
	if snapshot.PluginsDir != "plugins" {
		t.Errorf("plugins dir = %q, want %q", snapshot.PluginsDir, "plugins")
	}
	// A present zero is not a missing key.
	if snapshot.LogsMaxTotalSizeMB == nil || *snapshot.LogsMaxTotalSizeMB != 0 {
		t.Errorf("logs-max-total-size-mb = %v, want a pointer to 0", snapshot.LogsMaxTotalSizeMB)
	}
	if snapshot.CommercialMode == nil || *snapshot.CommercialMode != false {
		t.Errorf("commercial-mode = %v, want a pointer to false", snapshot.CommercialMode)
	}
}

// TestReadStoreInstallShape covers the shape CPA writes after a store install, and pins
// that no credential value reaches the snapshot even when the value sits in a block the
// snapshot walks.
func TestReadStoreInstallShape(t *testing.T) {
	const secret = "sk-TESTKEY-should-never-appear"
	cfg := writeConfig(t, `
plugins:
  configs:
    clinepass-channel-monitor:
      enabled: true
      store:
        version: 2
        source-id: store
    other-plugin:
      enabled: true
      plan_api_key: `+secret+`
openai-compatibility:
  - name: cline
    base-url: https://api.cline.bot/api/v1
    api-key: `+secret+`
    keys:
      - api-key: `+secret+`
`)
	snapshot := Read(cfg)
	if !snapshot.Readable {
		t.Fatalf("readable = false, error = %q", snapshot.Error)
	}
	if len(snapshot.PluginBlockKeys) != 2 || snapshot.PluginBlockKeys[0] != "enabled" || snapshot.PluginBlockKeys[1] != "store" {
		t.Errorf("plugin block keys = %v, want [enabled store] in file order", snapshot.PluginBlockKeys)
	}
	if snapshot.StoreVersion == "" {
		t.Error("store version must be reported")
	}
	if snapshot.StoreSource == "" {
		t.Error("store source must be reported")
	}
	encoded, errMarshal := json.Marshal(snapshot)
	if errMarshal != nil {
		t.Fatalf("snapshot must marshal: %v", errMarshal)
	}
	if strings.Contains(string(encoded), secret) {
		t.Errorf("snapshot JSON leaked a credential value: %s", encoded)
	}
}

// TestReadUnreadable covers a path that cannot be read at all.
func TestReadUnreadable(t *testing.T) {
	cfg := config.Config{PlanConfigPath: filepath.Join(t.TempDir(), "missing.yaml")}
	snapshot := Read(cfg)
	if snapshot.Readable {
		t.Error("readable = true, want false for a missing file")
	}
	if snapshot.Error == "" {
		t.Error("error must describe why the file could not be read")
	}
	if snapshot.Path != cfg.PlanConfigPath {
		t.Errorf("path = %q, want the last candidate %q", snapshot.Path, cfg.PlanConfigPath)
	}
}

// TestReadInvalidYAML covers a file that reads but does not parse: it stays readable,
// carries the parse error, and must not panic.
func TestReadInvalidYAML(t *testing.T) {
	cfg := writeConfig(t, "plugins: [unterminated\n")
	snapshot := Read(cfg)
	if !snapshot.Readable {
		t.Error("readable = false, want true for a file that was read")
	}
	if snapshot.Error == "" {
		t.Error("error must describe the parse failure")
	}
	if snapshot.PluginsEnabled != nil || snapshot.RequestLog != nil || snapshot.CommercialMode != nil {
		t.Error("a failed parse must leave every field empty")
	}
}

// TestPaths pins the candidate order and the override rule.
func TestPaths(t *testing.T) {
	got := Paths("")
	want := []string{"/CLIProxyAPI/config.yaml", "/app/config.yaml"}
	if len(got) != len(want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("paths = %v, want %v", got, want)
		}
	}
	if override := Paths("/tmp/only.yaml"); len(override) != 1 || override[0] != "/tmp/only.yaml" {
		t.Errorf("override paths = %v, want [/tmp/only.yaml]", override)
	}
}

// TestGatewayHint covers the one line the plugin logs at load time: silent once the plugin-side
// switch is on, specific about the CPA-side half when it is off, and never guessing at a file it
// could not read.
func TestGatewayHint(t *testing.T) {
	if got := GatewayHint(config.Config{ChannelLogEnabled: true}); got != "" {
		t.Errorf("hint with the scanner already on = %q, want empty", got)
	}
	off := writeConfig(t, `
observability:
  logs:
    request-log: false
server:
  commercial-mode: true
`)
	hint := GatewayHint(off)
	for _, want := range []string{"channel_log_enabled=false", "observability.logs.request-log: true", "server.commercial-mode: false", "配置自检"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint %q is missing %q", hint, want)
		}
	}
	right := writeConfig(t, `
observability:
  logs:
    request-log: true
server:
  commercial-mode: false
`)
	hint = GatewayHint(right)
	if strings.Contains(hint, "request-log") || strings.Contains(hint, "commercial-mode") {
		t.Errorf("hint %q must not list the CPA switches once they are right", hint)
	}
	unreadable := config.Config{PlanConfigPath: filepath.Join(t.TempDir(), "missing.yaml")}
	if hint = GatewayHint(unreadable); strings.Contains(hint, "request-log") {
		t.Errorf("hint %q must not guess the CPA side when the file cannot be read", hint)
	}
}
