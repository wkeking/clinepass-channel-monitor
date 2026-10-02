package observation

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wkeking/clinepass-channel-monitor/internal/channellog"
)

// The scanner's own fact ring only holds the newest facts and is empty right after a restart,
// which is not enough to answer a window question: the 24-hour view asks about records the
// process may never have seen arrive. This source therefore reads the persisted fact files
// (channel-log-<UTC date>.jsonl, written by internal/channellog) and merges them with whatever
// the live ring still holds.
//
// It is a read-time join, like off_baseline: nothing is rewritten, and a window is re-scored
// against the facts that exist on disk at the moment it is rendered. The files are small
// (roughly 1 KB per request) and the result is cached for a few seconds so a page that fires
// /channel followed by /channel.csv does not parse the store twice.
const (
	// factSourceTTL is how long a loaded fact set is reused. It is short enough that a fresh
	// request shows up on the next page refresh and long enough that one page load does not
	// walk the files more than once.
	factSourceTTL = 5 * time.Second
	// factSourceMax bounds what one load keeps, newest first. The retention window is three
	// days; this is a safety valve, not a limit the deployment is expected to reach.
	factSourceMax = 40000
)

// storeFactSource merges the on-disk fact files with the live scanner ring.
type storeFactSource struct {
	dir  string
	ring FactSource

	mu       sync.Mutex
	cached   []channellog.Fact
	loadedAt time.Time
}

// FactsFromStoreAndScanner builds a fact source over the persisted fact files, merged with the
// scanner's ring when one is published. Either half may be absent: a deployment with the reader
// disabled still serves the historical facts, and a deployment whose store directory is empty
// still serves the ring.
func FactsFromStoreAndScanner(dir string, ring FactSource) FactSource {
	source := &storeFactSource{dir: strings.TrimSpace(dir), ring: ring}
	return FactSourceFunc(source.facts)
}

func (s *storeFactSource) facts() []channellog.Fact {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.loadedAt) < factSourceTTL {
		return s.cached
	}
	facts := loadFactFiles(s.dir)
	if s.ring != nil {
		facts = mergeFacts(facts, s.ring.Facts())
	}
	s.cached = facts
	s.loadedAt = time.Now()
	return facts
}

// loadFactFiles reads every fact file in the directory, newest file first, and never fails
// loudly: an unreadable directory or a torn line only costs coverage, which the page reports as
// unjoined requests rather than as a wrong channel.
func loadFactFiles(dir string) []channellog.Fact {
	if dir == "" {
		return nil
	}
	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		// Only the fact files: channel-<date>.jsonl holds the records themselves, and
		// main.log or a request log must never be parsed here.
		if !strings.HasPrefix(name, "channel-log-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		names = append(names, name)
	}
	// The date is in the name and the naming is fixed-width, so a reverse lexical sort is a
	// reverse chronological sort and the newest facts are kept when the cap bites.
	sortStringsDesc(names)

	var facts []channellog.Fact
	for _, name := range names {
		facts = appendFactsFromFile(facts, filepath.Join(dir, name))
		if len(facts) >= factSourceMax {
			break
		}
	}
	if len(facts) > factSourceMax {
		facts = facts[:factSourceMax]
	}
	return facts
}

func appendFactsFromFile(facts []channellog.Fact, path string) []channellog.Fact {
	handle, errOpen := os.Open(path)
	if errOpen != nil {
		return facts
	}
	defer func() { _ = handle.Close() }()
	scanner := bufio.NewScanner(handle)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var fact channellog.Fact
		if errUnmarshal := json.Unmarshal([]byte(line), &fact); errUnmarshal != nil {
			continue
		}
		facts = append(facts, fact)
		if len(facts) >= factSourceMax {
			break
		}
	}
	return facts
}

// mergeFacts keeps the union of the on-disk facts and the live ring, newest first. A fact the
// ring and the file both hold is one request and must be joined once: the key is the file it
// was parsed from plus its timestamp, which is what identifies a request log.
func mergeFacts(stored, live []channellog.Fact) []channellog.Fact {
	if len(live) == 0 {
		return stored
	}
	if len(stored) == 0 {
		return live
	}
	seen := make(map[string]struct{}, len(stored))
	for _, fact := range stored {
		seen[factKey(fact)] = struct{}{}
	}
	merged := stored
	for _, fact := range live {
		key := factKey(fact)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		merged = append(merged, fact)
	}
	sortFactsNewestFirst(merged)
	if len(merged) > factSourceMax {
		merged = merged[:factSourceMax]
	}
	return merged
}

func factKey(fact channellog.Fact) string {
	return fact.SourceFile + "|" + fact.Time.UTC().Format(time.RFC3339Nano)
}

func sortStringsDesc(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] > values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func sortFactsNewestFirst(facts []channellog.Fact) {
	for i := 1; i < len(facts); i++ {
		for j := i; j > 0 && facts[j].Time.After(facts[j-1].Time); j-- {
			facts[j], facts[j-1] = facts[j-1], facts[j]
		}
	}
}
