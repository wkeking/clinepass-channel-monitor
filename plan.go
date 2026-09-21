package main

import (
	"crypto/sha256"
	"encoding/hex"
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
	// planDefaultUsageRefresh is how often the official per-request records are paged.
	planDefaultUsageRefresh = 5 * time.Minute
	// planAccountStagger separates two credentials in one polling round so several accounts
	// do not page the same upstream endpoint back to back.
	planAccountStagger = 500 * time.Millisecond
	// planMaxCredentials bounds how many credentials are polled at all.
	planMaxCredentials = 8
	planRequestTimeout      = 20 * time.Second
	planUsageWindowDays     = 31
	microUSD                = 1_000_000.0
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

// planAccountSnapshot is the official view of one Cline credential. Several Cline entries
// or several keys inside one entry each get their own snapshot: every key can belong to a
// different Cline account with its own quota.
type planAccountSnapshot struct {
	// ID is a stable, key-derived identifier (never the key itself); the page uses it to
	// remember which account the user selected.
	ID          string                         `json:"id"`
	Label       string                         `json:"label"`
	Source      string                         `json:"source,omitempty"`
	Available   bool                           `json:"available"`
	Account     string                         `json:"account,omitempty"`
	PlanName    string                         `json:"plan_name,omitempty"`
	PlanPrice   string                         `json:"plan_price,omitempty"`
	Limits      []quotaWindow                  `json:"limits,omitempty"`
	Tokens      quotaTokens                    `json:"tokens"`
	TokensError string                         `json:"tokens_error,omitempty"`
	Windows     map[string]officialUsageWindow `json:"windows,omitempty"`
	Usage       officialUsageState             `json:"usage"`
	FetchedAt   string                         `json:"fetched_at,omitempty"`
	Error       string                         `json:"error,omitempty"`
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
	// TokensError explains why the official token totals are missing instead of leaving the
	// page to show zeros.
	TokensError string `json:"tokens_error,omitempty"`
	// Windows carries the official per-request aggregates for the page windows (1h/24h/7d).
	Windows map[string]officialUsageWindow `json:"windows,omitempty"`
	// Usage describes the official record collector behind Windows.
	Usage     officialUsageState `json:"usage"`
	FetchedAt string             `json:"fetched_at,omitempty"`
	Error     string             `json:"error,omitempty"`
	// Accounts lists every configured Cline credential. The fields above mirror the primary
	// account (the first one that answered) so single-account clients keep working.
	Accounts []planAccountSnapshot `json:"accounts,omitempty"`
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
	CreatedAt   string `json:"createdAt"`
}, error) {
	var out struct {
		ID          string `json:"id"`
		DisplayName string `json:"displayName"`
		CreatedAt   string `json:"createdAt"`
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

// planCredential is one Cline credential the poller may use. Its label never contains the
// whole key: it is built from the provider entry name and a masked tail.
type planCredential struct {
	Key    string
	Label  string
	Source string
}

// planAccount is the polling state of one credential.
type planAccount struct {
	id        string
	label     string
	source    string
	key       string
	usage     *officialUsageCollector
	userID    string
	created   time.Time
	tokens    quotaTokens
	tokensErr string
	window7   *officialUsageWindow
	lastDaily time.Time
	snapshot  planAccountSnapshot
}

func newPlanAccount(cred planCredential) *planAccount {
	return &planAccount{
		id:     credentialID(cred.Key),
		label:  cred.Label,
		source: cred.Source,
		key:    cred.Key,
		usage:  newOfficialUsageCollector(),
	}
}

// planPoller keeps one cached snapshot per Cline credential plus the primary view the page
// shows by default. A deployment may configure several Cline entries, or several keys inside
// one entry, and each key can belong to a different Cline account with its own quota, so
// every credential is polled as its own account.
type planPoller struct {
	mu       sync.RWMutex
	quota    planQuota
	stopped  chan struct{}
	once     sync.Once
	accounts []*planAccount
	// loggedEmpty avoids repeating the "no credential" line on every cycle.
	loggedEmpty bool
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

// syncAccounts keeps the polling state of the credentials that are still configured and
// drops the ones that disappeared, so a removed key stops being polled.
func (p *planPoller) syncAccounts(creds []planCredential) []*planAccount {
	byID := make(map[string]*planAccount, len(p.accounts))
	for _, account := range p.accounts {
		byID[account.id] = account
	}
	out := make([]*planAccount, 0, len(creds))
	for _, cred := range creds {
		id := credentialID(cred.Key)
		if existing, ok := byID[id]; ok {
			existing.label = cred.Label
			existing.source = cred.Source
			existing.key = cred.Key
			out = append(out, existing)
			continue
		}
		out = append(out, newPlanAccount(cred))
	}
	return out
}

// refresh polls every configured credential once and publishes the combined snapshot.
// Accounts are polled sequentially and each paces its own paging, so several credentials do
// not turn into a burst against the same upstream.
func (p *planPoller) refresh() {
	cfg := currentConfig()
	creds := resolvePlanCredentials(cfg)
	p.accounts = p.syncAccounts(creds)
	if len(p.accounts) == 0 {
		if !p.loggedEmpty {
			hostLogAsync("warn", pluginID+": no Cline api key available for the subscription card", map[string]string{
				"hint": "set plan_api_key in the plugin config, or point plan_config_path at CPA's config.yaml",
			})
			p.loggedEmpty = true
		}
		p.store(planQuota{Source: "cline-api", Error: "no Cline api key available yet"})
		return
	}
	p.loggedEmpty = false

	// Two keys of the same Cline account share one quota and one rate limit; polling both
	// would only duplicate upstream calls.
	byUserID := make(map[string]string, len(p.accounts))
	for index, account := range p.accounts {
		if index > 0 && cfg.PlanUsageEnabled {
			time.Sleep(planAccountStagger)
		}
		if covered, ok := byUserID[account.userID]; ok && account.userID != "" {
			account.snapshot = planAccountSnapshot{
				ID: account.id, Label: account.label, Source: account.source,
				Account: shortAccountID(account.userID),
				Error:   "与 " + covered + " 是同一个 Cline 账号，已合并（未重复拉取）",
			}
			continue
		}
		p.refreshAccount(cfg, account)
		if account.userID != "" {
			byUserID[account.userID] = account.label
		}
	}
	p.publish()
}

// refreshAccount performs one round of upstream calls for a single credential.
func (p *planPoller) refreshAccount(cfg config, account *planAccount) {
	client := newPlanClient(cfg.PlanBaseURL, account.key)
	now := time.Now()
	snapshot := planAccountSnapshot{
		ID: account.id, Label: account.label, Source: account.source,
		Tokens: account.tokens, TokensError: account.tokensErr,
	}

	user, errMe := client.me()
	if errMe != nil {
		if account.userID == "" {
			snapshot.Error = errMe.Error()
			snapshot.FetchedAt = time.Now().Format(time.RFC3339)
			p.attachUsage(cfg, account, &snapshot, now)
			account.snapshot = snapshot
			return
		}
	} else {
		account.userID = user.ID
		if createdAt, errParse := time.Parse(time.RFC3339Nano, user.CreatedAt); errParse == nil {
			account.created = createdAt.UTC()
		}
		snapshot.Account = shortAccountID(user.ID)
	}

	if cfg.PlanUsageEnabled {
		account.usage.refreshIfDue(client, account.userID, cfg.PlanUsageRefresh.Or(planDefaultUsageRefresh), now)
	}

	limits, errLimits := client.usageLimits()
	if errLimits != nil {
		snapshot.Error = errLimits.Error()
		snapshot.FetchedAt = time.Now().Format(time.RFC3339)
		p.attachUsage(cfg, account, &snapshot, now)
		account.snapshot = snapshot
		return
	}
	if plan, errPlan := client.plan(); errPlan == nil {
		snapshot.PlanName = plan.Name
		if plan.PriceUSD > 0 {
			snapshot.PlanPrice = fmt.Sprintf("$%.2f / 月", plan.PriceUSD)
		}
	}
	snapshot.Limits = limits
	snapshot.Available = true

	// The daily endpoint rejects ranges above 31 days (inclusive), so the window is
	// today-30 .. today. It is refreshed at most once per hour and the totals are kept
	// between refreshes.
	if cfg.PlanDailyEnabled && (account.lastDaily.IsZero() || time.Since(account.lastDaily) > planDefaultDailyEvery) {
		utcNow := time.Now().UTC()
		from := utcNow.AddDate(0, 0, -(planUsageWindowDays - 1)).Format("2006-01-02")
		to := utcNow.Format("2006-01-02")
		items, errDaily := client.dailyUsage(account.userID, from, to)
		if errDaily == nil {
			totals := quotaTokens{FromDate: from, ToDate: to}
			for _, item := range items {
				totals.InputTokens += item.PromptTokens
				totals.OutputTokens += item.CompletionTokens
				totals.CostUSD += float64(item.CostUnits) / microUSD
				totals.Requests++
			}
			totals.TotalTokens = totals.InputTokens + totals.OutputTokens
			if balance, errBalance := client.balance(account.userID); errBalance == nil {
				totals.BalanceUSD = balance
			}
			window7 := officialDailyWindow(items, time.Now(), account.created)
			account.tokens = totals
			account.tokensErr = ""
			account.window7 = &window7
			account.lastDaily = time.Now()
			snapshot.Tokens = totals
			snapshot.TokensError = ""
		} else {
			account.tokensErr = errDaily.Error()
			snapshot.TokensError = errDaily.Error()
		}
	}

	p.attachUsage(cfg, account, &snapshot, now)
	snapshot.FetchedAt = time.Now().Format(time.RFC3339)
	snapshot.Error = ""
	account.snapshot = snapshot
}

// attachUsage hangs the official per-request windows and the collector state onto a
// snapshot.
func (p *planPoller) attachUsage(cfg config, account *planAccount, snapshot *planAccountSnapshot, now time.Time) {
	if !cfg.PlanUsageEnabled {
		return
	}
	snapshot.Windows = account.usage.aggregate(now, account.created)
	if account.window7 != nil {
		snapshot.Windows["7d"] = *account.window7
	}
	snapshot.Usage = account.usage.state(true)
}

// publish exposes the per-account snapshots plus the primary one under the legacy fields.
func (p *planPoller) publish() {
	quota := planQuota{Source: "cline-api"}
	for _, account := range p.accounts {
		quota.Accounts = append(quota.Accounts, account.snapshot)
	}
	primary := p.primaryAccount()
	if primary != nil {
		snapshot := primary.snapshot
		quota.Available = snapshot.Available
		quota.Account = snapshot.Account
		quota.PlanName = snapshot.PlanName
		quota.PlanPrice = snapshot.PlanPrice
		quota.Limits = snapshot.Limits
		quota.Tokens = snapshot.Tokens
		quota.TokensError = snapshot.TokensError
		quota.Windows = snapshot.Windows
		quota.Usage = snapshot.Usage
		quota.FetchedAt = snapshot.FetchedAt
		quota.Error = snapshot.Error
	}
	p.store(quota)
}

// primaryAccount is the account the page shows by default: the first one that answered,
// otherwise the first configured one.
func (p *planPoller) primaryAccount() *planAccount {
	var first *planAccount
	for _, account := range p.accounts {
		if first == nil {
			first = account
		}
		if account.snapshot.Available {
			return account
		}
	}
	return first
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

// resolvePlanCredentials collects every Cline credential this deployment can use. A Cline
// entry may carry several keys, and several entries may point at Cline, so the poller works
// on a list instead of a single key.
//
// Order of preference (earlier sources win when the same key appears twice):
//
//  1. plan_api_key in the plugin configuration (explicit wins),
//  2. the keys of every matching openai-compatibility entry in CPA's configuration file,
//     which is where CPA keeps the provider keys it was configured with,
//  3. the credentials CPA exposes through the host auth callbacks,
//  4. the bearer token observed on an intercepted upstream request.
//
// Values stay in memory: they are never logged, never written to disk and never returned to
// the page (labels only carry a masked tail).
func resolvePlanCredentials(cfg config) []planCredential {
	out := make([]planCredential, 0, 4)
	seen := make(map[string]struct{}, 4)
	add := func(key, label, source string) {
		trimmed := strings.TrimSpace(key)
		if trimmed == "" || len(out) >= planMaxCredentials {
			return
		}
		if _, duplicate := seen[trimmed]; duplicate {
			return
		}
		seen[trimmed] = struct{}{}
		out = append(out, planCredential{Key: trimmed, Label: label, Source: source})
	}

	if key := strings.TrimSpace(cfg.PlanAPIKey); key != "" {
		add(key, "插件配置 · "+maskCredential(key), "plugin-config")
	}
	for _, cred := range clineCredentialsFromConfigFile(cfg) {
		add(cred.Key, cred.Label, cred.Source)
	}
	for _, cred := range clineCredentialsFromHostAuth(cfg) {
		add(cred.Key, cred.Label, cred.Source)
	}
	if key := upstreamBearer(); key != "" {
		add(key, "上游请求头 · "+maskCredential(key), "observed-header")
	}
	if len(out) > 0 {
		return out
	}
	return nil
}

// credentialID derives the stable identifier the page uses to remember a selection. It is a
// hash prefix, so the id itself never carries the key.
func credentialID(key string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(key)))
	return hex.EncodeToString(sum[:])[:12]
}

// maskCredential keeps at most the first three and the last four characters of a key.
func maskCredential(key string) string {
	trimmed := strings.TrimSpace(key)
	if len(trimmed) <= 8 {
		return "…"
	}
	return trimmed[:3] + "…" + trimmed[len(trimmed)-4:]
}

// shortAccountID masks a Cline account id for display.
func shortAccountID(id string) string {
	trimmed := strings.TrimSpace(id)
	if len(trimmed) <= 12 {
		return trimmed
	}
	return trimmed[:8] + "…" + trimmed[len(trimmed)-4:]
}

// startPlanPoller refreshes every configured credential in the background until stop is
// called. Credentials are resolved again on every cycle, so a deployment that only reveals
// its key later (for example through an observed upstream request) still gets the card.
func startPlanPoller() *planPoller {
	poller := newPlanPoller()
	interval := currentConfig().PlanRefresh.Or(planDefaultRefresh)
	if interval < time.Minute {
		interval = time.Minute
	}
	go func() {
		for {
			poller.refresh()
			select {
			case <-poller.stopped:
				return
			case <-time.After(interval):
			}
		}
	}()
	return poller
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

// clineCredentialsFromHostAuth reads every Cline credential CPA exposes through the host auth
// callbacks (auth files). Credentials CPA synthesizes from openai-compatibility entries are
// not listed here; those are read from the configuration file.
func clineCredentialsFromHostAuth(cfg config) []planCredential {
	out := make([]planCredential, 0, 2)
	raw, ok := callHost(pluginabi.MethodHostAuthList, nil)
	if !ok {
		return out
	}
	var listed struct {
		Files []hostAuthFileEntry `json:"files"`
	}
	if errUnmarshal := json.Unmarshal(raw, &listed); errUnmarshal != nil {
		return out
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
		key := strings.TrimSpace(apiKeyFromAuthJSON(got.JSON))
		if key == "" {
			continue
		}
		name := strings.TrimSpace(entry.Name)
		if name == "" {
			name = strings.TrimSpace(entry.Type)
		}
		if name == "" {
			name = "host-auth"
		}
		out = append(out, planCredential{
			Key:    key,
			Label:  name + " · " + maskCredential(key),
			Source: "host-auth",
		})
	}
	hostLogAsync("info", pluginID+": host auth lookup finished", map[string]string{
		"auth_files": strconv.Itoa(count),
		"matched":    strconv.Itoa(matched),
		"usable":     strconv.Itoa(len(out)),
	})
	return out
}

// clineCredentialsFromConfigFile reads CPA's configuration and collects the keys of every
// openai-compatibility entry that points at Cline: matched by base_url host, or by the entry
// name being "Cline" when the host does not match. CPA persists the keys it was configured
// with under api-key-entries, so this is normally the source that needs no user input.
func clineCredentialsFromConfigFile(cfg config) []planCredential {
	out := make([]planCredential, 0, 2)
	paths := planConfigPaths
	if override := strings.TrimSpace(cfg.PlanConfigPath); override != "" {
		paths = []string{override}
	}
	for _, path := range paths {
		raw, errRead := os.ReadFile(path)
		if errRead != nil {
			hostLogAsync("info", pluginID+": config attempt: "+path+": "+errRead.Error(), nil)
			continue
		}
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
			hostLogAsync("info", pluginID+": config attempt: "+path+": decode failed", nil)
			continue
		}
		found := 0
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
			name := strings.TrimSpace(entry.Name)
			if name == "" {
				name = host
			}
			keys := make([]string, 0, len(entry.APIKeys)+len(entry.APIKeyEntries))
			keys = append(keys, entry.APIKeys...)
			for _, keyEntry := range entry.APIKeyEntries {
				keys = append(keys, keyEntry.APIKey)
			}
			index := 0
			for _, key := range keys {
				trimmed := strings.TrimSpace(key)
				if trimmed == "" {
					continue
				}
				index++
				out = append(out, planCredential{
					Key:    trimmed,
					Label:  fmt.Sprintf("%s #%d · %s", name, index, maskCredential(trimmed)),
					Source: "config-file",
				})
			}
			if index > 0 {
				hostLogAsync("info", pluginID+": using Cline credentials from CPA's configuration file", map[string]string{
					"source": "config-file",
					"host":   host,
					"entry":  name,
					"keys":   strconv.Itoa(index),
				})
				found += index
			}
		}
		if found > 0 {
			return out
		}
	}
	return out
}

// planConfigPaths are the locations CPA's configuration can be read from inside the
// container and from a host-side deployment.
var planConfigPaths = []string{
	"/CLIProxyAPI/config.yaml",
	"/app/config.yaml",
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
