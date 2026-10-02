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
	for _, needle := range []string{"Cline 套餐用量", "plan-cards", "% 已用", "近 31 天已用 Token（官方）", "套餐详情"} {
		if !strings.Contains(page, needle) {
			t.Errorf("page is missing the %q plan element", needle)
		}
	}
	// The overview keeps the official usage view the collector still gathers: the window
	// selector, the three official cards, their charts, and the per-model table below.
	for _, needle := range []string{"概览", "请求数", "总 Token 数", "缓存命中率", "sparkline", "近 7 天", "官方用量明细", "id=\"models\""} {
		if !strings.Contains(page, needle) {
			t.Errorf("page is missing the %q overview element", needle)
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

// TestOverviewRendersIndependentlyOfThePlanCard pins a regression that only shows up with
// several credentials: when the selected credential answers 401/403 on the plan and limits
// calls, renderPlan returns early with an error message. If the overview were rendered from
// inside renderPlan, the previous credential's numbers would stay on screen and be
// misattributed. The overview must be rendered by its callers instead.
func TestOverviewRendersIndependentlyOfThePlanCard(t *testing.T) {
	page := string(indexHTML(nil))
	if body := jsFunctionBody(t, page, "renderPlan"); strings.Contains(body, "renderCards(") || strings.Contains(body, "renderModels(") {
		t.Error("renderPlan must not render the overview or the per-model table: its early return would leave the previous account's numbers on screen")
	}
	if body := jsFunctionBody(t, page, "renderCards"); strings.Contains(body, "renderPlan(") {
		t.Error("renderCards must not depend on renderPlan")
	}
	if body := jsFunctionBody(t, page, "renderModels"); strings.Contains(body, "renderPlan(") {
		t.Error("renderModels must not depend on renderPlan")
	}
	for _, call := range []string{"renderCards(health.plan)", "renderModels(health.plan)"} {
		if !strings.Contains(page, call) {
			t.Errorf("load() must call %s for every refresh, independent of the plan card", call)
		}
	}
	if !strings.Contains(page, `$("plan-account").addEventListener("change"`) {
		t.Fatal("page is missing the account picker listener")
	}
	lineStart := strings.Index(page, `$("plan-account").addEventListener("change"`)
	lineEnd := strings.Index(page[lineStart:], "\n")
	listener := page[lineStart : lineStart+lineEnd]
	for _, call := range []string{"renderPlan(", "renderCards(", "renderModels("} {
		if !strings.Contains(listener, call) {
			t.Errorf("switching accounts must re-render everything, %s is missing from: %s", call, listener)
		}
	}
}

// TestOverviewDoesNotInventZeroTotals pins the other half of the same case: when the plan
// and limits calls fail, the official totals were never fetched, so the card must say so
// instead of printing "0 token" as if the account had no usage. Only an account that
// answered may show a number.
func TestOverviewDoesNotInventZeroTotals(t *testing.T) {
	body := jsFunctionBody(t, string(indexHTML(nil)), "renderCards")
	if !strings.Contains(body, "available!==false") {
		t.Error("renderCards must gate the totals on the credential being available")
	}
	if !strings.Contains(body, `value:"—",unit:"token"`) {
		t.Error("an unavailable credential must render an unknown total, not a zero")
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
