package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/Sievehouse/gpu-sentinel/internal/contract"
	"github.com/Sievehouse/gpu-sentinel/internal/events"
	"github.com/Sievehouse/gpu-sentinel/internal/evidence"
	"github.com/Sievehouse/gpu-sentinel/internal/gaps"
	"github.com/Sievehouse/gpu-sentinel/internal/keyring"
)

func cmdEventEmit(args []string, s streams) error {
	fs := newFlags("event emit", s)
	logPath := fs.String("log", "", "evidence log to append to")
	keyPath := fs.String("key", "", "private signing key (PEM)")
	keyDir := fs.String("key-dir", "", "directory holding the signing key; created on first use (instead of --key)")
	role := fs.String("role", "provider-integration", "role announced with a new key")
	node := fs.String("node", "", "node name as listed in the contract")
	typ := fs.String("type", "", "down | up | degraded_start | degraded_end | maintenance_start | maintenance_end")
	cause := fs.String("cause", "", "cause, e.g. xid:79, network:rail-3, customer:oom, repaired")
	ts := fs.String("ts", "", "event time (RFC 3339); default now")
	notice := fs.String("notice-ts", "", "for maintenance_start: when the window was announced (RFC 3339)")
	source := fs.String("source", "manual", "who or what produced the event")
	note := fs.String("note", "", "free-text note")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := required("node", *node, "type", *typ); err != nil {
		return err
	}
	priv, err := signingKey(*keyPath, *keyDir, s)
	if err != nil {
		return err
	}
	w, err := openLog(*logPath, priv, "", *role)
	if err != nil {
		return err
	}
	if *ts == "" {
		*ts = time.Now().UTC().Format(time.RFC3339)
	}
	// Seq is the evidence sequence number, so events written in the same
	// second keep their recorded order (ids alone would sort "-10" before "-9").
	ev := events.Event{
		ID: fmt.Sprintf("%s-%d", w.KeyID(), w.NextSeq()), Seq: w.NextSeq(), TS: *ts, Node: *node, Type: events.Type(*typ),
		Cause: *cause, NoticeTS: *notice, Source: *source, Note: *note,
	}
	if err := ev.Normalize(); err != nil {
		return err
	}
	entry, err := w.Append(evidence.KindEvent, ev)
	if err != nil {
		return err
	}
	fmt.Fprintf(s.out, "appended event %s (seq %d, hash %s)\n", ev.ID, entry.Seq, entry.Hash)
	return nil
}

// logData is one evidence log read into memory.
type logData struct {
	path    string
	entries []evidence.Entry
	report  evidence.Report
}

// readLogs reads every log once and, unless kr is nil, verifies the very
// bytes it parses against the keyring (signatures, chain, validity windows
// and node bindings). Any problem stops the command.
func readLogs(paths []string, kr *keyring.Keyring, s streams) ([]logData, error) {
	if len(paths) == 0 {
		return nil, usagef("--log is required")
	}
	var out []logData
	failed := false
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		ld := logData{path: p}
		if kr != nil {
			ld.report = evidence.VerifyWith(bytes.NewReader(raw), kr.PublicKeys(), kr.EntryCheck())
			ld.report.Path = p
			if !ld.report.OK {
				failed = true
				for _, pr := range ld.report.Problems {
					fmt.Fprintf(s.err, "%s: %s\n", p, pr)
				}
			}
		}
		if ld.entries, err = evidence.ReadEntries(bytes.NewReader(raw)); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, ld)
	}
	if failed {
		return nil, codedError{1, "evidence verification failed"}
	}
	return out, nil
}

// trustOrSkip returns the keyring to verify with, or nil with --no-verify.
func trustOrSkip(noVerify bool, keyringPath string, pubs []string, s streams) (*keyring.Keyring, error) {
	if noVerify {
		fmt.Fprintln(s.err, "warning: --no-verify: signatures and keyring rules are not checked; do not use the output for a contract")
		return nil, nil
	}
	return trustFrom(keyringPath, pubs)
}

func eventsFromLogs(logs []logData) ([]events.Event, error) {
	var all []events.Event
	for _, ld := range logs {
		for _, e := range ld.entries {
			if e.Kind != evidence.KindEvent {
				continue
			}
			var ev events.Event
			if err := e.DecodePayload(&ev); err != nil {
				return nil, fmt.Errorf("%s seq %d: %w", ld.path, e.Seq, err)
			}
			if ev.Evidence == "" {
				ev.Evidence = e.Hash
			}
			if err := ev.Normalize(); err != nil {
				return nil, fmt.Errorf("%s seq %d: %w", ld.path, e.Seq, err)
			}
			all = append(all, ev)
		}
	}
	return all, nil
}

// gapFlags are the gap-detection flags shared by "events export" and
// "evidence gaps".
type gapFlags struct {
	role, period, since, until, contract *string
}

func addGapFlags(fs interface {
	String(name, value, usage string) *string
}) gapFlags {
	return gapFlags{
		role:     fs.String("gap-role", "", "count only heartbeats from this role: agent or tenant-probe (default: any)"),
		period:   fs.String("period", "", "window: calendar month in UTC, e.g. 2026-10"),
		since:    fs.String("since", "", "window start (RFC 3339), instead of --period"),
		until:    fs.String("until", "", "window end, exclusive (RFC 3339)"),
		contract: fs.String("contract", "", "contract whose nodes must all send heartbeats (YAML or JSON)"),
	}
}

func findGaps(logs []logData, maxSilence time.Duration, g gapFlags) ([]gaps.Gap, error) {
	if maxSilence < time.Second {
		return nil, usagef("the tolerated silence must be at least 1s")
	}
	since, until, err := parseWindow(*g.period, *g.since, *g.until)
	if err != nil {
		return nil, err
	}
	var nodes []string
	if *g.contract != "" {
		spec, err := contract.Load(*g.contract)
		if err != nil {
			return nil, err
		}
		nodes = spec.SortedNodes()
	}
	var beats []gaps.Beat
	for _, ld := range logs {
		bs, err := gaps.BeatsFromEntries(ld.entries, nil) // node bindings were checked during verification
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ld.path, err)
		}
		for _, b := range bs {
			if *g.role == "" || b.Role == *g.role {
				beats = append(beats, b)
			}
		}
	}
	return gaps.Find(beats, gaps.Options{MaxSilence: int64(maxSilence / time.Second), Since: since, Until: until, Nodes: nodes})
}

func cmdEventsExport(args []string, s streams) error {
	fs := newFlags("events export", s)
	var logs, pubs stringList
	fs.Var(&logs, "log", "evidence log; repeat for several logs")
	fs.Var(&pubs, "pub", "trusted public key (PEM); repeat as needed")
	keyringPath := fs.String("keyring", "", "keyring of trusted keys, with node bindings and validity windows")
	noVerify := fs.Bool("no-verify", false, "skip verification (never for contract use)")
	derive := fs.Duration("derive-gaps", 0, "also export unmonitored time: heartbeat silences longer than this, e.g. 5m")
	g := addGapFlags(fs)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	kr, err := trustOrSkip(*noVerify, *keyringPath, pubs, s)
	if err != nil {
		return err
	}
	data, err := readLogs(logs, kr, s)
	if err != nil {
		return err
	}
	all, err := eventsFromLogs(data)
	if err != nil {
		return err
	}
	if *derive > 0 {
		gs, err := findGaps(data, *derive, g)
		if err != nil {
			return err
		}
		all = append(all, gaps.Events(gs, "capledger-gaps")...)
		fmt.Fprintf(s.err, "derived %d gap(s) longer than %s\n", len(gs), *derive)
	} else if *g.role != "" || *g.period != "" || *g.since != "" || *g.until != "" || *g.contract != "" {
		return usagef("--gap-role, --period, --since, --until and --contract only apply with --derive-gaps")
	}
	if err := checkUniqueIDs(all); err != nil {
		return err
	}
	events.Sort(all)
	return writeJSONLines(s.out, all)
}

func cmdEvidenceGaps(args []string, s streams) error {
	fs := newFlags("evidence gaps", s)
	var logs, pubs stringList
	fs.Var(&logs, "log", "evidence log; repeat for several logs")
	fs.Var(&pubs, "pub", "trusted public key (PEM); repeat as needed")
	keyringPath := fs.String("keyring", "", "keyring of trusted keys")
	noVerify := fs.Bool("no-verify", false, "skip verification")
	maxSilence := fs.Duration("max-silence", 5*time.Minute, "longest tolerated time between heartbeats")
	format := fs.String("format", "text", "text or json")
	g := addGapFlags(fs)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := checkFormat(*format); err != nil {
		return err
	}
	kr, err := trustOrSkip(*noVerify, *keyringPath, pubs, s)
	if err != nil {
		return err
	}
	data, err := readLogs(logs, kr, s)
	if err != nil {
		return err
	}
	gs, err := findGaps(data, *maxSilence, g)
	if err != nil {
		return err
	}
	if *format == "json" {
		if gs == nil {
			gs = []gaps.Gap{}
		}
		return writeJSON(s.out, gs)
	}
	if len(gs) == 0 {
		fmt.Fprintf(s.out, "no silences longer than %s\n", *maxSilence)
		return nil
	}
	tw := tabwriter.NewWriter(s.out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tSTART\tEND\tSECONDS\tREASON")
	var total int64
	for _, gp := range gs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", gp.Node, gp.From, gp.To, gp.Seconds, gp.Reason)
		total += gp.Seconds
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(s.out, "%d gap(s), %d node-seconds unmonitored\n", len(gs), total)
	return nil
}

func cmdEvidenceVerify(args []string, s streams) error {
	fs := newFlags("evidence verify", s)
	var logs, pubs stringList
	fs.Var(&logs, "log", "evidence log; repeat for several logs")
	fs.Var(&pubs, "pub", "trusted public key (PEM); repeat as needed")
	keyringPath := fs.String("keyring", "", "keyring of trusted keys, with node bindings and validity windows")
	format := fs.String("format", "json", "json or text")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := checkFormat(*format); err != nil {
		return err
	}
	if len(logs) == 0 {
		return usagef("--log is required")
	}
	kr, err := trustFrom(*keyringPath, pubs)
	if err != nil {
		return err
	}
	ok := true
	reps := make([]evidence.Report, 0, len(logs))
	for _, p := range logs {
		rep := evidence.VerifyFileWith(p, kr.PublicKeys(), kr.EntryCheck())
		ok = ok && rep.OK
		reps = append(reps, rep)
	}
	if *format == "json" {
		if err := writeJSON(s.out, reps); err != nil {
			return err
		}
	} else {
		for _, r := range reps {
			status := "OK"
			if !r.OK {
				status = "FAILED"
			}
			fmt.Fprintf(s.out, "%s  %s  %d entries, keys %v, merkle root %s\n", r.Path, status, r.Entries, r.KeyIDs, r.MerkleRoot)
			for _, p := range r.Problems {
				fmt.Fprintf(s.out, "  %s\n", p)
			}
		}
	}
	if !ok {
		return codedError{1, "evidence verification failed"}
	}
	return nil
}

func cmdEvidenceAppend(args []string, s streams) error {
	fs := newFlags("evidence append", s)
	logPath := fs.String("log", "", "evidence log")
	keyPath := fs.String("key", "", "private signing key (PEM)")
	keyDir := fs.String("key-dir", "", "directory holding the signing key; created on first use (instead of --key)")
	kind := fs.String("kind", "", "entry kind, e.g. statement, acceptance-report, test-result")
	payloadPath := fs.String("payload", "", "JSON file to record, or - for stdin")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := required("kind", *kind, "payload", *payloadPath); err != nil {
		return err
	}
	if *kind == evidence.KindKey {
		return usagef("key announcements are written automatically; use a different kind")
	}
	var raw []byte
	var err error
	if *payloadPath == "-" {
		raw, err = io.ReadAll(s.in)
	} else {
		raw, err = os.ReadFile(*payloadPath)
	}
	if err != nil {
		return err
	}
	if !json.Valid(raw) {
		return fmt.Errorf("payload is not valid JSON")
	}
	priv, err := signingKey(*keyPath, *keyDir, s)
	if err != nil {
		return err
	}
	w, err := openLog(*logPath, priv, "", "")
	if err != nil {
		return err
	}
	entry, err := w.Append(*kind, json.RawMessage(raw))
	if err != nil {
		return err
	}
	fmt.Fprintf(s.out, "appended %s entry (seq %d, hash %s)\n", *kind, entry.Seq, entry.Hash)
	return nil
}
