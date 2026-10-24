// Package gaps finds stretches with no heartbeat in evidence logs and turns
// them into unmonitored_start / unmonitored_end events for the SLA engine.
//
// A node that stops sending heartbeats is not necessarily down: the agent may
// have crashed, or the host may have lost power. Either way nobody can vouch
// for it, and the contract decides what that means ("report" or
// "count-as-down"). Making silence visible closes the easiest way to shade
// the record: stopping the agent during an outage.
package gaps

import (
	"fmt"
	"sort"
	"time"

	"github.com/Sievehouse/capacity-ledger/internal/events"
	"github.com/Sievehouse/capacity-ledger/internal/evidence"
)

// Heartbeat is the payload of an evidence.KindHeartbeat entry.
type Heartbeat struct {
	Node            string `json:"node"`
	TS              string `json:"ts"`
	IntervalSeconds int64  `json:"intervalSeconds"`
	Role            string `json:"role,omitempty"`
	GPUs            *int   `json:"gpus,omitempty"`
}

// Beat is one heartbeat in time.
type Beat struct {
	Node     string
	TS       int64 // Unix seconds
	Interval int64 // promised seconds until the next beat; 0 if unknown
}

// Gap is one stretch with no heartbeat for a node, [Start, End).
type Gap struct {
	Node    string `json:"node"`
	Start   int64  `json:"-"`
	End     int64  `json:"-"`
	From    string `json:"start"`
	To      string `json:"end"`
	Seconds int64  `json:"seconds"`
	Reason  string `json:"reason"`
}

// Reasons a gap was flagged.
const (
	ReasonSilence      = "heartbeats stopped"
	ReasonBeforeFirst  = "no heartbeat between the window start and the first heartbeat"
	ReasonAfterLast    = "no heartbeat between the last heartbeat and the window end"
	ReasonNoHeartbeats = "no heartbeat at all in the window"
)

// Options control gap detection.
type Options struct {
	// MaxSilence is the longest tolerated time between two heartbeats, in
	// seconds. Longer silences are gaps. It must be positive.
	MaxSilence int64
	// Since and Until bound the window (Unix seconds; 0 = open). Gaps are
	// clipped to it, and silence before the first or after the last
	// heartbeat is only detectable when the window is bounded.
	Since, Until int64
	// Nodes that must send heartbeats (for example the contract's nodes).
	// With a bounded window, a node that never sent one is a single gap.
	Nodes []string
}

// Find returns gaps sorted by node and start time.
func Find(beats []Beat, opt Options) ([]Gap, error) {
	if opt.MaxSilence <= 0 {
		return nil, fmt.Errorf("max silence must be positive")
	}
	if opt.Since > 0 && opt.Until > 0 && opt.Until <= opt.Since {
		return nil, fmt.Errorf("window end must be after its start")
	}
	byNode := map[string][]Beat{}
	for _, b := range beats {
		byNode[b.Node] = append(byNode[b.Node], b)
	}
	for _, n := range opt.Nodes {
		if _, ok := byNode[n]; !ok {
			byNode[n] = nil
		}
	}
	names := make([]string, 0, len(byNode))
	for n := range byNode {
		names = append(names, n)
	}
	sort.Strings(names)

	var out []Gap
	add := func(node string, start, end int64, reason string) {
		if opt.Since > 0 && start < opt.Since {
			start = opt.Since
		}
		if opt.Until > 0 && end > opt.Until {
			end = opt.Until
		}
		if end <= start {
			return
		}
		out = append(out, Gap{Node: node, Start: start, End: end, From: events.FormatTime(start), To: events.FormatTime(end), Seconds: end - start, Reason: reason})
	}
	due := func(b Beat, limit int64) int64 {
		if b.Interval > 0 && b.TS+b.Interval < limit {
			return b.TS + b.Interval
		}
		return b.TS
	}
	for _, n := range names {
		bs := byNode[n]
		sort.Slice(bs, func(i, j int) bool { return bs[i].TS < bs[j].TS })
		if len(bs) == 0 {
			if opt.Since > 0 && opt.Until > 0 && opt.Until-opt.Since > opt.MaxSilence {
				add(n, opt.Since, opt.Until, ReasonNoHeartbeats)
			}
			continue
		}
		if opt.Since > 0 && bs[0].TS-opt.Since > opt.MaxSilence {
			add(n, opt.Since, bs[0].TS, ReasonBeforeFirst)
		}
		for i := 1; i < len(bs); i++ {
			prev, next := bs[i-1], bs[i]
			if next.TS-prev.TS > opt.MaxSilence {
				add(n, due(prev, next.TS), next.TS, ReasonSilence)
			}
		}
		last := bs[len(bs)-1]
		if opt.Until > 0 && opt.Until-last.TS > opt.MaxSilence {
			add(n, due(last, opt.Until), opt.Until, ReasonAfterLast)
		}
	}
	return out, nil
}

// Events turns gaps into unmonitored_start / unmonitored_end events with
// deterministic ids, so the same logs always export the same events.
func Events(gs []Gap, source string) []events.Event {
	out := make([]events.Event, 0, 2*len(gs))
	for _, g := range gs {
		id := fmt.Sprintf("gap:%s:%s", g.Node, g.From)
		out = append(out,
			events.Event{ID: id, TS: g.From, Node: g.Node, Type: events.UnmonitoredStart, Cause: "no-heartbeat", Source: source, Note: g.Reason},
			events.Event{ID: id + ":end", TS: g.To, Node: g.Node, Type: events.UnmonitoredEnd, Source: source},
		)
	}
	return out
}

// NodeCheck decides whether the key that signed an entry may speak for node.
type NodeCheck func(e evidence.Entry, node string) error

// BeatsFromEntries extracts heartbeats from verified log entries.
func BeatsFromEntries(entries []evidence.Entry, check NodeCheck) ([]Beat, error) {
	var out []Beat
	for _, e := range entries {
		if e.Kind != evidence.KindHeartbeat {
			continue
		}
		var hb Heartbeat
		if err := e.DecodePayload(&hb); err != nil {
			return nil, fmt.Errorf("seq %d: %w", e.Seq, err)
		}
		if hb.Node == "" {
			return nil, fmt.Errorf("seq %d: heartbeat without node", e.Seq)
		}
		if check != nil {
			if err := check(e, hb.Node); err != nil {
				return nil, fmt.Errorf("seq %d: %w", e.Seq, err)
			}
		}
		t, err := time.Parse(time.RFC3339, hb.TS)
		if err != nil {
			return nil, fmt.Errorf("seq %d: heartbeat ts: %w", e.Seq, err)
		}
		out = append(out, Beat{Node: hb.Node, TS: t.Unix(), Interval: hb.IntervalSeconds})
	}
	return out, nil
}
