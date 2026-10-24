package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sievehouse/capacity-ledger/internal/events"
	"github.com/Sievehouse/capacity-ledger/internal/evidence"
	"github.com/Sievehouse/capacity-ledger/internal/gaps"
	"github.com/Sievehouse/capacity-ledger/internal/xid"
)

func setup(t *testing.T) (*Agent, string, map[string]ed25519.PublicKey) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	path := filepath.Join(t.TempDir(), "evidence.jsonl")
	w, err := evidence.OpenWriter(path, priv)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := xid.DefaultPolicy()
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(Config{
		Node: "gpu-003", Policy: policy, Writer: w, Suppress: 10 * time.Minute,
		Now: func() time.Time { return time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return a, path, map[string]ed25519.PublicKey{evidence.KeyID(pub): pub}
}

func TestSampleKernelLog(t *testing.T) {
	a, path, keys := setup(t)
	f, err := os.Open("../../examples/samples/kern.log")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := a.Run(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	st := a.Stats()
	// Two XID 79 lines (the second suppressed), one XID 94, one SXID.
	if st.Observations != 4 || st.Events != 1 || st.Suppressed != 1 {
		t.Fatalf("unexpected stats %+v", st)
	}
	rep := evidence.VerifyFile(path, keys)
	if !rep.OK || rep.Entries != 5 {
		t.Fatalf("log should verify with 5 entries: %+v", rep)
	}
	lf, _ := os.Open(path)
	defer lf.Close()
	entries, _ := evidence.ReadEntries(lf)
	var ev events.Event
	for _, e := range entries {
		if e.Kind == "event" {
			if err := e.DecodePayload(&ev); err != nil {
				t.Fatal(err)
			}
		}
	}
	if ev.Type != events.Down || ev.Cause != "xid:79" || ev.TS != "2026-10-03T04:11:58Z" || ev.Evidence == "" {
		t.Fatalf("unexpected event %+v", ev)
	}
}

func TestGPUCountDrop(t *testing.T) {
	a, _, _ := setup(t)
	if err := a.HandleGPUCount(8, 8); err != nil {
		t.Fatal(err)
	}
	if err := a.HandleGPUCount(7, 8); err != nil {
		t.Fatal(err)
	}
	if st := a.Stats(); st.Events != 1 || st.Observations != 1 {
		t.Fatalf("unexpected stats %+v", st)
	}
	if CountGPUs("0, 00000000:18:00.0\n1, 00000000:2A:00.0\n\n") != 2 {
		t.Fatal("CountGPUs miscounted")
	}
}

func TestFollowFileReadsAppendedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kern.log")
	if err := os.WriteFile(path, []byte("old line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got := make(chan string, 4)
	go func() {
		_ = FollowFile(ctx, path, false, 20*time.Millisecond, func(s string) error { got <- s; return nil })
	}()
	time.Sleep(100 * time.Millisecond)
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("new line\n")
	f.Close()
	select {
	case s := <-got:
		if s != "new line" {
			t.Fatalf("got %q, want the appended line only", s)
		}
	case <-ctx.Done():
		t.Fatal("appended line was not read")
	}
}

// Heartbeats written by the agent are what gap detection reads back.
func TestHeartbeatsFeedGapDetection(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	path := filepath.Join(t.TempDir(), "evidence.jsonl")
	w, err := evidence.OpenWriter(path, priv)
	if err != nil {
		t.Fatal(err)
	}
	policy, _ := xid.DefaultPolicy()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	a, err := New(Config{Node: "gpu-003", Policy: policy, Writer: w, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.HandleGPUCount(8, 8); err != nil {
		t.Fatal(err)
	}
	for _, offset := range []time.Duration{0, time.Minute, 2 * time.Minute, 30 * time.Minute} {
		now = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC).Add(offset)
		if err := a.Heartbeat(time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if a.Stats().Heartbeats != 4 {
		t.Fatalf("stats %+v", a.Stats())
	}
	if rep := evidence.VerifyFile(path, map[string]ed25519.PublicKey{evidence.KeyID(pub): pub}); !rep.OK {
		t.Fatalf("log does not verify: %v", rep.Problems)
	}
	entries, _ := evidence.ReadFile(path)
	var hb gaps.Heartbeat
	if err := entries[0].DecodePayload(&hb); err != nil || hb.GPUs == nil || *hb.GPUs != 8 || hb.IntervalSeconds != 60 {
		t.Fatalf("heartbeat payload %+v (%v)", hb, err)
	}
	beats, err := gaps.BeatsFromEntries(entries, nil)
	if err != nil {
		t.Fatal(err)
	}
	found, err := gaps.Find(beats, gaps.Options{MaxSilence: 300})
	if err != nil {
		t.Fatal(err)
	}
	// Silent from 12:03 (next beat due) to 12:30.
	if len(found) != 1 || found[0].From != "2026-10-03T12:03:00Z" || found[0].To != "2026-10-03T12:30:00Z" {
		t.Fatalf("gaps %+v", found)
	}
}

func TestAMDKernelLogAndRAS(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	path := filepath.Join(t.TempDir(), "evidence.jsonl")
	w, err := evidence.OpenWriter(path, priv)
	if err != nil {
		t.Fatal(err)
	}
	policy, _ := xid.DefaultPolicy()
	// Three minutes after the uncorrectable error in the sample log.
	now := time.Date(2026, 10, 7, 11, 5, 45, 0, time.UTC)
	a, err := New(Config{Node: "mi355-02", Policy: policy, Writer: w, Suppress: 10 * time.Minute, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]ed25519.PublicKey{evidence.KeyID(pub): pub}
	f, err := os.Open("../../examples/samples/kern-amd.log")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := a.Run(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	// Six classified lines; only the uncorrectable RAS error opens an incident.
	if st := a.Stats(); st.Observations != 6 || st.Events != 1 {
		t.Fatalf("stats %+v", st)
	}
	baseline := []xid.RASCounter{{Device: "0000:05:00.0", Card: "card1", Block: "umc", UE: 1, CE: 3}}
	if err := a.HandleRAS(baseline); err != nil {
		t.Fatal(err)
	}
	later := []xid.RASCounter{{Device: "0000:05:00.0", Card: "card1", Block: "umc", UE: 2, CE: 3}}
	if err := a.HandleRAS(later); err != nil {
		t.Fatal(err)
	}
	// The counter increase is an observation; its down event is suppressed
	// because the node went down less than the suppression window ago.
	if st := a.Stats(); st.Observations != 7 || st.Events != 1 || st.Suppressed != 1 {
		t.Fatalf("stats after RAS %+v", st)
	}
	if rep := evidence.VerifyFile(path, keys); !rep.OK {
		t.Fatal(rep.Problems)
	}
	entries, _ := evidence.ReadFile(path)
	var ev events.Event
	for _, e := range entries {
		if e.Kind == evidence.KindEvent {
			_ = e.DecodePayload(&ev)
		}
	}
	if ev.Cause != "amdgpu:ras_uncorrectable:umc" || ev.TS != "2026-10-07T11:02:45Z" {
		t.Fatalf("event %+v", ev)
	}
}
