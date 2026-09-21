package main

import (
	"sync"
	"time"
)

const (
	statsWindowHour = time.Hour
	statsWindowDay  = 24 * time.Hour
	statsWindowWeek = 7 * 24 * time.Hour
)

// store keeps the in-memory view of recorded requests and the counters used for
// self-diagnosis. Every exported method is safe for concurrent use.
type store struct {
	mu   sync.RWMutex
	cfg  config
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
type statsTotals struct {
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

type unmatchedHostSample struct {
	Host     string `json:"host"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Time     string `json:"time"`
}

type pendingChannel struct {
	// requestHash is the identity the response hook was observed for.
	requestHash string
	// routing is what the response hook saw for that request.
	routing   routingState
	identity  identity
	meta      *ChannelMetadata
	createdAt time.Time
	consumed  bool
}

func newStore(cfg config) *store {
	return &store{
		cfg:  cfg,
		ring: make([]*event, cfg.RingSize),
	}
}

func (s *store) reconfigure(cfg config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cfg.RingSize != s.cfg.RingSize {
		s.ring = make([]*event, cfg.RingSize)
		s.head = 0
		s.size = 0
	}
	s.cfg = cfg
}

func (s *store) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = nil
}

func (s *store) config() config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

func (s *store) add(e *event) {
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
func (s *store) events(filter eventFilter) []*event {
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

func (s *store) addChannel(c *pendingChannel) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = append(s.pending, c)
	s.trimPendingLocked()
}

// consumeChannel finds the channel observation matching a usage record and marks it
// consumed so one observation can never be joined twice.
//
// Matching order follows the correlation contract: (session, model) within the join
// window first, then model only. An unconsumed observation is never reused.
func (s *store) consumeChannel(id identity, requestedAt time.Time, latency time.Duration) *pendingChannel {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := s.cfg
	window := cfg.JoinWindow.Or(defaultJoinWindow)

	best := s.pickLocked(func(candidate *pendingChannel) bool {
		return sameSessionAndModel(candidate.identity, id)
	}, requestedAt, latency, window)
	if best == nil {
		best = s.pickLocked(func(candidate *pendingChannel) bool {
			return candidate.identity.Model != "" && candidate.identity.Model == id.Model
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
func (s *store) pickLocked(accept func(*pendingChannel) bool, requestedAt time.Time, latency, window time.Duration) *pendingChannel {
	var best *pendingChannel
	var bestDistance time.Duration
	for _, candidate := range s.pending {
		if candidate.consumed || candidate.meta == nil {
			continue
		}
		if !accept(candidate) {
			continue
		}
		delta := candidate.createdAt.Sub(requestedAt)
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
func (s *store) trimPendingLocked() {
	ttl := s.cfg.OrphanTTL.Or(defaultOrphanTTL)
	now := time.Now()
	kept := s.pending[:0]
	for _, candidate := range s.pending {
		if candidate.consumed {
			continue
		}
		if now.Sub(candidate.createdAt) > ttl {
			s.counters.orphanChannel++
			continue
		}
		kept = append(kept, candidate)
	}
	s.pending = kept
}

func (s *store) recordUnmatchedHost(sample unmatchedHostSample) {
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

func (s *store) snapshotCounters() counters {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.counters
}

// totals returns the lifetime counters plus a windowed subset.
func (s *store) totals() statsTotals {
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

func (s *store) unmatchedHostSamples() []unmatchedHostSample {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]unmatchedHostSample, len(s.unmatchedSamples))
	copy(out, s.unmatchedSamples)
	return out
}

// used returns how many events the ring currently holds.
func (s *store) used() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.size
}

func (s *store) countObservations() int {
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
func (s *store) bump(apply func(*counters)) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	apply(&s.counters)
}

func (s *store) incRequests()       { s.bump(func(c *counters) { c.requests++ }) }
func (s *store) incHostMatched()    { s.bump(func(c *counters) { c.hostMatched++ }) }
func (s *store) incMarkerMissing()  { s.bump(func(c *counters) { c.markerMissing++ }) }
func (s *store) incParseError()     { s.bump(func(c *counters) { c.parseError++ }) }
func (s *store) incChannelMissing() { s.bump(func(c *counters) { c.channelMissing++ }) }
func (s *store) incWriteError()     { s.bump(func(c *counters) { c.writeError++ }) }
