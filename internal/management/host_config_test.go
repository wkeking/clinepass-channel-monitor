package management

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHealthCarriesHostConfig pins the two fields the configuration self-check card
// reads on /health, and that the payload still carries no credential from CPA's file.
func TestHealthCarriesHostConfig(t *testing.T) {
	const secret = "sk-TESTKEY-should-never-appear"
	path := filepath.Join(t.TempDir(), "cpa-config.yaml")
	contents := `
plugins:
  enabled: true
  dir: plugins
  configs:
    clinepass-channel-monitor:
      enabled: true
      store:
        version: 2
        source-id: store
    other-plugin:
      plan_api_key: ` + secret + `
openai-compatibility:
  - name: cline
    base-url: https://api.cline.bot/api/v1
    api-key: ` + secret + `
observability:
  logs:
    request-log: true
server:
  commercial-mode: false
`
	if errWrite := os.WriteFile(path, []byte(contents), 0o600); errWrite != nil {
		t.Fatalf("write fixture: %v", errWrite)
	}
	loadTestConfig([]byte("enabled: true\nplan_config_path: " + path + "\n"))

	// UptimeSeconds truncates sub-second uptime, so wait until the process has been up for
	// at least a second before asserting it is positive.
	for time.Since(pluginStart) < time.Second {
		time.Sleep(10 * time.Millisecond)
	}

	req := pluginAPIRequest(BasePath + "/health")
	resp := route(&req)
	if resp.StatusCode != 200 {
		t.Fatalf("health status = %d, want 200", resp.StatusCode)
	}

	var decoded map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(resp.Body, &decoded); errUnmarshal != nil {
		t.Fatalf("health payload must be an object: %v", errUnmarshal)
	}

	rawHostConfig, ok := decoded["host_config"]
	if !ok {
		t.Fatal("health payload is missing host_config")
	}
	rawUptime, ok := decoded["uptime_seconds"]
	if !ok {
		t.Fatal("health payload is missing uptime_seconds")
	}
	var uptime int64
	if errUnmarshal := json.Unmarshal(rawUptime, &uptime); errUnmarshal != nil {
		t.Fatalf("uptime_seconds must be a number: %v", errUnmarshal)
	}
	if uptime <= 0 {
		t.Errorf("uptime_seconds = %d, want > 0", uptime)
	}

	var hostConfig map[string]any
	if errUnmarshal := json.Unmarshal(rawHostConfig, &hostConfig); errUnmarshal != nil {
		t.Fatalf("host_config must be an object: %v", errUnmarshal)
	}
	hostPath, ok := hostConfig["path"].(string)
	if !ok || hostPath == "" {
		t.Errorf("host_config.path = %v, want a non-empty string", hostConfig["path"])
	}
	if _, ok := hostConfig["readable"].(bool); !ok {
		t.Errorf("host_config.readable = %v, want a boolean", hostConfig["readable"])
	}

	if strings.Contains(string(resp.Body), secret) {
		t.Errorf("health payload leaked a credential value: %s", resp.Body)
	}
}
