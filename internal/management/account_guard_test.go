package management

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wkeking/clinepass-channel-monitor/internal/guard"
	"github.com/wkeking/clinepass-channel-monitor/internal/state"
)

// TestHealthCarriesTheAccountGuard pins the contract between the plugin and the page: what the
// guard decided is served on /health, and the page has the block that renders it. The snapshot
// carries account names, channels, counters and timestamps - never a credential value.
func TestHealthCarriesTheAccountGuard(t *testing.T) {
	loadTestConfig([]byte("enabled: true\n"))
	seenAt := time.Date(2026, 10, 5, 4, 0, 0, 0, time.UTC)
	retryAt := seenAt.Add(30 * time.Minute)
	state.SetAccountGuard(&guard.Status{
		Enabled:   true,
		DryRun:    true,
		Judging:   true,
		Path:      "/CLIProxyAPI/config.yaml",
		Entries:   3,
		Threshold: 3,
		Baseline:  "deepseek",
		Accounts: []guard.StatusAccount{{
			Name: "Cline1", ProviderKey: "openai-compatible-cline1",
			Streak: 2, LastChannel: "fireworks", LastSampleAt: &seenAt,
			NextRetryAt: &retryAt, DisableCount: 1,
		}},
		Recent: []guard.Decision{{
			Time: seenAt, Name: "Cline1", ProviderKey: "openai-compatible-cline1",
			Action: "disable_dry_run", Streak: 3, Reason: "Cline1 连续 3 次没落在 deepseek",
		}},
	})
	defer state.SetAccountGuard(nil)

	req := pluginAPIRequest(BasePath + "/health")
	resp := route(&req)
	if resp.StatusCode != 200 {
		t.Fatalf("health status = %d, want 200", resp.StatusCode)
	}
	var decoded map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(resp.Body, &decoded); errUnmarshal != nil {
		t.Fatalf("health payload must be an object: %v", errUnmarshal)
	}
	raw, ok := decoded["account_guard"]
	if !ok {
		t.Fatal("health payload is missing account_guard")
	}
	var status guard.Status
	if errUnmarshal := json.Unmarshal(raw, &status); errUnmarshal != nil {
		t.Fatalf("account_guard: %v", errUnmarshal)
	}
	if !status.Enabled || !status.DryRun || status.Threshold != 3 || status.Baseline != "deepseek" {
		t.Errorf("status = %#v, want the rule the guard is applying", status)
	}
	if len(status.Accounts) != 1 || status.Accounts[0].Streak != 2 || status.Accounts[0].LastChannel != "fireworks" {
		t.Errorf("accounts = %#v", status.Accounts)
	}
	if status.Accounts[0].NextRetryAt == nil || !status.Accounts[0].NextRetryAt.Equal(retryAt) {
		t.Errorf("next_retry_at = %v, want %v", status.Accounts[0].NextRetryAt, retryAt)
	}
	if len(status.Recent) != 1 || status.Recent[0].Action != "disable_dry_run" {
		t.Errorf("recent = %#v", status.Recent)
	}
}

// TestAccountGuardBlockIsOnThePage pins the rendering contract: the block exists, it is hidden
// until the snapshot arrives, and it says out loud that a dry run does not touch the file.
func TestAccountGuardBlockIsOnThePage(t *testing.T) {
	page := string(indexHTML(nil))
	for _, needle := range []string{
		`id="guard-block"`, `id="guard-accounts"`, `id="guard-flag"`, `id="guard-recent"`,
		"function renderAccountGuard(", "function guardStateText(", "function guardStateClass(",
		"renderAccountGuard(health.account_guard", "已被守卫关闭", "写入待确认", "未判定",
	} {
		if !strings.Contains(page, needle) {
			t.Errorf("page is missing %q", needle)
		}
	}
	body := jsFunctionBody(t, page, "renderAccountGuard")
	for _, needle := range []string{"试运行：只记录，不改 CPA 配置", "还没有真实渠道数据", "guard-block"} {
		if !strings.Contains(body, needle) {
			t.Errorf("renderAccountGuard must state %q, got:\n%s", needle, body)
		}
	}
	if !strings.Contains(page, `id="guard-block" class="hidden"`) {
		t.Error("the guard block must start hidden and only appear with a snapshot")
	}
}
