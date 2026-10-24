// Package events defines the availability events the SLA engine consumes and
// loads them from JSON Lines.
//
// An event is one transition for one node: it went down, came back, became
// degraded, recovered, or entered or left maintenance. Timestamps have
// one-second resolution; sub-second parts are truncated.
package events

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Sievehouse/gpu-sentinel/internal/canonical"
)

// Type is the kind of availability transition.
type Type string

// Event types understood by the SLA engine.
const (
	Down             Type = "down"
	Up               Type = "up"
	DegradedStart    Type = "degraded_start"
	DegradedEnd      Type = "degraded_end"
	MaintenanceStart Type = "maintenance_start"
	MaintenanceEnd   Type = "maintenance_end"
	// UnmonitoredStart and UnmonitoredEnd bound a stretch with no heartbeat
	// from a node's monitoring. They are derived from heartbeats by
	// "capledger events export --derive-gaps", not written by hand.
	UnmonitoredStart Type = "unmonitored_start"
	UnmonitoredEnd   Type = "unmonitored_end"
)

// Valid reports whether t is a known event type.
func (t Type) Valid() bool {
	switch t {
	case Down, Up, DegradedStart, DegradedEnd, MaintenanceStart, MaintenanceEnd, UnmonitoredStart, UnmonitoredEnd:
		return true
	}
	return false
}

// Event is one availability transition for one node.
type Event struct {
	ID       string `json:"id"`
	TS       string `json:"ts"`
	Seq      int64  `json:"seq,omitempty"`
	Node     string `json:"node"`
	Type     Type   `json:"type"`
	Cause    string `json:"cause,omitempty"`
	NoticeTS string `json:"noticeTs,omitempty"`
	Source   string `json:"source,omitempty"`
	Evidence string `json:"evidence,omitempty"`
	Note     string `json:"note,omitempty"`

	unix      int64
	notice    int64
	hasNotice bool
}

// Unix returns the event time in Unix seconds. It is only meaningful after
// Normalize has succeeded.
func (e *Event) Unix() int64 { return e.unix }

// Notice returns when a maintenance window was announced, if known.
func (e *Event) Notice() (int64, bool) { return e.notice, e.hasNotice }

// Normalize validates the event, parses its timestamps and rewrites them in
// canonical form (RFC 3339, UTC, whole seconds).
func (e *Event) Normalize() error {
	if strings.TrimSpace(e.ID) == "" {
		return errors.New("event without id")
	}
	if strings.TrimSpace(e.Node) == "" {
		return fmt.Errorf("event %s: missing node", e.ID)
	}
	if !e.Type.Valid() {
		return fmt.Errorf("event %s: unknown type %q", e.ID, e.Type)
	}
	t, err := ParseTime(e.TS)
	if err != nil {
		return fmt.Errorf("event %s: ts: %w", e.ID, err)
	}
	e.unix = t
	e.TS = FormatTime(t)
	e.notice, e.hasNotice = 0, false
	if e.NoticeTS != "" {
		n, err := ParseTime(e.NoticeTS)
		if err != nil {
			return fmt.Errorf("event %s: noticeTs: %w", e.ID, err)
		}
		e.notice, e.hasNotice = n, true
		e.NoticeTS = FormatTime(n)
	}
	return nil
}

// ParseTime parses an RFC 3339 timestamp and returns Unix seconds.
func ParseTime(s string) (int64, error) {
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(s))
	if err != nil {
		return 0, err
	}
	return t.Unix(), nil
}

// FormatTime formats Unix seconds as RFC 3339 in UTC.
func FormatTime(u int64) string {
	return time.Unix(u, 0).UTC().Format(time.RFC3339)
}

// Sort orders normalised events by time, then seq, then id, so processing
// never depends on input order. Producers that emit several events for one
// node in the same second should set seq to make the order explicit.
func Sort(evs []Event) {
	sort.SliceStable(evs, func(i, j int) bool {
		a, b := evs[i], evs[j]
		if a.unix != b.unix {
			return a.unix < b.unix
		}
		if a.Seq != b.Seq {
			return a.Seq < b.Seq
		}
		return a.ID < b.ID
	})
}

// NormalizeAll normalises every event and rejects duplicate ids.
func NormalizeAll(evs []Event) error {
	seen := make(map[string]bool, len(evs))
	for i := range evs {
		if err := evs[i].Normalize(); err != nil {
			return err
		}
		if seen[evs[i].ID] {
			return fmt.Errorf("duplicate event id %q", evs[i].ID)
		}
		seen[evs[i].ID] = true
	}
	return nil
}

// ReadJSONL reads events from JSON Lines. Blank lines and lines starting with
// '#' are skipped. Unknown fields are rejected so that a typo cannot silently
// change an SLA calculation. The result is normalised and sorted.
func ReadJSONL(r io.Reader) ([]Event, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var out []Event
	seen := map[string]int{}
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(text))
		dec.DisallowUnknownFields()
		var e Event
		if err := dec.Decode(&e); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if err := e.Normalize(); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if first, dup := seen[e.ID]; dup {
			return nil, fmt.Errorf("line %d: duplicate event id %q (first seen on line %d)", line, e.ID, first)
		}
		seen[e.ID] = line
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	Sort(out)
	return out, nil
}

// ReadFile reads events from a JSON Lines file.
func ReadFile(path string) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	evs, err := ReadJSONL(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return evs, nil
}

// Digest returns the SHA-256 of the canonical encoding of the sorted events.
// Events must already be normalised.
func Digest(evs []Event) (string, error) {
	cp := append([]Event(nil), evs...)
	Sort(cp)
	if cp == nil {
		cp = []Event{}
	}
	return canonical.SHA256Hex(cp)
}
