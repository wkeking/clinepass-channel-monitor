package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"

	"github.com/wkeking/clinepass-channel-monitor/internal/observation"
	"github.com/wkeking/clinepass-channel-monitor/internal/state"
)

// reconfigure hands a configuration block to the plugin the way the host does: one
// plugin.reconfigure call carrying the block as YAML.
func reconfigure(t *testing.T, block string) {
	t.Helper()
	request, errMarshal := json.Marshal(map[string]any{"config_yaml": []byte(block)})
	if errMarshal != nil {
		t.Fatalf("marshal the reconfigure request: %v", errMarshal)
	}
	if _, errCall := HandleMethod(pluginabi.MethodPluginReconfigure, request); errCall != nil {
		t.Fatalf("plugin.reconfigure: %v", errCall)
	}
}

// TestChannelLogFollowsTheConfiguration drives the lifecycle the host actually calls: a
// reconfigure starts the request-log scanner when the block asks for it, a quiesce stops it,
// and a block that leaves the switch alone never publishes one at all — which is what keeps
// the default state free of a goroutine and of any directory access.
func TestChannelLogFollowsTheConfiguration(t *testing.T) {
	dir := t.TempDir()
	previous := state.Config()
	t.Cleanup(func() {
		Shutdown()
		state.SetConfig(previous)
	})
	// The plan poller is off so the test reaches no upstream API; the observation collector
	// writes into the same temporary directory.
	base := "plan_enabled: false\nchannel_store_dir: " + dir + "\n"

	reconfigure(t, base+"channel_log_dir: "+dir+"\n")
	if scanner := state.ChannelLog(); scanner != nil {
		t.Error("a block without channel_log_enabled must publish no scanner")
	}

	reconfigure(t, base+"channel_log_enabled: true\nchannel_log_dir: "+dir+"\n")
	scanner := state.ChannelLog()
	if scanner == nil {
		t.Fatal("channel_log_enabled: true must publish a running scanner")
	}
	options := scanner.Options()
	if options.Dir != dir || !options.DeleteAfterRead {
		t.Errorf("scanner options = %+v, want the configured directory and the default deletion", options)
	}

	// A second reconfigure must replace the scanner, not add a second reader on the same
	// directory: two of them would race for the same files.
	reconfigure(t, base+"channel_log_enabled: true\nchannel_log_dir: "+dir+"\n")
	replaced := state.ChannelLog()
	if replaced == nil || replaced == scanner {
		t.Error("a reconfigure must start a fresh scanner in place of the previous one")
	}

	if _, errCall := HandleMethod(pluginabi.MethodPluginQuiesce, nil); errCall != nil {
		t.Fatalf("plugin.quiesce: %v", errCall)
	}
	if scanner := state.ChannelLog(); scanner != nil {
		t.Error("a quiesce must stop the scanner and unpublish it")
	}
	// A quiesce may be followed by a shutdown: stopping twice must stay harmless.
	Shutdown()
	if scanner := state.ChannelLog(); scanner != nil {
		t.Error("a shutdown must leave no scanner published")
	}
}

// capabilitiesWithObservation renders the declared capabilities the way the host receives
// them, with channel observation forced on or off. The JSON is what the host reads, so
// asserting on it is also what catches a capability that is declared by accident: a field
// that does not exist in the struct cannot appear in the object at all.
func capabilitiesWithObservation(t *testing.T, enabled bool) map[string]bool {
	t.Helper()
	previous := state.Config()
	defer state.SetConfig(previous)
	cfg := previous
	cfg.ChannelObserveEnabled = enabled
	state.SetConfig(cfg)

	raw, errMarshal := json.Marshal(buildRegistration().Capabilities)
	if errMarshal != nil {
		t.Fatalf("marshal capabilities: %v", errMarshal)
	}
	declared := map[string]bool{}
	if errUnmarshal := json.Unmarshal(raw, &declared); errUnmarshal != nil {
		t.Fatalf("capabilities are not a JSON object: %v (%s)", errUnmarshal, raw)
	}
	return declared
}

// TestPanelHidesFixedDefaults pins the configuration panel contract: only the knobs a
// deployment genuinely has to move are offered there, while the fixed defaults stay out.
func TestPanelHidesFixedDefaults(t *testing.T) {
	fields := map[string]bool{}
	for _, field := range buildRegistration().Metadata.ConfigFields {
		fields[field.Name] = true
	}
	for _, name := range []string{
		"mask_api_key", "log_events", "store_planning_reasoning", "sample_rate",
		"plan_enabled", "plan_api_key", "plan_base_url", "plan_daily_enabled",
		"plan_usage_enabled", "plan_usage_refresh",
		// The per-request statistics keys are gone from the plugin entirely.
		"hosts", "require_routing_marker", "unmatched_host_samples", "ring_size",
		"jsonl_enabled", "jsonl_dir", "retention_days", "join_window", "orphan_ttl",
		"capture_cost", "capture_cache",
	} {
		if fields[name] {
			t.Errorf("%s must not be exposed in the configuration panel", name)
		}
	}
	for _, name := range []string{
		"timezone", "plan_config_path", "plan_refresh",
		// The channel observation knobs must stay reachable from the panel: turning the
		// collector off is the documented rollback for the per-frame scan cost.
		"channel_observe_enabled", "channel_store_dir", "channel_retention_days",
		"channel_max_size_mb", "channel_baseline_provider",
		// The request-log scanner's own knobs, including the switch: it is off by default and
		// the only way to turn it on is through the configuration block.
		"channel_log_enabled", "channel_log_dir", "channel_log_delete_after_read",
		"channel_log_min_age_seconds",
	} {
		if !fields[name] {
			t.Errorf("%s must stay in the configuration panel", name)
		}
	}
}

// TestDeclaresManagementAndUsageOnly is the regression guard for the reason this release
// exists: every request-path capability makes CPA clone request bodies and hand them across
// the plugin ABI, and the old response_stream_interceptor only ever saw OpenAI chat traffic.
// The usage hook is the one capability this plugin declares, and only while the collector
// runs; nothing may come back that puts the plugin on the request path.
func TestDeclaresManagementAndUsageOnly(t *testing.T) {
	declared := capabilitiesWithObservation(t, true)
	if !declared["management_api"] {
		t.Error("the plugin must keep its Management API capability")
	}
	if !declared["usage_plugin"] {
		t.Error("usage_plugin must be declared while channel observation is on")
	}
	for _, name := range []string{
		"request_interceptor",
		"request_translator",
		"request_normalizer",
		"response_translator",
		"response_before_translator",
		"response_after_translator",
		"response_stream_interceptor",
		"thinking_applier",
		"executor",
		"model_registrar",
		"model_provider",
		"auth_provider",
		"frontend_auth_provider",
		"command_line_plugin",
	} {
		if declared[name] {
			t.Errorf("%s must not be declared: it would put the plugin back on the request path", name)
		}
	}
}

// TestUsageCapabilityFollowsObservation pins the switch: with observation off the host must
// not call usage.handle at all, which is what keeps the disabled state free of any per-request
// work.
func TestUsageCapabilityFollowsObservation(t *testing.T) {
	if declared := capabilitiesWithObservation(t, false); declared["usage_plugin"] {
		t.Error("usage_plugin must not be declared while channel observation is off")
	}
	if declared := capabilitiesWithObservation(t, true); !declared["usage_plugin"] || !declared["management_api"] {
		t.Error("turning observation back on must declare usage_plugin and keep the Management API capability")
	}
}

// TestManagementRoutesAreDataEndpoints pins the served routes: the v0.1.x statistics
// endpoints stay gone, while the channel view endpoints must be declared here because the
// host only forwards declared paths (an undeclared one is answered by the host's own 404).
func TestManagementRoutesAreDataEndpoints(t *testing.T) {
	const base = "/v0/management/plugins/clinepass-channel-monitor"
	want := []string{base + "/health", base + "/channel", base + "/channel.csv"}
	routes := buildManagementRegistration().Routes
	if len(routes) != len(want) {
		t.Fatalf("routes = %+v, want %v", routes, want)
	}
	for i, path := range want {
		if routes[i].Path != path {
			t.Errorf("routes[%d] = %q, want %q", i, routes[i].Path, path)
		}
	}
	for _, gone := range []string{base + "/stats", base + "/events", base + "/export"} {
		for _, route := range routes {
			if route.Path == gone {
				t.Errorf("route %q must not exist: the v0.1.x statistics endpoints stay gone", route.Path)
			}
		}
	}
}

// requestLogText is one CPA request log in the section order CPA writes it, carrying the
// gateway routing block the Responses translation drops. Only the timestamp and the session
// header matter to the join; the rest makes the fixture a request log rather than a JSON blob.
func requestLogText(sessionUUID string, at time.Time) string {
	return "=== REQUEST INFO ===\nVersion: v8.0.8\nURL: /v1/responses\nMethod: POST\n" +
		"Timestamp: " + at.UTC().Format(time.RFC3339Nano) + "\n\n" +
		"=== HEADERS ===\nAuthorization: Bearer sk-REDACTED-fixture-only\n" +
		"Session_id: session-" + sessionUUID + "\n" +
		"=== REQUEST BODY ===\n{\"input\":\"fixture\"}\n\n" +
		"=== API RESPONSE 1 ===\n" +
		`data: {"choices":[{"delta":{"provider_metadata":{"gateway":{"cost":"0.00001515","routing":{` +
		`"finalProvider":"moonshot","resolvedProvider":"moonshot",` +
		`"canonicalSlug":"moonshot/deepseek-v4.1-flash","originalModelId":"moonshot/deepseek-v4.1-flash",` +
		`"modelAttemptCount":1,"totalProviderAttemptCount":1,"fallbacksAvailable":[],` +
		`"affinity":{"outcome":"confirmed","pinnedProvider":"moonshot"}}}}}}],"id":"gen_fixture"}` + "\n" +
		"data: [DONE]\n\n" +
		"=== RESPONSE ===\nStatus: 200\n"
}

// usagePayloadForJoin is the usage.handle payload of the request that log belongs to: the same
// session, the same instant, and the CPA credential — which is all the usage hook knows.
func usagePayloadForJoin(sessionUUID string, at time.Time) []byte {
	return []byte(fmt.Sprintf(
		`{"Provider":"openai-compatible-cline1","BaseURL":"https://api.cline.bot",`+
			`"ExecutorType":"OpenAICompatExecutor","Model":"cline-pass/deepseek-v4.1-flash",`+
			`"RequestID":"req-join-fixture","TraceID":"req-join-fixture",`+
			`"SessionID":"codex:session-%s","AuthID":"openai-compatibility:cline1:fixture",`+
			`"AuthType":"apikey","Stream":true,"Failed":false,`+
			`"Detail":{"InputTokens":800,"OutputTokens":200,"TotalTokens":1000},`+
			`"RequestedAt":"%s","Latency":1097125788,"TTFT":643260112}`,
		sessionUUID, at.UTC().Format(time.RFC3339Nano)))
}

// TestObservationJoinsTheRequestLog drives the wiring a reconfigure does: the collector is
// handed the running request-log scanner, so a record the host reports afterwards has to come
// back carrying the gateway channel only that log knew. This is the seam the whole feature
// rests on, and a wiring that silently passed a nil source would look exactly like a
// deployment with no traffic in it.
func TestObservationJoinsTheRequestLog(t *testing.T) {
	dir := t.TempDir()
	previous := state.Config()
	t.Cleanup(func() {
		Shutdown()
		state.SetConfig(previous)
	})

	const sessionUUID = "5e4d3c2b1a09"
	at := time.Now().UTC()
	path := filepath.Join(dir, "req.log")
	if errWrite := os.WriteFile(path, []byte(requestLogText(sessionUUID, at)), 0o644); errWrite != nil {
		t.Fatalf("write %s: %v", path, errWrite)
	}
	// The scanner only reads a file CPA has stopped writing, so the fixture is backdated past
	// the age gate.
	old := time.Now().Add(-time.Hour)
	if errChtimes := os.Chtimes(path, old, old); errChtimes != nil {
		t.Fatalf("backdate %s: %v", path, errChtimes)
	}

	reconfigure(t, "plan_enabled: false\n"+
		"channel_store_dir: "+dir+"\n"+
		"channel_log_enabled: true\nchannel_log_dir: "+dir+"\n")

	scanner := state.ChannelLog()
	if scanner == nil {
		t.Fatal("channel_log_enabled: true must publish a running scanner")
	}
	recorder := state.Observation()
	if recorder == nil {
		t.Fatal("channel_observe_enabled defaults to true, so a recorder must be published")
	}
	// The scanner's first pass runs in its own goroutine: wait for the fact instead of racing
	// it.
	deadline := time.Now().Add(10 * time.Second)
	for len(scanner.Facts()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(scanner.Facts()) == 0 {
		t.Fatal("the scanner never parsed the deployed request log")
	}

	if _, errHandle := observation.HandleUsage(usagePayloadForJoin(sessionUUID, at)); errHandle != nil {
		t.Fatalf("usage.handle: %v", errHandle)
	}
	recorder.Flush()

	records, errRecords := recorder.ChannelRecordsSince(observation.Window24h)
	if errRecords != nil {
		t.Fatalf("ChannelRecordsSince: %v", errRecords)
	}
	if len(records) != 1 {
		t.Fatalf("want exactly 1 record, got %d: %+v", len(records), records)
	}
	record := records[0]
	if record.CPProvider != "openai-compatible-cline1" {
		t.Errorf("cpa_provider = %q, want the credential the payload reported", record.CPProvider)
	}
	if record.GatewayProvider != "moonshot" || record.FinalProvider != "moonshot" {
		t.Errorf("channel = %q/%q, want the channel the request log named", record.GatewayProvider, record.FinalProvider)
	}
	if record.ChannelSource != observation.ChannelSourceLog {
		t.Errorf("channel_source = %q, want %q: the record was not joined to the fact",
			record.ChannelSource, observation.ChannelSourceLog)
	}
	summary := recorder.Summary(observation.Window24h)
	if summary.Resolved != 1 || summary.Unresolved != 0 {
		t.Errorf("resolved/unresolved = %d/%d, want 1/0", summary.Resolved, summary.Unresolved)
	}
	if len(summary.Providers) != 1 || summary.Providers[0].Provider != "moonshot" {
		t.Errorf("providers = %+v, want the real channel", summary.Providers)
	}
	if len(summary.CPAProviders) != 1 || summary.CPAProviders[0].Provider != "openai-compatible-cline1" {
		t.Errorf("cpa_providers = %+v, want the credential", summary.CPAProviders)
	}
}
