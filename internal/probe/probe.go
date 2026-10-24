// Package probe is the tenant-side vantage point. It runs where the
// customer's jobs run, checks that every rented GPU is visible and,
// optionally, that a short benchmark stays above an agreed floor, and writes
// signed heartbeats, observations and availability events to the tenant's
// own evidence log.
//
// The provider's agent and this probe see the same nodes from opposite
// sides; "capledger sla corroborate" compares what each recorded.
//
// The probe never changes anything: it runs read-only queries and the
// benchmark command it is given.
package probe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Sievehouse/gpu-sentinel/internal/events"
	"github.com/Sievehouse/gpu-sentinel/internal/evidence"
	"github.com/Sievehouse/gpu-sentinel/internal/gaps"
	"github.com/Sievehouse/gpu-sentinel/internal/xid"
)

// Runner runs a command and returns its standard output.
type Runner func(ctx context.Context, name string, args ...string) (string, error)

// ExecRunner runs commands with os/exec.
func ExecRunner(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}

// Config configures a Probe.
type Config struct {
	Node         string
	Vendor       string // "nvidia" (default) or "amd"
	ExpectedGPUs int
	// FailAfter consecutive bad samples open an incident; RecoverAfter
	// consecutive good samples close it. Events carry the time of the first
	// sample of the run, not of the sample that confirmed it.
	FailAfter    int
	RecoverAfter int
	Interval     time.Duration // promised time between cycles, recorded in heartbeats
	QueryTimeout time.Duration

	BenchCmd     []string // optional; the last number printed is the metric
	BenchFloor   float64  // higher is better
	BenchEvery   time.Duration
	BenchTimeout time.Duration

	SysRoot string // for AMD GPU counting; default "/sys"
	Source  string // recorded in events; default "tenant-probe"
	Writer  *evidence.Writer
	Run     Runner
	Now     func() time.Time
	Log     io.Writer
}

// Stats counts what the probe has done.
type Stats struct {
	Cycles       int `json:"cycles"`
	Heartbeats   int `json:"heartbeats"`
	Observations int `json:"observations"`
	Events       int `json:"events"`
	BenchRuns    int `json:"benchRuns"`
}

// GPU is one row of nvidia-smi output, kept as text.
type GPU struct {
	Index      string `json:"index"`
	PCI        string `json:"pci"`
	TempC      string `json:"tempC,omitempty"`
	PowerW     string `json:"powerW,omitempty"`
	SMClockMHz string `json:"smClockMHz,omitempty"`
}

// Sample is the result of one GPU-visibility check.
type Sample struct {
	OK    bool   `json:"ok"` // the query itself worked
	GPUs  int    `json:"gpus"`
	Rows  []GPU  `json:"rows,omitempty"`
	Error string `json:"error,omitempty"`
}

// Probe holds the hysteresis state between cycles.
type Probe struct {
	mu        sync.Mutex
	cfg       Config
	down      bool
	degraded  bool
	run       streak // GPU visibility
	bench     streak // benchmark floor
	lastBench time.Time
	stats     Stats
}

// streak tracks consecutive good or bad samples and when the run started.
type streak struct {
	bad, good     int
	badSince      time.Time
	goodSince     time.Time
	lastBadDetail string
}

func (s *streak) observe(ok bool, now time.Time) {
	if ok {
		if s.good == 0 {
			s.goodSince = now
		}
		s.good++
		s.bad = 0
		return
	}
	if s.bad == 0 {
		s.badSince = now
	}
	s.bad++
	s.good = 0
}

// New validates cfg and returns a Probe.
func New(cfg Config) (*Probe, error) {
	if strings.TrimSpace(cfg.Node) == "" {
		return nil, errors.New("probe: node name is required")
	}
	if cfg.Writer == nil {
		return nil, errors.New("probe: evidence writer is required")
	}
	if cfg.ExpectedGPUs < 1 {
		return nil, errors.New("probe: expected GPU count must be at least 1")
	}
	switch cfg.Vendor {
	case "":
		cfg.Vendor = "nvidia"
	case "nvidia", "amd":
	default:
		return nil, fmt.Errorf("probe: unknown vendor %q (nvidia or amd)", cfg.Vendor)
	}
	if len(cfg.BenchCmd) > 0 && cfg.BenchFloor <= 0 {
		return nil, errors.New("probe: a benchmark needs a positive floor")
	}
	if cfg.FailAfter < 1 {
		cfg.FailAfter = 3
	}
	if cfg.RecoverAfter < 1 {
		cfg.RecoverAfter = 2
	}
	if cfg.Interval <= 0 {
		cfg.Interval = time.Minute
	}
	if cfg.QueryTimeout <= 0 {
		cfg.QueryTimeout = 30 * time.Second
	}
	if cfg.BenchEvery <= 0 {
		cfg.BenchEvery = 10 * time.Minute
	}
	if cfg.BenchTimeout <= 0 {
		cfg.BenchTimeout = 5 * time.Minute
	}
	if cfg.SysRoot == "" {
		cfg.SysRoot = "/sys"
	}
	if cfg.Source == "" {
		cfg.Source = "tenant-probe"
	}
	if cfg.Run == nil {
		cfg.Run = ExecRunner
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = io.Discard
	}
	return &Probe{cfg: cfg}, nil
}

// Stats returns counters so far.
func (p *Probe) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stats
}

// Down reports whether the probe currently considers the node down.
func (p *Probe) Down() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.down
}

// Cycle runs one check, an optional benchmark and a heartbeat.
func (p *Probe) Cycle(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stats.Cycles++
	now := p.cfg.Now().UTC()

	s := p.sample(ctx)
	healthy := s.OK && s.GPUs >= p.cfg.ExpectedGPUs
	p.run.observe(healthy, now)
	if !healthy {
		detail := fmt.Sprintf("gpu-missing:%d/%d", s.GPUs, p.cfg.ExpectedGPUs)
		if !s.OK {
			detail = "probe-error"
		}
		p.run.lastBadDetail = detail
		if err := p.observation(map[string]any{"check": "gpu-visibility", "expected": p.cfg.ExpectedGPUs, "sample": s}); err != nil {
			return err
		}
	}
	switch {
	case !p.down && p.run.bad >= p.cfg.FailAfter:
		if err := p.emit(events.Down, p.run.badSince, "tenant:"+p.run.lastBadDetail); err != nil {
			return err
		}
		p.down = true
	case p.down && p.run.good >= p.cfg.RecoverAfter:
		if err := p.emit(events.Up, p.run.goodSince, "tenant:gpus-visible"); err != nil {
			return err
		}
		p.down = false
	}

	if len(p.cfg.BenchCmd) > 0 && !p.down && (p.lastBench.IsZero() || now.Sub(p.lastBench) >= p.cfg.BenchEvery) {
		p.lastBench = now
		if err := p.runBench(ctx, now); err != nil {
			return err
		}
	}

	var gpus *int
	if s.OK {
		n := s.GPUs
		gpus = &n
	}
	hb := gaps.Heartbeat{Node: p.cfg.Node, TS: now.Format(time.RFC3339), IntervalSeconds: int64(p.cfg.Interval / time.Second), Role: "tenant-probe", GPUs: gpus}
	if _, err := p.cfg.Writer.Append(evidence.KindHeartbeat, hb); err != nil {
		return err
	}
	p.stats.Heartbeats++
	return nil
}

func (p *Probe) sample(ctx context.Context) Sample {
	if p.cfg.Vendor == "amd" {
		n, err := xid.CountAMDGPUs(p.cfg.SysRoot)
		if err != nil {
			return Sample{Error: err.Error()}
		}
		return Sample{OK: true, GPUs: n}
	}
	cctx, cancel := context.WithTimeout(ctx, p.cfg.QueryTimeout)
	defer cancel()
	out, err := p.cfg.Run(cctx, "nvidia-smi", "--query-gpu=index,pci.bus_id,temperature.gpu,power.draw,clocks.sm", "--format=csv,noheader,nounits")
	if err != nil {
		return Sample{Error: firstLine(err.Error())}
	}
	return ParseNvidiaSMI(out)
}

// ParseNvidiaSMI reads nvidia-smi CSV rows (index first). Lines that do not
// start with a GPU index, such as per-GPU error messages, are not GPUs.
func ParseNvidiaSMI(out string) Sample {
	s := Sample{OK: true}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, ",")
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		if _, err := strconv.Atoi(f[0]); err != nil {
			continue
		}
		g := GPU{Index: f[0]}
		fields := []*string{&g.PCI, &g.TempC, &g.PowerW, &g.SMClockMHz}
		for i, dst := range fields {
			if i+1 < len(f) {
				*dst = f[i+1]
			}
		}
		s.Rows = append(s.Rows, g)
	}
	s.GPUs = len(s.Rows)
	return s
}

var numberRe = regexp.MustCompile(`[-+]?[0-9]*\.?[0-9]+(?:[eE][-+]?[0-9]+)?`)

// LastNumber returns the last number in text.
func LastNumber(text string) (float64, bool) {
	all := numberRe.FindAllString(text, -1)
	if len(all) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(all[len(all)-1], 64)
	return v, err == nil
}

func (p *Probe) runBench(ctx context.Context, now time.Time) error {
	p.stats.BenchRuns++
	cctx, cancel := context.WithTimeout(ctx, p.cfg.BenchTimeout)
	defer cancel()
	out, err := p.cfg.Run(cctx, p.cfg.BenchCmd[0], p.cfg.BenchCmd[1:]...)
	record := map[string]any{"check": "benchmark", "command": strings.Join(p.cfg.BenchCmd, " "), "floor": p.cfg.BenchFloor}
	if err != nil {
		record["error"] = firstLine(err.Error())
		// A failed benchmark is recorded but is neither good nor bad: it
		// may be the tool, not the GPU.
		return p.observation(record)
	}
	v, ok := LastNumber(out)
	if !ok {
		record["error"] = "no number in benchmark output"
		return p.observation(record)
	}
	record["value"] = v
	if err := p.observation(record); err != nil {
		return err
	}
	p.bench.observe(v >= p.cfg.BenchFloor, now)
	switch {
	case !p.degraded && p.bench.bad >= p.cfg.FailAfter:
		cause := fmt.Sprintf("tenant:bench-below-floor:%s<%s", trimFloat(v), trimFloat(p.cfg.BenchFloor))
		if err := p.emit(events.DegradedStart, p.bench.badSince, cause); err != nil {
			return err
		}
		p.degraded = true
	case p.degraded && p.bench.good >= p.cfg.RecoverAfter:
		if err := p.emit(events.DegradedEnd, p.bench.goodSince, "tenant:bench-recovered"); err != nil {
			return err
		}
		p.degraded = false
	}
	return nil
}

func (p *Probe) observation(payload map[string]any) error {
	payload["node"] = p.cfg.Node
	payload["source"] = p.cfg.Source
	if _, err := p.cfg.Writer.Append(evidence.KindObservation, payload); err != nil {
		return err
	}
	p.stats.Observations++
	return nil
}

func (p *Probe) emit(typ events.Type, ts time.Time, cause string) error {
	w := p.cfg.Writer
	ev := events.Event{
		ID: fmt.Sprintf("%s-%d", w.KeyID(), w.NextSeq()), Seq: w.NextSeq(),
		TS: ts.UTC().Format(time.RFC3339), Node: p.cfg.Node, Type: typ, Cause: cause, Source: p.cfg.Source,
	}
	if _, err := w.Append(evidence.KindEvent, ev); err != nil {
		return err
	}
	p.stats.Events++
	fmt.Fprintf(p.cfg.Log, "event %s %s on %s at %s (%s)\n", ev.ID, typ, p.cfg.Node, ev.TS, cause)
	return nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return strings.TrimSpace(s)
}

func trimFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
