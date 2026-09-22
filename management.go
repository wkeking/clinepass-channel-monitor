package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// managementBasePath is the Management API base path for this plugin. Data endpoints
// live underneath it, which is the only place the plugin exposes data: the host
// authenticates every Management API request, unlike /v0/resource/plugins/...
const managementBasePath = "/v0/management/plugins/" + pluginID

const (
	resourceIndexPath = "/index.html"
	defaultEventLimit = 200
	maxEventLimit     = 1000
)

// handleManagement answers Management API and resource requests.
func handleManagement(request []byte) ([]byte, error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			hostLogAsync("error", pluginID+": management handler recovered from panic", map[string]string{
				"panic": fmt.Sprint(recovered),
			})
		}
	}()
	var req pluginapi.ManagementRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return okEnvelope(pluginapi.ManagementResponse{
			StatusCode: http.StatusBadRequest,
			Headers:    jsonHeaders(),
			Body:       []byte(`{"error":"invalid_request"}`),
		})
	}
	resp := routeManagementRequest(&req)
	return okEnvelope(resp)
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

// routeManagementRequest dispatches one request. The path is matched on its suffix so
// both the Management API path and the resource path reach the same handler.
func routeManagementRequest(req *pluginapi.ManagementRequest) pluginapi.ManagementResponse {
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
		hostLogAsync("warn", pluginID+": unknown management path", map[string]string{"path": req.Path})
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
	PlanUsage officialUsageState `json:"plan_usage"`
	// PlanAccounts lists every configured Cline credential so a deployment with several
	// entries or several keys can be checked at a glance.
	PlanAccounts []planAccountHealth `json:"plan_accounts,omitempty"`
	// RequestHeaderNames lists the header names seen on the last intercepted request and
	// the length of its bearer token; it exists to diagnose credential discovery and
	// never contains a credential value.
	RequestHeaderNames string `json:"request_header_names,omitempty"`
	RequestBearerLen   int    `json:"request_bearer_len,omitempty"`
	statsTotals
	UnmatchedHostSamples []unmatchedHostSample `json:"unmatched_host_samples"`
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
	cfg := currentConfig()
	st := currentStore()
	headerNames, bearerLen := upstreamBearerDiagnostics()
	resp := healthResponse{
		Plugin:               pluginID,
		Version:              pluginVersion,
		Enabled:              cfg.Enabled,
		Mode:                 cfg.matchMode(),
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
	resp.PlanUsage = officialUsageState{Enabled: cfg.PlanUsageEnabled}
	if poller := currentPlan(); poller != nil {
		snapshot := poller.snapshot()
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
	resp.statsTotals = st.totals()
	resp.RingUsed = st.used()
	resp.PendingObservations = st.countObservations()
	resp.UnmatchedHostSamples = st.unmatchedHostSamples()
	resp.InFlightIdentities = currentIdentities().len()
	return resp
}

func buildStatsResponse(query url.Values) windowStats {
	window, label := resolveWindow(query.Get("window"))
	st := currentStore()
	if st == nil {
		return windowStats{Window: label}
	}
	return *st.statsWindow(window, label, time.Now())
}

func resolveWindow(raw string) (time.Duration, string) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1h", "hour":
		return statsWindowHour, "1h"
	case "7d", "week":
		return statsWindowWeek, "7d"
	case "24h", "day", "":
		return statsWindowDay, "24h"
	default:
		if parsed, errParse := time.ParseDuration(raw); errParse == nil && parsed > 0 {
			return parsed, raw
		}
		return statsWindowDay, "24h"
	}
}

type eventsResponse struct {
	Window  string   `json:"window"`
	Total   int      `json:"total"`
	Offset  int      `json:"offset"`
	Limit   int      `json:"limit"`
	Filters filters  `json:"filters"`
	Events  []*event `json:"events"`
}

type filters struct {
	Channel string `json:"channel"`
	Model   string `json:"model"`
	Source  string `json:"source"`
	Result  string `json:"result"`
}

func eventFilterFromQuery(query url.Values) eventFilter {
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
	return eventFilter{
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
	filter := eventFilterFromQuery(query)
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
	st := currentStore()
	if st == nil {
		return resp
	}
	all := st.events(eventFilter{
		Since:   window,
		Channel: filter.Channel,
		Model:   filter.Model,
		Source:  filter.Source,
		Result:  filter.Result,
	})
	resp.Total = len(all)
	if filter.Offset >= len(all) {
		resp.Events = []*event{}
		return resp
	}
	end := filter.Offset + filter.Limit
	if end > len(all) {
		end = len(all)
	}
	resp.Events = all[filter.Offset:end]
	if resp.Events == nil {
		resp.Events = []*event{}
	}
	return resp
}

// exportResponse renders the current selection as CSV.
func exportResponse(query url.Values) pluginapi.ManagementResponse {
	filter := eventFilterFromQuery(query)
	window, _ := resolveWindow(query.Get("window"))
	st := currentStore()
	var events []*event
	if st != nil {
		events = st.events(eventFilter{
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
