package channellog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"time"
)

// The CPA request log is a plain text file of named sections, in this order:
//
//	=== REQUEST INFO ===       Version / URL / Method / Timestamp of the request as it arrived
//	=== HEADERS ===            the downstream request headers, verbatim
//	=== REQUEST BODY ===       the client's own body: a plaintext prompt, megabytes for a long context
//	=== API REQUEST n ===      one outbound upstream request (there may be several)
//	=== API ERROR RESPONSE === present when an attempt failed
//	=== API RESPONSE n ===     the upstream response, as the SSE frames CPA received
//	=== RESPONSE ===           what CPA sent downstream
//
// The channel block only exists inside the upstream response frames, at
// choices[].delta.provider_metadata.gateway.routing, and only when the upstream answered at
// all: a request whose attempt failed carries no routing block, which is why "no channel" has
// to be an answer this parser can produce rather than an error.
//
// Two rules drive the implementation, and both are load bearing:
//
//   - A frame is decoded, never text-matched. The literal string "provider_metadata" occurs
//     inside prompts and inside the request body, so a byte search for it invents a channel
//     for a request that never had one.
//   - Nothing is guessed. A file with no response section, or one whose frames carry no
//     provider_metadata, yields a fact whose channel fields are empty.
const (
	// readBuffer is the size of the streaming buffer, and therefore the hard bound on how much
	// of any one line is ever held. A line that does not fit it is skipped, not accumulated:
	// the lines measured in megabytes in these files are request bodies, while the frames that
	// carry the routing block are kilobytes.
	readBuffer = 1 << 20

	// logSuffix and mainLogName select the files this package may read. main.log is CPA's own
	// process log: it is not a request log at all, and it names no channel.
	logSuffix   = ".log"
	mainLogName = "main.log"
)

var (
	// sectionOpen and sectionClose bracket a section line: "=== NAME ===".
	sectionOpen  = []byte("=== ")
	sectionClose = []byte(" ===")
	// ssePrefix introduces one SSE frame inside an API RESPONSE section.
	ssePrefix = []byte("data:")
	// sessionHeader is the header the join uses: the value is "session-<uuid>", and the same
	// uuid reaches the observation store as "codex:session-<uuid>".
	sessionHeader = "session_id"
	sessionPrefix = "session-"
)

// parseLog reads one CPA request log and returns the fact it carries.
//
// It streams: memory is bounded by readBuffer, never by the size of the file, and a line that
// does not fit the buffer is discarded rather than grown into. A read error is returned with
// the partial fact, so a caller that wants the best-effort answer still has it.
func parseLog(source io.Reader, name string, parsedAt time.Time) (Fact, error) {
	parser := &parser{fact: Fact{SourceFile: name, ParsedAt: parsedAt}}
	reader := &lineReader{reader: bufio.NewReaderSize(source, readBuffer)}
	for {
		line, truncated, errRead := reader.next()
		// A truncated line is never interpreted: only its prefix is known, and half a frame is
		// not a frame. Its section state is kept, so the sections around it still parse.
		if !truncated && len(line) > 0 {
			parser.line(bytes.TrimRight(line, "\r\n"))
		}
		if errRead != nil {
			if errRead == io.EOF || errRead == io.ErrUnexpectedEOF {
				return parser.finish(), nil
			}
			return parser.finish(), errRead
		}
	}
}

// lineReader walks a file one line at a time over a fixed buffer.
type lineReader struct {
	reader *bufio.Reader
}

// next returns the next line without its terminator. truncated marks a line that did not fit
// the buffer: the rest of it is consumed and discarded, so the reader always resynchronises
// on the following line.
func (r *lineReader) next() (line []byte, truncated bool, err error) {
	chunk, errRead := r.reader.ReadSlice('\n')
	if errRead != bufio.ErrBufferFull {
		return chunk, false, errRead
	}
	for {
		_, errDrain := r.reader.ReadSlice('\n')
		switch errDrain {
		case nil:
			return chunk, true, nil
		case bufio.ErrBufferFull:
			continue
		default:
			return chunk, true, errDrain
		}
	}
}

// parser is the section state machine. Sections are opened and closed by their header lines,
// and only the ones this package reads are decoded at all: the request body is never parsed,
// let alone kept.
type parser struct {
	fact Fact

	section  sectionKind
	response *responseSection
	// best is the last response section that carried a provider_metadata block. A file may hold
	// a failed attempt before a successful retry, and only the last parseable answer describes
	// the channel the client was actually served by.
	best *responseSection
	// lastFrames is the frame count of the last response section, kept so a file that yielded
	// no channel can still report how much of an answer it did carry.
	lastFrames int

	attempts int
	hadError bool
}

type sectionKind int

const (
	sectionOther sectionKind = iota
	sectionRequestInfo
	sectionHeaders
	sectionResponse
	sectionErrorResponse
)

// responseSection accumulates the frames of one API RESPONSE section.
type responseSection struct {
	frames  int
	routing routing
	found   bool
}

// line dispatches one complete line. Section headers are recognised first, because every
// other line only means something inside the section that introduced it.
func (p *parser) line(line []byte) {
	if name, ok := sectionHeader(line); ok {
		p.open(classify(name))
		return
	}
	switch p.section {
	case sectionRequestInfo:
		p.infoLine(line)
	case sectionHeaders:
		p.headerLine(line)
	case sectionResponse:
		p.frameLine(line)
	}
}

// open switches sections, closing the previous response section. Every section counts as one
// upstream attempt except the ones CPA writes about the downstream side of the exchange; an
// attempt that errored counts too, which is what makes a retry visible as two.
func (p *parser) open(kind sectionKind) {
	switch kind {
	case sectionResponse:
		p.closeResponse()
		p.response = &responseSection{}
		p.attempts++
	case sectionErrorResponse:
		p.closeResponse()
		p.attempts++
		p.hadError = true
	default:
		p.closeResponse()
	}
	p.section = kind
}

// closeResponse promotes a parsed response section to best, keeping the latest one.
func (p *parser) closeResponse() {
	if p.response == nil {
		return
	}
	p.lastFrames = p.response.frames
	if p.response.found {
		p.best = p.response
	}
	p.response = nil
}

// infoLine reads the three REQUEST INFO fields this package uses. The timestamp is the
// request's arrival time, and it is the anchor the usage record is joined on.
func (p *parser) infoLine(line []byte) {
	key, value, ok := keyValue(line)
	if !ok {
		return
	}
	switch {
	case strings.EqualFold(key, "URL"):
		p.fact.Path = value
	case strings.EqualFold(key, "Method"):
		p.fact.Method = value
	case strings.EqualFold(key, "Timestamp"):
		if parsed, errParse := time.Parse(time.RFC3339Nano, value); errParse == nil {
			p.fact.Time = parsed
		}
	}
}

// headerLine reads the session header. It is the join key: the log file name's trailing
// characters are a CPA-internal id that never reaches the usage payload, while this value
// does, prefixed with "codex:" on the observation side. A client that sends no such header is
// recorded as such (has_session false) so the token-and-time fallback stays possible.
func (p *parser) headerLine(line []byte) {
	key, value, ok := keyValue(line)
	if !ok || normaliseHeader(key) != sessionHeader {
		return
	}
	p.fact.HasSession = true
	p.fact.SessionID = value
	p.fact.SessionUUID = strings.TrimPrefix(value, sessionPrefix)
}

// frameLine decodes one SSE frame and keeps the routing block if the frame carries one. A line
// that is not a data frame, or whose payload is not a JSON object (the terminating "[DONE]"
// marker), is not a frame and is not counted as one.
func (p *parser) frameLine(line []byte) {
	payload, ok := dataPayload(line)
	if !ok {
		return
	}
	var frame responseFrame
	if errUnmarshal := json.Unmarshal(payload, &frame); errUnmarshal != nil {
		return
	}
	p.response.frames++
	for _, choice := range frame.Choices {
		metadata := choice.Delta.ProviderMetadata
		if metadata == nil {
			metadata = choice.Message.ProviderMetadata
		}
		if metadata == nil {
			continue
		}
		p.response.routing = metadata.routing()
		p.response.found = true
	}
}

// finish closes the last section and materialises the fact. The channel fields stay empty
// when no response section carried a provider_metadata block: an empty channel is the honest
// answer for a request that failed, and it is never replaced by a default.
func (p *parser) finish() Fact {
	p.closeResponse()
	fact := p.fact
	fact.AttemptsSeen = p.attempts
	fact.HadErrorResponse = p.hadError
	// Frames describes the answer the fact is built on. With no channel there is no winning
	// section, and the last one seen is still worth reporting: it is the difference between
	// "the upstream answered without a routing block" and "there was no answer at all".
	fact.Frames = p.lastFrames
	if p.best != nil {
		fact.FinalProvider = p.best.routing.finalProvider
		fact.ResolvedProvider = p.best.routing.resolvedProvider
		fact.CanonicalSlug = p.best.routing.canonicalSlug
		fact.OriginalModelID = p.best.routing.originalModelID
		fact.PinnedProvider = p.best.routing.pinnedProvider
		fact.AffinityOutcome = p.best.routing.affinityOutcome
		fact.ModelAttemptCount = p.best.routing.modelAttemptCount
		fact.TotalProviderAttemptCount = p.best.routing.totalProviderAttemptCount
		fact.FallbacksAvailable = p.best.routing.fallbacksAvailable
		fact.GatewayCost = p.best.routing.cost
		fact.Frames = p.best.frames
	}
	if fact.FallbacksAvailable == nil {
		// The key is a list on every path: a page or a merger that has to special-case a null
		// list is a bug waiting to happen.
		fact.FallbacksAvailable = []string{}
	}
	return fact
}

// sectionHeader recognises a section line, and nothing else. A frame payload or a header line
// never matches: the markers are at both ends of the line.
func sectionHeader(line []byte) (string, bool) {
	if !bytes.HasPrefix(line, sectionOpen) || !bytes.HasSuffix(line, sectionClose) {
		return "", false
	}
	name := bytes.TrimSpace(line[len(sectionOpen) : len(line)-len(sectionClose)])
	if len(name) == 0 {
		return "", false
	}
	return string(name), true
}

func classify(name string) sectionKind {
	switch {
	case name == "REQUEST INFO":
		return sectionRequestInfo
	case name == "HEADERS":
		return sectionHeaders
	case name == "API ERROR RESPONSE":
		return sectionErrorResponse
	case strings.HasPrefix(name, "API RESPONSE "):
		return sectionResponse
	default:
		return sectionOther
	}
}

// dataPayload returns the JSON of one SSE data line. Both "data: {...}" and the no-space form
// are accepted, which is what the SSE grammar allows.
func dataPayload(line []byte) ([]byte, bool) {
	if !bytes.HasPrefix(line, ssePrefix) {
		return nil, false
	}
	payload := bytes.TrimSpace(line[len(ssePrefix):])
	if len(payload) == 0 {
		return nil, false
	}
	return payload, true
}

// keyValue splits one "Key: value" line.
func keyValue(line []byte) (key, value string, ok bool) {
	index := bytes.IndexByte(line, ':')
	if index <= 0 {
		return "", "", false
	}
	key = string(bytes.TrimSpace(line[:index]))
	if key == "" {
		return "", "", false
	}
	return key, string(bytes.TrimSpace(line[index+1:])), true
}

// normaliseHeader folds the two spellings a header may arrive in ("Session_id", "Session-Id")
// onto one, because the join must not depend on which one CPA writes.
func normaliseHeader(key string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(key)), "-", "_")
}

// responseFrame is the decoded shape of one upstream SSE frame. Only the path to the routing
// block is declared: the rest of the frame (the model's own text, the usage block) is skipped
// by the decoder, so a frame costs the routing block, not the frame.
type responseFrame struct {
	Choices []struct {
		Delta struct {
			// ProviderMetadata is a pointer so that "present" and "absent" are distinguishable:
			// a frame without the block must not be mistaken for one with an empty block.
			ProviderMetadata *providerMetadata `json:"provider_metadata"`
		} `json:"delta"`
		// Message is where a NON-streaming upstream answer carries the same block (the gateway
		// answers a non-stream chat call with one frame whose `message` holds it). Streaming
		// traffic never uses this path, but the reader must not return an empty channel just
		// because CPA was asked for a non-streaming completion.
		Message struct {
			ProviderMetadata *providerMetadata `json:"provider_metadata"`
		} `json:"message"`
	} `json:"choices"`
}

type providerMetadata struct {
	Gateway struct {
		// Cost is the cost the gateway reports for this generation. It is informational: the
		// channel is what this collector exists for.
		Cost    flexFloat   `json:"cost"`
		Routing routingWire `json:"routing"`
	} `json:"gateway"`
}

// routing is the routing block as this package uses it.
type routing struct {
	finalProvider             string
	resolvedProvider          string
	canonicalSlug             string
	originalModelID           string
	pinnedProvider            string
	affinityOutcome           string
	modelAttemptCount         int
	totalProviderAttemptCount int
	fallbacksAvailable        []string
	cost                      float64
}

// routingWire is the wire shape of the routing block. The field names are the gateway's, and
// they are the contract: the log format belongs to CPA and Cline, not to this plugin.
type routingWire struct {
	FinalProvider             string     `json:"finalProvider"`
	ResolvedProvider          string     `json:"resolvedProvider"`
	CanonicalSlug             string     `json:"canonicalSlug"`
	OriginalModelID           string     `json:"originalModelId"`
	ModelAttemptCount         flexInt    `json:"modelAttemptCount"`
	TotalProviderAttemptCount flexInt    `json:"totalProviderAttemptCount"`
	FallbacksAvailable        stringList `json:"fallbacksAvailable"`
	Affinity                  struct {
		Outcome        string `json:"outcome"`
		PinnedProvider string `json:"pinnedProvider"`
	} `json:"affinity"`
}

func (m *providerMetadata) routing() routing {
	wire := m.Gateway.Routing
	return routing{
		finalProvider:             wire.FinalProvider,
		resolvedProvider:          wire.ResolvedProvider,
		canonicalSlug:             wire.CanonicalSlug,
		originalModelID:           wire.OriginalModelID,
		pinnedProvider:            wire.Affinity.PinnedProvider,
		affinityOutcome:           wire.Affinity.Outcome,
		modelAttemptCount:         int(wire.ModelAttemptCount),
		totalProviderAttemptCount: int(wire.TotalProviderAttemptCount),
		fallbacksAvailable:        wire.FallbacksAvailable,
		cost:                      float64(m.Gateway.Cost),
	}
}

// flexInt and flexFloat decode a JSON number or a string holding one. A gateway that sends
// "1" where the fixture sent 1 must not cost the routing block stored beside it, so neither
// ever reports an error; a value that is not a number at all stays zero.
type flexInt int

type flexFloat float64

func (n *flexInt) UnmarshalJSON(raw []byte) error {
	if value, ok := numberFrom(raw); ok {
		*n = flexInt(value)
	}
	return nil
}

func (n *flexFloat) UnmarshalJSON(raw []byte) error {
	if value, ok := numberFrom(raw); ok {
		*n = flexFloat(value)
	}
	return nil
}

func numberFrom(raw []byte) (float64, bool) {
	text := string(bytes.TrimSpace(raw))
	if len(text) >= 2 && text[0] == '"' && text[len(text)-1] == '"' {
		text = text[1 : len(text)-1]
	}
	if text == "" || text == "null" {
		return 0, false
	}
	value, errParse := strconv.ParseFloat(text, 64)
	if errParse != nil {
		return 0, false
	}
	return value, true
}

// stringList decodes the fallback entries. The fixtures carry an empty array and the element
// shape of a non-empty one is not part of what this format guarantees, so a non-string element
// is kept as its compact JSON text instead of failing the frame around it.
type stringList []string

func (l *stringList) UnmarshalJSON(raw []byte) error {
	var elements []json.RawMessage
	if errUnmarshal := json.Unmarshal(raw, &elements); errUnmarshal != nil {
		return nil
	}
	out := make([]string, 0, len(elements))
	for _, element := range elements {
		var text string
		if errText := json.Unmarshal(element, &text); errText == nil {
			out = append(out, text)
			continue
		}
		out = append(out, string(bytes.TrimSpace(element)))
	}
	*l = out
	return nil
}
