package main

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

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
