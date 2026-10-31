package k8s

import (
	"errors"
	"fmt"
	"sort"

	"github.com/Sievehouse/gpu-sentinel/internal/events"
	"github.com/Sievehouse/gpu-sentinel/internal/evidence"
)

// Transition is one change of a node's state, ready to become an event.
type Transition struct {
	Node  string
	Type  events.Type
	TS    int64 // Unix seconds
	Cause string
	Note  string
}

// nodeState is what the watcher last recorded for a node.
type nodeState struct {
	down, degraded bool
	last           int64 // time of the last recorded event
}

// Watcher compares each new view of the cluster with what it last recorded
// and reports the transitions. It is not safe for concurrent use.
type Watcher struct {
	rules Rules
	// scope, if not empty, lists the nodes to record (the contract's); the
	// watcher ignores other nodes and reports scoped nodes missing from the
	// cluster as down.
	scope map[string]bool
	state map[string]*nodeState
}

// NewWatcher returns a watcher. scope may be nil to record every node.
func NewWatcher(rules Rules, scope []string) (*Watcher, error) {
	if err := rules.Validate(); err != nil {
		return nil, err
	}
	w := &Watcher{rules: rules, state: map[string]*nodeState{}}
	if len(scope) > 0 {
		w.scope = map[string]bool{}
		for _, n := range scope {
			w.scope[n] = true
		}
	}
	return w, nil
}

// Resume restores what the watcher recorded before a restart from the
// event entries of its own log, so that a restart neither repeats nor
// forgets an open outage.
func (w *Watcher) Resume(entries []evidence.Entry) error {
	for _, en := range entries {
		if en.Kind != evidence.KindEvent {
			continue
		}
		var ev events.Event
		if err := en.DecodePayload(&ev); err != nil {
			return fmt.Errorf("entry %d: %w", en.Seq, err)
		}
		if err := ev.Normalize(); err != nil {
			return fmt.Errorf("entry %d: %w", en.Seq, err)
		}
		st := w.node(ev.Node)
		switch ev.Type {
		case events.Down:
			st.down = true
		case events.Up:
			st.down = false
		case events.DegradedStart:
			st.degraded = true
		case events.DegradedEnd:
			st.degraded = false
		default:
			continue
		}
		if ev.Unix() > st.last {
			st.last = ev.Unix()
		}
	}
	return nil
}

func (w *Watcher) node(name string) *nodeState {
	st, ok := w.state[name]
	if !ok {
		st = &nodeState{}
		w.state[name] = st
	}
	return st
}

// Open returns the nodes the watcher currently records as down or degraded.
func (w *Watcher) Open() (down, degraded []string) {
	for n, st := range w.state {
		if st.down {
			down = append(down, n)
		}
		if st.degraded {
			degraded = append(degraded, n)
		}
	}
	sort.Strings(down)
	sort.Strings(degraded)
	return down, degraded
}

// Step compares a view of the cluster taken at now (Unix seconds) with the
// recorded state, updates it and returns the transitions in a stable order.
//
// A transition is dated by the Kubernetes condition that caused it when the
// condition carries a transition time, otherwise by now. Dates are clamped
// between the node's previous event and now, so that events for a node
// never go backwards and never lie in the future.
func (w *Watcher) Step(list *NodeList, now int64) ([]Transition, error) {
	if list == nil {
		return nil, errors.New("no node list")
	}
	seen := map[string]bool{}
	var out []Transition
	for _, n := range list.Items {
		name := w.rules.Name(n)
		if name == "" {
			continue
		}
		if w.scope != nil && !w.scope[name] {
			continue
		}
		if seen[name] {
			return nil, fmt.Errorf("two Kubernetes nodes map to the contract node %q", name)
		}
		seen[name] = true
		out = append(out, w.apply(name, w.rules.Assess(n), now)...)
	}
	// Nodes the watcher knows (from the contract or from earlier views)
	// that the cluster no longer lists are down: the control plane cannot
	// vouch for them.
	var missing []string
	for name := range w.scope {
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	if w.scope == nil {
		for name := range w.state {
			if !seen[name] {
				missing = append(missing, name)
			}
		}
	}
	sort.Strings(missing)
	for _, name := range missing {
		h := Health{Down: []Reason{{Cause: "k8s:node-missing", Detail: "not in the cluster's node list"}}}
		out = append(out, w.apply(name, h, now)...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].TS != out[j].TS {
			return out[i].TS < out[j].TS
		}
		return out[i].Node < out[j].Node
	})
	return out, nil
}

func (w *Watcher) apply(name string, h Health, now int64) []Transition {
	st := w.node(name)
	var out []Transition
	date := func(t int64) int64 {
		if t <= 0 || t > now {
			t = now
		}
		if t < st.last {
			t = st.last
		}
		return t
	}
	switch {
	case h.IsDown() && !st.down:
		out = append(out, Transition{Node: name, Type: events.Down, TS: date(earliest(h.Down)), Cause: Cause(h.Down), Note: Detail(h.Down)})
	case !h.IsDown() && st.down:
		out = append(out, Transition{Node: name, Type: events.Up, TS: date(latestAfter(h.Changes, st.last)), Cause: "k8s:healthy"})
	}
	switch {
	case h.IsDegraded() && !st.degraded:
		out = append(out, Transition{Node: name, Type: events.DegradedStart, TS: date(earliest(h.Degraded)), Cause: Cause(h.Degraded), Note: Detail(h.Degraded)})
	case !h.IsDegraded() && st.degraded:
		out = append(out, Transition{Node: name, Type: events.DegradedEnd, TS: date(latestAfter(h.Changes, st.last)), Cause: "k8s:healthy"})
	}
	for _, t := range out {
		if t.TS > st.last {
			st.last = t.TS
		}
	}
	st.down, st.degraded = h.IsDown(), h.IsDegraded()
	return out
}

// earliest returns the earliest known start among reasons, or 0. A node is
// down from the moment its first fault began.
func earliest(rs []Reason) int64 {
	var t int64
	for _, r := range rs {
		if r.Since > 0 && (t == 0 || r.Since < t) {
			t = r.Since
		}
	}
	return t
}

// latestAfter returns the latest transition time after since, or 0. A node
// is healthy again from the moment its last fault cleared; a condition that
// last changed before the outage was recorded did not end it, so recoveries
// that no condition dates (a taint removed, GPUs back) are dated now.
func latestAfter(ts []int64, since int64) int64 {
	var t int64
	for _, v := range ts {
		if v > since && v > t {
			t = v
		}
	}
	return t
}
