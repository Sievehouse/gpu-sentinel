package webhook

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/Sievehouse/gpu-sentinel/internal/events"
)

// MaxBody is the largest delivery the receiver reads.
const MaxBody = 1 << 20

// Record writes one event (signed, in the receiver's evidence log) and
// returns its id. The receiver fills in everything except ID and Seq.
type Record func(ev events.Event) (string, error)

// nodeState is what the receiver last recorded for a node.
type nodeState struct {
	open map[events.Type]bool // down, degraded_start, maintenance_start
	last int64
}

// Receiver is the webhook HTTP endpoint. It records a transition only when
// it changes the node's state, so redelivered or repeated webhooks are
// harmless.
type Receiver struct {
	m      *Mapping
	scope  map[string]bool
	record Record
	logw   io.Writer
	now    func() time.Time

	mu    sync.Mutex
	state map[string]*nodeState
}

// NewReceiver returns a receiver. scope, if not empty, lists the nodes that
// may be reported (the contract's).
func NewReceiver(m *Mapping, scope []string, record Record, logw io.Writer) *Receiver {
	r := &Receiver{m: m, record: record, logw: logw, now: time.Now, state: map[string]*nodeState{}}
	if r.logw == nil {
		r.logw = io.Discard
	}
	if len(scope) > 0 {
		r.scope = map[string]bool{}
		for _, n := range scope {
			r.scope[n] = true
		}
	}
	return r
}

// SetClock replaces the clock (for tests).
func (r *Receiver) SetClock(now func() time.Time) { r.now = now }

// opener returns the type that opens the stretch t belongs to, and whether
// t opens it.
func opener(t events.Type) (events.Type, bool) {
	switch t {
	case events.Down:
		return events.Down, true
	case events.Up:
		return events.Down, false
	case events.DegradedStart:
		return events.DegradedStart, true
	case events.DegradedEnd:
		return events.DegradedStart, false
	case events.MaintenanceStart:
		return events.MaintenanceStart, true
	case events.MaintenanceEnd:
		return events.MaintenanceStart, false
	}
	return "", false
}

func (r *Receiver) node(name string) *nodeState {
	st, ok := r.state[name]
	if !ok {
		st = &nodeState{open: map[events.Type]bool{}}
		r.state[name] = st
	}
	return st
}

// Resume restores the state from the receiver's earlier events.
func (r *Receiver) Resume(evs []events.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ev := range evs {
		if err := ev.Normalize(); err != nil {
			return err
		}
		kind, opens := opener(ev.Type)
		if kind == "" {
			continue
		}
		st := r.node(ev.Node)
		st.open[kind] = opens
		if ev.Unix() > st.last {
			st.last = ev.Unix()
		}
	}
	return nil
}

// Open lists the nodes with an open down, degraded or maintenance stretch.
func (r *Receiver) Open() map[string][]events.Type {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string][]events.Type{}
	for n, st := range r.state {
		for _, k := range []events.Type{events.Down, events.DegradedStart, events.MaintenanceStart} {
			if st.open[k] {
				out[n] = append(out[n], k)
			}
		}
	}
	return out
}

// Result is the receiver's answer to a delivery.
type Result struct {
	Recorded bool   `json:"recorded"`
	Event    string `json:"event,omitempty"`
	Node     string `json:"node,omitempty"`
	Type     string `json:"type,omitempty"`
	TS       string `json:"ts,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Error    string `json:"error,omitempty"`
}

func reply(w http.ResponseWriter, code int, res Result) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(res)
}

// ServeHTTP handles POST / (deliveries) and GET /healthz.
func (r *Receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodGet && req.URL.Path == "/healthz" {
		io.WriteString(w, "ok\n")
		return
	}
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		reply(w, http.StatusMethodNotAllowed, Result{Error: "deliveries are POST requests"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, MaxBody+1))
	if err != nil {
		reply(w, http.StatusBadRequest, Result{Error: err.Error()})
		return
	}
	if len(body) > MaxBody {
		reply(w, http.StatusRequestEntityTooLarge, Result{Error: "delivery larger than 1 MiB"})
		return
	}
	if !r.m.Auth.Authenticate(body, req.Header.Get) {
		r.logf("rejected a delivery from %s: bad signature or token", req.RemoteAddr)
		reply(w, http.StatusUnauthorized, Result{Error: "missing or invalid signature or token"})
		return
	}
	d, err := r.m.Read(body)
	if err != nil {
		reply(w, http.StatusBadRequest, Result{Error: err.Error()})
		return
	}
	code, res := r.Apply(d)
	reply(w, code, res)
}

// Apply records a delivery if it changes the node's state, and returns the
// HTTP status and answer the receiver gives for it.
func (r *Receiver) Apply(d *Delivery) (int, Result) {
	res := Result{Node: d.Node, Type: string(d.Type)}
	if d.Node == "" {
		res.Error = "empty node"
		return http.StatusBadRequest, res
	}
	if r.scope != nil && !r.scope[d.Node] {
		res.Error = fmt.Sprintf("node %q is not in the contract", d.Node)
		return http.StatusUnprocessableEntity, res
	}
	if d.Type == "" {
		res.Reason = fmt.Sprintf("status %q is not mapped to an event", d.Status)
		return http.StatusAccepted, res
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.node(d.Node)
	kind, opens := opener(d.Type)
	if st.open[kind] == opens {
		if opens {
			res.Reason = fmt.Sprintf("%s is already open for %s", kind, d.Node)
		} else {
			res.Reason = fmt.Sprintf("no open %s for %s", kind, d.Node)
		}
		return http.StatusOK, res
	}
	now := r.now().Unix()
	ts := d.TS
	if ts > 0 && ts < st.last {
		// Older than what the node last recorded: a late or replayed
		// delivery. Recording it would rewrite history, so it is refused.
		res.Reason = fmt.Sprintf("dated %s, before %s's last recorded event at %s", events.FormatTime(ts), d.Node, events.FormatTime(st.last))
		return http.StatusOK, res
	}
	if ts <= 0 || ts > now {
		ts = now
	}
	if ts < st.last {
		ts = st.last
	}
	ev := events.Event{TS: events.FormatTime(ts), Node: d.Node, Type: d.Type, Cause: d.Cause(), Source: r.m.Source, Note: note(d)}
	if d.Type == events.MaintenanceStart && d.NoticeTS > 0 {
		ev.NoticeTS = events.FormatTime(d.NoticeTS)
	}
	id, err := r.record(ev)
	if err != nil {
		r.logf("recording %s %s failed: %v", d.Node, d.Type, err)
		res.Error = "recording failed"
		return http.StatusInternalServerError, res
	}
	st.open[kind] = opens
	st.last = ts
	res.Recorded, res.Event, res.TS = true, id, ev.TS
	r.logf("%s %s %s %s (%s)", ev.TS, d.Node, d.Type, ev.Cause, id)
	return http.StatusCreated, res
}

func note(d *Delivery) string {
	s := "status " + d.Status
	if d.Summary != "" {
		s += ": " + d.Summary
	}
	if len(s) > 500 {
		s = s[:497] + "..."
	}
	return s
}

func (r *Receiver) logf(format string, a ...any) {
	fmt.Fprintf(r.logw, format+"\n", a...)
}
