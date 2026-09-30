package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wkeking/clinepass-channel-monitor/internal/config"
	"github.com/wkeking/clinepass-channel-monitor/internal/plan"
	"github.com/wkeking/clinepass-channel-monitor/internal/state"
)

// syntheticPlanKey is visibly synthetic: scripts/check-secrets.sh rejects anything that
// looks like a real credential, and this must never look like one.
const syntheticPlanKey = "cline-test-plan-key-0001"

// fakeClineAPI serves the Cline dashboard endpoints the plan poller calls. It is a trimmed
// copy of the plan package's own fixture: the management package has to bind /health to a
// live poller, and the plan package cannot export its test helper without widening its API.
func fakeClineAPI(t *testing.T, now time.Time) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		write := func(data any) {
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
		}
		switch r.URL.Path {
		case "/users/me":
			write(map[string]any{
				"id":          "usr-fixture0001",
				"displayName": "Fixture",
				"createdAt":   now.AddDate(0, 0, -30).Format(time.RFC3339Nano),
			})
		case "/users/me/plan":
			write(map[string]any{
				"plan": map[string]any{
					"displayName":       "Cline Pass (Monthly)",
					"pricePerSeatCents": 999,
					"description":       "Fixture plan description.",
					"interval":          "Monthly",
					"type":              "individual",
					"isActive":          true,
					"features":          map[string]any{"included": []string{"Benefit one", "Benefit two"}},
				},
				"currentPeriodStart": now.AddDate(0, 0, -12).Format(time.RFC3339),
				"currentPeriodEnd":   now.AddDate(0, 0, 18).Format(time.RFC3339),
			})
		case "/users/me/plan/usage-limits":
			write(map[string]any{"limits": []map[string]any{
				{"type": "five_hour", "percentUsed": 16, "resetsAt": now.Add(30 * time.Minute).Format(time.RFC3339Nano)},
				{"type": "weekly", "percentUsed": 59, "resetsAt": now.Add(80 * time.Hour).Format(time.RFC3339Nano)},
			}})
		case "/users/usr-fixture0001/usages":
			write(map[string]any{"items": []map[string]any{
				{"id": "usg-fixture0001", "createdAt": now.Add(-10 * time.Minute).Format(time.RFC3339Nano),
					"costUsd": 1000000, "creditsUsed": 0, "operation": "chat_completion",
					"promptTokens": 1000, "completionTokens": 100, "totalTokens": 1100, "cachedTokens": 800,
					"metadata": map[string]any{"raw_model": "deepseek/deepseek-v4.1-flash", "is_stream": true, "is_byok": false}},
				{"id": "usg-fixture0002", "createdAt": now.Add(-90 * time.Minute).Format(time.RFC3339Nano),
					"costUsd": 500000, "creditsUsed": 0, "operation": "chat_completion",
					"promptTokens": 2000, "completionTokens": 200, "totalTokens": 2200, "cachedTokens": 900,
					"metadata": map[string]any{"raw_model": "z-ai/glm-5.3", "is_stream": true, "is_byok": false}},
			}, "nextToken": "", "total": 2})
		case "/users/usr-fixture0001/usages/daily":
			to := time.Now().UTC()
			write(map[string]any{"items": []map[string]any{
				{"date": to.Format("2006-01-02"), "operation": "chat_completion", "costUsd": 1000000,
					"promptTokens": 1000, "completionTokens": 100},
			}})
		case "/users/usr-fixture0001/balance":
			write(map[string]any{"balance": 499865})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// TestHealthBindsToTheLivePlanSnapshot is the test the recorded fixtures cannot give: it
// starts a real poller against a fake Cline API, publishes it as the runtime state and
// reads /health back. Deleting the snapshot hand-off in buildHealthResponse, or dropping a
// field from the payload, fails here instead of on the deployed page.
func TestHealthBindsToTheLivePlanSnapshot(t *testing.T) {
	server := fakeClineAPI(t, time.Now().UTC())
	cfg := config.Default()
	cfg.PlanAPIKey = syntheticPlanKey
	cfg.PlanBaseURL = server.URL
	// Point credential discovery at an empty directory so the test never reads the host's
	// own CPA configuration.
	cfg.PlanConfigPath = filepath.Join(t.TempDir(), "missing.yaml")
	cfg.PlanRefresh = config.Duration{Value: time.Minute, Set: true}

	state.SetConfig(cfg)
	poller := plan.Start(cfg)
	state.SetPlan(poller)
	t.Cleanup(func() {
		poller.Stop()
		state.SetPlan(nil)
	})

	deadline := time.Now().Add(15 * time.Second)
	for {
		snapshot := poller.Snapshot()
		if len(snapshot.Accounts) > 0 && snapshot.Accounts[0].Available {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the poller never produced a snapshot: %+v", poller.Snapshot())
		}
		time.Sleep(25 * time.Millisecond)
	}

	resp := buildHealthResponse()
	if !resp.Plan.Available || resp.Plan.PlanName == "" || len(resp.Plan.Limits) == 0 {
		t.Fatalf("plan = %+v, want an available plan with its limits", resp.Plan)
	}
	if len(resp.Plan.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(resp.Plan.Accounts))
	}
	account := resp.Plan.Accounts[0]
	if len(account.Windows["1h"].Series.Requests) == 0 || account.Windows["24h"].Requests == 0 {
		t.Errorf("the account must carry the official windows the overview plots: %+v", account.Windows)
	}
	if week, ok := account.Windows["7d"]; !ok || week.Detail {
		t.Errorf("the 7d window must come from the daily totals and report detail=false: %+v", week)
	}
	// The plan detail and the per-model split both come from responses the poller already
	// fetches; they must reach the page without an extra upstream call.
	if resp.Plan.PlanDescription == "" || len(resp.Plan.PlanBenefits) != 2 || resp.Plan.PlanPeriodEnd == "" {
		t.Errorf("the plan detail must reach the page: %+v", resp.Plan)
	}
	models := account.Windows["24h"].Models
	if len(models) != 2 {
		t.Fatalf("the 24h window must carry its per-model split: %+v", models)
	}
	if models[0].Model != "z-ai/glm-5.3" {
		t.Errorf("per-model rows come from metadata.raw_model, largest first: %+v", models)
	}
	if account.Windows["24h"].StreamRequests != 2 {
		t.Errorf("the window must count its streamed records: %+v", account.Windows["24h"])
	}
	if account.Tokens.TotalTokens == 0 || account.Tokens.BalanceUSD == 0 {
		t.Errorf("the official totals must reach the page: %+v", account.Tokens)
	}
	if len(resp.PlanAccounts) != 1 || resp.PlanAccounts[0].Items == 0 {
		t.Errorf("plan_accounts = %+v, want the credential diagnostics", resp.PlanAccounts)
	}

	// §6: a credential may only ever appear masked, and never in the payload the page gets.
	raw, errMarshal := json.Marshal(resp)
	if errMarshal != nil {
		t.Fatalf("health payload must marshal: %v", errMarshal)
	}
	payload := string(raw)
	if strings.Contains(payload, syntheticPlanKey) {
		t.Error("the health payload must never carry the credential itself")
	}
	if !strings.Contains(payload, "…") || !strings.Contains(resp.PlanAccounts[0].Label, "…") {
		t.Errorf("the credential must still be identifiable by its masked label: %q", resp.PlanAccounts[0].Label)
	}
	if resp.PlanAccounts[0].Source != "plugin-config" {
		t.Errorf("credential source = %q, want plugin-config", resp.PlanAccounts[0].Source)
	}
}
