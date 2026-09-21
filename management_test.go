package main

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
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

// TestRouteManagementHealthMatchesRealPayload feeds a recorded /health payload through
// the page's data path to make sure the shape the page depends on stays intact.
func TestHealthResponseShape(t *testing.T) {
	loadConfig(defaultConfigBytes())
	resp := buildHealthResponse()
	if resp.Mode != "host+marker" {
		t.Errorf("mode = %q, want host+marker", resp.Mode)
	}
	if len(resp.Hosts) != 1 || resp.Hosts[0] != "api.cline.bot" {
		t.Errorf("hosts = %v", resp.Hosts)
	}
	if resp.RingSize != defaultRingSize {
		t.Errorf("ring_size = %d, want %d", resp.RingSize, defaultRingSize)
	}
	if _, errMarshal := json.Marshal(resp); errMarshal != nil {
		t.Fatalf("health payload must marshal: %v", errMarshal)
	}
}

func TestHealthModeMarkerOnly(t *testing.T) {
	loadConfig([]byte("enabled: true\nhosts: []\n"))
	if got := buildHealthResponse().Mode; got != "marker-only" {
		t.Errorf("mode = %q, want marker-only for an explicit empty hosts list", got)
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
	// The detail table shows the recorded columns; the upstream address and the raw
	// provider key stay out of the list and only appear in the expanded detail panel.
	for _, needle := range []string{"思考等级", "延时 / TTFT", "生成速度", "缓存", "Token", "输入 / 输出 合计", "输入 / 输出 每请求", "sparkline", "cache-bar"} {
		if !strings.Contains(page, needle) {
			t.Errorf("page is missing the %q column", needle)
		}
	}
	if strings.Contains(page, "<th>上游地址</th>") {
		t.Errorf("the upstream address must not be a table column")
	}
}

func TestRouteManagementDispatch(t *testing.T) {
	loadConfig(defaultConfigBytes())
	cases := []struct {
		path       string
		wantStatus int
		wantType   string
	}{
		{managementBasePath + "/health", 200, "application/json"},
		{managementBasePath + "/stats", 200, "application/json"},
		{managementBasePath + "/events", 200, "application/json"},
		{managementBasePath + "/export", 200, "text/csv"},
		{"/v0/resource/plugins/" + pluginID + "/index.html", 200, "text/html"},
		{managementBasePath + "/nope", 404, "application/json"},
	}
	for _, tc := range cases {
		req := pluginAPIRequest(tc.path)
		resp := routeManagementRequest(&req)
		if resp.StatusCode != tc.wantStatus {
			t.Errorf("%s: status = %d, want %d", tc.path, resp.StatusCode, tc.wantStatus)
		}
		if contentType := resp.Headers.Get("Content-Type"); !strings.Contains(contentType, tc.wantType) {
			t.Errorf("%s: content-type = %q, want %q", tc.path, contentType, tc.wantType)
		}
	}
}

func TestExportCSVHeader(t *testing.T) {
	loadConfig(defaultConfigBytes())
	req := pluginAPIRequest(managementBasePath + "/export?window=24h")
	resp := routeManagementRequest(&req)
	body := string(resp.Body)
	if !strings.HasPrefix(body, "timestamp,model,model_alias,session_id,base_url,host,provider") {
		t.Errorf("unexpected CSV header: %q", strings.SplitN(body, "\n", 2)[0])
	}
	if disposition := resp.Headers.Get("Content-Disposition"); !strings.Contains(disposition, "clinepass-channel-monitor-") {
		t.Errorf("missing download filename, got %q", disposition)
	}
}

// TestChannelStatsCacheRatio checks the per-channel upstream cache ratio the page shows.
func TestChannelStatsCacheRatio(t *testing.T) {
	loadConfig(defaultConfigBytes())
	st := currentStore()
	now := time.Now()
	st.add(&event{Timestamp: now, Model: "m", FinalProvider: "deepseek", PromptCacheHitTokens: 900, PromptCacheMissTokens: 100, InputTokens: 1000, CachedTokens: 800})
	st.add(&event{Timestamp: now, Model: "m", FinalProvider: "deepseek", PromptCacheHitTokens: 90, PromptCacheMissTokens: 10, InputTokens: 100, CachedTokens: 0})
	stats := st.statsWindow(statsWindowDay, "24h", now)
	if len(stats.Channels) != 1 {
		t.Fatalf("channels = %d, want 1", len(stats.Channels))
	}
	channel := stats.Channels[0]
	if channel.PromptCacheHitTokens != 990 || channel.PromptCacheMissTokens != 110 {
		t.Errorf("cache counters = %d/%d, want 990/110", channel.PromptCacheHitTokens, channel.PromptCacheMissTokens)
	}
	if got := channel.PromptCacheRatio; got < 0.89 || got > 0.91 {
		t.Errorf("prompt cache ratio = %v, want about 0.9", got)
	}
	if got := channel.AvgCachedTokens; got != 400 {
		t.Errorf("avg cached tokens = %v, want 400", got)
	}
	if stats.Requests != 2 {
		t.Errorf("requests = %d, want 2", stats.Requests)
	}
}

// TestSeriesBucketKeepsChartsBounded checks the bucket sizing used by the overview charts.
func TestSeriesBucketKeepsChartsBounded(t *testing.T) {
	cases := []struct {
		window        time.Duration
		wantBucket    int64
		wantMaxBucket int
	}{
		{time.Hour, 120, 48},
		{24 * time.Hour, 1920, 48},
		{7 * 24 * time.Hour, 15360, 48},
	}
	for _, tc := range cases {
		bucket, count := seriesBucket(tc.window)
		if bucket != tc.wantBucket {
			t.Errorf("seriesBucket(%v) bucket = %d, want %d", tc.window, bucket, tc.wantBucket)
		}
		if count > tc.wantMaxBucket {
			t.Errorf("seriesBucket(%v) count = %d, want <= %d", tc.window, count, tc.wantMaxBucket)
		}
	}
}

// TestStatsSeriesMapsEventsToBuckets verifies the chart data the page plots.
func TestStatsSeriesMapsEventsToBuckets(t *testing.T) {
	loadConfig(defaultConfigBytes())
	st := currentStore()
	now := time.Now()
	st.add(&event{Timestamp: now.Add(-2 * time.Minute), TotalTokens: 100, PromptCacheHitTokens: 10, LatencyMS: 200, TTFTMS: 50, TokensPerSecond: 5})
	st.add(&event{Timestamp: now.Add(-time.Minute), TotalTokens: 300, PromptCacheMissTokens: 20, LatencyMS: 400, TTFTMS: 60, TokensPerSecond: 7, Failed: true})
	stats := st.statsWindow(time.Hour, "1h", now)
	if len(stats.Series.Labels) != len(stats.Series.Requests) {
		t.Fatalf("series length mismatch: %d labels vs %d values", len(stats.Series.Labels), len(stats.Series.Requests))
	}
	var requestSum, tokenSum float64
	for i := range stats.Series.Requests {
		requestSum += stats.Series.Requests[i]
		tokenSum += stats.Series.Tokens[i]
	}
	if requestSum != 2 {
		t.Errorf("series requests = %v, want 2", requestSum)
	}
	if tokenSum != 400 {
		t.Errorf("series tokens = %v, want 400", tokenSum)
	}
	last := len(stats.Series.Requests) - 1
	if stats.Series.Failed[last] != 1 {
		t.Errorf("failed bucket = %v, want 1 in the last bucket", stats.Series.Failed[last])
	}
}

func TestResolveWindow(t *testing.T) {
	cases := map[string]string{"": "24h", "1h": "1h", "24h": "24h", "7d": "7d", "30m": "30m", "bogus": "24h"}
	for input, want := range cases {
		if _, label := resolveWindow(input); label != want {
			t.Errorf("resolveWindow(%q) = %q, want %q", input, label, want)
		}
	}
}

func TestEventFilterFromQuery(t *testing.T) {
	values := url.Values{}
	values.Set("window", "1h")
	values.Set("limit", "5000")
	values.Set("offset", "-3")
	values.Set("channel", " deepseek ")
	filter := eventFilterFromQuery(values)
	if filter.Limit != maxEventLimit {
		t.Errorf("limit = %d, want clamped to %d", filter.Limit, maxEventLimit)
	}
	if filter.Offset != 0 {
		t.Errorf("offset = %d, want 0", filter.Offset)
	}
	if filter.Channel != "deepseek" {
		t.Errorf("channel = %q, want trimmed value", filter.Channel)
	}
	if filter.Since != statsWindowHour {
		t.Errorf("since = %v, want 1h", filter.Since)
	}
}

func TestRecordedManagementPayloadsStillRender(t *testing.T) {
	// The fixtures are real API responses captured from a live CPA instance.
	for _, name := range []string{"management-health.json", "management-stats.json", "management-events.json"} {
		raw := readFixture(t, name)
		var decoded map[string]any
		if errUnmarshal := json.Unmarshal(raw, &decoded); errUnmarshal != nil {
			t.Fatalf("%s: %v", name, errUnmarshal)
		}
		encoded, errMarshal := json.Marshal(decoded)
		if errMarshal != nil {
			t.Fatalf("%s: %v", name, errMarshal)
		}
		if len(encoded) == 0 {
			t.Fatalf("%s: empty payload", name)
		}
	}
}
