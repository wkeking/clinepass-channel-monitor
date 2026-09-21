package main

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeUsageAPI serves a Cline-like /users/{id}/usages endpoint over items (newest first).
// It mirrors the two upstream behaviours the collector depends on: cursor pagination and
// a 200-item page ceiling.
func fakeUsageAPI(t *testing.T, items *[]officialUsageRawItem) (*httptest.Server, *int) {
	t.Helper()
	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		if !strings.HasPrefix(r.URL.Path, "/users/usr-test/usages") {
			http.NotFound(w, r)
			return
		}
		limit := 200
		if raw := r.URL.Query().Get("limit"); raw != "" {
			if parsed, errAtoi := strconv.Atoi(raw); errAtoi == nil && parsed > 0 && parsed < limit {
				limit = parsed
			}
		}
		start := 0
		if raw := r.URL.Query().Get("cursor"); raw != "" {
			if parsed, errAtoi := strconv.Atoi(raw); errAtoi == nil {
				start = parsed
			}
		}
		list := *items
		end := start + limit
		if end > len(list) {
			end = len(list)
		}
		next := ""
		if end < len(list) {
			next = strconv.Itoa(end)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": map[string]any{
				"items":     list[start:end],
				"nextToken": next,
				"total":     len(list),
			},
		})
	}))
	t.Cleanup(server.Close)
	return server, &pages
}

func usageItem(id string, at time.Time, prompt, completion, cached, costMicro int64) officialUsageRawItem {
	return officialUsageRawItem{
		ID:               id,
		CreatedAt:        at.UTC().Format(time.RFC3339Nano),
		Operation:        "chat_completion",
		AIModelTypeName:  "cline-pass",
		AIModelName:      "cline-pass/deepseek-v4.1-flash",
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      prompt + completion,
		CachedTokens:     cached,
		CostUnits:        costMicro,
	}
}

func usageClient(t *testing.T, server *httptest.Server) *planClient {
	t.Helper()
	return newPlanClient(server.URL, "test-key")
}

// newTestCollector returns a collector without the politeness delay.
func newTestCollector() *officialUsageCollector {
	collector := newOfficialUsageCollector()
	collector.pageDelay = 0
	return collector
}

// TestOfficialUsageWindowsAggregateOfficialRecords checks the page windows, the micro-USD
// conversion and that the sparkline buckets add up to the window totals.
func TestOfficialUsageWindowsAggregateOfficialRecords(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	items := []officialUsageRawItem{
		usageItem("usg-1", now.Add(-10*time.Minute), 1000, 100, 800, 1_000_000),
		usageItem("usg-2", now.Add(-20*time.Minute), 2000, 200, 1500, 2_000_000),
		usageItem("usg-3", now.Add(-5*time.Hour), 400, 40, 100, 500_000),
		usageItem("usg-4", now.Add(-40*time.Hour), 700, 70, 200, 250_000),
		{ID: "usg-broken", CreatedAt: "not-a-timestamp", PromptTokens: 999},
	}
	server, pages := fakeUsageAPI(t, &items)
	collector := newTestCollector()
	collector.fetch(usageClient(t, server), "usr-test", planDefaultUsageRefresh, now)

	if *pages != 1 {
		t.Fatalf("保留窗口内的条目应一次拉完，实际请求 %d 页", *pages)
	}
	state := collector.state(true)
	if state.Items != 3 {
		t.Fatalf("采集条目数 = %d，期望 3（超出保留窗口与时间戳非法的条目应被跳过）", state.Items)
	}
	if state.Error != "" || state.Failures != 0 {
		t.Errorf("不应有错误: %+v", state)
	}

	windows := collector.aggregate(now, now.AddDate(0, 0, -30))
	if _, exists := windows["7d"]; exists {
		t.Errorf("官方窗口只覆盖 1h/24h，不应出现 7d")
	}
	hour := windows["1h"]
	if hour.Requests != 2 || hour.InputTokens != 3000 || hour.OutputTokens != 300 || hour.TotalTokens != 3300 || hour.CachedTokens != 2300 {
		t.Errorf("1h 窗口 = %+v，期望 requests=2 input=3000 output=300 cached=2300", hour)
	}
	if math.Abs(hour.CostUSD-3.0) > 1e-9 {
		t.Errorf("1h 成本 = %v，期望 3.0 USD（上游以微美元计价）", hour.CostUSD)
	}
	if math.Abs(hour.CacheRatio-2300.0/3000.0) > 1e-9 {
		t.Errorf("1h 缓存命中率 = %v，期望 cached/prompt", hour.CacheRatio)
	}
	if !hour.Covered {
		t.Errorf("最早条目已在 1h 窗口之外，1h 应标记为已覆盖")
	}

	day := windows["24h"]
	if day.Requests != 3 || day.InputTokens != 3400 || day.TotalTokens != 3740 {
		t.Errorf("24h 窗口 = %+v", day)
	}
	if day.Covered {
		t.Errorf("保留的记录只到 5 小时前（40 小时前的条目已超出保留窗口），24h 不应标记 covered")
	}
	if len(day.Series.Tokens) != 24 || len(day.Series.Requests) != 24 || len(day.Series.Cached) != 24 {
		t.Fatalf("24h 折线桶数 = %d，期望 24", len(day.Series.Tokens))
	}
	var seriesTokens, seriesRequests, seriesCached int64
	for i := range day.Series.Tokens {
		seriesTokens += day.Series.Tokens[i]
		seriesRequests += day.Series.Requests[i]
		seriesCached += day.Series.Cached[i]
	}
	if seriesTokens != day.TotalTokens || seriesRequests != day.Requests || seriesCached != day.CachedTokens {
		t.Errorf("折线桶合计 (%d/%d/%d) 与窗口总计 (%d/%d/%d) 不一致",
			seriesRequests, seriesTokens, seriesCached, day.Requests, day.TotalTokens, day.CachedTokens)
	}
}

// TestOfficialUsageWindowCoverage checks the coverage flag that tells the page whether an
// official window is a total or only a lower bound.
func TestOfficialUsageWindowCoverage(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	items := []officialUsageRawItem{
		usageItem("usg-1", now.Add(-30*time.Minute), 100, 10, 50, 100_000),
		usageItem("usg-2", now.Add(-90*time.Minute), 100, 10, 50, 100_000),
	}
	server, _ := fakeUsageAPI(t, &items)
	collector := newTestCollector()
	collector.fetch(usageClient(t, server), "usr-test", planDefaultUsageRefresh, now)

	windows := collector.aggregate(now, now.AddDate(0, 0, -30))
	if !windows["1h"].Covered {
		t.Errorf("保留的记录已早于 1h 窗口起点，1h 应标记 covered")
	}
	if windows["24h"].Covered {
		t.Errorf("保留的记录只到 90 分钟前，24h 不应标记 covered")
	}
	// An account younger than the window cannot have older records, so the window is
	// complete even though the retained records stop early.
	if windows := collector.aggregate(now, now.Add(-10*time.Minute)); !windows["24h"].Covered {
		t.Errorf("账号创建时间在 24h 窗口内时，24h 应标记 covered")
	}
}

// TestOfficialUsageIncrementalFetchStopsAtKnownRecords checks that a second round only
// pages the new records and does not double count them.
func TestOfficialUsageIncrementalFetchStopsAtKnownRecords(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	items := []officialUsageRawItem{
		usageItem("usg-1", now.Add(-30*time.Minute), 100, 10, 50, 100_000),
		usageItem("usg-2", now.Add(-40*time.Minute), 200, 20, 60, 100_000),
	}
	server, pages := fakeUsageAPI(t, &items)
	collector := newTestCollector()
	client := usageClient(t, server)
	collector.fetch(client, "usr-test", planDefaultUsageRefresh, now)

	items = append([]officialUsageRawItem{
		usageItem("usg-3", now.Add(time.Minute), 300, 30, 70, 100_000),
	}, items...)
	collector.fetch(client, "usr-test", planDefaultUsageRefresh, now.Add(2*time.Minute))

	if *pages != 2 {
		t.Fatalf("增量刷新应只拉取新条目所在的页，累计请求 %d 页", *pages)
	}
	state := collector.state(true)
	if state.Items != 3 {
		t.Fatalf("去重后条目数 = %d，期望 3", state.Items)
	}
	window := collector.aggregate(now.Add(2*time.Minute), time.Time{})["1h"]
	if window.Requests != 3 || window.TotalTokens != 660 {
		t.Errorf("增量后的 1h 窗口 = %+v，期望 requests=3 total=660（不应重复计数）", window)
	}
}

// TestOfficialUsageKeepsSnapshotOnUpstreamError checks the fail-open path: an upstream
// failure keeps the previous records and only reports the error.
func TestOfficialUsageKeepsSnapshotOnUpstreamError(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	items := []officialUsageRawItem{usageItem("usg-1", now.Add(-5*time.Minute), 100, 10, 50, 100_000)}
	server, _ := fakeUsageAPI(t, &items)
	collector := newTestCollector()
	collector.fetch(usageClient(t, server), "usr-test", planDefaultUsageRefresh, now)
	before := collector.state(true)

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer broken.Close()
	collector.fetch(usageClient(t, broken), "usr-test", planDefaultUsageRefresh, now.Add(time.Minute))
	after := collector.state(true)

	if after.Items != before.Items || after.FetchedAt != before.FetchedAt {
		t.Errorf("上游失败后不应丢弃已有快照: before=%+v after=%+v", before, after)
	}
	if !strings.Contains(after.Error, "500") {
		t.Errorf("上游失败应记录状态码，实际 %q", after.Error)
	}
	window := collector.aggregate(now.Add(time.Minute), time.Time{})["1h"]
	if window.Requests != 1 {
		t.Errorf("上游失败后仍应服务上一次的快照，实际 requests=%d", window.Requests)
	}
}

// TestOfficialUsageBacksOffAfterRateLimit checks that a rate limited upstream is not
// hammered on every cycle.
func TestOfficialUsageBacksOffAfterRateLimit(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	attempts := 0
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer limited.Close()
	collector := newTestCollector()
	client := usageClient(t, limited)

	collector.fetch(client, "usr-test", 5*time.Minute, now)
	state := collector.state(true)
	if !strings.Contains(state.Error, "429") || state.Failures != 1 {
		t.Fatalf("限流后应记录失败次数与状态码: %+v", state)
	}
	if collector.due(now.Add(time.Minute), 5*time.Minute) {
		t.Errorf("退避期内不应再次请求上游")
	}
	if !collector.due(now.Add(5*time.Minute), 5*time.Minute) {
		t.Errorf("退避结束后应允许重试")
	}
	collector.fetch(client, "usr-test", 5*time.Minute, now.Add(5*time.Minute))
	if state := collector.state(true); state.Failures != 2 {
		t.Errorf("连续失败应累加，实际 %+v", state)
	}
	if attempts != 2 {
		t.Errorf("上游被调用 %d 次，期望 2", attempts)
	}
}

// TestOfficialUsageRecoversAfterRateLimit checks that a successful round clears the
// backoff and the error.
func TestOfficialUsageRecoversAfterRateLimit(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	items := []officialUsageRawItem{usageItem("usg-1", now.Add(-5*time.Minute), 100, 10, 50, 100_000)}
	server, _ := fakeUsageAPI(t, &items)
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer limited.Close()

	collector := newTestCollector()
	collector.fetch(usageClient(t, limited), "usr-test", 5*time.Minute, now)
	collector.fetch(usageClient(t, server), "usr-test", 5*time.Minute, now.Add(10*time.Minute))
	state := collector.state(true)
	if state.Error != "" || state.Failures != 0 || state.RetryAt != "" {
		t.Errorf("成功一轮后应清除退避状态: %+v", state)
	}
	if state.Items != 1 || state.FetchedAt == "" {
		t.Errorf("成功一轮后应保存记录: %+v", state)
	}
}

// TestOfficialUsageMarksTruncatedWalk checks that a walk cut short by the item cap is
// reported instead of being presented as a complete window.
func TestOfficialUsageMarksTruncatedWalk(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	items := []officialUsageRawItem{
		usageItem("usg-1", now.Add(-1*time.Minute), 100, 10, 50, 100_000),
		usageItem("usg-2", now.Add(-2*time.Minute), 100, 10, 50, 100_000),
		usageItem("usg-3", now.Add(-3*time.Minute), 100, 10, 50, 100_000),
	}
	server, _ := fakeUsageAPI(t, &items)

	capped := newTestCollector()
	capped.maxItems = 2
	capped.fetch(usageClient(t, server), "usr-test", planDefaultUsageRefresh, now)
	if !capped.state(true).Truncated {
		t.Errorf("达到条目上限时应标记 truncated")
	}
	if capped.state(true).Items != 2 {
		t.Errorf("条目上限应生效，实际 %d", capped.state(true).Items)
	}

	full := newTestCollector()
	full.maxPages = 1
	full.fetch(usageClient(t, server), "usr-test", planDefaultUsageRefresh, now)
	if full.state(true).Items != 3 {
		t.Errorf("一页足以放下全部条目时应全部收录，实际 %+v", full.state(true))
	}
	if full.state(true).Truncated {
		t.Errorf("走到了记录末尾就不应标记 truncated: %+v", full.state(true))
	}
}

// TestOfficialUsagePrunesRetentionWindow checks that records leaving the retention window
// are dropped, and that a repeat fetch of the same records adds nothing.
func TestOfficialUsagePrunesRetentionWindow(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	items := []officialUsageRawItem{
		usageItem("usg-1", now.Add(-1*time.Hour), 100, 10, 50, 100_000),
		usageItem("usg-2", now.Add(-25*time.Hour), 200, 20, 60, 100_000),
	}
	server, pages := fakeUsageAPI(t, &items)
	collector := newTestCollector()
	client := usageClient(t, server)
	collector.fetch(client, "usr-test", planDefaultUsageRefresh, now)
	if collector.state(true).Items != 2 {
		t.Fatalf("首次抓取应保留 2 条，实际 %d", collector.state(true).Items)
	}

	// Four hours later the 25-hour-old record has left the 26-hour retention window.
	collector.fetch(client, "usr-test", planDefaultUsageRefresh, now.Add(4*time.Hour))
	state := collector.state(true)
	if state.Items != 1 {
		t.Errorf("超出保留窗口的条目应被清理，实际保留 %d 条", state.Items)
	}
	if *pages != 2 {
		t.Errorf("第二次刷新不应重复拉取历史，累计请求 %d 页", *pages)
	}
	window := collector.aggregate(now.Add(4*time.Hour), time.Time{})["24h"]
	if window.Requests != 1 {
		t.Errorf("清理后的 24h 窗口 = %+v，期望 requests=1", window)
	}
}

// TestOfficialDailyWindowGroupsByNaturalDay checks the 7-day window built from the daily
// totals: rows are bucketed by UTC day, rows outside the window are ignored, and the
// window reports Detail=false because the daily totals carry no request or cache data.
func TestOfficialDailyWindowGroupsByNaturalDay(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	day := func(offset int) string { return now.AddDate(0, 0, offset).Format("2006-01-02") }
	rows := []dailyUsageItem{
		{Date: day(0), CostUnits: 1_000_000, PromptTokens: 1000, CompletionTokens: 100},
		{Date: day(-1), CostUnits: 500_000, PromptTokens: 2000, CompletionTokens: 200},
		{Date: day(-6), CostUnits: 250_000, PromptTokens: 400, CompletionTokens: 40},
		// 窗口之外：7 天窗口是今天-6 ~ 今天
		{Date: day(-7), CostUnits: 99_000_000, PromptTokens: 999_999, CompletionTokens: 9},
		{Date: "bad-date", CostUnits: 1, PromptTokens: 1},
	}
	window := officialDailyWindow(rows, now, now.AddDate(0, 0, -30))

	if window.Detail {
		t.Errorf("按日汇总窗口的 detail 应为 false")
	}
	if window.Window != "7d" || len(window.Series.Tokens) != 7 {
		t.Fatalf("7d 窗口结构不对: %+v", window)
	}
	if window.TotalTokens != 3740 || window.InputTokens != 3400 || window.OutputTokens != 340 {
		t.Errorf("7d 汇总 = %+v，期望 input=3400 output=340 total=3740（不含窗口外与非法日期）", window)
	}
	if math.Abs(window.CostUSD-1.75) > 1e-9 {
		t.Errorf("7d 成本 = %v，期望 1.75 USD（微美元换算）", window.CostUSD)
	}
	if !window.Covered {
		t.Errorf("账号创建于窗口之前，covered 应为 true")
	}
	if today := window.Series.Tokens[6]; today != 1100 {
		t.Errorf("今天所在桶 = %d，期望 1100", today)
	}
	if yesterday := window.Series.Tokens[5]; yesterday != 2200 {
		t.Errorf("昨天所在桶 = %d，期望 2200", yesterday)
	}
	if older := officialDailyWindow(rows, now, time.Time{}); older.Covered {
		t.Errorf("不知道账号创建时间时不应声称已覆盖")
	}
}
