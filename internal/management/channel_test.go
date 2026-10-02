package management

import (
	"encoding/csv"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wkeking/clinepass-channel-monitor/internal/observation"
	"github.com/wkeking/clinepass-channel-monitor/internal/state"
)

// The 「渠道」 view is three things that have to agree: the page's static skeleton, the JSON
// behind it (/channel) and the export beside it (/channel.csv). One completed request is fed
// through a real recorder over the same usage.handle path a deployment uses, and the fixture
// lands on the baseline channel, which is what makes the off_baseline column checkable.

const (
	// channelFixtureBaseline is the provider the recorder is configured to treat as the
	// official channel. The fixture reports it, so the fed request is on baseline.
	channelFixtureBaseline = "deepseek"
	// channelFixtureRequestID identifies the one request every test here feeds.
	channelFixtureRequestID = "req-channel-fixture-1"
	// channelFixtureModel is the model the client asked for.
	channelFixtureModel = "deepseek-flash-1"
)

// channelUsagePayload renders the usage.handle payload of one completed request. Provider is
// the credential the request was routed to — the channel the view answers with — and
// RequestedAt is stamped at call time so the record falls inside the window the handlers
// query. Both timings are zero on purpose: such a record carries no measured decode speed,
// which is what makes the "no tokens_per_second key" assertion below meaningful.
func channelUsagePayload() []byte {
	return []byte(`{"Provider":"` + channelFixtureBaseline + `","BaseURL":"https://api.cline.bot",` +
		`"ExecutorType":"OpenAICompatExecutor","Model":"` + channelFixtureModel + `","Alias":"deepseek-flash-2",` +
		`"APIKey":"sk_channel_fixture","RequestID":"` + channelFixtureRequestID + `","TraceID":"` + channelFixtureRequestID + `",` +
		`"SessionID":"sess-channel-fixture","ParentSessionID":"","AuthID":"openai-compatibility:cline1:fixture",` +
		`"AuthIndex":"fixture-index","AuthType":"apikey","Source":"sk_channel_fixture","ReasoningEffort":"high",` +
		`"ServiceTier":"auto","ResponseServiceTier":"","ResponseModel":"deepseek/deepseek-v4.1-flash",` +
		`"Generate":true,"Stream":true,"Failed":false,"Failure":{"StatusCode":0,"Body":""},` +
		`"Detail":{"InputTokens":800,"OutputTokens":200,"ReasoningTokens":200,"CachedTokens":512,` +
		`"CacheReadTokens":0,"CacheCreationTokens":0,"TotalTokens":1000},` +
		`"RequestedAt":"` + time.Now().UTC().Format(time.RFC3339) + `","Latency":0,"TTFT":0}`)
}

// startChannelRecorder publishes a started collector and points both entry points at it:
// the handlers read state.Observation(), the usage entry point reads observation.Active().
// The directory is returned because /health and the view both report it. Both are package
// state, so the cleanup unsets them: a leak here would decide the next test's answer.
func startChannelRecorder(t *testing.T) (*observation.Recorder, string) {
	t.Helper()
	loadTestConfig(defaultConfigBytes())
	directory := t.TempDir()
	recorder := observation.New(observation.Options{
		Enabled:       true,
		Directory:     directory,
		RetentionDays: 3,
		MaxSizeMB:     16,
		Baseline:      channelFixtureBaseline,
	})
	recorder.Start()
	t.Cleanup(func() {
		state.SetObservation(nil)
		observation.SetActive(nil)
		recorder.Stop()
	})
	state.SetObservation(recorder)
	observation.SetActive(recorder)
	return recorder, directory
}

// feedChannelRequest replays one completed request the way the host does: a single
// usage.handle call. Flush is mandatory, not cosmetic: the writer is asynchronous and the
// view reads its records back from the store.
func feedChannelRequest(t *testing.T, recorder *observation.Recorder) {
	t.Helper()
	if _, errHandle := observation.HandleUsage(channelUsagePayload()); errHandle != nil {
		t.Fatalf("HandleUsage: %v", errHandle)
	}
	recorder.Flush()
}

// decodeObject decodes one JSON object into its raw members, so a key can be asserted on the
// wire instead of through a struct this test chose the shape of.
func decodeObject(t *testing.T, raw []byte, context string) map[string]json.RawMessage {
	t.Helper()
	var object map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(raw, &object); errUnmarshal != nil {
		t.Fatalf("%s must be a JSON object: %v", context, errUnmarshal)
	}
	return object
}

// requireKeys fails for every member the payload is expected to carry and does not.
func requireKeys(t *testing.T, context string, object map[string]json.RawMessage, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if _, ok := object[key]; !ok {
			t.Errorf("%s is missing %q", context, key)
		}
	}
}

// firstElement returns the first member of a JSON array, failing when the array is empty or
// not an array at all.
func firstElement(t *testing.T, raw []byte, context string) []byte {
	t.Helper()
	var elements []json.RawMessage
	if errUnmarshal := json.Unmarshal(raw, &elements); errUnmarshal != nil {
		t.Fatalf("%s must be a JSON array: %v", context, errUnmarshal)
	}
	if len(elements) == 0 {
		t.Fatalf("%s is empty, want the record of the fed request", context)
	}
	return elements[0]
}

// TestChannelViewIsPresent asserts the page carries the channel view's static skeleton and
// the render functions the loader wires to it. The skeleton alone is not enough: the loader
// has to ask for the payload, or the section never fills.
//
// The baseline channel name must come from the payload at render time. The page is a static
// shell that embeds no recorded data (TestIndexPageCarriesNoData enforces that), so nothing
// asserted here may name a provider.
func TestChannelViewIsPresent(t *testing.T) {
	page := string(indexHTML(nil))
	for _, id := range []string{
		"channel-section", "channel-csv", "channel-headline", "channel-cards",
		"channel-providers", "channel-models", "channel-hours", "channel-records",
		"channel-timeline",
	} {
		if !strings.Contains(page, `id="`+id+`"`) {
			t.Errorf("page is missing the %s element", id)
		}
	}
	if !strings.Contains(page, "导出 CSV") {
		t.Error("page is missing the 导出 CSV button")
	}
	for _, name := range []string{"renderChannel", "channelBaselineName", "channelOfRecord", "channelIsOff"} {
		if !strings.Contains(page, "function "+name+"(") {
			t.Errorf("page is missing the %s function", name)
		}
	}
	if !strings.Contains(page, `api("/channel?window="`) {
		t.Error(`the loader must fetch the view from "/channel?window="`)
	}
	if body := jsFunctionBody(t, page, "channelBaselineName"); !strings.Contains(body, "baseline_provider") {
		t.Errorf("channelBaselineName must read the baseline from the payload, got: %s", body)
	}
	if body := jsFunctionBody(t, page, "channelIsOff"); !strings.Contains(body, "channelOfRecord(record)") {
		t.Errorf("channelIsOff must ask channelOfRecord which channel a row landed on, got: %s", body)
	}
}

// TestChannelViewPayload drives a real recorder through the usage entry point and
// reads the answer back through the /channel handler: the enabled flag, the window summary,
// the raw record behind it and the collector's health.
func TestChannelViewPayload(t *testing.T) {
	recorder, directory := startChannelRecorder(t)
	feedChannelRequest(t, recorder)

	request := pluginAPIRequest(BasePath + "/channel?window=24h")
	response := route(&request)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /channel: status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	if contentType := response.Headers.Get("Content-Type"); !strings.Contains(contentType, "application/json") {
		t.Fatalf("GET /channel: content-type = %q, want application/json", contentType)
	}

	envelope := decodeObject(t, response.Body, "the /channel payload")
	requireKeys(t, "the /channel payload", envelope, "enabled", "summary", "records", "records_total", "health")
	requireKeys(t, "summary", decodeObject(t, envelope["summary"], "summary"),
		"resolved_requests", "off_baseline_requests", "baseline_provider", "hours")
	recordKeys := decodeObject(t, firstElement(t, envelope["records"], "records"), "records[0]")
	requireKeys(t, "records[0]", recordKeys, "final_provider", "resolved_provider", "ttft_ms")

	var payload struct {
		Enabled      bool                 `json:"enabled"`
		Summary      observation.Summary  `json:"summary"`
		Records      []observation.Record `json:"records"`
		RecordsTotal int                  `json:"records_total"`
		Health       observation.Health   `json:"health"`
	}
	if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
		t.Fatalf("the /channel payload must decode into the view shape: %v", errUnmarshal)
	}
	if !payload.Enabled {
		t.Error("enabled = false, want true while a recorder is published")
	}
	if payload.Summary.Resolved != 1 {
		t.Errorf("resolved_requests = %d, want 1 for the one fed request", payload.Summary.Resolved)
	}
	if payload.Summary.OffBaseline != 0 {
		t.Errorf("off_baseline_requests = %d, want 0: the fixture landed on the baseline channel", payload.Summary.OffBaseline)
	}
	if payload.Summary.Baseline != channelFixtureBaseline {
		t.Errorf("baseline_provider = %q, want %q", payload.Summary.Baseline, channelFixtureBaseline)
	}
	if payload.Summary.Window != "24h" {
		t.Errorf("window = %q, want 24h", payload.Summary.Window)
	}
	// The timeline covers the window hour by hour, not only the hours that saw traffic: a
	// quiet hour has to be a gap, not a missing point that shifts its neighbours.
	hours := payload.Summary.Hours
	if len(hours) < 24 {
		t.Errorf("hours = %d points, want the whole 24h window covered", len(hours))
	}
	if len(hours) > 0 {
		first, last := hours[0].Hour, hours[len(hours)-1].Hour
		if want := payload.Summary.From.Truncate(time.Hour); !first.Equal(want) {
			t.Errorf("the timeline starts at %v, want %v", first, want)
		}
		if want := payload.Summary.To.Truncate(time.Hour); !last.Equal(want) {
			t.Errorf("the timeline ends at %v, want %v", last, want)
		}
	}
	if payload.RecordsTotal != 1 || len(payload.Records) != 1 {
		t.Fatalf("records_total = %d with %d records, want the one fed request", payload.RecordsTotal, len(payload.Records))
	}
	record := payload.Records[0]
	if record.RequestID != channelFixtureRequestID {
		t.Errorf("request_id = %q, want %q", record.RequestID, channelFixtureRequestID)
	}
	if record.FinalProvider != channelFixtureBaseline || record.ResolvedProvider != channelFixtureBaseline {
		t.Errorf("channel = %q/%q, want %q/%q", record.FinalProvider, record.ResolvedProvider,
			channelFixtureBaseline, channelFixtureBaseline)
	}
	if record.TTFTMs != 0 {
		t.Errorf("ttft_ms = %d, want 0: the fixture payload reports no TTFT", record.TTFTMs)
	}
	if !payload.Health.Enabled || payload.Health.Directory != directory {
		t.Errorf("health = enabled %v in %q, want enabled true in %q", payload.Health.Enabled, payload.Health.Directory, directory)
	}
	if payload.Health.Events != 1 {
		t.Errorf("health events = %d, want the one fed request", payload.Health.Events)
	}
	// tokens_per_second is the one channel field this fixture cannot carry: Record.TPS is
	// `omitempty` on the record and is computed only from a decode window at or above
	// observation.minDecodeWindowMs, while the fixture reports zero timings. The contract is
	// pinned in both halves instead: a zero-speed record leaves the key out, which is the case
	// the page renders as "—", and a record that does carry a speed marshals it under the same
	// key the view reads (checked below).
	if _, present := recordKeys["tokens_per_second"]; present {
		t.Errorf("records[0] carries tokens_per_second = %s for a record with no measured decode time, want the key omitted",
			recordKeys["tokens_per_second"])
	}
	encoded, errMarshal := json.Marshal(observation.Record{
		FinalProvider:    channelFixtureBaseline,
		ResolvedProvider: channelFixtureBaseline,
		TTFTMs:           42,
		TPS:              1234.5,
	})
	if errMarshal != nil {
		t.Fatalf("a record must marshal: %v", errMarshal)
	}
	for _, key := range []string{"final_provider", "resolved_provider", "ttft_ms", "tokens_per_second"} {
		if !strings.Contains(string(encoded), `"`+key+`"`) {
			t.Errorf("the record the view marshals is missing the %s key: %s", key, encoded)
		}
	}
}

// TestChannelInvalidWindow asserts both endpoints reject a window they cannot answer with a
// JSON error code the page can branch on, instead of quietly serving the default one.
func TestChannelInvalidWindow(t *testing.T) {
	loadTestConfig(defaultConfigBytes())
	state.SetObservation(nil)
	for _, path := range []string{"/channel", "/channel.csv"} {
		request := pluginAPIRequest(BasePath + path + "?window=30d")
		response := route(&request)
		if response.StatusCode != http.StatusBadRequest {
			t.Errorf("GET %s?window=30d: status = %d, want %d", path, response.StatusCode, http.StatusBadRequest)
			continue
		}
		if contentType := response.Headers.Get("Content-Type"); !strings.Contains(contentType, "application/json") {
			t.Errorf("GET %s?window=30d: content-type = %q, want application/json", path, contentType)
		}
		var payload struct {
			Error string `json:"error"`
		}
		if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
			t.Fatalf("GET %s?window=30d: the error must be JSON: %v", path, errUnmarshal)
		}
		if payload.Error != "invalid_window" {
			t.Errorf("GET %s?window=30d: error = %q, want invalid_window", path, payload.Error)
		}
	}
}

// TestChannelCSVExport asserts the export is a real attachment with the channel columns and
// one row per stored record. The row of the fed request is on the baseline channel, so its
// off_baseline cell has to be empty rather than "no": the column marks deviation.
func TestChannelCSVExport(t *testing.T) {
	recorder, _ := startChannelRecorder(t)
	feedChannelRequest(t, recorder)

	request := pluginAPIRequest(BasePath + "/channel.csv?window=24h")
	response := route(&request)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /channel.csv: status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	if contentType := response.Headers.Get("Content-Type"); !strings.Contains(contentType, "text/csv") {
		t.Errorf("GET /channel.csv: content-type = %q, want text/csv", contentType)
	}
	if disposition := response.Headers.Get("Content-Disposition"); !strings.HasPrefix(disposition, "attachment") {
		t.Errorf("GET /channel.csv: content-disposition = %q, want an attachment", disposition)
	}

	rows, errRead := csv.NewReader(strings.NewReader(string(response.Body))).ReadAll()
	if errRead != nil {
		t.Fatalf("the export must parse as CSV: %v", errRead)
	}
	if len(rows) < 2 {
		t.Fatalf("the export has %d rows, want a header and the data row: %q", len(rows), response.Body)
	}
	header, row := rows[0], rows[1]
	if len(row) != len(header) {
		t.Fatalf("the data row has %d fields, the header has %d", len(row), len(header))
	}
	column := map[string]int{}
	for position, name := range header {
		column[name] = position
	}
	for _, name := range []string{"final_provider", "resolved_provider", "pinned_provider", "canonical_slug", "off_baseline"} {
		if _, ok := column[name]; !ok {
			t.Fatalf("the export has no %s column: %v", name, header)
		}
	}
	if got := row[column["final_provider"]]; got != channelFixtureBaseline {
		t.Errorf("final_provider = %q, want %q", got, channelFixtureBaseline)
	}
	if got := row[column["resolved_provider"]]; got != channelFixtureBaseline {
		t.Errorf("resolved_provider = %q, want %q", got, channelFixtureBaseline)
	}
	if got := row[column["off_baseline"]]; got != "" {
		t.Errorf("off_baseline = %q for a row on the baseline channel, want it empty", got)
	}
	if got, want := response.Headers.Get("X-Record-Count"), strconv.Itoa(len(rows)-1); got != want {
		t.Errorf("X-Record-Count = %q, want %q", got, want)
	}
}
