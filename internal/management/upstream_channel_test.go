package management

import (
	"encoding/json"
	"math"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wkeking/clinepass-channel-monitor/internal/config"
	"github.com/wkeking/clinepass-channel-monitor/internal/plan"
	"github.com/wkeking/clinepass-channel-monitor/internal/state"
)

// The 「上游推理渠道（官方 usage）」 dimension is the official-source answer beside the
// CPA-credential channel view: /channel carries it for the 渠道 section, and the same records
// reach the 官方用量明细 table through /health. Both halves are asserted on the wire here,
// because the page reads keys, not structs.

// startLivePoller publishes a real poller bound to the fake Cline API (the same fixture the
// /health binding test uses) and waits until it has fetched the official records. The poller
// polls in the background, so the cleanup stops it and unpublishes the state.
func startLivePoller(t *testing.T) *plan.Poller {
	t.Helper()
	server := fakeClineAPI(t, time.Now().UTC())
	cfg := config.Default()
	cfg.PlanAPIKey = syntheticPlanKey
	cfg.PlanBaseURL = server.URL
	// Point credential discovery at an empty directory so the test never reads the host's
	// own CPA configuration.
	cfg.PlanConfigPath = filepath.Join(t.TempDir(), "missing.yaml")
	cfg.PlanRefresh = config.Duration{Value: time.Minute, Set: true}

	state.SetConfig(cfg)
	poller := plan.Start(cfg)
	state.SetPlan(poller)
	t.Cleanup(func() {
		poller.Stop()
		state.SetPlan(nil)
	})

	deadline := time.Now().Add(15 * time.Second)
	for {
		snapshot := poller.Snapshot()
		if len(snapshot.Accounts) > 0 && len(snapshot.OfficialChannels) > 0 {
			return poller
		}
		if time.Now().After(deadline) {
			t.Fatalf("the poller never produced the upstream-channel snapshot: %+v", poller.Snapshot())
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// TestChannelViewOfficialChannelsWithoutCollector pins the empty state the page has to render
// honestly: with no poller running the key is present and empty, never null, and the metadata
// beside it says the collector is not the source of the emptiness.
func TestChannelViewOfficialChannelsWithoutCollector(t *testing.T) {
	loadTestConfig(defaultConfigBytes())
	state.SetPlan(nil)
	state.SetObservation(nil)
	t.Cleanup(func() { state.SetPlan(nil) })

	request := pluginAPIRequest(BasePath + "/channel?window=24h")
	response := route(&request)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /channel: status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	envelope := decodeObject(t, response.Body, "the /channel payload")
	requireKeys(t, "the /channel payload", envelope, "official_channels", "official_usage")
	if got := strings.TrimSpace(string(envelope["official_channels"])); got != "[]" {
		t.Errorf("official_channels = %s, want [] (an empty list, not null)", got)
	}

	var payload struct {
		OfficialChannels []plan.OfficialChannelRow `json:"official_channels"`
		OfficialUsage    plan.UsageState           `json:"official_usage"`
	}
	if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
		t.Fatalf("the /channel payload must decode: %v", errUnmarshal)
	}
	if payload.OfficialChannels == nil {
		t.Error("official_channels must decode into an empty slice, not nil")
	}
	if len(payload.OfficialChannels) != 0 {
		t.Errorf("official_channels = %+v, want no rows without a collector", payload.OfficialChannels)
	}
	if payload.OfficialUsage.Enabled || payload.OfficialUsage.Items != 0 || payload.OfficialUsage.Oldest != "" {
		t.Errorf("official_usage = %+v, want the disabled state without a poller", payload.OfficialUsage)
	}
}

// TestUpstreamChannelReachesThePage drives the whole path: a live poller against the fake
// Cline API, then the two payloads the page reads. /channel must carry the upstream-channel
// rows plus the collector's coverage, and the 官方用量明细 rows in /health must carry the
// provider of each record.
func TestUpstreamChannelReachesThePage(t *testing.T) {
	recorder, _ := startChannelRecorder(t)
	feedChannelRequest(t, recorder)
	startLivePoller(t)

	request := pluginAPIRequest(BasePath + "/channel?window=24h")
	response := route(&request)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /channel: status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	var payload struct {
		OfficialChannels []plan.OfficialChannelRow `json:"official_channels"`
		OfficialUsage    plan.UsageState           `json:"official_usage"`
	}
	if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
		t.Fatalf("the /channel payload must carry the official dimension: %v", errUnmarshal)
	}
	// The fixture serves one record per channel, so the rows are ordered by model name:
	// deepseek on vercel, glm on bedrock.
	want := []struct {
		provider string
		model    string
		input    int64
		cached   int64
		cost     float64
	}{
		{provider: "vercel", model: "deepseek/deepseek-v4.1-flash", input: 1000, cached: 800, cost: 1.0},
		{provider: "bedrock", model: "z-ai/glm-5.3", input: 2000, cached: 900, cost: 0.5},
	}
	if len(payload.OfficialChannels) != len(want) {
		t.Fatalf("official_channels = %+v, want %d rows", payload.OfficialChannels, len(want))
	}
	for index, expected := range want {
		row := payload.OfficialChannels[index]
		if row.InferenceProvider != expected.provider || row.Model != expected.model {
			t.Errorf("official_channels[%d] = %+v, want %s on %s", index, row, expected.model, expected.provider)
		}
		if row.Requests != 1 || row.InputTokens != expected.input || row.CachedTokens != expected.cached {
			t.Errorf("official_channels[%d] = %+v, want requests=1 input=%d cached=%d",
				index, row, expected.input, expected.cached)
		}
		if math.Abs(row.CostUSD-expected.cost) > 1e-9 {
			t.Errorf("official_channels[%d] cost = %v USD, want %v (micro-USD on the wire)",
				index, row.CostUSD, expected.cost)
		}
	}
	// The caption beside the table says how far back the records reach; that comes from the
	// collector's own state, so the payload has to carry it.
	if payload.OfficialUsage.Items != 2 || payload.OfficialUsage.Oldest == "" {
		t.Errorf("official_usage = %+v, want the retained records and their oldest timestamp", payload.OfficialUsage)
	}

	// The 官方用量明细 table reads the same records through /health: every per-model row has
	// to name the channel its records were served from, or the new column renders empty.
	healthRequest := pluginAPIRequest(BasePath + "/health")
	healthResponse := route(&healthRequest)
	if healthResponse.StatusCode != http.StatusOK {
		t.Fatalf("GET /health: status = %d, want %d", healthResponse.StatusCode, http.StatusOK)
	}
	healthEnvelope := decodeObject(t, healthResponse.Body, "the /health payload")
	requireKeys(t, "the /health payload", healthEnvelope, "plan")
	planEnvelope := decodeObject(t, healthEnvelope["plan"], "plan")
	// The plan payload keeps the collector state under its existing "usage" key; the
	// channel view carries the same state as "official_usage". Only the aggregate is new
	// here, and it must be on the wire.
	requireKeys(t, "plan", planEnvelope, "official_channels", "usage", "windows")

	var health struct {
		Plan plan.Quota `json:"plan"`
	}
	if errUnmarshal := json.Unmarshal(healthResponse.Body, &health); errUnmarshal != nil {
		t.Fatalf("the /health payload must decode: %v", errUnmarshal)
	}
	if len(health.Plan.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(health.Plan.Accounts))
	}
	models := health.Plan.Accounts[0].Windows["24h"].Models
	if len(models) != 2 {
		t.Fatalf("24h per-model rows = %+v, want the two fixture records", models)
	}
	channels := map[string]string{}
	for _, row := range models {
		channels[row.Model] = row.InferenceProvider
	}
	if channels["deepseek/deepseek-v4.1-flash"] != "vercel" || channels["z-ai/glm-5.3"] != "bedrock" {
		t.Errorf("per-model rows must carry the upstream channel of their records, got %+v", channels)
	}
	// The plan payload mirrors the primary account's aggregate, so a client that only reads
	// the primary view still sees the official channel dimension.
	if len(health.Plan.OfficialChannels) != len(want) {
		t.Errorf("plan official_channels = %+v, want the primary account's rows", health.Plan.OfficialChannels)
	}
}

// TestUpstreamChannelTableIsPresent asserts the page carries the new table next to the
// CPA-side 渠道分布 table, the caliber caption, the column on 官方用量明细, and the render
// function the loader wires to the payload.
func TestUpstreamChannelTableIsPresent(t *testing.T) {
	page := string(indexHTML(nil))
	for _, id := range []string{"official-channels", "official-channels-note", "official-channels-caption"} {
		if !strings.Contains(page, `id="`+id+`"`) {
			t.Errorf("page is missing the %s element", id)
		}
	}
	if !strings.Contains(page, "上游推理渠道（官方 usage）") {
		t.Error("page is missing the 上游推理渠道（官方 usage） heading")
	}
	if !strings.Contains(page, "<th>上游渠道</th>") {
		t.Error("page is missing the 上游渠道 column header")
	}
	if !strings.Contains(page, "function renderOfficialChannels(") {
		t.Error("page is missing the renderOfficialChannels function")
	}
	// The new table sits beside the CPA-credential channel distribution and its caption follows
	// it. (The per-model table that used to come after was removed: it answered no question the
	// two channel tables do not answer better.)
	providers := strings.Index(page, `id="channel-providers"`)
	official := strings.Index(page, `id="official-channels"`)
	caption := strings.Index(page, `id="official-channels-caption"`)
	if providers < 0 || official < 0 || caption < 0 {
		t.Fatalf("page is missing a table of the 渠道 section: providers=%d official=%d caption=%d",
			providers, official, caption)
	}
	if !(providers < official && official < caption) {
		t.Errorf("the upstream-channel table must follow 渠道分布: providers=%d official=%d caption=%d",
			providers, official, caption)
	}

	body := jsFunctionBody(t, page, "renderOfficialChannels")
	for _, needle := range []string{
		"official_channels", "official_usage", "moneyText(row.cost_usd)", "empty-row",
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("renderOfficialChannels must use %s, got:\n%s", needle, body)
		}
	}
	// The caliber has to be on the page: official records only, so failures can only be
	// counted per CPA credential, and the coverage is stated with the oldest record.
	for _, needle := range []string{"来自 Cline 官方 per-request usage", "只含成功计费请求", "失败(502)不在其中", "数据覆盖到 "} {
		if !strings.Contains(body, needle) {
			t.Errorf("the caliber caption is missing %q, got:\n%s", needle, body)
		}
	}
	// An empty collector says the records have not arrived, not that they are zero.
	for _, needle := range []string{"官方用量记录还没有到", "不是 0"} {
		if !strings.Contains(body, needle) {
			t.Errorf("the empty state must say %q, got:\n%s", needle, body)
		}
	}
	// The overview's per-model table reads the same field; its info tooltip must not keep
	// claiming the official records carry no channel at all.
	if strings.Contains(page, "官方记录里没有最终上游渠道") {
		t.Error("the 官方用量明细 tooltip still claims the official records carry no upstream channel")
	}
}
