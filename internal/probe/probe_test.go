package probe

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sievehouse/capacity-ledger/internal/events"
	"github.com/Sievehouse/capacity-ledger/internal/evidence"
	"github.com/Sievehouse/capacity-ledger/internal/gaps"
)

const eightGPUs = `0, 00000000:18:00.0, 41, 118.52, 1980
1, 00000000:2A:00.0, 40, 117.10, 1980
2, 00000000:3A:00.0, 42, 120.03, 1980
3, 00000000:5D:00.0, 39, 115.77, 1980
4, 00000000:9A:00.0, 41, 119.40, 1980
5, 00000000:AB:00.0, 40, 116.88, 1980
6, 00000000:BA:00.0, 43, 121.65, 1980
7, 00000000:DB:00.0, 41, 118.02, 1980
`

// script returns a Runner that answers nvidia-smi and the benchmark from
// queues, one answer per call.
type script struct {
	smi   []string
	bench []string
}

func (s *script) run(_ context.Context, name string, _ ...string) (string, error) {
	q := &s.smi
	if name != "nvidia-smi" {
		q = &s.bench
	}
	if len(*q) == 0 {
		return "", errors.New("script exhausted")
	}
	out := (*q)[0]
	*q = (*q)[1:]
	if strings.HasPrefix(out, "ERR:") {
		return "", errors.New(strings.TrimPrefix(out, "ERR:"))
	}
	return out, nil
}

func newProbe(t *testing.T, s *script, bench bool) (*Probe, string, *time.Time) {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	path := filepath.Join(t.TempDir(), "tenant.jsonl")
	w, err := evidence.OpenWriter(path, priv)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 3, 4, 0, 0, 0, time.UTC)
	cfg := Config{Node: "gpu-003", ExpectedGPUs: 8, Writer: w, Run: s.run, Now: func() time.Time { return now }, Interval: time.Minute}
	if bench {
		cfg.BenchCmd = []string{"python3", "gemm_bench.py"}
		cfg.BenchFloor = 600
		cfg.BenchEvery = time.Minute
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p, path, &now
}

func eventsIn(t *testing.T, path string) []events.Event {
	t.Helper()
	entries, err := evidence.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []events.Event
	for _, e := range entries {
		if e.Kind == evidence.KindEvent {
			var ev events.Event
			if err := e.DecodePayload(&ev); err != nil {
				t.Fatal(err)
			}
			out = append(out, ev)
		}
	}
	return out
}

func TestGPUVisibilityHysteresis(t *testing.T) {
	seven := strings.Join(strings.Split(eightGPUs, "\n")[:7], "\n")
	// good, bad, bad, bad (incident opens at the first bad sample), good, good (closes at the first good one)
	s := &script{smi: []string{eightGPUs, seven, "ERR:NVIDIA-SMI has failed", seven, eightGPUs, eightGPUs}}
	p, path, now := newProbe(t, s, false)
	for i := 0; i < 6; i++ {
		if err := p.Cycle(context.Background()); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(time.Minute)
	}
	evs := eventsIn(t, path)
	if len(evs) != 2 {
		t.Fatalf("events %+v", evs)
	}
	if evs[0].Type != events.Down || evs[0].TS != "2026-10-03T04:01:00Z" || evs[0].Cause != "tenant:gpu-missing:7/8" || evs[0].Source != "tenant-probe" {
		t.Fatalf("down event %+v", evs[0])
	}
	if evs[1].Type != events.Up || evs[1].TS != "2026-10-03T04:04:00Z" {
		t.Fatalf("up event %+v", evs[1])
	}
	if st := p.Stats(); st.Heartbeats != 6 || st.Observations != 3 || p.Down() {
		t.Fatalf("stats %+v down=%v", st, p.Down())
	}
	// Heartbeats carry the GPU count when the query worked.
	entries, _ := evidence.ReadFile(path)
	var hbs []gaps.Heartbeat
	for _, e := range entries {
		if e.Kind == evidence.KindHeartbeat {
			var hb gaps.Heartbeat
			_ = e.DecodePayload(&hb)
			hbs = append(hbs, hb)
		}
	}
	if len(hbs) != 6 || *hbs[1].GPUs != 7 || hbs[2].GPUs != nil {
		t.Fatalf("heartbeats %+v", hbs)
	}
}

func TestSingleBlipDoesNotOpenIncident(t *testing.T) {
	s := &script{smi: []string{eightGPUs, "ERR:timeout", eightGPUs, eightGPUs}}
	p, path, now := newProbe(t, s, false)
	for i := 0; i < 4; i++ {
		if err := p.Cycle(context.Background()); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(time.Minute)
	}
	if evs := eventsIn(t, path); len(evs) != 0 {
		t.Fatalf("a single failed query opened an incident: %+v", evs)
	}
}

func TestBenchmarkFloor(t *testing.T) {
	s := &script{
		smi:   []string{eightGPUs, eightGPUs, eightGPUs, eightGPUs, eightGPUs, eightGPUs, eightGPUs},
		bench: []string{"gemm bf16: 712.4 TFLOPS\n712.4\n", "580.0", "ERR:CUDA error", "575.5", "590", "701", "705.2"},
	}
	p, path, now := newProbe(t, s, true)
	for i := 0; i < 7; i++ {
		if err := p.Cycle(context.Background()); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(time.Minute)
	}
	evs := eventsIn(t, path)
	// Below floor at 04:01, (04:02 failed: neither), 04:03, 04:04 -> third bad sample opens at 04:01.
	// Above floor at 04:05 and 04:06 -> closes at 04:05.
	if len(evs) != 2 || evs[0].Type != events.DegradedStart || evs[0].TS != "2026-10-03T04:01:00Z" || evs[0].Cause != "tenant:bench-below-floor:590<600" {
		t.Fatalf("events %+v", evs)
	}
	if evs[1].Type != events.DegradedEnd || evs[1].TS != "2026-10-03T04:05:00Z" {
		t.Fatalf("end event %+v", evs[1])
	}
	if st := p.Stats(); st.BenchRuns != 7 {
		t.Fatalf("stats %+v", st)
	}
}

func TestAMDVisibilityFromSysfs(t *testing.T) {
	root := t.TempDir()
	drv := filepath.Join(root, "bus", "pci", "drivers", "amdgpu")
	for _, n := range []string{"0000:05:00.0", "0000:65:00.0"} {
		if err := os.MkdirAll(filepath.Join(drv, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	w, _ := evidence.OpenWriter(filepath.Join(t.TempDir(), "t.jsonl"), priv)
	p, err := New(Config{Node: "mi355-02", Vendor: "amd", ExpectedGPUs: 2, SysRoot: root, Writer: w})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := p.Stats(); st.Observations != 0 || st.Heartbeats != 1 {
		t.Fatalf("stats %+v", st)
	}
}

func TestHelpers(t *testing.T) {
	if v, ok := LastNumber("warmup 1.5\nresult: 712.25 TFLOPS"); !ok || v != 712.25 {
		t.Fatalf("got %v %v", v, ok)
	}
	if _, ok := LastNumber("no numbers here"); ok {
		t.Fatal("found a number in text without one")
	}
	s := ParseNvidiaSMI("0, 00000000:18:00.0, 41, 118.52, 1980\nUnable to determine the device handle for GPU 0000:2A:00.0: Unknown Error\n")
	if s.GPUs != 1 || s.Rows[0].PCI != "00000000:18:00.0" {
		t.Fatalf("got %+v", s)
	}
	if _, err := New(Config{Node: "n", ExpectedGPUs: 8, Writer: &evidence.Writer{}, Vendor: "intel"}); err == nil {
		t.Fatal("unknown vendor accepted")
	}
}
