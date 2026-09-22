// Package store keeps the recorded requests: a bounded in-memory ring buffer for the
// management pages, the counters that describe this instance, and the daily JSONL files that
// keep the full history.
package store

import (
	"sync"
	"time"

	"github.com/wkeking/clinepass-channel-monitor/internal/config"
	"github.com/wkeking/clinepass-channel-monitor/internal/metadata"
)

// 包内沿用的短名：对外是导出的类型名，对内保持原来的写法。
type (
	store               = Store
	event               = Event
	eventFilter         = Filter
	statsTotals         = Totals
	windowStats         = WindowStats
	channelStat         = ChannelStat
	unmatchedHostSample = UnmatchedHostSample
	pendingChannel      = PendingChannel
)

// The page windows the management API understands.
const (
	StatsWindowHour = time.Hour
	StatsWindowDay  = 24 * time.Hour
	StatsWindowWeek = 7 * 24 * time.Hour
)

// store keeps the in-memory view of recorded requests and the counters used for
// self-diagnosis. Every exported method is safe for concurrent use.
type Store struct {
	mu   sync.RWMutex
	cfg  config.Config
	ring []*event
	head int
	size int

	// pending holds channel observations that still wait for their usage record.
	pending []*pendingChannel
	// unmatchedSamples keeps the most recent "routing marker seen but host not
	// matched" observations so a misconfigured hosts list is visible instead of silent.
	unmatchedSamples []unmatchedHostSample

	counters counters
}

type counters struct {
	requests             int64
	recorded             int64
	hostMatched          int64
	skippedUnmatchedHost int64
	markerMissing        int64
	parseError           int64
	orphanChannel        int64
	channelMissing       int64
	writeError           int64
	joinFailures         int64
}

// statsTotals is the lifetime summary exposed by /health.
type Totals struct {
	Requests        int64 `json:"requests"`
	Recorded        int64 `json:"recorded"`
	HostMatched     int64 `json:"host_matched"`
	SkippedUnmached int64 `json:"skipped_unmatched_host"`
	MarkerMissing   int64 `json:"marker_missing"`
	ParseErrors     int64 `json:"parse_errors"`
	OrphanChannels  int64 `json:"orphan_channel"`
	ChannelMissing  int64 `json:"channel_missing"`
	WriteErrors     int64 `json:"write_error"`
	JoinFailures    int64 `json:"join_failures"`
	Fused           bool  `json:"fused"`
}

type UnmatchedHostSample struct {
	Host     string `json:"host"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Time     string `json:"time"`
}

// PendingChannel is one channel observation waiting for its usage record. Its fields are
// exported because the hooks package fills it in.
type PendingChannel struct {
	// RequestHash is the identity the response hook was observed for.
	RequestHash string
	// Routing is what the response hook saw for that request.
	Routing  RoutingState
	Identity Identity
	Meta     *metadata.ChannelMetadata
	// CreatedAt is when the observation was recorded, used for the orphan window.
	CreatedAt time.Time
	consumed  bool
}

func New(cfg config.Config) *Store {
	return &store{
		cfg:  cfg,
		ring: make([]*event, cfg.RingSize),
	}
}

func (s *Store) Reconfigure(cfg config.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cfg.RingSize != s.cfg.RingSize {
		s.ring = make([]*event, cfg.RingSize)
		s.head = 0
		s.size = 0
	}
	s.cfg = cfg
}

// reset clears the in-memory view: a reconfigured plugin starts with empty counters
// so that /health reflects the current instance instead of a previous configuration.
func (s *Store) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ring = make([]*event, len(s.ring))
	s.head = 0
	s.size = 0
	s.pending = nil
	s.unmatchedSamples = nil
	s.counters = counters{}
}

func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = nil
}

func (s *Store) config() config.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

func (s *Store) Add(e *Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ring[s.head] = e
	s.head = (s.head + 1) % len(s.ring)
	if s.size < len(s.ring) {
		s.size++
	}
	s.counters.recorded++
}

// events returns the stored events newest first, filtered by the request.
func (s *Store) Events(filter Filter) []*Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	var out []*event
	for i := 0; i < s.size; i++ {
		index := (s.head - 1 - i + len(s.ring)) % len(s.ring)
		if index < 0 {
			continue
		}
		e := s.ring[index]
		if e == nil {
			continue
		}
		if filter.Since > 0 && now.Sub(e.Timestamp) > filter.Since {
			continue
		}
		if !filter.matches(e) {
			continue
		}
		out = append(out, e)
		if filter.Limit > 0 && len(out) >= filter.Offset+filter.Limit {
			break
		}
	}
	return out
}

func (s *Store) AddChannel(c *PendingChannel) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = append(s.pending, c)
	s.trimPendingLocked()
}

// ConsumeChannel finds the channel observation matching a usage record and marks it
// consumed so one observation can never be joined twice.
//
// Matching order follows the correlation contract: (session, model) within the join
// window first, then model only. An unconsumed observation is never reused.
func (s *Store) ConsumeChannel(id Identity, requestedAt time.Time, latency time.Duration) *PendingChannel {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := s.cfg
	window := cfg.JoinWindow.Or(config.DefaultJoinWindow)

	best := s.pickLocked(func(candidate *pendingChannel) bool {
		return sameSessionAndModel(candidate.Identity, id)
	}, requestedAt, latency, window)
	if best == nil {
		best = s.pickLocked(func(candidate *pendingChannel) bool {
			return candidate.Identity.Model != "" && candidate.Identity.Model == id.Model
		}, requestedAt, latency, window)
	}
	if best == nil {
		return nil
	}
	best.consumed = true
	s.trimPendingLocked()
	return best
}

// pickLocked returns the closest unconsumed observation accepted by the predicate.
func (s *Store) pickLocked(accept func(*pendingChannel) bool, requestedAt time.Time, latency, window time.Duration) *pendingChannel {
	var best *pendingChannel
	var bestDistance time.Duration
	for _, candidate := range s.pending {
		if candidate.consumed || candidate.Meta == nil {
			continue
		}
		if !accept(candidate) {
			continue
		}
		delta := candidate.CreatedAt.Sub(requestedAt)
		if delta < -window || delta > latency+window {
			continue
		}
		distance := delta
		if distance < 0 {
			distance = -distance
		}
		if best == nil || distance < bestDistance {
			best = candidate
			bestDistance = distance
		}
	}
	return best
}

func sameSessionAndModel(a, b identity) bool {
	if a.Model == "" || a.Model != b.Model {
		return false
	}
	if a.SessionID == "" || b.SessionID == "" {
		return false
	}
	return a.SessionID == b.SessionID
}

// trimPendingLocked drops consumed entries and expired orphans. Orphans are counted
// separately because they are the signal that channel metadata is not being consumed.
func (s *Store) trimPendingLocked() {
	ttl := s.cfg.OrphanTTL.Or(config.DefaultOrphanTTL)
	now := time.Now()
	kept := s.pending[:0]
	for _, candidate := range s.pending {
		if candidate.consumed {
			continue
		}
		if now.Sub(candidate.CreatedAt) > ttl {
			s.counters.orphanChannel++
			continue
		}
		kept = append(kept, candidate)
	}
	s.pending = kept
}

func (s *Store) RecordUnmatchedHost(sample UnmatchedHostSample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counters.skippedUnmatchedHost++
	if s.cfg.UnmatchedHostSmpl <= 0 {
		return
	}
	if s.unmatchedSamples == nil {
		s.unmatchedSamples = make([]unmatchedHostSample, 0, s.cfg.UnmatchedHostSmpl)
	}
	s.unmatchedSamples = append(s.unmatchedSamples, sample)
	if len(s.unmatchedSamples) > s.cfg.UnmatchedHostSmpl {
		s.unmatchedSamples = s.unmatchedSamples[len(s.unmatchedSamples)-s.cfg.UnmatchedHostSmpl:]
	}
}

func (s *Store) snapshotCounters() counters {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.counters
}

// totals returns the lifetime counters plus a windowed subset.
func (s *Store) Totals() Totals {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return statsTotals{
		Requests:        s.counters.requests,
		Recorded:        s.counters.recorded,
		HostMatched:     s.counters.hostMatched,
		SkippedUnmached: s.counters.skippedUnmatchedHost,
		MarkerMissing:   s.counters.markerMissing,
		ParseErrors:     s.counters.parseError,
		OrphanChannels:  s.counters.orphanChannel,
		ChannelMissing:  s.counters.channelMissing,
		WriteErrors:     s.counters.writeError,
		JoinFailures:    s.counters.joinFailures,
	}
}

func (s *Store) UnmatchedHostSamples() []UnmatchedHostSample {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]unmatchedHostSample, len(s.unmatchedSamples))
	copy(out, s.unmatchedSamples)
	return out
}

// used returns how many events the ring currently holds.
func (s *Store) Used() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.size
}

func (s *Store) CountObservations() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	total := 0
	for _, candidate := range s.pending {
		if !candidate.consumed {
			total++
		}
	}
	return total
}

// bump applies one counter increment under the store lock.
func (s *Store) bump(apply func(*counters)) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	apply(&s.counters)
}

func (s *Store) IncRequests()       { s.bump(func(c *counters) { c.requests++ }) }
func (s *Store) IncHostMatched()    { s.bump(func(c *counters) { c.hostMatched++ }) }
func (s *Store) IncMarkerMissing()  { s.bump(func(c *counters) { c.markerMissing++ }) }
func (s *Store) IncParseError()     { s.bump(func(c *counters) { c.parseError++ }) }
func (s *Store) IncChannelMissing() { s.bump(func(c *counters) { c.channelMissing++ }) }
func (s *Store) IncWriteError()     { s.bump(func(c *counters) { c.writeError++ }) }
