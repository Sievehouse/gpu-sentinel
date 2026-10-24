package gaps

import (
	"reflect"
	"testing"

	"github.com/Sievehouse/gpu-sentinel/internal/events"
)

const t0 = 1790812800 // 2026-10-01T00:00:00Z

func beats(node string, interval int64, times ...int64) []Beat {
	var out []Beat
	for _, s := range times {
		out = append(out, Beat{Node: node, TS: t0 + s, Interval: interval})
	}
	return out
}

func TestSilenceBetweenBeats(t *testing.T) {
	bs := beats("n1", 60, 0, 60, 120, 600, 660)
	got, err := Find(bs, Options{MaxSilence: 300})
	if err != nil {
		t.Fatal(err)
	}
	// Silence from 120 to 600: the gap starts when the next beat was due (180).
	want := []Gap{{Node: "n1", Start: t0 + 180, End: t0 + 600, From: "2026-10-01T00:03:00Z", To: "2026-10-01T00:10:00Z", Seconds: 420, Reason: ReasonSilence}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
	// Exactly MaxSilence apart is not a gap.
	if got, _ := Find(beats("n1", 60, 0, 300), Options{MaxSilence: 300}); len(got) != 0 {
		t.Fatalf("boundary flagged: %+v", got)
	}
}

func TestWindowEdgesAndSilentNodes(t *testing.T) {
	bs := append(beats("n1", 60, 1000, 1060), beats("n2", 60, 0, 60)...)
	got, err := Find(bs, Options{MaxSilence: 300, Since: t0, Until: t0 + 3600, Nodes: []string{"n1", "n2", "n3"}})
	if err != nil {
		t.Fatal(err)
	}
	type key struct {
		node       string
		start, end int64
		reason     string
	}
	var keys []key
	for _, g := range got {
		keys = append(keys, key{g.Node, g.Start - t0, g.End - t0, g.Reason})
	}
	want := []key{
		{"n1", 0, 1000, ReasonBeforeFirst},
		{"n1", 1120, 3600, ReasonAfterLast},
		{"n2", 120, 3600, ReasonAfterLast},
		{"n3", 0, 3600, ReasonNoHeartbeats},
	}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("got %+v\nwant %+v", keys, want)
	}
}

func TestGapsAreClippedToWindow(t *testing.T) {
	bs := beats("n1", 0, -1000, 2000)
	got, _ := Find(bs, Options{MaxSilence: 300, Since: t0, Until: t0 + 1500})
	if len(got) != 1 || got[0].Start != t0 || got[0].End != t0+1500 {
		t.Fatalf("got %+v", got)
	}
}

func TestEventsAreDeterministicAndValid(t *testing.T) {
	gs, _ := Find(beats("n1", 60, 0, 600), Options{MaxSilence: 300})
	evs := Events(gs, "gap-detector")
	if len(evs) != 2 || evs[0].ID != "gap:n1:2026-10-01T00:01:00Z" || evs[1].ID != "gap:n1:2026-10-01T00:01:00Z:end" {
		t.Fatalf("got %+v", evs)
	}
	if err := events.NormalizeAll(evs); err != nil {
		t.Fatal(err)
	}
	if evs[0].Type != events.UnmonitoredStart || evs[1].Type != events.UnmonitoredEnd {
		t.Fatal("wrong event types")
	}
}

func TestOptionsValidation(t *testing.T) {
	if _, err := Find(nil, Options{}); err == nil {
		t.Fatal("zero MaxSilence accepted")
	}
	if _, err := Find(nil, Options{MaxSilence: 1, Since: 10, Until: 5}); err == nil {
		t.Fatal("inverted window accepted")
	}
}
