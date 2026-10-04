package management

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/wkeking/clinepass-channel-monitor/internal/buildinfo"
	"github.com/wkeking/clinepass-channel-monitor/internal/config"
	"github.com/wkeking/clinepass-channel-monitor/internal/state"
)

// defaultConfigBytes is the configuration a host with an empty plugin block sends.
func defaultConfigBytes() []byte {
	return []byte("enabled: true\npriority: 1\n")
}

// pluginAPIRequest builds a management request the way the host delivers it.
func pluginAPIRequest(rawPath string) pluginapi.ManagementRequest {
	parsed, errParse := url.Parse(rawPath)
	if errParse != nil {
		panic(errParse)
	}
	return pluginapi.ManagementRequest{Method: "GET", Path: parsed.Path, Query: parsed.Query()}
}

// TestHealthResponseShape pins the payload the page reads: the subscription view is there,
// and the per-request statistics counters are gone.
func TestHealthResponseShape(t *testing.T) {
	loadTestConfig(defaultConfigBytes())
	raw, errMarshal := json.Marshal(buildHealthResponse())
	if errMarshal != nil {
		t.Fatalf("health payload must marshal: %v", errMarshal)
	}
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(raw, &decoded); errUnmarshal != nil {
		t.Fatalf("health payload must be an object: %v", errUnmarshal)
	}
	for _, key := range []string{"plugin", "version", "enabled", "uptime", "plan", "plan_enabled", "plan_usage"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("health payload is missing %q", key)
		}
	}
	for _, key := range []string{
		"mode", "hosts", "require_routing_marker", "jsonl_enabled", "jsonl_dir", "retention_days",
		"ring_size", "ring_used", "pending_observations", "in_flight_identities",
		"recorded", "marker_missing", "unmatched_host_samples",
	} {
		if _, ok := decoded[key]; ok {
			t.Errorf("health payload must not carry the statistics field %q", key)
		}
	}
}

// TestHealthPlanSnapshotRenders feeds a recorded /health payload through the shape the page
// depends on, so a field renamed in plan would fail here rather than on the deployed page.
func TestHealthPlanSnapshotRenders(t *testing.T) {
	raw := readFixture(t, "management-health.json")
	var payload healthResponse
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		t.Fatalf("recorded health payload: %v", errUnmarshal)
	}
	if !payload.Plan.Available || len(payload.Plan.Accounts) != 1 {
		t.Fatalf("plan snapshot = %+v, want one available account", payload.Plan)
	}
	account := payload.Plan.Accounts[0]
	if len(account.Limits) == 0 {
		t.Fatal("the account must carry its rolling limits")
	}
	if account.PlanName == "" && account.PlanPrice == "" {
		t.Error("the account must report a plan name or price")
	}
	if got := payload.Plan.Tokens.TotalTokens; got == 0 {
		t.Error("the official token totals must reach the page")
	}
	// The overview reads the per-account windows, not just the Quota mirror: a page that
	// only saw the mirror would show empty cards as soon as several credentials exist.
	day, ok := account.Windows["24h"]
	if !ok || day.Requests == 0 || day.CacheRatio == 0 || len(day.Series.Requests) == 0 {
		t.Errorf("the account must carry the official window the overview plots: %+v", day)
	}
	if week, ok := account.Windows["7d"]; !ok || week.Detail {
		t.Errorf("the 7d window must come from the daily totals and report detail=false: %+v", week)
	}
	if usage := account.Usage; usage.FetchedAt == "" || usage.Items == 0 {
		t.Errorf("the page needs the collector's own fetch time and item count: %+v", usage)
	}
	// The plan detail and the per-model rows ride along in the same responses: the page
	// shows them without asking Cline for anything else.
	if payload.Plan.PlanDescription == "" || len(payload.Plan.PlanBenefits) == 0 || payload.Plan.PlanPeriodEnd == "" {
		t.Errorf("the plan detail must reach the page: description=%q benefits=%d period_end=%q",
			payload.Plan.PlanDescription, len(payload.Plan.PlanBenefits), payload.Plan.PlanPeriodEnd)
	}
	if len(payload.Plan.PlanCanceledAt) == 0 {
		t.Error("a cancelled subscription must stay visible")
	}
	if rows := day.Models; len(rows) != 2 || rows[0].Model != "deepseek/deepseek-v4.1-flash" {
		t.Errorf("the 24h window must carry its per-model split, raw model names first: %+v", rows)
	}
	if rows := account.Windows["7d"].Models; len(rows) != 2 || rows[0].Requests != 0 {
		t.Errorf("the 7d rows come from the daily totals and invent no request count: %+v", rows)
	}
	if len(payload.PlanAccounts) != 1 || payload.PlanAccounts[0].Items == 0 {
		t.Errorf("plan_accounts = %+v, want the credential diagnostics", payload.PlanAccounts)
	}
}

// TestHealthWithRejectedAndFailedCredentials covers the multi-credential case the page has to
// survive: one credential answers, a second was refused upstream (401/403) and a third
// failed for another reason. The healthy account must keep its official windows; the
// refused one stays in plan_accounts but out of the page's picker; the third is selectable
// and its empty window map is what keeps the previous account's numbers off the screen.
func TestHealthWithRejectedAndFailedCredentials(t *testing.T) {
	raw := readFixture(t, "management-health-multi-account.json")
	var payload healthResponse
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		t.Fatalf("recorded health payload: %v", errUnmarshal)
	}
	if len(payload.Plan.Accounts) != 3 {
		t.Fatalf("accounts = %d, want 3", len(payload.Plan.Accounts))
	}
	good, refused, failed := payload.Plan.Accounts[0], payload.Plan.Accounts[1], payload.Plan.Accounts[2]
	if !good.Available || len(good.Windows) == 0 {
		t.Errorf("the healthy account must keep its official windows: %+v", good)
	}
	if refused.Available || !refused.Rejected || refused.Error == "" {
		t.Errorf("the refused account must stay visible and explain itself: %+v", refused)
	}
	if failed.Available || failed.Rejected || failed.Error == "" || len(failed.Windows) != 0 {
		t.Errorf("the selectable failed account must carry no windows and an error: %+v", failed)
	}
	if len(payload.PlanAccounts) != 3 || !payload.PlanAccounts[1].Rejected || payload.PlanAccounts[2].Rejected {
		t.Errorf("plan_accounts = %+v, want all three credentials with only the refused one flagged", payload.PlanAccounts)
	}
}

// TestIndexPageCarriesNoData asserts the resource page is a static shell: the host
// serves it without management authentication, so it must never embed observations.
func TestIndexPageCarriesNoData(t *testing.T) {
	page := string(indexHTML(nil))
	if len(page) == 0 {
		t.Fatal("embedded page is empty")
	}
	for _, needle := range []string{"deepseek", "api.cline.bot", "cline-pass/", "gen_01M", "pck:session"} {
		if strings.Contains(page, needle) {
			t.Errorf("page must not contain recorded data, found %q", needle)
		}
	}
	for _, needle := range []string{"/v0/management/plugins/clinepass-channel-monitor", "localStorage", "Authorization"} {
		if !strings.Contains(page, needle) {
			t.Errorf("page is missing %q", needle)
		}
	}
	for _, needle := range []string{"Cline 套餐用量", "plan-cards", "% 已用", "近 31 天已用 Token（官方）"} {
		if !strings.Contains(page, needle) {
			t.Errorf("page is missing the %q plan element", needle)
		}
	}
	// The channel section's three cards are the page's summary now: the official 概览 cards went
	// away with the per-request pull they were built from.
	for _, needle := range []string{"请求数", "偏离基准渠道", "缓存命中率", "sparkline"} {
		if !strings.Contains(page, needle) {
			t.Errorf("page is missing the %q channel element", needle)
		}
	}
	// The plan detail block, the two official-source tables and the 概览 cards left the page on
	// request; the payloads behind them are still served by /health and /channel, so only the
	// page drops them.
	for _, needle := range []string{
		"套餐详情", "官方用量明细", `id="models"`, "上游推理渠道（官方 usage）", `id="official-channels"`,
		`id="cards"`, ">概览<",
	} {
		if strings.Contains(page, needle) {
			t.Errorf("page still carries the removed %q element", needle)
		}
	}
	if !strings.Contains(page, "grid-template-columns:repeat(3,minmax(0,1fr))") {
		t.Errorf("the overview must render its three cards on a single row")
	}
	// The local statistics v0.1.x kept are still gone: no latency or generation-speed card
	// from the local store, no /stats or /events view, and no /export endpoint. What
	// replaced them is the channel view, which reports which upstream served each request
	// rather than how fast the local store answered, so its own labels are allowed: see
	// TestChannelViewIsPresent below. The labels here are matched with their card syntax so
	// an explanatory sentence about what the official API does not provide is not mistaken
	// for the card.
	for _, needle := range []string{"/stats", "/events", "/export", "cache-bar",
		`label:"平均延时"`, `label:"生成速度"`} {
		if strings.Contains(page, needle) {
			t.Errorf("page must not keep the %q local statistics element", needle)
		}
	}
}

// jsFunctionBody returns the source of one top-level function, from its declaration up to
// the first closing brace in column 0. The page is a single embedded script with no test
// runner of its own, so this is how the render wiring stays checkable.
func jsFunctionBody(t *testing.T, page, name string) string {
	t.Helper()
	marker := "function " + name + "("
	start := strings.Index(page, marker)
	if start < 0 {
		t.Fatalf("page is missing the %s function", name)
	}
	end := strings.Index(page[start:], "\n}\n")
	if end < 0 {
		t.Fatalf("%s has no closing brace", name)
	}
	return page[start : start+end]
}

// TestChannelTimelineAndRecordPaging pins the two channel-section changes: the hourly timeline
// draws the newest ten hours only, and the raw-record table pages through the window instead of
// stopping at the newest twenty rows.
func TestChannelTimelineAndRecordPaging(t *testing.T) {
	page := string(indexHTML(nil))
	if !strings.Contains(page, "const timelineHourLimit=10;") {
		t.Error("the timeline limit must be ten hours")
	}
	hours := jsFunctionBody(t, page, "newestChannelHours")
	if !strings.Contains(hours, "timelineHourLimit") {
		t.Fatalf("newestChannelHours must cut the timeline at the hour limit, got:\n%s", hours)
	}
	for _, name := range []string{"renderChannelHourChart", "renderChannelHours"} {
		if body := jsFunctionBody(t, page, name); !strings.Contains(body, "newestChannelHours(summary)") {
			t.Errorf("%s must render the newest hours, got:\n%s", name, body)
		}
	}
	// 时间线的两张卡撤掉，换成请求 / 偏离 / 失败三条线的走势图；下方的小时表留着给精确值。
	if strings.Contains(page, `id="channel-timeline"`) {
		t.Error("the two timeline cards are gone, so the channel-timeline container must be gone with them")
	}
	chart := jsFunctionBody(t, page, "renderChannelHourChart")
	for _, needle := range []string{"off_baseline", "failed", "polyline", "viewBox"} {
		if !strings.Contains(chart, needle) {
			t.Errorf("renderChannelHourChart must use %s, got:\n%s", needle, chart)
		}
	}
	for _, needle := range []string{
		`id="channel-records-more"`, `id="channel-records-load"`, "function loadMoreRecords(",
		"IntersectionObserver", "const recordsPageSize=20;", "records_total",
	} {
		if !strings.Contains(page, needle) {
			t.Errorf("the raw-record table is missing %q", needle)
		}
	}
	if body := jsFunctionBody(t, page, "loadMoreRecords"); !strings.Contains(body, `"&offset="`) {
		t.Errorf("loadMoreRecords must ask the API for the next offset, got:\n%s", body)
	}
	// A refresh has to carry the depth the page already shows, otherwise the 60s auto-refresh
	// would throw the reader back to the first page.
	if body := jsFunctionBody(t, page, "loadChannel"); !strings.Contains(body, "recordsDepth()") {
		t.Errorf("loadChannel must ask for the depth the page already shows, got:\n%s", body)
	}
}

// TestChannelOverviewIsLocal pins what replaced the official 概览 section: the page reads no
// per-request usage window at all, and the channel section's three cards are the summary. The
// cache hit rate is built from the plugin's own records (summary.cached_tokens /
// summary.input_tokens), so it keeps working with the per-request pull switched off.
func TestChannelOverviewIsLocal(t *testing.T) {
	page := string(indexHTML(nil))
	for _, gone := range []string{
		"function renderCards(", "function renderOverviewNote(", "function officialView(",
		"renderCards(", "cache_ratio",
	} {
		if strings.Contains(page, gone) {
			t.Errorf("the official overview left the page, so %q must be gone with it", gone)
		}
	}
	body := jsFunctionBody(t, page, "renderChannel")
	for _, needle := range []string{
		"summary.input_tokens", "summary.cached_tokens",
		`label:"请求数"`, `label:"偏离基准渠道"`, `label:"缓存命中率"`,
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("renderChannel must build the three local cards, %q is missing from:\n%s", needle, body)
		}
	}
	if !strings.Contains(page, `$("plan-account").addEventListener("change"`) {
		t.Fatal("page is missing the account picker listener")
	}
	lineStart := strings.Index(page, `$("plan-account").addEventListener("change"`)
	lineEnd := strings.Index(page[lineStart:], "\n")
	listener := page[lineStart : lineStart+lineEnd]
	if !strings.Contains(listener, "renderPlan(") {
		t.Errorf("switching accounts must still re-render the plan card: %s", listener)
	}
	if strings.Contains(listener, "renderCards(") {
		t.Errorf("the account picker must not call the removed renderCards: %s", listener)
	}
}

func TestRouteManagementDispatch(t *testing.T) {
	loadTestConfig(defaultConfigBytes())
	cases := []struct {
		path       string
		wantStatus int
		wantType   string
	}{
		{BasePath + "/health", 200, "application/json"},
		{"/v0/resource/plugins/" + buildinfo.ID + "/index.html", 200, "text/html"},
		{BasePath + "/stats", 404, "application/json"},
		{BasePath + "/events", 404, "application/json"},
		{BasePath + "/export", 404, "application/json"},
		{BasePath + "/nope", 404, "application/json"},
	}
	for _, tc := range cases {
		req := pluginAPIRequest(tc.path)
		resp := route(&req)
		if resp.StatusCode != tc.wantStatus {
			t.Errorf("%s: status = %d, want %d", tc.path, resp.StatusCode, tc.wantStatus)
		}
		if contentType := resp.Headers.Get("Content-Type"); !strings.Contains(contentType, tc.wantType) {
			t.Errorf("%s: content-type = %q, want %q", tc.path, contentType, tc.wantType)
		}
	}
}

// readFixture loads one captured response from the package testdata directory.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, errRead := os.ReadFile(filepath.Join("testdata", name))
	if errRead != nil {
		t.Fatalf("read fixture %s: %v", name, errRead)
	}
	return raw
}

// loadTestConfig mirrors what the plugin package does on load: parse the block and publish
// it. The management API only reads the published state, so the test wires it directly
// instead of importing the plugin package (which imports this one).
func loadTestConfig(raw []byte) {
	cfg, errParse := config.Parse(raw)
	if errParse != nil {
		cfg = config.Default()
	}
	state.SetConfig(cfg)
}
