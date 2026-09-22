package store

import (
	"testing"
	"time"

	"github.com/wkeking/clinepass-channel-monitor/internal/config"
	"github.com/wkeking/clinepass-channel-monitor/internal/metadata"
)

// A streaming response repeats the same channel evidence on every frame it tags, so the
// store must keep one observation per request: the repeats would otherwise pile up until
// the orphan TTL and be reported as orphans.
func TestAddChannelKeepsOneObservationPerRequest(t *testing.T) {
	st := New(config.Default())
	now := time.Now()
	const model = "cline-pass/glm-5.3-flash"
	for i := 0; i < 20; i++ {
		st.AddChannel(&PendingChannel{
			RequestHash: "hash-1",
			Routing:     RoutingConfirmed,
			Identity:    Identity{RequestHash: "hash-1", Model: model},
			Meta:        &metadata.ChannelMetadata{FinalProvider: "Relace"},
			CreatedAt:   now,
		})
	}
	if got := st.CountObservations(); got != 1 {
		t.Fatalf("pending observations = %d, want 1 after 20 frames of one request", got)
	}

	st.AddChannel(&PendingChannel{
		RequestHash: "hash-2",
		Routing:     RoutingConfirmed,
		Identity:    Identity{RequestHash: "hash-2", Model: "cline-pass/deepseek-v4.1-flash"},
		Meta:        &metadata.ChannelMetadata{FinalProvider: "deepseek"},
		CreatedAt:   now,
	})
	if got := st.CountObservations(); got != 2 {
		t.Fatalf("pending observations = %d, want 2 (one per request)", got)
	}

	joined := st.ConsumeChannel(Identity{Model: model}, now, 0)
	if joined == nil || joined.Meta == nil {
		t.Fatalf("consume returned %+v, want the merged observation of %s", joined, model)
	}
	if joined.Meta.FinalProvider != "Relace" {
		t.Errorf("joined final_provider = %q, want Relace", joined.Meta.FinalProvider)
	}
	if joined.RequestHash != "hash-1" {
		t.Errorf("joined request hash = %q, want hash-1", joined.RequestHash)
	}
	if got := st.CountObservations(); got != 1 {
		t.Fatalf("pending observations after the join = %d, want 1", got)
	}
	if second := st.ConsumeChannel(Identity{Model: model}, now, 0); second != nil {
		t.Errorf("a consumed observation must never be joined twice, got %+v", second)
	}
}

// mergePendingChannel must not lose the strongest routing state or a known identity.
func TestAddChannelKeepsTheStrongestEvidence(t *testing.T) {
	st := New(config.Default())
	now := time.Now()
	st.AddChannel(&PendingChannel{RequestHash: "h", Routing: RoutingConfirmed, CreatedAt: now})
	st.AddChannel(&PendingChannel{
		RequestHash: "h",
		Routing:     RoutingAbsent,
		Identity:    Identity{RequestHash: "h", Model: "m", SessionID: "s"},
		Meta:        &metadata.ChannelMetadata{FinalProvider: "baseten"},
		CreatedAt:   now,
	})
	pending := st.pending
	if len(pending) != 1 {
		t.Fatalf("pending entries = %d, want 1", len(pending))
	}
	if pending[0].Routing != RoutingConfirmed {
		t.Errorf("routing = %v, want RoutingConfirmed", pending[0].Routing)
	}
	if pending[0].Meta == nil || pending[0].Meta.FinalProvider != "baseten" {
		t.Errorf("meta = %+v, want the newest observation", pending[0].Meta)
	}
	if pending[0].Identity.Model != "m" || pending[0].Identity.SessionID != "s" {
		t.Errorf("identity = %+v, want the fields from the second observation", pending[0].Identity)
	}
}
