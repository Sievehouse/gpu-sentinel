// Package agent is the in-cluster collector. It reads kernel log lines,
// recognises NVIDIA XID and SXID messages, records every one as a signed
// observation and turns node-level faults into SLA down events.
//
// The agent is read-only towards the host: it never resets GPUs, drains
// nodes or touches the scheduler. Remediation stays with the provider's own
// tooling. Recovery (up) events come from repair or health-check integrations,
// not from the agent.
package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Sievehouse/capacity-ledger/internal/events"
	"github.com/Sievehouse/capacity-ledger/internal/evidence"
	"github.com/Sievehouse/capacity-ledger/internal/gaps"
	"github.com/Sievehouse/capacity-ledger/internal/xid"
)

// Config configures an Agent.
type Config struct {
	Node     string
	Policy   xid.Policy
	Writer   *evidence.Writer
	Suppress time.Duration // minimum gap between down events for this node
	Source   string        // recorded in emitted events
	Now      func() time.Time
	Log      io.Writer // human-readable progress log
}

// Stats counts what the agent has processed.
type Stats struct {
	Heartbeats   int `json:"heartbeats"`
	Lines        int `json:"lines"`
	Observations int `json:"observations"`
	Events       int `json:"events"`
	Suppressed   int `json:"suppressed"`
}

// Agent turns kernel log lines into evidence entries. Its methods are safe
// for concurrent use (the log reader and the GPU poller share one writer).
type Agent struct {
	mu       sync.Mutex
	cfg      Config
	lastDown time.Time
	haveDown bool
	stats    Stats
	lastGPUs *int
	ras      *xid.RASTracker
}

// ObservationPayload is the payload of an "observation" evidence entry.
type ObservationPayload struct {
	Node          string          `json:"node"`
	Observation   xid.Observation `json:"observation"`
	Action        xid.Action      `json:"action"`
	Summary       string          `json:"summary,omitempty"`
	PolicyVersion string          `json:"policyVersion"`
}

// New validates cfg and returns an Agent.
func New(cfg Config) (*Agent, error) {
	if strings.TrimSpace(cfg.Node) == "" {
		return nil, errors.New("agent: node name is required")
	}
	if cfg.Writer == nil {
		return nil, errors.New("agent: evidence writer is required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = io.Discard
	}
	if cfg.Source == "" {
		cfg.Source = "capledger-agent"
	}
	return &Agent{cfg: cfg}, nil
}

// Stats returns counters so far.
func (a *Agent) Stats() Stats {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stats
}

// HandleLine processes one kernel log line.
func (a *Agent) HandleLine(line string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stats.Lines++
	o, ok := xid.ParseLine(line)
	if !ok {
		return nil
	}
	return a.record(o)
}

// HandleRAS records amdgpu RAS counter increases since the previous reading.
// The first reading only sets the baseline.
func (a *Agent) HandleRAS(readings []xid.RASCounter) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ras == nil {
		a.ras = xid.NewRASTracker()
	}
	for _, o := range a.ras.Update(readings) {
		if err := a.record(o); err != nil {
			return err
		}
	}
	return nil
}

// record writes an observation and, if the policy says so, an SLA event.
// The caller holds a.mu.
func (a *Agent) record(o xid.Observation) error {
	rule := a.cfg.Policy.Classify(o)
	entry, err := a.cfg.Writer.Append(evidence.KindObservation, ObservationPayload{
		Node: a.cfg.Node, Observation: o, Action: rule.Action, Summary: rule.Summary, PolicyVersion: a.cfg.Policy.Version,
	})
	if err != nil {
		return err
	}
	a.stats.Observations++
	label := fmt.Sprintf("%s %d", o.Kind, o.Code)
	cause := fmt.Sprintf("%s:%d", o.Kind, o.Code)
	if o.Kind == "amdgpu" {
		label = "amdgpu " + o.Class
		cause = "amdgpu:" + o.Class
		if o.Block != "" {
			cause += ":" + o.Block
		}
	}
	fmt.Fprintf(a.cfg.Log, "observed %s on %s (%s): %s\n", label, o.Device, rule.Action, rule.Summary)

	var typ events.Type
	switch rule.Action {
	case xid.NodeDown:
		typ = events.Down
	case xid.Degraded:
		typ = events.DegradedStart
	default:
		return nil
	}
	ts := a.cfg.Now().UTC()
	if o.TS != "" {
		if t, err := time.Parse(time.RFC3339, o.TS); err == nil {
			ts = t
		}
	}
	return a.emit(typ, ts, cause, entry.Hash)
}

// HandleGPUCount records a down event when fewer GPUs are visible than expected.
func (a *Agent) HandleGPUCount(found, expected int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	seen := found
	a.lastGPUs = &seen
	if expected <= 0 || found >= expected {
		return nil
	}
	entry, err := a.cfg.Writer.Append("observation", map[string]any{
		"node": a.cfg.Node, "check": "gpu-count", "found": found, "expected": expected,
	})
	if err != nil {
		return err
	}
	a.stats.Observations++
	fmt.Fprintf(a.cfg.Log, "only %d of %d GPUs visible on %s\n", found, expected, a.cfg.Node)
	return a.emit(events.Down, a.cfg.Now().UTC(), fmt.Sprintf("gpu-missing:%d/%d", found, expected), entry.Hash)
}

// Heartbeat records that the agent was alive now and promises the next
// heartbeat within interval. Gap detection flags longer silences.
func (a *Agent) Heartbeat(interval time.Duration) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	hb := gaps.Heartbeat{
		Node:            a.cfg.Node,
		TS:              a.cfg.Now().UTC().Format(time.RFC3339),
		IntervalSeconds: int64(interval / time.Second),
		Role:            "agent",
		GPUs:            a.lastGPUs,
	}
	if _, err := a.cfg.Writer.Append(evidence.KindHeartbeat, hb); err != nil {
		return err
	}
	a.stats.Heartbeats++
	return nil
}

func (a *Agent) emit(typ events.Type, ts time.Time, cause, evidenceHash string) error {
	if typ == events.Down && a.haveDown {
		if gap := ts.Sub(a.lastDown); gap >= 0 && gap < a.cfg.Suppress {
			a.stats.Suppressed++
			return nil
		}
	}
	ev := events.Event{
		ID:       fmt.Sprintf("%s-%d", a.cfg.Writer.KeyID(), a.cfg.Writer.NextSeq()),
		Seq:      a.cfg.Writer.NextSeq(),
		TS:       ts.UTC().Format(time.RFC3339),
		Node:     a.cfg.Node,
		Type:     typ,
		Cause:    cause,
		Source:   a.cfg.Source,
		Evidence: evidenceHash,
	}
	if _, err := a.cfg.Writer.Append("event", ev); err != nil {
		return err
	}
	if typ == events.Down {
		a.haveDown, a.lastDown = true, ts
	}
	a.stats.Events++
	fmt.Fprintf(a.cfg.Log, "event %s %s on %s at %s (%s)\n", ev.ID, typ, a.cfg.Node, ev.TS, cause)
	return nil
}

// Run feeds every line from r to HandleLine until EOF or cancellation.
func (a *Agent) Run(ctx context.Context, r io.Reader) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := a.HandleLine(sc.Text()); err != nil {
			return err
		}
	}
	return sc.Err()
}

// FollowFile tails a regular file, polling for appended lines. It reopens
// the file if it shrinks (log rotation by truncation). With fromStart unset
// it starts at the current end of the file.
func FollowFile(ctx context.Context, path string, fromStart bool, poll time.Duration, handle func(string) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { f.Close() }()
	var offset int64
	if !fromStart {
		if offset, err = f.Seek(0, io.SeekEnd); err != nil {
			return err
		}
	}
	reader := bufio.NewReader(f)
	var partial strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			offset += int64(len(line))
			partial.WriteString(line)
			if strings.HasSuffix(line, "\n") {
				if herr := handle(strings.TrimRight(partial.String(), "\n")); herr != nil {
					return herr
				}
				partial.Reset()
			}
		}
		if err == nil {
			continue
		}
		if !errors.Is(err, io.EOF) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
		if st, statErr := os.Stat(path); statErr == nil && st.Size() < offset {
			f.Close()
			if f, err = os.Open(path); err != nil {
				return err
			}
			offset = 0
			partial.Reset()
			reader = bufio.NewReader(f)
		}
	}
}

// FollowKmsg reads /dev/kmsg records until cancellation. Each read returns
// one record; records lost to ring-buffer overwrites (EPIPE) are skipped.
func FollowKmsg(ctx context.Context, fromStart bool, handle func(string) error) error {
	f, err := os.Open("/dev/kmsg")
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); f.Close() }()
	if !fromStart {
		if _, err := f.Seek(0, io.SeekEnd); err != nil {
			return err
		}
	}
	buf := make([]byte, 8192)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			for _, rec := range strings.Split(strings.TrimRight(string(buf[:n]), "\n"), "\n") {
				if herr := handle(rec); herr != nil {
					return herr
				}
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if strings.Contains(err.Error(), "broken pipe") {
				continue
			}
			return err
		}
	}
}

// CountGPUs counts GPUs in `nvidia-smi --query-gpu=index,... --format=csv,noheader`
// output: one GPU per line that starts with a GPU index. Error lines such as
// "Unable to determine the device handle for GPU ..." are not GPUs.
func CountGPUs(out string) int {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		first, _, _ := strings.Cut(line, ",")
		if _, err := strconv.Atoi(strings.TrimSpace(first)); err == nil {
			n++
		}
	}
	return n
}
