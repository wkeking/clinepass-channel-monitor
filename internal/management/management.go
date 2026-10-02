// Package management serves the plugin's Management API routes and the embedded page.
//
// Data only ever leaves through Management API paths, which the host authenticates. The
// resource route (/v0/resource/plugins/...) is deliberately a static shell with no data.
package management

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/wkeking/clinepass-channel-monitor/internal/abi"
	"github.com/wkeking/clinepass-channel-monitor/internal/buildinfo"
	"github.com/wkeking/clinepass-channel-monitor/internal/hostapi"
	"github.com/wkeking/clinepass-channel-monitor/internal/observation"
	"github.com/wkeking/clinepass-channel-monitor/internal/plan"
	"github.com/wkeking/clinepass-channel-monitor/internal/state"
)

// indexPage is the single-file management page. It is embedded so a deployment never
// depends on extra files next to the plugin binary, and it carries no data of its own.
//
//go:embed index.html
var indexPage []byte

// BasePath is the Management API base path for this plugin. Data endpoints
// live underneath it, which is the only place the plugin exposes data: the host
// authenticates every Management API request, unlike /v0/resource/plugins/...
const BasePath = "/v0/management/plugins/" + buildinfo.ID

const resourceIndexPath = "/index.html"

// Handle answers Management API and resource requests.
func Handle(request []byte) ([]byte, error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			hostapi.LogAsync("error", buildinfo.ID+": management handler recovered from panic", map[string]string{
				"panic": fmt.Sprint(recovered),
			})
		}
	}()
	var req pluginapi.ManagementRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return abi.OK(pluginapi.ManagementResponse{
			StatusCode: http.StatusBadRequest,
			Headers:    jsonHeaders(),
			Body:       []byte(`{"error":"invalid_request"}`),
		})
	}
	resp := route(&req)
	return abi.OK(resp)
}

// indexHTML serves the embedded single-file page. It contains no data: the page asks
// the authenticated Management API for everything it shows.
func indexHTML(_ *pluginapi.ManagementRequest) []byte {
	return indexPage
}

func jsonHeaders() http.Header {
	return http.Header{"Content-Type": []string{"application/json; charset=utf-8"}}
}

func htmlHeaders() http.Header {
	return http.Header{
		"Content-Type":           []string{"text/html; charset=utf-8"},
		"Cache-Control":          []string{"no-store"},
		"X-Content-Type-Options": []string{"nosniff"},
		"Referrer-Policy":        []string{"no-referrer"},
	}
}

// route dispatches one request. The path is matched on its suffix so
// both the Management API path and the resource path reach the same handler.
func route(req *pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	path := strings.TrimRight(req.Path, "/")
	switch {
	case strings.HasSuffix(path, resourceIndexPath):
		return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: htmlHeaders(), Body: indexHTML(req)}
	case strings.HasSuffix(path, "/health"):
		return jsonResponse(buildHealthResponse())
	case strings.HasSuffix(path, "/channel.csv"):
		return buildChannelCSV(req)
	case strings.HasSuffix(path, "/channel"):
		return buildChannelView(req)
	default:
		hostapi.LogAsync("warn", buildinfo.ID+": unknown management path", map[string]string{"path": req.Path})
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusNotFound,
			Headers:    jsonHeaders(),
			Body:       []byte(`{"error":"not_found"}`),
		}
	}
}

func jsonResponse(payload any) pluginapi.ManagementResponse {
	encoded, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		encoded = []byte(`{"error":"encode_failed"}`)
	}
	return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: jsonHeaders(), Body: encoded}
}

// healthResponse is the self-diagnosis surface. It carries the subscription view the page
// renders plus the credential diagnostics that make a missing key obvious.
type healthResponse struct {
	Plugin  string `json:"plugin"`
	Version string `json:"version"`
	Enabled bool   `json:"enabled"`
	Uptime  string `json:"uptime"`
	// Plan is the full subscription snapshot: which plan the account is on, how much of
	// each rolling quota is used, the official totals and the per-credential view.
	Plan        plan.Quota `json:"plan"`
	PlanEnabled bool       `json:"plan_enabled"`
	// PlanUsage reports the official per-request collector that backs the account windows
	// (retained records, coverage, last error) without exposing any credential. When several
	// Cline credentials are configured it describes the primary one; PlanAccounts lists all.
	PlanUsage plan.UsageState `json:"plan_usage"`
	// PlanAccounts lists every configured Cline credential so a deployment with several
	// entries or several keys can be checked at a glance.
	PlanAccounts []planAccountHealth `json:"plan_accounts,omitempty"`
	// ChannelObservation is the per-request channel collector: whether it runs, how many
	// streamed responses it resolved a channel for, how many it had to drop, and when it
	// last wrote a record. A deployment with no new rows on the 「渠道」 view starts here.
	ChannelObservation observation.Health `json:"channel_observation"`
	// ChannelLog is the CPA request-log scanner, exposed beside ChannelObservation because the
	// two answer different halves of the same question: the collector reports the credential CPA
	// routed to, and the scanner reports the gateway channel the upstream response named. A
	// deployment whose 「渠道」 rows show a credential but no gateway channel starts here.
	ChannelLog channelLogView `json:"channel_log"`
	// RequestHeaderNames and RequestBearerLen describe the last request seen on the request
	// path. This build declares no request capability, so they stay empty; they are kept
	// because they never contain a credential value, only header names and a length.
	RequestHeaderNames string `json:"request_header_names,omitempty"`
	RequestBearerLen   int    `json:"request_bearer_len,omitempty"`
}

// planAccountHealth is the per-credential diagnostic summary shown by /health.
type planAccountHealth struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Source    string `json:"source,omitempty"`
	Available bool   `json:"available"`
	// Rejected marks a credential the upstream refused (401/403).
	Rejected  bool   `json:"rejected,omitempty"`
	Account   string `json:"account,omitempty"`
	Items     int    `json:"items"`
	Oldest    string `json:"oldest,omitempty"`
	Truncated bool   `json:"truncated"`
	Failures  int    `json:"failures,omitempty"`
	Error     string `json:"error,omitempty"`
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

var pluginStart = time.Now()

func buildHealthResponse() healthResponse {
	cfg := state.Config()
	headerNames, bearerLen := plan.BearerDiagnostics()
	resp := healthResponse{
		Plugin:             buildinfo.ID,
		Version:            buildinfo.Version,
		Enabled:            cfg.Enabled,
		Uptime:             time.Since(pluginStart).Round(time.Second).String(),
		PlanEnabled:        cfg.PlanEnabled,
		RequestHeaderNames: headerNames,
		RequestBearerLen:   bearerLen,
		ChannelObservation: observationHealth(),
		ChannelLog:         channelLogSnapshot(),
	}
	resp.PlanUsage = plan.UsageState{Enabled: cfg.PlanUsageEnabled}
	if poller := state.Plan(); poller != nil {
		snapshot := poller.Snapshot()
		resp.Plan = snapshot
		if len(snapshot.Accounts) > 0 {
			resp.PlanUsage = snapshot.Usage
		}
		for _, account := range snapshot.Accounts {
			resp.PlanAccounts = append(resp.PlanAccounts, planAccountHealth{
				ID:        account.ID,
				Label:     account.Label,
				Source:    account.Source,
				Available: account.Available,
				Rejected:  account.Rejected,
				Account:   account.Account,
				Items:     account.Usage.Items,
				Oldest:    account.Usage.Oldest,
				Truncated: account.Usage.Truncated,
				Failures:  account.Usage.Failures,
				Error:     firstNonEmpty(account.Error, account.Usage.Error),
			})
		}
	}
	// The upstream-channel list is part of the payload the page reads, so it is a list on
	// every path: without a poller there is nothing to report, which is an empty list, not a
	// null the page would have to special-case.
	if resp.Plan.OfficialChannels == nil {
		resp.Plan.OfficialChannels = []plan.OfficialChannelRow{}
	}
	return resp
}
