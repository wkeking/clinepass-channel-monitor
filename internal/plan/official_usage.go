package plan

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wkeking/clinepass-channel-monitor/internal/config"
)

// This file keeps a rolling view of Cline's own per-request usage records so the overview
// cards can show the official numbers instead of the plugin's local count.
//
// GET /api/v1/users/{id}/usages is cursor paginated (200 items per page at most) and only
// supports paging from the newest record backwards: it ignores startDate/endDate. Walking
// the whole history on every refresh would cost dozens of requests — and a burst gets
// rate limited (observed: HTTP 429 after ~25 rapid pages) — so the collector
//
//   - keeps the last officialUsageRetention of records in memory,
//   - pages only until it reaches the records it already has (steady state: one page),
//   - paces the pages of a multi-page walk and stops after a small budget,
//   - backs off exponentially when the upstream answers with an error.
//
// Coverage therefore grows over a few refresh cycles instead of being complete after the
// first one, and every window reports whether the retained records reach back far enough.
const (
	// officialUsageRetention bounds how far back the official records are kept. The
	// per-request endpoint ignores date filters, so covering a window needs one page per
	// 200 records in it: the 24-hour window measured 3411 records (17 pages), a 7-day
	// window would need a hundred. Keeping a day plus headroom keeps a full backfill at
	// tens of pages instead of hundreds, so the 7-day page window stays on the local view
	// while the plan card carries the official longer-horizon totals.
	officialUsageRetention = 26 * time.Hour
	officialUsagePageSize  = 200
	officialUsageMaxPages  = 80
	officialUsageMaxItems  = 40000
	// officialUsagePageDelay paces the pages of one walk so a deep backfill does not look
	// like a burst. A burst of ~25 rapid pages was observed to answer HTTP 429.
	officialUsagePageDelay = 300 * time.Millisecond
	// officialUsagePagesSteady is the per-walk page budget once the retention window is
	// covered; officialUsagePagesExtend is the budget used while it is not.
	officialUsagePagesSteady = 5
	officialUsagePagesExtend = 60
	officialUsageMaxBackoff  = 30 * time.Minute
)

// Empty labels keep a record whose payload lacks a field visible in the tables instead of
// dropping it or showing a blank cell: "unknown" is a fact the page can state.
const (
	unknownModelLabel    = "未知模型"
	unknownProviderLabel = "未知上游渠道"
)

// officialUsageItem is one billable Cline request, trimmed to the fields the page needs.
type officialUsageItem struct {
	ID         string
	At         time.Time
	Prompt     int64
	Completion int64
	Cached     int64
	CostMicro  int64
	// Model is the model that actually ran, taken from metadata.raw_model ("deepseek/
	// deepseek-v4.1-flash"), which is more precise than the routed name the record carries
	// in aiModelName ("cline-pass/deepseek-v4.1-flash").
	Model string
	// Provider is the upstream inference channel the request was served from
	// (aiInferenceProviderName, observed value so far: "vercel"). Only a successful,
	// billable request reaches this endpoint at all, so a failed request (the host is
	// currently seeing HTTP 502s) has no record here and can never be attributed to a
	// provider: failures stay a CPA-credential count.
	Provider string
	// Credits is what the request deducted from the credit balance. A flat subscription
	// (ClinePass) reports 0 for every record.
	Credits int64
	BYOK    bool
	Stream  bool
}

// officialUsageRawItem mirrors the upstream JSON.
type officialUsageRawItem struct {
	ID              string `json:"id"`
	CreatedAt       string `json:"createdAt"`
	CostUnits       int64  `json:"costUsd"`
	CreditsUsed     int64  `json:"creditsUsed"`
	Operation       string `json:"operation"`
	AIModelTypeName string `json:"aiModelTypeName"`
	AIModelName     string `json:"aiModelName"`
	// AIInferenceProviderName is the upstream inference channel that served the request.
	// It is a top-level field of the record, not a metadata one.
	AIInferenceProviderName string `json:"aiInferenceProviderName"`
	PromptTokens            int64  `json:"promptTokens"`
	CompletionTokens        int64  `json:"completionTokens"`
	TotalTokens             int64  `json:"totalTokens"`
	CachedTokens            int64  `json:"cachedTokens"`
	// Metadata carries the routing detail of the request: which model actually ran, and
	// through which routing entry. The upstream channel is next to it, in
	// aiInferenceProviderName.
	Metadata struct {
		IsBYOK   bool   `json:"is_byok"`
		IsStream bool   `json:"is_stream"`
		RawModel string `json:"raw_model"`
		ModelTyp string `json:"model_type"`
	} `json:"metadata"`
}

type officialUsageSeries struct {
	Requests []int64 `json:"requests"`
	Tokens   []int64 `json:"tokens"`
	Cached   []int64 `json:"cached"`
}

// UsageModelRow is one model's share of a window. It is derived from records the collector
// already holds (or from the daily rows it already fetched), so the breakdown needs no
// extra upstream call.
type UsageModelRow struct {
	Model string `json:"model"`
	// InferenceProvider is the upstream inference channel the record reported
	// (aiInferenceProviderName). It is empty for the rows built from the daily totals,
	// which carry no provider, and for records written before Cline added the field.
	InferenceProvider string  `json:"inference_provider,omitempty"`
	Requests          int64   `json:"requests"`
	InputTokens       int64   `json:"input_tokens"`
	OutputTokens      int64   `json:"output_tokens"`
	TotalTokens       int64   `json:"total_tokens"`
	CachedTokens      int64   `json:"cached_tokens"`
	CacheRatio        float64 `json:"cache_ratio"`
	CostUSD           float64 `json:"cost_usd"`
	CreditsUsed       int64   `json:"credits_used,omitempty"`
}

// OfficialChannelRow is one upstream inference channel crossed with the model that actually
// ran, over the whole window the collector retains. It is the second dimension beside the
// CPA-credential channel view: the CPA-side channel answers "which CPA credential served the
// request", this answers "which upstream channel Cline sent it to".
//
// The records behind it are the successful, billable ones only, so a failed request has no
// row here at all: failures can only be counted per CPA credential, never per upstream
// channel. CostUSD follows the same micro-USD convention as the rest of the official data.
type OfficialChannelRow struct {
	InferenceProvider string  `json:"inference_provider"`
	Model             string  `json:"model"`
	Requests          int64   `json:"requests"`
	InputTokens       int64   `json:"input_tokens"`
	OutputTokens      int64   `json:"output_tokens"`
	CachedTokens      int64   `json:"cached_tokens"`
	CostUSD           float64 `json:"cost_usd"`
}

// UsageWindow is the official aggregate for one page window.
type UsageWindow struct {
	Window       string              `json:"window"`
	From         string              `json:"from"`
	To           string              `json:"to"`
	Requests     int64               `json:"requests"`
	InputTokens  int64               `json:"input_tokens"`
	OutputTokens int64               `json:"output_tokens"`
	TotalTokens  int64               `json:"total_tokens"`
	CachedTokens int64               `json:"cached_tokens"`
	CacheRatio   float64             `json:"cache_ratio"`
	CostUSD      float64             `json:"cost_usd"`
	Series       officialUsageSeries `json:"series"`
	// Models splits the same window by model, largest first. For a window built from the
	// daily totals it carries tokens and cost only, because those rows have nothing else.
	Models []UsageModelRow `json:"models,omitempty"`
	// CreditsUsed sums the credits the window consumed; 0 on a flat subscription.
	CreditsUsed int64 `json:"credits_used,omitempty"`
	// BYOKRequests and StreamRequests describe the same records the totals come from.
	BYOKRequests   int64 `json:"byok_requests,omitempty"`
	StreamRequests int64 `json:"stream_requests,omitempty"`
	// Covered is false when the retained records do not reach back to the start of the
	// window, which makes the numbers below a lower bound rather than a total.
	Covered bool `json:"covered"`
	// Detail is false for a window built from the daily totals: those carry tokens and cost
	// only, so requests and cache stay on the local view.
	Detail bool `json:"detail"`
}

// UsageState describes the collector itself, so the page and /health can explain
// why an official number is missing or incomplete.
type UsageState struct {
	Enabled   bool   `json:"enabled"`
	Items     int    `json:"items"`
	Oldest    string `json:"oldest,omitempty"`
	FetchedAt string `json:"fetched_at,omitempty"`
	Truncated bool   `json:"truncated"`
	Failures  int    `json:"failures,omitempty"`
	RetryAt   string `json:"retry_at,omitempty"`
	Error     string `json:"error,omitempty"`
}

// officialWindowSpec drives one page window.
type officialWindowSpec struct {
	Label    string
	Duration time.Duration
	Buckets  int
}

// officialWindowSpecs are the page windows the official records can serve. They are
// bounded by officialUsageRetention.
var officialWindowSpecs = []officialWindowSpec{
	{Label: "1h", Duration: time.Hour, Buckets: 12},
	{Label: "24h", Duration: 24 * time.Hour, Buckets: 24},
}

// officialUsageCollector holds the rolling record set. Items are kept newest first.
type officialUsageCollector struct {
	mu        sync.Mutex
	items     []officialUsageItem
	seen      map[string]struct{}
	fetchedAt time.Time
	lastError string
	truncated bool
	fetching  bool
	failures  int
	retryAt   time.Time
	// maxPages, maxItems and pageDelay bound one walk; tests set them small. Zero means the
	// package default.
	maxPages  int
	maxItems  int
	pageDelay time.Duration
}

func newOfficialUsageCollector() *officialUsageCollector {
	return &officialUsageCollector{
		seen:      make(map[string]struct{}),
		maxPages:  officialUsageMaxPages,
		maxItems:  officialUsageMaxItems,
		pageDelay: officialUsagePageDelay,
	}
}

func (c *officialUsageCollector) pageLimit() int {
	if c.maxPages > 0 {
		return c.maxPages
	}
	return officialUsageMaxPages
}

func (c *officialUsageCollector) itemLimit() int {
	if c.maxItems > 0 {
		return c.maxItems
	}
	return officialUsageMaxItems
}

// usagePage fetches one page of official usage records.
func (c *planClient) usagePage(userID, cursor string, limit int) ([]officialUsageRawItem, string, error) {
	var payload struct {
		Items     []officialUsageRawItem `json:"items"`
		NextToken string                 `json:"nextToken"`
	}
	path := fmt.Sprintf("/users/%s/usages?limit=%d", userID, limit)
	if cursor != "" {
		path += "&cursor=" + url.QueryEscape(cursor)
	}
	if errGet := c.get(path, &payload); errGet != nil {
		return nil, "", errGet
	}
	return payload.Items, payload.NextToken, nil
}

func parseOfficialItem(raw officialUsageRawItem) (officialUsageItem, bool) {
	if raw.ID == "" || raw.CreatedAt == "" {
		return officialUsageItem{}, false
	}
	at, errParse := time.Parse(time.RFC3339Nano, raw.CreatedAt)
	if errParse != nil {
		return officialUsageItem{}, false
	}
	// metadata.raw_model is the model that actually ran; the record's own aiModelName is
	// the routed name the request asked for.
	model := strings.TrimSpace(raw.Metadata.RawModel)
	if model == "" {
		model = strings.TrimSpace(raw.AIModelName)
	}
	return officialUsageItem{
		ID:         raw.ID,
		At:         at,
		Prompt:     raw.PromptTokens,
		Completion: raw.CompletionTokens,
		Cached:     raw.CachedTokens,
		CostMicro:  raw.CostUnits,
		Model:      model,
		Provider:   strings.TrimSpace(raw.AIInferenceProviderName),
		Credits:    raw.CreditsUsed,
		BYOK:       raw.Metadata.IsBYOK,
		Stream:     raw.Metadata.IsStream,
	}, true
}

// addModelRow folds one record into the per-model rows of a window, split by the upstream
// inference channel the record reported: two records of the same model served by different
// channels are two rows, because the row is what the page labels with its channel. counted
// is false for rows built from the daily totals, which have neither a request count nor
// cache columns.
func addModelRow(rows map[string]*UsageModelRow, provider, model string, prompt, completion, cached, costMicro, credits int64, counted bool) {
	model = strings.TrimSpace(model)
	if model == "" {
		model = unknownModelLabel
	}
	provider = strings.TrimSpace(provider)
	key := provider + "\x00" + model
	row, ok := rows[key]
	if !ok {
		row = &UsageModelRow{Model: model, InferenceProvider: provider}
		rows[key] = row
	}
	if counted {
		row.Requests++
	}
	row.InputTokens += prompt
	row.OutputTokens += completion
	row.TotalTokens += prompt + completion
	row.CachedTokens += cached
	row.CostUSD += float64(costMicro) / microUSD
	row.CreditsUsed += credits
}

// modelRows finishes the per-model view: cache ratio per row, largest first. Rows with the
// same size fall back to the model name and then to the channel, so the order is stable
// between refreshes.
func modelRows(rows map[string]*UsageModelRow) []UsageModelRow {
	if len(rows) == 0 {
		return nil
	}
	out := make([]UsageModelRow, 0, len(rows))
	for _, row := range rows {
		if row.InputTokens > 0 {
			row.CacheRatio = float64(row.CachedTokens) / float64(row.InputTokens)
		}
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TotalTokens != out[j].TotalTokens {
			return out[i].TotalTokens > out[j].TotalTokens
		}
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].InferenceProvider < out[j].InferenceProvider
	})
	return out
}

// officialChannelRows aggregates the records the collector retains by (upstream inference
// channel, model that actually ran). The window is the retention window, not one of the page
// windows: the point of the table is which upstream channels served the traffic, and the
// coverage line beside it says how far back the records reach.
//
// The result is never nil: an empty collector is an empty list, not a missing value. A record
// whose payload carries no provider (older cached data, or a field the upstream stopped
// sending) lands in the explicit unknown channel row instead of an empty one, so the row
// still says where those requests went unattributed.
func (c *officialUsageCollector) officialChannelRows() []OfficialChannelRow {
	rows := make([]OfficialChannelRow, 0)
	if c == nil {
		return rows
	}
	c.mu.Lock()
	items := make([]officialUsageItem, len(c.items))
	copy(items, c.items)
	c.mu.Unlock()

	byKey := make(map[string]*OfficialChannelRow, 8)
	for _, item := range items {
		provider := strings.TrimSpace(item.Provider)
		if provider == "" {
			provider = unknownProviderLabel
		}
		model := strings.TrimSpace(item.Model)
		if model == "" {
			model = unknownModelLabel
		}
		key := provider + "\x00" + model
		row, ok := byKey[key]
		if !ok {
			row = &OfficialChannelRow{InferenceProvider: provider, Model: model}
			byKey[key] = row
		}
		row.Requests++
		row.InputTokens += item.Prompt
		row.OutputTokens += item.Completion
		row.CachedTokens += item.Cached
		row.CostUSD += float64(item.CostMicro) / microUSD
	}
	out := make([]OfficialChannelRow, 0, len(byKey))
	for _, row := range byKey {
		out = append(out, *row)
	}
	// Most requests first, then the model name, then the channel: the page shows the busiest
	// channel/model pairs on top and the order never depends on map iteration.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].InferenceProvider < out[j].InferenceProvider
	})
	return out
}

// fetch pages from the newest record backwards until it reaches records it already has,
// walks past the retention horizon, or runs out of the per-walk page budget. The HTTP
// calls happen without the collector lock so the page never waits on a deep walk.
func (c *officialUsageCollector) fetch(client *planClient, userID string, every time.Duration, now time.Time) {
	if c == nil || client == nil || userID == "" {
		return
	}
	cutoff := now.Add(-officialUsageRetention)

	c.mu.Lock()
	if c.fetching {
		c.mu.Unlock()
		return
	}
	c.fetching = true
	var watermark time.Time
	if len(c.items) > 0 {
		watermark = c.items[0].At
	}
	// covered is true when the retained records already reach the retention horizon, in
	// which case a short walk is enough to pick up what happened since the last round.
	covered := false
	if len(c.items) > 0 {
		covered = !c.items[len(c.items)-1].At.After(cutoff)
	}
	known := make(map[string]struct{}, len(c.seen))
	for id := range c.seen {
		known[id] = struct{}{}
	}
	c.mu.Unlock()

	budget := officialUsagePagesSteady
	if !covered {
		budget = officialUsagePagesExtend
	}
	if limit := c.pageLimit(); budget > limit {
		budget = limit
	}

	collected := make([]officialUsageItem, 0, officialUsagePageSize)
	added, cursor := 0, ""
	full := false
	errText := ""
	for page := 0; ; page++ {
		if page >= budget {
			break
		}
		if page > 0 && c.pageDelay > 0 {
			time.Sleep(c.pageDelay)
		}
		items, next, errPage := client.usagePage(userID, cursor, officialUsagePageSize)
		if errPage != nil {
			errText = errPage.Error()
			break
		}
		if len(items) == 0 {
			full = true
			break
		}
		stop := false
		for _, raw := range items {
			item, ok := parseOfficialItem(raw)
			if !ok {
				continue
			}
			// Records arrive newest first: once we are back at the older edge of the
			// retention window, or at the records collected by the previous round, the
			// walk is done.
			if !item.At.After(cutoff) || (!watermark.IsZero() && !item.At.After(watermark)) {
				full = true
				stop = true
				break
			}
			if _, exists := known[item.ID]; exists {
				continue
			}
			if len(collected) >= c.itemLimit() {
				stop = true
				break
			}
			collected = append(collected, item)
			known[item.ID] = struct{}{}
			added++
		}
		if stop {
			break
		}
		if next == "" {
			full = true
			break
		}
		cursor = next
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fetching = false
	if added == 0 && errText != "" {
		// Nothing new and the upstream call failed: keep the previous records untouched.
		c.lastError = errText
		c.noteFailureLocked(now, every)
		return
	}
	c.items = append(c.items, collected...)
	for _, item := range collected {
		c.seen[item.ID] = struct{}{}
	}
	sort.SliceStable(c.items, func(i, j int) bool { return c.items[i].At.After(c.items[j].At) })
	c.pruneLocked(cutoff)
	c.lastError = errText
	c.fetchedAt = now
	// truncated means the retained records do not reach the retention horizon yet; the
	// per-window coverage flag is what the page acts on.
	c.truncated = !full
	if errText == "" {
		c.failures = 0
		c.retryAt = time.Time{}
	} else {
		c.noteFailureLocked(now, every)
	}
}

// noteFailureLocked backs off exponentially after an upstream error so a rate limited or
// failing endpoint is not hammered on every cycle.
func (c *officialUsageCollector) noteFailureLocked(now time.Time, every time.Duration) {
	c.failures++
	if every <= 0 {
		every = config.DefaultPlanUsageRefresh
	}
	shift := c.failures - 1
	if shift > 4 {
		shift = 4
	}
	if shift < 0 {
		shift = 0
	}
	backoff := every * time.Duration(1<<shift)
	if backoff > officialUsageMaxBackoff {
		backoff = officialUsageMaxBackoff
	}
	c.retryAt = now.Add(backoff)
}

// pruneLocked drops the records that fell out of the retention window.
func (c *officialUsageCollector) pruneLocked(cutoff time.Time) {
	kept := c.items[:0]
	for _, item := range c.items {
		if item.At.Before(cutoff) {
			continue
		}
		kept = append(kept, item)
	}
	for i := len(kept); i < len(c.items); i++ {
		delete(c.seen, c.items[i].ID)
	}
	c.items = kept
}

// due reports whether the next fetch should happen now.
func (c *officialUsageCollector) due(now time.Time, every time.Duration) bool {
	if c == nil {
		return false
	}
	if every <= 0 {
		every = config.DefaultPlanUsageRefresh
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.retryAt.IsZero() && now.Before(c.retryAt) {
		return false
	}
	return c.fetchedAt.IsZero() || now.Sub(c.fetchedAt) >= every
}

// refreshIfDue fetches only when the configured interval has elapsed.
func (c *officialUsageCollector) refreshIfDue(client *planClient, userID string, every time.Duration, now time.Time) {
	if c != nil && c.due(now, every) {
		c.fetch(client, userID, every, now)
	}
}

// aggregate turns the retained records into the page windows. accountCreated (zero when
// unknown) is used to tell "the account had no usage yet" apart from "the records do not
// reach back far enough".
func (c *officialUsageCollector) aggregate(now time.Time, accountCreated time.Time) map[string]UsageWindow {
	out := make(map[string]UsageWindow, len(officialWindowSpecs))
	if c == nil {
		return out
	}
	c.mu.Lock()
	items := make([]officialUsageItem, len(c.items))
	copy(items, c.items)
	c.mu.Unlock()

	var oldest time.Time
	if len(items) > 0 {
		oldest = items[len(items)-1].At
	}
	for _, spec := range officialWindowSpecs {
		from := now.Add(-spec.Duration)
		window := UsageWindow{
			Window:  spec.Label,
			From:    from.UTC().Format(time.RFC3339),
			To:      now.UTC().Format(time.RFC3339),
			Covered: true,
			Detail:  true,
			Series: officialUsageSeries{
				Requests: make([]int64, spec.Buckets),
				Tokens:   make([]int64, spec.Buckets),
				Cached:   make([]int64, spec.Buckets),
			},
		}
		// The window is only complete if the retained records reach its start, unless the
		// account itself is younger than the window.
		if !accountCreated.IsZero() && !accountCreated.Before(from) {
			window.Covered = true
		} else if oldest.IsZero() || oldest.After(from) {
			window.Covered = false
		}
		width := spec.Duration / time.Duration(spec.Buckets)
		if width <= 0 {
			width = time.Second
		}
		rows := make(map[string]*UsageModelRow, 8)
		for _, item := range items {
			if item.At.Before(from) {
				continue
			}
			window.Requests++
			window.InputTokens += item.Prompt
			window.OutputTokens += item.Completion
			window.TotalTokens += item.Prompt + item.Completion
			window.CachedTokens += item.Cached
			window.CostUSD += float64(item.CostMicro) / microUSD
			window.CreditsUsed += item.Credits
			if item.BYOK {
				window.BYOKRequests++
			}
			if item.Stream {
				window.StreamRequests++
			}
			addModelRow(rows, item.Provider, item.Model, item.Prompt, item.Completion, item.Cached, item.CostMicro, item.Credits, true)
			bucket := int(item.At.Sub(from) / width)
			if bucket < 0 {
				bucket = 0
			}
			if bucket >= spec.Buckets {
				bucket = spec.Buckets - 1
			}
			window.Series.Requests[bucket]++
			window.Series.Tokens[bucket] += item.Prompt + item.Completion
			window.Series.Cached[bucket] += item.Cached
		}
		if window.InputTokens > 0 {
			window.CacheRatio = float64(window.CachedTokens) / float64(window.InputTokens)
		}
		window.Models = modelRows(rows)
		out[spec.Label] = window
	}
	return out
}

// state reports the collector diagnostics.
func (c *officialUsageCollector) state(enabled bool) UsageState {
	if c == nil {
		return UsageState{Enabled: enabled}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := UsageState{
		Enabled:   enabled,
		Items:     len(c.items),
		Truncated: c.truncated,
		Failures:  c.failures,
		Error:     c.lastError,
	}
	if len(c.items) > 0 {
		out.Oldest = c.items[len(c.items)-1].At.UTC().Format(time.RFC3339)
	}
	if !c.fetchedAt.IsZero() {
		out.FetchedAt = c.fetchedAt.UTC().Format(time.RFC3339)
	}
	if !c.retryAt.IsZero() {
		out.RetryAt = c.retryAt.UTC().Format(time.RFC3339)
	}
	return out
}

// officialDailyWindow builds the 7-day window from the daily totals. The per-request
// endpoint ignores date filters, so covering 7 days there would need hundreds of pages;
// the daily rows are one request for the whole range. They carry tokens and cost only,
// which is why this window reports Detail=false and the page keeps requests and cache on
// the local view. Days are natural UTC days, so the range is today-6 .. today.
func officialDailyWindow(rows []dailyUsageItem, now time.Time, accountCreated time.Time) UsageWindow {
	const days = 7
	utcNow := now.UTC()
	from := time.Date(utcNow.Year(), utcNow.Month(), utcNow.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -(days - 1))
	window := UsageWindow{
		Window: "7d",
		From:   from.Format("2006-01-02"),
		To:     utcNow.Format("2006-01-02"),
		// The daily endpoint returns every day it has rows for, so the window is complete as
		// long as the account is known (an account younger than the window simply has no
		// older usage).
		Covered: !accountCreated.IsZero(),
		Detail:  false,
		Series: officialUsageSeries{
			Requests: make([]int64, days),
			Tokens:   make([]int64, days),
			Cached:   make([]int64, days),
		},
	}
	models := make(map[string]*UsageModelRow, 8)
	for _, row := range rows {
		date, errParse := time.Parse("2006-01-02", strings.TrimSpace(row.Date))
		if errParse != nil {
			continue
		}
		index := int(date.UTC().Sub(from).Hours() / 24)
		if index < 0 || index >= days {
			continue
		}
		window.InputTokens += row.PromptTokens
		window.OutputTokens += row.CompletionTokens
		window.TotalTokens += row.PromptTokens + row.CompletionTokens
		window.CostUSD += float64(row.CostUnits) / microUSD
		window.Series.Tokens[index] += row.PromptTokens + row.CompletionTokens
		// The daily rows carry the routed model name, not the raw one, and no cache, credit
		// or upstream-channel columns: the per-model table says so instead of showing zeros.
		addModelRow(models, "", rowModelName(row), row.PromptTokens, row.CompletionTokens, 0, row.CostUnits, 0, false)
	}
	window.Models = modelRows(models)
	return window
}

// rowModelName is what a daily row calls its model. The routed name is the only one the
// daily endpoint reports.
func rowModelName(row dailyUsageItem) string {
	if name := strings.TrimSpace(row.AIModelName); name != "" {
		return name
	}
	return strings.TrimSpace(row.AIModelTypeName)
}
