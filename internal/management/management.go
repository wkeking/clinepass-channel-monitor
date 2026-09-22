// Package management serves the plugin's Management API routes and the embedded page.
//
// Data only ever leaves through Management API paths, which the host authenticates. The
// resource route (/v0/resource/plugins/...) is deliberately a static shell with no data.
package management

import (
	"bytes"
	_ "embed"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/wkeking/clinepass-channel-monitor/internal/abi"
	"github.com/wkeking/clinepass-channel-monitor/internal/buildinfo"
	"github.com/wkeking/clinepass-channel-monitor/internal/hooks"
	"github.com/wkeking/clinepass-channel-monitor/internal/hostapi"
	"github.com/wkeking/clinepass-channel-monitor/internal/plan"
	"github.com/wkeking/clinepass-channel-monitor/internal/state"
	"github.com/wkeking/clinepass-channel-monitor/internal/store"
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

const (
	resourceIndexPath = "/index.html"
	defaultEventLimit = 200
	maxEventLimit     = 1000
)

// handleManagement answers Management API and resource requests.
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

func textHeaders(contentType string) http.Header {
	return http.Header{
		"Content-Type":           []string{contentType},
		"Cache-Control":          []string{"no-store"},
		"X-Content-Type-Options": []string{"nosniff"},
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
	case strings.HasSuffix(path, "/stats"):
		return jsonResponse(buildStatsResponse(req.Query))
	case strings.HasSuffix(path, "/events"):
		return jsonResponse(buildEventsResponse(req.Query))
	case strings.HasSuffix(path, "/export"):
		return exportResponse(req.Query)
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

// healthResponse is the self-diagnosis surface described in FR-2.
type healthResponse struct {
	Plugin  string `json:"plugin"`
	Version string `json:"version"`
	Enabled bool   `json:"enabled"`
	// Mode is host+marker (normal) or marker-only (hosts is an explicit empty list).
	Mode string `json:"mode"`
	// Hosts is the hosts list this instance will match against.
	Hosts                []string `json:"hosts"`
	RequireRoutingMarker bool     `json:"require_routing_marker"`
	JSONL                bool     `json:"jsonl_enabled"`
	JSONLDir             string   `json:"jsonl_dir"`
	RetentionDays        int      `json:"retention_days"`
	RingSize             int      `json:"ring_size"`
	RingUsed             int      `json:"ring_used"`
	PendingObservations  int      `json:"pending_observations"`
	InFlightIdentities   int      `json:"in_flight_identities"`
	Uptime               string   `json:"uptime"`
	PlanEnabled          bool     `json:"plan_enabled"`
	// PlanUsage reports the official per-request collector that backs the overview cards
	// (retained records, coverage, last error) without exposing any credential. When several
	// Cline credentials are configured it describes the primary one; PlanAccounts lists all.
	PlanUsage plan.UsageState `json:"plan_usage"`
	// PlanAccounts lists every configured Cline credential so a deployment with several
	// entries or several keys can be checked at a glance.
	PlanAccounts []planAccountHealth `json:"plan_accounts,omitempty"`
	// RequestHeaderNames lists the header names seen on the last intercepted request and
	// the length of its bearer token; it exists to diagnose credential discovery and
	// never contains a credential value.
	RequestHeaderNames string `json:"request_header_names,omitempty"`
	RequestBearerLen   int    `json:"request_bearer_len,omitempty"`
	store.Totals
	UnmatchedHostSamples []store.UnmatchedHostSample `json:"unmatched_host_samples"`
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
	st := state.Store()
	headerNames, bearerLen := plan.BearerDiagnostics()
	resp := healthResponse{
		Plugin:               buildinfo.ID,
		Version:              buildinfo.Version,
		Enabled:              cfg.Enabled,
		Mode:                 cfg.MatchMode(),
		Hosts:                cfg.Hosts,
		RequireRoutingMarker: cfg.RequireRoutingMark,
		JSONL:                cfg.JSONLEnabled,
		JSONLDir:             cfg.JSONLDir,
		RetentionDays:        cfg.RetentionDays,
		RingSize:             cfg.RingSize,
		Uptime:               time.Since(pluginStart).Round(time.Second).String(),
		PlanEnabled:          cfg.PlanEnabled,
		RequestHeaderNames:   headerNames,
		RequestBearerLen:     bearerLen,
	}
	resp.PlanUsage = plan.UsageState{Enabled: cfg.PlanUsageEnabled}
	if poller := state.Plan(); poller != nil {
		snapshot := poller.Snapshot()
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
	if st == nil {
		return resp
	}
	resp.Totals = st.Totals()
	resp.RingUsed = st.Used()
	resp.PendingObservations = st.CountObservations()
	resp.UnmatchedHostSamples = st.UnmatchedHostSamples()
	resp.InFlightIdentities = hooks.InFlight()
	return resp
}

func buildStatsResponse(query url.Values) store.WindowStats {
	window, label := resolveWindow(query.Get("window"))
	st := state.Store()
	if st == nil {
		return store.WindowStats{Window: label}
	}
	return *st.StatsWindow(window, label, time.Now(), planSnapshot())
}

// planSnapshot returns the cached subscription view, or an empty one when the poller is off.
func planSnapshot() plan.Quota {
	if poller := state.Plan(); poller != nil {
		return poller.Snapshot()
	}
	return plan.Quota{}
}

func resolveWindow(raw string) (time.Duration, string) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1h", "hour":
		return store.StatsWindowHour, "1h"
	case "7d", "week":
		return store.StatsWindowWeek, "7d"
	case "24h", "day", "":
		return store.StatsWindowDay, "24h"
	default:
		if parsed, errParse := time.ParseDuration(raw); errParse == nil && parsed > 0 {
			return parsed, raw
		}
		return store.StatsWindowDay, "24h"
	}
}

type eventsResponse struct {
	Window  string         `json:"window"`
	Total   int            `json:"total"`
	Offset  int            `json:"offset"`
	Limit   int            `json:"limit"`
	Filters filters        `json:"filters"`
	Events  []*store.Event `json:"events"`
}

type filters struct {
	Channel string `json:"channel"`
	Model   string `json:"model"`
	Source  string `json:"source"`
	Result  string `json:"result"`
}

func filterFromQuery(query url.Values) store.Filter {
	window, _ := resolveWindow(query.Get("window"))
	limit, errLimit := strconv.Atoi(strings.TrimSpace(query.Get("limit")))
	if errLimit != nil || limit <= 0 {
		limit = defaultEventLimit
	}
	if limit > maxEventLimit {
		limit = maxEventLimit
	}
	offset, errOffset := strconv.Atoi(strings.TrimSpace(query.Get("offset")))
	if errOffset != nil || offset < 0 {
		offset = 0
	}
	return store.Filter{
		Since:   window,
		Limit:   limit,
		Offset:  offset,
		Channel: strings.TrimSpace(query.Get("channel")),
		Model:   strings.TrimSpace(query.Get("model")),
		Source:  strings.TrimSpace(query.Get("source")),
		Result:  strings.TrimSpace(query.Get("result")),
	}
}

func buildEventsResponse(query url.Values) eventsResponse {
	filter := filterFromQuery(query)
	window, label := resolveWindow(query.Get("window"))
	resp := eventsResponse{
		Window: label,
		Offset: filter.Offset,
		Limit:  filter.Limit,
		Filters: filters{
			Channel: filter.Channel,
			Model:   filter.Model,
			Source:  filter.Source,
			Result:  filter.Result,
		},
	}
	st := state.Store()
	if st == nil {
		return resp
	}
	all := st.Events(store.Filter{
		Since:   window,
		Channel: filter.Channel,
		Model:   filter.Model,
		Source:  filter.Source,
		Result:  filter.Result,
	})
	resp.Total = len(all)
	if filter.Offset >= len(all) {
		resp.Events = []*store.Event{}
		return resp
	}
	end := filter.Offset + filter.Limit
	if end > len(all) {
		end = len(all)
	}
	resp.Events = all[filter.Offset:end]
	if resp.Events == nil {
		resp.Events = []*store.Event{}
	}
	return resp
}

// exportResponse renders the current selection as CSV.
func exportResponse(query url.Values) pluginapi.ManagementResponse {
	filter := filterFromQuery(query)
	window, _ := resolveWindow(query.Get("window"))
	st := state.Store()
	var events []*store.Event
	if st != nil {
		events = st.Events(store.Filter{
			Since:   window,
			Channel: filter.Channel,
			Model:   filter.Model,
			Source:  filter.Source,
			Result:  filter.Result,
		})
	}
	var buffer bytes.Buffer
	writer := csv.NewWriter(&buffer)
	header := []string{
		"timestamp", "model", "model_alias", "session_id", "base_url", "host", "provider",
		"reasoning_effort", "service_tier", "failed", "status_code", "error",
		"latency_ms", "ttft_ms", "tokens_per_second", "input_tokens", "output_tokens",
		"reasoning_tokens", "total_tokens", "cached_tokens", "cache_read_tokens",
		"cache_creation_tokens", "prompt_cache_hit_tokens", "prompt_cache_miss_tokens",
		"final_provider", "resolved_provider", "canonical_slug", "model_attempt_count",
		"total_provider_attempt_count", "fallbacks_available_count", "cost", "input_cost",
		"output_cost", "generation_id", "client_protocol", "upstream_protocol", "stream",
		"channel_missing", "event_id",
	}
	_ = writer.Write(header)
	for _, e := range events {
		_ = writer.Write([]string{
			e.Timestamp.Format(time.RFC3339), e.Model, e.ModelAlias, e.SessionID, e.BaseURL, e.Host, e.Provider,
			e.ReasoningEffort, e.ServiceTier, strconv.FormatBool(e.Failed), strconv.Itoa(e.StatusCode), e.Error,
			strconv.FormatInt(e.LatencyMS, 10), strconv.FormatInt(e.TTFTMS, 10),
			strconv.FormatFloat(e.TokensPerSecond, 'f', -1, 64),
			strconv.FormatInt(e.InputTokens, 10), strconv.FormatInt(e.OutputTokens, 10),
			strconv.FormatInt(e.ReasoningTokens, 10), strconv.FormatInt(e.TotalTokens, 10),
			strconv.FormatInt(e.CachedTokens, 10), strconv.FormatInt(e.CacheReadTokens, 10),
			strconv.FormatInt(e.CacheCreationTokens, 10), strconv.FormatInt(e.PromptCacheHitTokens, 10),
			strconv.FormatInt(e.PromptCacheMissTokens, 10),
			e.FinalProvider, e.ResolvedProvider, e.CanonicalSlug,
			strconv.FormatInt(e.ModelAttemptCount, 10), strconv.FormatInt(e.TotalProviderAttemptCount, 10),
			strconv.FormatInt(e.FallbacksAvailableCount, 10), e.Cost, e.InputCost,
			e.OutputCost, e.GenerationID, e.ClientProtocol, e.UpstreamProtocol,
			strconv.FormatBool(e.Stream), strconv.FormatBool(e.ChannelMissing), e.EventID,
		})
	}
	writer.Flush()
	filename := fmt.Sprintf("clinepass-channel-monitor-%s.csv", time.Now().Format("20060102-150405"))
	headers := textHeaders("text/csv; charset=utf-8")
	headers.Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: headers, Body: buffer.Bytes()}
}
