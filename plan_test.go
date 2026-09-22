package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestClineAPIKeyFromConfigFileReadsAPIKeyEntries guards the credential discovery path:
// CPA injects the keys it is configured with through the management API into
// api-key-entries, so the file usually carries the key there rather than in api-keys.
func TestClineAPIKeyFromConfigFileReadsAPIKeyEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "openai-compatibility:\n" +
		"  - name: OpenCode Go\n" +
		"    base-url: https://opencode.ai/zen/go/v1\n" +
		"    api-key-entries:\n" +
		"      - api-key: other-key\n" +
		"  - name: Cline\n" +
		"    base-url: https://api.cline.bot/api/v1\n" +
		"    api-key-entries:\n" +
		"      - api-key: key-from-entries\n"
	if errWrite := os.WriteFile(path, []byte(content), 0o600); errWrite != nil {
		t.Fatalf("write fixture: %v", errWrite)
	}
	cfg := defaultConfig()
	cfg.PlanConfigPath = path
	creds := clineCredentialsFromConfigFile(cfg)
	if len(creds) != 1 || creds[0].Key != "key-from-entries" {
		t.Fatalf("creds = %+v，期望从 api-key-entries 读到 key-from-entries", creds)
	}
	if creds[0].Label != "Cline #1 · key…ries" {
		t.Errorf("label = %q，期望「条目名 #序号 · 掩码key」", creds[0].Label)
	}
	if strings.Contains(creds[0].Label, "key-from-entries") {
		t.Errorf("label 不应包含完整 key: %q", creds[0].Label)
	}
}

// fakeClineAPI serves the Cline dashboard endpoints the plan poller uses.
func fakeClineAPI(t *testing.T, now time.Time) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		write := func(data any) {
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
		}
		switch {
		case r.URL.Path == "/users/me":
			write(map[string]any{
				"id":          "usr-test-0001",
				"displayName": "Test",
				"createdAt":   now.AddDate(0, 0, -30).Format(time.RFC3339Nano),
			})
		case r.URL.Path == "/users/me/plan":
			write(map[string]any{"plan": map[string]any{"displayName": "Cline Pass (Monthly)", "pricePerSeatCents": 999}})
		case r.URL.Path == "/users/me/plan/usage-limits":
			write(map[string]any{"limits": []map[string]any{
				{"type": "five_hour", "percentUsed": 16, "resetsAt": now.Add(30 * time.Minute).Format(time.RFC3339Nano)},
				{"type": "weekly", "percentUsed": 59, "resetsAt": now.Add(80 * time.Hour).Format(time.RFC3339Nano)},
				{"type": "monthly", "percentUsed": 29, "resetsAt": now.Add(600 * time.Hour).Format(time.RFC3339Nano)},
			}})
		case r.URL.Path == "/users/usr-test-0001/usages":
			write(map[string]any{"items": []officialUsageRawItem{
				usageItem("usg-a", now.Add(-10*time.Minute), 1000, 100, 800, 1_000_000),
				usageItem("usg-b", now.Add(-3*time.Hour), 2000, 200, 900, 2_000_000),
			}, "nextToken": "", "total": 2})
		case r.URL.Path == "/users/usr-test-0001/usages/daily":
			// The upstream rejects inclusive ranges above 31 days: the poller must ask for
			// today-30 .. today.
			from, errFrom := time.Parse("2006-01-02", r.URL.Query().Get("startDate"))
			to, errTo := time.Parse("2006-01-02", r.URL.Query().Get("endDate"))
			if errFrom != nil || errTo != nil {
				_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "error": "missing date range"})
				return
			}
			if days := int(to.Sub(from).Hours()/24) + 1; days > 31 {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "error": "Date range must not exceed 31 days"})
				return
			}
			write(map[string]any{"items": []map[string]any{
				{"date": to.Format("2006-01-02"), "operation": "chat_completion", "costUsd": 1_000_000, "promptTokens": 1000, "completionTokens": 100},
				{"date": to.AddDate(0, 0, -3).Format("2006-01-02"), "operation": "chat_completion", "costUsd": 500_000, "promptTokens": 2000, "completionTokens": 200},
			}})
		case r.URL.Path == "/users/usr-test-0001/balance":
			write(map[string]any{"balance": 499865})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// TestPlanPollerRefreshServesOfficialWindows checks one poller round end to end against a
// fake Cline API: plan, limits, official token totals and the page windows.
func TestPlanPollerRefreshServesOfficialWindows(t *testing.T) {
	now := time.Now().UTC()
	server := fakeClineAPI(t, now)
	loadConfig([]byte(fmt.Sprintf(
		"plan_enabled: false\nplan_api_key: test-key\nplan_base_url: %s\nplan_daily_enabled: true\nplan_usage_enabled: true\n",
		server.URL)))
	defer func() {
		loadConfig([]byte("plan_enabled: false\n"))
	}()

	poller := newPlanPoller()
	poller.refresh()
	quota := poller.snapshot()

	if !quota.Available || quota.Error != "" {
		t.Fatalf("快照应可用，实际 available=%v error=%q", quota.Available, quota.Error)
	}
	if len(quota.Limits) != 3 || quota.Limits[0].Label != "5 小时滚动窗口" {
		t.Errorf("限额 = %+v", quota.Limits)
	}
	if quota.Limits[1].PercentUsed != 59 {
		t.Errorf("周限额 = %v，期望 59", quota.Limits[1].PercentUsed)
	}
	if quota.PlanName != "Cline Pass (Monthly)" || quota.PlanPrice != "$9.99 / 月" {
		t.Errorf("套餐 = %q %q", quota.PlanName, quota.PlanPrice)
	}
	totals := quota.Tokens
	if totals.TotalTokens != 3300 || totals.InputTokens != 3000 || totals.OutputTokens != 300 {
		t.Errorf("官方总量 = %+v，期望 input=3000 output=300", totals)
	}
	if totals.Requests != 2 {
		t.Errorf("官方计费条目 = %d，期望 2（按日逐模型的行数）", totals.Requests)
	}
	if math.Abs(totals.CostUSD-1.5) > 1e-9 {
		t.Errorf("官方成本 = %v，期望 1.5 USD（微美元换算）", totals.CostUSD)
	}
	if totals.BalanceUSD != 0.499865 {
		t.Errorf("余额 = %v，期望 0.499865", totals.BalanceUSD)
	}
	if quota.TokensError != "" {
		t.Errorf("31 天官方汇总不应报错: %q", quota.TokensError)
	}
	if !quota.Usage.Enabled || quota.Usage.Items != 2 || quota.Usage.Error != "" || quota.Usage.Truncated {
		t.Errorf("官方明细采集状态 = %+v", quota.Usage)
	}
	day, ok := quota.Windows["24h"]
	if !ok {
		t.Fatalf("缺少 24h 官方窗口: %+v", quota.Windows)
	}
	if day.Requests != 2 || day.TotalTokens != 3300 || day.CachedTokens != 1700 {
		t.Errorf("24h 官方窗口 = %+v", day)
	}
	if hour := quota.Windows["1h"]; hour.Requests != 1 {
		t.Errorf("1h 官方窗口 = %+v，期望只有 10 分钟前的那条", hour)
	}
	// 7 天窗口来自官方按自然日汇总：只有 token 与成本，明细字段留给本机口径。
	week, ok := quota.Windows["7d"]
	if !ok {
		t.Fatalf("缺少 7d 官方窗口: %+v", quota.Windows)
	}
	if week.Detail {
		t.Errorf("7d 窗口来自按日汇总，detail 应为 false: %+v", week)
	}
	if len(totals.Series) != 31 {
		t.Errorf("31 天逐日序列长度 = %d，期望 31", len(totals.Series))
	} else {
		if totals.Series[30] != 1100 {
			t.Errorf("序列最后一位 = %d，期望今天的 1100", totals.Series[30])
		}
		if totals.Series[27] != 2200 {
			t.Errorf("序列倒数第四位 = %d，期望三天前的 2200", totals.Series[27])
		}
	}
	if week.TotalTokens != 3300 || math.Abs(week.CostUSD-1.5) > 1e-9 {
		t.Errorf("7d 官方窗口 = %+v，期望 tokens=3300 cost=1.5", week)
	}
	if !week.Covered {
		t.Errorf("账号创建于 30 天前，7d 窗口应标记 covered: %+v", week)
	}
	if len(week.Series.Tokens) != 7 {
		t.Errorf("7d 折线桶数 = %d，期望 7（按自然日）", len(week.Series.Tokens))
	}
}

// TestPlanPollerSurvivesUpstreamFailure checks the fail-open behaviour: a failing upstream
// keeps the poller serving a snapshot that explains itself.
func TestPlanPollerSurvivesUpstreamFailure(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer broken.Close()
	loadConfig([]byte(fmt.Sprintf("plan_enabled: false\nplan_api_key: test-key\nplan_base_url: %s\n", broken.URL)))

	poller := newPlanPoller()
	poller.refresh()
	quota := poller.snapshot()
	if quota.Available {
		t.Errorf("上游失败时不应标记可用")
	}
	if !strings.Contains(quota.Error, "401") {
		t.Errorf("错误信息应包含状态码，实际 %q", quota.Error)
	}
}

// TestPlanPollerPollsEveryConfiguredCredential checks that several keys inside one Cline
// entry become several accounts with their own quota and a label that never leaks the key.
func TestPlanPollerPollsEveryConfiguredCredential(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "openai-compatibility:\n" +
		"  - name: Cline\n" +
		"    base-url: https://api.cline.bot/api/v1\n" +
		"    api-key-entries:\n" +
		"      - api-key: key-one-aaaaaaaaaaaaaaaa\n" +
		"      - api-key: key-two-bbbbbbbbbbbbbbbb\n"
	if errWrite := os.WriteFile(path, []byte(content), 0o600); errWrite != nil {
		t.Fatalf("write fixture: %v", errWrite)
	}

	// 每把 key 属于不同账号：用 Authorization 头区分。
	limitsByKey := map[string]float64{
		"key-one-aaaaaaaaaaaaaaaa": 11,
		"key-two-bbbbbbbbbbbbbbbb": 42,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		w.Header().Set("Content-Type", "application/json")
		write := func(data any) { _ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data}) }
		switch {
		case r.URL.Path == "/users/me":
			write(map[string]any{"id": "usr-" + key, "displayName": key, "createdAt": "2026-09-17T00:00:00Z"})
		case r.URL.Path == "/users/me/plan":
			write(map[string]any{"plan": map[string]any{"displayName": "Cline Pass (Monthly)", "pricePerSeatCents": 999}})
		case r.URL.Path == "/users/me/plan/usage-limits":
			write(map[string]any{"limits": []map[string]any{
				{"type": "five_hour", "percentUsed": limitsByKey[key], "resetsAt": "2026-09-21T20:00:00Z"},
			}})
		default:
			write(map[string]any{"items": []any{}})
		}
	}))
	defer server.Close()

	loadConfig([]byte(fmt.Sprintf(
		"plan_enabled: false\nplan_config_path: %s\nplan_base_url: %s\nplan_usage_enabled: false\nplan_daily_enabled: false\n",
		path, server.URL)))
	defer func() { loadConfig([]byte("plan_enabled: false\n")) }()

	poller := newPlanPoller()
	poller.refresh()
	quota := poller.snapshot()
	if len(quota.Accounts) != 2 {
		t.Fatalf("accounts = %+v，期望两把 key 各一个账号", quota.Accounts)
	}
	byLabel := map[string]planAccountSnapshot{}
	for _, account := range quota.Accounts {
		if account.ID == "" {
			t.Errorf("账号缺少 id: %+v", account)
		}
		if strings.Contains(account.Label, "key-one") || strings.Contains(account.Label, "key-two") {
			t.Errorf("label 不应包含完整 key: %q", account.Label)
		}
		byLabel[account.Label] = account
	}
	first, okFirst := byLabel["Cline #1 · key…aaaa"]
	second, okSecond := byLabel["Cline #2 · key…bbbb"]
	if !okFirst || !okSecond {
		t.Fatalf("标签不符合预期: %v", byLabel)
	}
	if !first.Available || !second.Available {
		t.Fatalf("两个账号都应可用: %+v %+v", first, second)
	}
	if first.Limits[0].PercentUsed != 11 || second.Limits[0].PercentUsed != 42 {
		t.Errorf("每个账号应显示自己的限额: %+v %+v", first.Limits, second.Limits)
	}
	if first.ID == second.ID {
		t.Errorf("不同 key 的账号 id 不应相同")
	}
	// 主视图跟随第一个账号，页面单选时用得到。
	if quota.PlanName != "Cline Pass (Monthly)" || len(quota.Accounts) != 2 {
		t.Errorf("主视图 = %+v", quota)
	}
	if len(quota.Windows) != 0 {
		t.Errorf("plan_usage_enabled=false 时不应有官方窗口: %+v", quota.Windows)
	}
}

// TestResolvePlanCredentialsSkipsClientBearer guards the last-resort credential source: the
// bearer observed on an intercepted request is the client's key for CPA, not a Cline key, so
// accepting it produced a second, permanently unavailable account in the picker.
func TestResolvePlanCredentialsSkipsClientBearer(t *testing.T) {
	cfg := defaultConfig()
	cfg.PlanConfigPath = filepath.Join(t.TempDir(), "missing.yaml")
	resetBearer := func() {
		latestUpstreamBearer.Lock()
		latestUpstreamBearer.value = ""
		latestUpstreamBearer.Unlock()
	}
	defer resetBearer()

	headers := http.Header{}
	headers.Set("Authorization", "Bearer sk-TESTKEY00000000001") // 20 字符的下游 key
	rememberUpstreamBearer(headers)
	if creds := resolvePlanCredentials(cfg); len(creds) != 0 {
		t.Fatalf("下游客户端 key 不应被当成 Cline 凭据: %+v", creds)
	}

	clineKey := "sk_" + strings.Repeat("a", 64) // 67 字符
	headers = http.Header{}
	headers.Set("Authorization", "Bearer "+clineKey)
	rememberUpstreamBearer(headers)
	creds := resolvePlanCredentials(cfg)
	if len(creds) != 1 || creds[0].Source != "observed-header" {
		t.Fatalf("形如 Cline key 的 bearer 应被采用: %+v", creds)
	}
	if strings.Contains(creds[0].Label, clineKey) {
		t.Errorf("label 不应包含完整 key: %q", creds[0].Label)
	}
	resetBearer()
}

// TestPlanPollerMarksRejectedCredential checks that a credential the upstream refuses is
// flagged so the page can hide it from the account picker.
func TestPlanPollerMarksRejectedCredential(t *testing.T) {
	rejected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer rejected.Close()
	loadConfig([]byte(fmt.Sprintf("plan_enabled: false\nplan_api_key: bad-key\nplan_base_url: %s\n", rejected.URL)))

	poller := newPlanPoller()
	poller.refresh()
	quota := poller.snapshot()
	if len(quota.Accounts) != 1 {
		t.Fatalf("accounts = %+v", quota.Accounts)
	}
	if !quota.Accounts[0].Rejected {
		t.Errorf("401 应标记 rejected: %+v", quota.Accounts[0])
	}
	if quota.Accounts[0].Available {
		t.Errorf("被拒绝的凭据不应可用")
	}
}

// TestClineCredentialsMatchByHostOrName locks the rule that answers "which provider config
// does the official usage key come from": an entry matches when its base-url host is in
// hosts (default api.cline.bot) or when its name is exactly Cline (self-hosted relay), and
// disabled entries are skipped.
func TestClineCredentialsMatchByHostOrName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "openai-compatibility:\n" +
		"  - name: CP\n" +
		"    base-url: https://api.cline.bot/api/v1\n" +
		"    api-key-entries:\n" +
		"      - api-key: key-by-host\n" +
		"  - name: Cline\n" +
		"    base-url: https://relay.example.com/v1\n" +
		"    api-key-entries:\n" +
		"      - api-key: key-by-name\n" +
		"  - name: Other\n" +
		"    base-url: https://opencode.ai/zen/go/v1\n" +
		"    api-keys: [key-not-cline]\n" +
		"  - name: Cline\n" +
		"    base-url: https://api.cline.bot/api/v1\n" +
		"    disabled: true\n" +
		"    api-keys: [key-disabled]\n"
	if errWrite := os.WriteFile(path, []byte(content), 0o600); errWrite != nil {
		t.Fatalf("write fixture: %v", errWrite)
	}
	cfg := defaultConfig()
	cfg.PlanConfigPath = path
	creds := clineCredentialsFromConfigFile(cfg)
	got := map[string]string{}
	for _, cred := range creds {
		got[cred.Key] = cred.Label
	}
	if len(creds) != 2 {
		t.Fatalf("creds = %+v，期望按 host 与 name 各命中一个（disabled 与非 Cline 条目跳过）", creds)
	}
	if _, ok := got["key-by-host"]; !ok {
		t.Errorf("base-url host 命中 hosts 的条目应被采纳: %v", got)
	}
	if _, ok := got["key-by-name"]; !ok {
		t.Errorf("条目名是 Cline 的条目应被采纳（自建反代）: %v", got)
	}
	if _, ok := got["key-not-cline"]; ok {
		t.Errorf("不相关条目不应被采纳")
	}
	if _, ok := got["key-disabled"]; ok {
		t.Errorf("disabled 条目不应被采纳")
	}
	if label := got["key-by-host"]; label != "CP #1 · key…host" {
		t.Errorf("label = %q，期望用条目名做前缀", label)
	}
}
