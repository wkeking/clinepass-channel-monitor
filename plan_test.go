package main

import (
	"encoding/json"
	"fmt"
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
	if key := clineAPIKeyFromConfigFile(cfg); key != "key-from-entries" {
		t.Fatalf("key = %q, 期望从 api-key-entries 读到 key-from-entries", key)
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
			write(map[string]any{"id": "usr-test-0001", "displayName": "Test"})
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
	poller.refresh("test-key")
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
	if totals.TotalTokens != 1100 || totals.InputTokens != 1000 || totals.OutputTokens != 100 {
		t.Errorf("官方总量 = %+v", totals)
	}
	if totals.Requests != 1 {
		t.Errorf("官方计费条目 = %d，期望 1", totals.Requests)
	}
	if totals.CostUSD != 1.0 {
		t.Errorf("官方成本 = %v，期望 1.0 USD（微美元换算）", totals.CostUSD)
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
	poller.refresh("test-key")
	quota := poller.snapshot()
	if quota.Available {
		t.Errorf("上游失败时不应标记可用")
	}
	if !strings.Contains(quota.Error, "401") {
		t.Errorf("错误信息应包含状态码，实际 %q", quota.Error)
	}
}
