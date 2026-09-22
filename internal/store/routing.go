package store

// RoutingState is how much channel evidence one response carried. It is recorded per
// request so the usage hook can tell "the host matched but the response carried no routing
// marker" apart from "this request was never seen".
type RoutingState int

const (
	// RoutingAbsent means no upstream body carried the channel marker.
	RoutingAbsent RoutingState = iota
	// RoutingMarkerOnly means the marker was present without gateway routing evidence.
	RoutingMarkerOnly
	// RoutingConfirmed means gateway routing evidence was observed.
	RoutingConfirmed
)
