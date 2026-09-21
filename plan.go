package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"gopkg.in/yaml.v3"
)

// This file talks to Cline's own API to surface the subscription view: which plan the
// account is on, how much of each rolling quota is used, and the official token totals.
//
// The endpoints are the ones the Cline dashboard uses:
//
//	GET /api/v1/users/me
//	GET /api/v1/users/me/plan
//	GET /api/v1/users/me/plan/usage-limits
//	GET /api/v1/users/{id}/usages/daily?startDate&endDate   (range must be <= 31 days)
//
// Credits are reported in micro-USD: 0.000001 USD per unit.
const (
	planDefaultBaseURL    = "https://api.cline.bot/api/v1"
	planDefaultRefresh    = 5 * time.Minute
	planDefaultDailyEvery = time.Hour
	planRequestTimeout    = 20 * time.Second
	planUsageWindowDays   = 31
	microUSD              = 1_000_000.0
)

// quotaWindow is one rolling limit reported by the usage-limits endpoint.
type quotaWindow struct {
	Type        string  `json:"type"`
	Label       string  `json:"label"`
	PercentUsed float64 `json:"percent_used"`
	ResetsAt    string  `json:"resets_at,omitempty"`
	ResetsIn    string  `json:"resets_in,omitempty"`
}

// quotaTokens carries the official account totals for the last 31 days.
type quotaTokens struct {
	FromDate     string  `json:"from_date"`
	ToDate       string  `json:"to_date"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	TotalTokens  int64   `json:"total_tokens"`
	CostUSD      float64 `json:"cost_usd"`
	BalanceUSD   float64 `json:"balance_usd"`
	Requests     int64   `json:"requests"`
}

// planQuota is the cached view served to the page.
type planQuota struct {
	Available bool          `json:"available"`
	Source    string        `json:"source"`
	Account   string        `json:"account,omitempty"`
	PlanName  string        `json:"plan_name,omitempty"`
	PlanPrice string        `json:"plan_price,omitempty"`
	Limits    []quotaWindow `json:"limits"`
	Tokens    quotaTokens   `json:"tokens"`
	FetchedAt string        `json:"fetched_at,omitempty"`
	Error     string        `json:"error,omitempty"`
}

// planClient performs the upstream calls. It never logs the API key.
type planClient struct {
	http    *http.Client
	baseURL string
	apiKey  string
}

func newPlanClient(baseURL, apiKey string) *planClient {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = planDefaultBaseURL
	}
	return &planClient{
		http:    &http.Client{Timeout: planRequestTimeout},
		baseURL: base,
		apiKey:  strings.TrimSpace(apiKey),
	}
}

func (c *planClient) get(path string, out any) error {
	if c == nil || c.apiKey == "" {
		return fmt.Errorf("no api key configured")
	}
	request, errRequest := http.NewRequest(http.MethodGet, c.baseURL+path, nil)
	if errRequest != nil {
		return errRequest
	}
	request.Header.Set("Authorization", "Bearer "+c.apiKey)
	request.Header.Set("Accept", "application/json")
	response, errDo := c.http.Do(request)
	if errDo != nil {
		return errDo
	}
	defer response.Body.Close()
	body, errRead := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if errRead != nil {
		return errRead
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("upstream status %d", response.StatusCode)
	}
	// {"success":true,"data":...} on success, {"success":false,"error":"..."} otherwise.
	var envelope struct {
		Success bool            `json:"success"`
		Error   string          `json:"error"`
		Data    json.RawMessage `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(body, &envelope); errUnmarshal != nil {
		return fmt.Errorf("decode response: %w", errUnmarshal)
	}
	if !envelope.Success {
		if envelope.Error == "" {
			envelope.Error = "upstream reported failure"
		}
		return fmt.Errorf("%s", envelope.Error)
	}
	if out == nil {
		return nil
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return fmt.Errorf("upstream returned no data")
	}
	return json.Unmarshal(envelope.Data, out)
}

func (c *planClient) me() (struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}, error) {
	var out struct {
		ID          string `json:"id"`
		DisplayName string `json:"displayName"`
	}
	errGet := c.get("/users/me", &out)
	return out, errGet
}

func (c *planClient) usageLimits() ([]quotaWindow, error) {
	var payload struct {
		Limits []struct {
			Type        string  `json:"type"`
			PercentUsed float64 `json:"percentUsed"`
			ResetsAt    string  `json:"resetsAt"`
		} `json:"limits"`
	}
	if errGet := c.get("/users/me/plan/usage-limits", &payload); errGet != nil {
		return nil, errGet
	}
	out := make([]quotaWindow, 0, len(payload.Limits))
	for _, item := range payload.Limits {
		window := quotaWindow{
			Type:        item.Type,
			Label:       quotaWindowLabel(item.Type),
			PercentUsed: item.PercentUsed,
			ResetsAt:    item.ResetsAt,
		}
		if resetAt, errParse := time.Parse(time.RFC3339Nano, item.ResetsAt); errParse == nil {
			window.ResetsIn = time.Until(resetAt).Round(time.Minute).String()
		}
		out = append(out, window)
	}
	return out, nil
}

func quotaWindowLabel(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "five_hour", "five_hours", "5_hour", "5h":
		return "5 小时滚动窗口"
	case "weekly", "week":
		return "本周额度"
	case "monthly", "month":
		return "本月额度"
	default:
		return kind
	}
}

func (c *planClient) plan() (struct {
	Name     string  `json:"displayName"`
	PriceUSD float64 `json:"pricePerSeatCents"`
}, error) {
	var payload struct {
		Plan struct {
			DisplayName       string `json:"displayName"`
			PricePerSeatCents int64  `json:"pricePerSeatCents"`
		} `json:"plan"`
	}
	errGet := c.get("/users/me/plan", &payload)
	return struct {
		Name     string  `json:"displayName"`
		PriceUSD float64 `json:"pricePerSeatCents"`
	}{Name: payload.Plan.DisplayName, PriceUSD: float64(payload.Plan.PricePerSeatCents) / 100}, errGet
}

// dailyUsage returns the account totals for a date range of at most 31 days.
type dailyUsageItem struct {
	Date             string `json:"date"`
	Operation        string `json:"operation"`
	AIModelTypeName  string `json:"aiModelTypeName"`
	AIModelName      string `json:"aiModelName"`
	CostUnits        int64  `json:"costUsd"`
	PromptTokens     int64  `json:"promptTokens"`
	CompletionTokens int64  `json:"completionTokens"`
}

func (c *planClient) dailyUsage(userID, from, to string) ([]dailyUsageItem, error) {
	var payload struct {
		Items []dailyUsageItem `json:"items"`
	}
	path := fmt.Sprintf("/users/%s/usages/daily?startDate=%s&endDate=%s", userID, from, to)
	if errGet := c.get(path, &payload); errGet != nil {
		return nil, errGet
	}
	return payload.Items, nil
}

func (c *planClient) balance(userID string) (float64, error) {
	var payload struct {
		BalanceUnits int64 `json:"balance"`
	}
	if errGet := c.get("/users/"+userID+"/balance", &payload); errGet != nil {
		return 0, errGet
	}
	return float64(payload.BalanceUnits) / microUSD, nil
}

// planPoller keeps one cached snapshot of the upstream subscription state.
type planPoller struct {
	mu        sync.RWMutex
	quota     planQuota
	stopped   chan struct{}
	once      sync.Once
	lastDaily time.Time
}

func newPlanPoller() *planPoller {
	return &planPoller{stopped: make(chan struct{})}
}

func (p *planPoller) snapshot() planQuota {
	if p == nil {
		return planQuota{}
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.quota
}

func (p *planPoller) store(quota planQuota) {
	p.mu.Lock()
	p.quota = quota
	p.mu.Unlock()
}

func (p *planPoller) stop() {
	if p == nil {
		return
	}
	p.once.Do(func() { close(p.stopped) })
}

// refresh performs one round of upstream calls. Errors are reported in the snapshot so
// the page can show why the quota is unavailable instead of silently showing zeros.
func (p *planPoller) refresh(apiKey string) {
	cfg := currentConfig()
	client := newPlanClient(cfg.PlanBaseURL, apiKey)
	quota := planQuota{Available: false, Source: "cline-api"}

	user, errMe := client.me()
	if errMe != nil {
		quota.Error = errMe.Error()
		quota.FetchedAt = time.Now().Format(time.RFC3339)
		p.store(quota)
		return
	}
	if len(user.ID) > 12 {
		quota.Account = user.ID[:8] + "…" + user.ID[len(user.ID)-4:]
	} else {
		quota.Account = user.ID
	}

	limits, errLimits := client.usageLimits()
	if errLimits != nil {
		quota.Error = errLimits.Error()
		quota.FetchedAt = time.Now().Format(time.RFC3339)
		p.store(quota)
		return
	}
	if plan, errPlan := client.plan(); errPlan == nil {
		quota.PlanName = plan.Name
		if plan.PriceUSD > 0 {
			quota.PlanPrice = fmt.Sprintf("$%.2f / 月", plan.PriceUSD)
		}
	}
	quota.Limits = limits
	quota.Available = true

	// The daily endpoint rejects ranges above 31 days, so the totals cover the last
	// 31 days. It is refreshed at most once per hour to stay polite with the upstream.
	if cfg.PlanDailyEnabled && (p.lastDaily.IsZero() || time.Since(p.lastDaily) > planDefaultDailyEvery) {
		now := time.Now().UTC()
		from := now.AddDate(0, 0, -planUsageWindowDays).Format("2006-01-02")
		to := now.Format("2006-01-02")
		if items, errDaily := client.dailyUsage(user.ID, from, to); errDaily == nil {
			totals := quotaTokens{FromDate: from, ToDate: to}
			for _, item := range items {
				totals.InputTokens += item.PromptTokens
				totals.OutputTokens += item.CompletionTokens
				totals.CostUSD += float64(item.CostUnits) / microUSD
				totals.Requests++
			}
			totals.TotalTokens = totals.InputTokens + totals.OutputTokens
			if balance, errBalance := client.balance(user.ID); errBalance == nil {
				totals.BalanceUSD = balance
			}
			quota.Tokens = totals
			p.lastDaily = time.Now()
		}
	}
	quota.FetchedAt = time.Now().Format(time.RFC3339)
	quota.Error = ""
	p.store(quota)
}

// latestUpstreamBearer remembers the bearer token that CPA sent upstream for the most
// recent Cline request. It is the last-resort source for the subscription card when the
// deployment keeps the credential somewhere the plugin cannot read.
var latestUpstreamBearer struct {
	sync.Mutex
	value    string
	lastKeys string
	lastLen  int
}

func rememberUpstreamBearer(headers http.Header) {
	if headers == nil {
		return
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	auth := strings.TrimSpace(headers.Get("Authorization"))
	if auth == "" {
		auth = strings.TrimSpace(headers.Get("Api-Key"))
	}
	auth = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	// The header names are recorded for diagnosis; the values never are.
	latestUpstreamBearer.Lock()
	latestUpstreamBearer.lastKeys = strings.Join(names, ",")
	latestUpstreamBearer.lastLen = len(auth)
	if len(auth) >= 20 {
		latestUpstreamBearer.value = auth
	}
	latestUpstreamBearer.Unlock()
}

func upstreamBearer() string {
	latestUpstreamBearer.Lock()
	defer latestUpstreamBearer.Unlock()
	return latestUpstreamBearer.value
}

// upstreamBearerDiagnostics reports what the last intercepted request carried, without
// exposing any value.
func upstreamBearerDiagnostics() (string, int) {
	latestUpstreamBearer.Lock()
	defer latestUpstreamBearer.Unlock()
	return latestUpstreamBearer.lastKeys, latestUpstreamBearer.lastLen
}

// resolvePlanAPIKey finds the credential used for the subscription card: an explicit
// configuration value first, then CPA's own auth store, then CPA's configuration file,
// and finally the bearer token CPA itself sends to Cline.
func resolvePlanAPIKey() string {
	cfg := currentConfig()
	if key := strings.TrimSpace(cfg.PlanAPIKey); key != "" {
		return key
	}
	if key := discoverClineAPIKey(cfg); key != "" {
		return key
	}
	if key := clineAPIKeyFromConfigFile(cfg); key != "" {
		return key
	}
	if key := upstreamBearer(); key != "" {
		hostLogAsync("info", pluginID+": using the bearer token observed on an upstream Cline request", map[string]string{
			"source": "observed-header",
		})
		return key
	}
	hostLogAsync("warn", pluginID+": no Cline api key available for the subscription card", map[string]string{
		"hint": "set plan_api_key in the plugin config, or point plan_config_path at CPA's config.yaml",
	})
	return ""
}

// startPlanPoller refreshes the quota in the background until stop is called. The key is
// resolved again on every cycle, so a deployment that can only reveal its credential
// later (for example through an observed upstream request) still gets the card.
func startPlanPoller(apiKey string) *planPoller {
	poller := newPlanPoller()
	interval := currentConfig().PlanRefresh.Or(planDefaultRefresh)
	if interval < time.Minute {
		interval = time.Minute
	}
	go func() {
		key := apiKey
		for {
			if strings.TrimSpace(key) == "" {
				key = resolvePlanAPIKey()
			}
			if strings.TrimSpace(key) == "" {
				poller.store(planQuota{Source: "cline-api", Error: "no Cline api key available yet"})
			} else {
				poller.refresh(key)
			}
			select {
			case <-poller.stopped:
				return
			case <-time.After(interval):
			}
		}
	}()
	return poller
}

// clineAPIKey resolves the key used to query Cline, in order of preference:
//
//  1. plan_api_key in the plugin configuration (explicit wins),
//  2. the credential CPA is configured with, read through the host auth callbacks,
//  3. the same credential read from CPA's own configuration file, which is mounted
//     read-only into the container and is the last resort when the host does not
//     expose synthesized credentials.
//
// The value stays in memory: it is never logged, never written to disk and never
// returned to the page.
func clineAPIKey(cfg config) string {
	if key := strings.TrimSpace(cfg.PlanAPIKey); key != "" {
		return key
	}
	if key := discoverClineAPIKey(cfg); key != "" {
		return key
	}
	return clineAPIKeyFromConfigFile(cfg)
}

type hostAuthFileEntry struct {
	AuthIndex string `json:"auth_index"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	Provider  string `json:"provider"`
	Disabled  bool   `json:"disabled"`
	BaseURL   string `json:"base_url"`
}

type hostAuthGetResponse struct {
	AuthIndex string          `json:"auth_index"`
	Name      string          `json:"name"`
	JSON      json.RawMessage `json:"json"`
}

// discoverClineAPIKey reads the Cline credential through the host auth callbacks.
func discoverClineAPIKey(cfg config) string {
	raw, ok := callHost(pluginabi.MethodHostAuthList, nil)
	if !ok {
		return ""
	}
	var listed struct {
		Files []hostAuthFileEntry `json:"files"`
	}
	if errUnmarshal := json.Unmarshal(raw, &listed); errUnmarshal != nil {
		return ""
	}
	count, matched := 0, 0
	for _, entry := range listed.Files {
		count++
		if !entryMatchesCline(entry, cfg) {
			continue
		}
		matched++
		payload, errMarshal := json.Marshal(map[string]string{"auth_index": entry.AuthIndex})
		if errMarshal != nil {
			continue
		}
		rawAuth, okGet := callHost(pluginabi.MethodHostAuthGet, payload)
		if !okGet {
			continue
		}
		var got hostAuthGetResponse
		if errUnmarshal := json.Unmarshal(rawAuth, &got); errUnmarshal != nil {
			continue
		}
		if key := apiKeyFromAuthJSON(got.JSON); key != "" {
			hostLogAsync("info", pluginID+": using the Cline credential registered with CPA", map[string]string{
				"source":     "host-auth",
				"auth_index": entry.AuthIndex,
			})
			return key
		}
	}
	hostLogAsync("info", pluginID+": host auth lookup finished without a usable Cline credential", map[string]string{
		"auth_files": strconv.Itoa(count),
		"matched":    strconv.Itoa(matched),
	})
	return ""
}

// clineAPIKeyFromConfigFile reads CPA's configuration and picks the first key of the
// openai-compatibility entry whose base URL is a Cline host.
func clineAPIKeyFromConfigFile(cfg config) string {
	attempts := make([]string, 0, len(planConfigPaths))
	paths := planConfigPathOverride()
	if len(paths) == 0 {
		paths = planConfigPaths
	}
	for _, path := range paths {
		raw, errRead := os.ReadFile(path)
		if errRead != nil {
			attempts = append(attempts, path+": "+errRead.Error())
			continue
		}
		attempts = append(attempts, path+": read "+strconv.Itoa(len(raw))+" bytes")
		var file struct {
			OpenAICompatibility []struct {
				Name          string   `yaml:"name"`
				BaseURL       string   `yaml:"base-url"`
				Disabled      bool     `yaml:"disabled"`
				APIKeys       []string `yaml:"api-keys"`
				APIKeyEntries []struct {
					APIKey string `yaml:"api-key"`
				} `yaml:"api-key-entries"`
			} `yaml:"openai-compatibility"`
		}
		if errUnmarshal := yaml.Unmarshal(raw, &file); errUnmarshal != nil {
			continue
		}
		attempts[len(attempts)-1] += " entries=" + strconv.Itoa(len(file.OpenAICompatibility))
		for _, entry := range file.OpenAICompatibility {
			if entry.Disabled {
				continue
			}
			host := hostFromBaseURL(entry.BaseURL)
			if host == "" {
				continue
			}
			matches := false
			if len(cfg.Hosts) > 0 {
				_, matches = cfg.hostMatched(entry.BaseURL)
			}
			if !matches && !strings.EqualFold(strings.TrimSpace(entry.Name), "cline") {
				continue
			}
			attempts[len(attempts)-1] += " keys[" + entry.Name + "]=" + strconv.Itoa(len(entry.APIKeys))
			for _, key := range entry.APIKeys {
				if trimmed := strings.TrimSpace(key); trimmed != "" {
					hostLogAsync("info", pluginID+": using the Cline credential from CPA's configuration file", map[string]string{
						"source": "config-file",
						"host":   host,
					})
					return trimmed
				}
			}
		}
	}
	for _, attempt := range attempts {
		hostLogAsync("info", pluginID+": config attempt: "+attempt, nil)
	}
	return ""
}

// planConfigPaths are the locations CPA's configuration can be read from inside the
// container and from a host-side deployment.
var planConfigPaths = []string{
	"/CLIProxyAPI/config.yaml",
	"/app/config.yaml",
}

// planConfigPathOverride is set from the plugin configuration so a deployment can point
// at wherever CPA keeps its configuration file inside the container.
func planConfigPathOverride() []string {
	if path := strings.TrimSpace(currentConfig().PlanConfigPath); path != "" {
		return []string{path}
	}
	return nil
}

// apiKeyFromAuthJSON pulls the upstream key out of a credential document.
func apiKeyFromAuthJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var fields map[string]any
	if errUnmarshal := json.Unmarshal(raw, &fields); errUnmarshal != nil {
		return ""
	}
	for _, name := range []string{"api_key", "apiKey", "key", "access_token"} {
		if value, ok := fields[name].(string); ok {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

// entryMatchesCline decides whether an auth entry belongs to Cline. The base URL host is
// the deciding factor; provider and name are only fallback hints.
func entryMatchesCline(entry hostAuthFileEntry, cfg config) bool {
	if entry.Disabled {
		return false
	}
	if len(cfg.Hosts) > 0 {
		if _, matched := cfg.hostMatched(entry.BaseURL); matched {
			return true
		}
	}
	haystack := strings.ToLower(entry.Provider + " " + entry.Name)
	for _, host := range cfg.Hosts {
		if host != "" && strings.Contains(haystack, strings.TrimPrefix(host, ".")) {
			return true
		}
	}
	return strings.Contains(haystack, "cline")
}

// entryMatchesCline decides whether an auth entry belongs to Cline. The base URL host is
// the deciding factor; the provider/name is only a fallback hint.

// planPoller keeps one cached snapshot of the upstream subscription state.
