package management

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/wkeking/clinepass-channel-monitor/internal/channellog"
	"github.com/wkeking/clinepass-channel-monitor/internal/config"
	"github.com/wkeking/clinepass-channel-monitor/internal/state"
)

// channelLogHealthKeys is the health contract of the request-log scanner, asserted on the wire
// rather than through a struct: the page reads keys.
var channelLogHealthKeys = []string{
	"enabled", "directory", "scanned", "parsed", "with_channel", "without_channel",
	"parse_failures", "deleted", "deleted_bytes", "skipped_young", "skipped_growing", "skipped_seen", "pruned", "pruned_bytes",
	"skip_main_log", "last_fact_at", "last_error", "last_error_at", "pending_files",
}

// channelLogFactKeys is the fact contract: every key is present on every fact, because the
// merge of the next round matches records to facts on the ones a failed request leaves empty.
var channelLogFactKeys = []string{
	"time", "path", "method", "session_id", "session_uuid", "has_session",
	"final_provider", "resolved_provider", "canonical_slug", "original_model_id",
	"pinned_provider", "affinity_outcome", "model_attempt_count",
	"total_provider_attempt_count", "fallbacks_available", "gateway_cost",
	"frames", "attempts_seen", "had_error_response", "source_file", "parsed_at",
}

// channelLogText is one request log in CPA's section order, carrying the gateway routing block
// the Responses translation drops. The session header and the arrival timestamp are the join
// key the next round needs, which is why they are part of the fixture.
func channelLogText(sessionHeader, timestamp string) string {
	return "=== REQUEST INFO ===\nVersion: v8.0.8\nURL: /v1/responses\nMethod: POST\n" +
		"Downstream Transport: http\nUpstream Transport: http\nTimestamp: " + timestamp + "\n\n" +
		"=== HEADERS ===\nAuthorization: Bearer sk-REDACTED-fixture-only\n" + sessionHeader + "\n" +
		"=== REQUEST BODY ===\n{\"input\":\"fixture\"}\n\n" +
		"=== API RESPONSE 1 ===\n" +
		`data: {"choices":[{"delta":{"provider_metadata":{"gateway":{"cost":"0.00001515","routing":{` +
		`"finalProvider":"deepseek","resolvedProvider":"deepseek",` +
		`"canonicalSlug":"deepseek/deepseek-v4.1-flash","originalModelId":"deepseek/deepseek-v4.1-flash",` +
		`"modelAttemptCount":1,"totalProviderAttemptCount":1,"fallbacksAvailable":[],` +
		`"affinity":{"outcome":"confirmed","pinnedProvider":"deepseek"}}}}}}],"id":"gen_fixture"}` + "\n" +
		"data: [DONE]\n\n" +
		"=== RESPONSE ===\nStatus: 200\n"
}

// deployChannelLog writes one request log, backdated past the scanner's age gate.
func deployChannelLog(t *testing.T, dir, name, sessionHeader, timestamp string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if errWrite := os.WriteFile(path, []byte(channelLogText(sessionHeader, timestamp)), 0o644); errWrite != nil {
		t.Fatalf("write %s: %v", path, errWrite)
	}
	old := time.Now().Add(-time.Hour)
	if errChtimes := os.Chtimes(path, old, old); errChtimes != nil {
		t.Fatalf("backdate %s: %v", path, errChtimes)
	}
	return path
}

// startChannelLogScanner publishes a started scanner over a temporary directory, the way the
// plugin does on a reconfigure. The state is package-wide, so the cleanup unsets it.
func startChannelLogScanner(t *testing.T, logDir string, deleteAfterRead bool) *channellog.Scanner {
	t.Helper()
	scanner := channellog.New(channellog.Options{
		Enabled:         true,
		Dir:             logDir,
		StoreDir:        t.TempDir(),
		MinAge:          0,
		DeleteAfterRead: deleteAfterRead,
	})
	scanner.Start()
	// One pass on the calling goroutine: the polling loop's first pass is asynchronous, and a
	// payload read immediately afterwards would race it.
	if _, errScan := scanner.ScanOnce(); errScan != nil {
		t.Fatalf("scan %s: %v", logDir, errScan)
	}
	t.Cleanup(func() {
		scanner.Stop()
		state.SetChannelLog(nil)
	})
	state.SetChannelLog(scanner)
	return scanner
}

// channelLogFromChannelView reads the channel_log member out of one /channel response.
func channelLogFromChannelView(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	envelope := decodeObject(t, body, "the /channel payload")
	requireKeys(t, "the /channel payload", envelope, "channel_log")
	return decodeObject(t, envelope["channel_log"], "channel_log")
}

// TestChannelLogIsPresentAndEmptyWhenDisabled pins the switched-off shape on both endpoints:
// every key present, the fact list an empty list rather than null, and the directory named
// from the defaults so an operator can see what the scanner would read.
func TestChannelLogIsPresentAndEmptyWhenDisabled(t *testing.T) {
	loadTestConfig(defaultConfigBytes())
	state.SetChannelLog(nil)
	t.Cleanup(func() { state.SetChannelLog(nil) })

	request := pluginAPIRequest(BasePath + "/channel?window=24h")
	response := route(&request)
	if response.StatusCode != 200 {
		t.Fatalf("GET /channel: status = %d", response.StatusCode)
	}
	channelLog := channelLogFromChannelView(t, response.Body)
	requireKeys(t, "channel_log", channelLog,
		"enabled", "delete_after_read", "min_age_seconds", "health", "facts", "facts_total")
	if got := string(channelLog["enabled"]); got != "false" {
		t.Errorf("channel_log.enabled = %s, want false with no scanner published", got)
	}
	if got := string(channelLog["delete_after_read"]); got != "true" {
		t.Errorf("channel_log.delete_after_read = %s, want the default true", got)
	}
	if got := string(channelLog["min_age_seconds"]); got != "5" {
		t.Errorf("channel_log.min_age_seconds = %s, want the default 5", got)
	}
	if got := string(channelLog["facts"]); got != "[]" {
		t.Errorf("channel_log.facts = %s, want an empty list rather than null", got)
	}
	if got := string(channelLog["facts_total"]); got != "0" {
		t.Errorf("channel_log.facts_total = %s, want 0", got)
	}
	health := decodeObject(t, channelLog["health"], "channel_log.health")
	requireKeys(t, "channel_log.health", health, channelLogHealthKeys...)
	if len(health) != len(channelLogHealthKeys) {
		t.Errorf("channel_log.health carries %d keys, want exactly %d: %v",
			len(health), len(channelLogHealthKeys), health)
	}
	if got := string(health["directory"]); got != `"`+config.DefaultChannelLogDir+`"` {
		t.Errorf("channel_log.health.directory = %s, want the default directory", got)
	}

	// The same object rides along in /health, beside channel_observation.
	raw, errMarshal := json.Marshal(buildHealthResponse())
	if errMarshal != nil {
		t.Fatalf("marshal /health: %v", errMarshal)
	}
	payload := decodeObject(t, raw, "the /health payload")
	requireKeys(t, "the /health payload", payload, "channel_observation", "channel_log")
	if got := string(decodeObject(t, payload["channel_log"], "channel_log")["facts"]); got != "[]" {
		t.Errorf("the /health channel_log facts = %s, want an empty list", got)
	}
}

// TestChannelLogCarriesTheParsedFacts drives a real scanner over a real log and reads it back
// through both endpoints: the health counters, the parsed channel, and the join key the merge
// of the next round needs.
func TestChannelLogCarriesTheParsedFacts(t *testing.T) {
	logDir := t.TempDir()
	deployChannelLog(t, logDir, "req.log", "Session_id: session-3f2a1b0c9d8e\n",
		"2026-10-02T22:18:46.622934644+08:00")
	startChannelLogScanner(t, logDir, true)
	loadTestConfig([]byte("channel_log_enabled: true\nchannel_log_dir: " + logDir + "\n"))

	request := pluginAPIRequest(BasePath + "/channel?window=24h")
	response := route(&request)
	channelLog := channelLogFromChannelView(t, response.Body)
	if got := string(channelLog["enabled"]); got != "true" {
		t.Errorf("channel_log.enabled = %s, want true while a scanner runs", got)
	}
	if got := string(channelLog["facts_total"]); got != "1" {
		t.Errorf("channel_log.facts_total = %s, want 1", got)
	}
	fact := decodeObject(t, firstElement(t, channelLog["facts"], "channel_log.facts"), "channel_log.facts[0]")
	requireKeys(t, "channel_log.facts[0]", fact, channelLogFactKeys...)
	if got := string(fact["final_provider"]); got != `"deepseek"` {
		t.Errorf("final_provider = %s, want deepseek", got)
	}
	if got := string(fact["session_id"]); got != `"session-3f2a1b0c9d8e"` {
		t.Errorf("session_id = %s, want the raw header value", got)
	}
	if got := string(fact["session_uuid"]); got != `"3f2a1b0c9d8e"` {
		t.Errorf("session_uuid = %s, want the header with the session- prefix stripped", got)
	}
	if got := string(fact["has_session"]); got != "true" {
		t.Errorf("has_session = %s, want true", got)
	}
	if got := string(fact["time"]); got != `"2026-10-02T22:18:46.622934644+08:00"` {
		t.Errorf("time = %s, want the log's own arrival timestamp", got)
	}

	health := decodeObject(t, channelLog["health"], "channel_log.health")
	if got := string(health["parsed"]); got != "1" {
		t.Errorf("channel_log.health.parsed = %s, want 1", got)
	}
	if got := string(health["with_channel"]); got != "1" {
		t.Errorf("channel_log.health.with_channel = %s, want 1", got)
	}
	if got := string(health["deleted"]); got != "1" {
		t.Errorf("channel_log.health.deleted = %s, want the one parsed file", got)
	}
}

// TestChannelLogFactsAreCappedNewestFirst pins the cap: the scanner holds what it scanned, the
// payload carries the newest channelLogFactLimit of them, and facts_total says what the cap
// hides.
func TestChannelLogFactsAreCappedNewestFirst(t *testing.T) {
	logDir := t.TempDir()
	const files = channelLogFactLimit + 1
	for index := 0; index < files; index++ {
		// One second apart, so "newest first" is a fact about the payload rather than about the
		// order the files happen to be listed in.
		stamp := time.Date(2026, 10, 2, 22, 0, index, 0, time.FixedZone("", 8*3600)).Format(time.RFC3339Nano)
		deployChannelLog(t, logDir, "req-"+strconv.Itoa(index)+".log", "", stamp)
	}
	startChannelLogScanner(t, logDir, false)

	request := pluginAPIRequest(BasePath + "/channel?window=24h")
	response := route(&request)
	channelLog := channelLogFromChannelView(t, response.Body)
	if got := string(channelLog["facts_total"]); got != strconv.Itoa(files) {
		t.Errorf("channel_log.facts_total = %s, want %d", got, files)
	}
	var facts []json.RawMessage
	if errUnmarshal := json.Unmarshal(channelLog["facts"], &facts); errUnmarshal != nil {
		t.Fatalf("channel_log.facts must be an array: %v", errUnmarshal)
	}
	if len(facts) != channelLogFactLimit {
		t.Fatalf("channel_log.facts carries %d facts, want the cap of %d", len(facts), channelLogFactLimit)
	}
	first := decodeObject(t, facts[0], "channel_log.facts[0]")
	want := time.Date(2026, 10, 2, 22, 0, files-1, 0, time.FixedZone("", 8*3600)).Format(time.RFC3339Nano)
	if got := string(first["time"]); got != `"`+want+`"` {
		t.Errorf("the newest fact is %s, want %s", got, want)
	}
}
