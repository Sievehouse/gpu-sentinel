// Command capledger is the Capacity Ledger command-line tool: the provider
// agent, the tenant probe, the SLA engine, evidence logs, keyrings and
// signed evidence bundles in one static binary.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Sievehouse/gpu-sentinel/internal/events"
	"github.com/Sievehouse/gpu-sentinel/internal/sla"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "0.2.0"

func main() { os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }

const usageText = `capledger: contract-grade acceptance and SLA evidence for GPU clusters

Usage:
  capledger <command> [flags]

Collect
  agent             provider side: watch kernel logs (NVIDIA XID/SXID, AMD amdgpu),
                    GPU counts and AMD RAS counters; write signed evidence and heartbeats
  probe             tenant side: check that every rented GPU is visible (and, optionally,
                    a benchmark floor); write signed evidence and heartbeats

SLA
  sla compute       compute a sealed SLA statement from a contract and events
  sla verify        check a statement's seal, or replay it from its inputs
  sla corroborate   compare the downtime two independent sources recorded
  contract check    validate a contract spec (YAML or JSON) and print its digest

Evidence
  event emit        append one signed availability event (for integrations and operators)
  events export     extract events from evidence logs; --derive-gaps adds unmonitored time
  evidence verify   verify hash chains, signatures and keyring rules of evidence logs
  evidence append   append a signed entry (for example a statement) to a log
  evidence gaps     list stretches without heartbeats
  bundle create     pack files into a signed evidence bundle (.tar.gz)
  bundle verify     verify a bundle and optionally extract it

Keys
  keygen            create an Ed25519 signing key pair
  keyring add       trust a public key: owner, role, nodes it may speak for, validity
  keyring import    trust keys announced in an evidence log, bound to their nodes
  keyring list      show the keys in a keyring

Other
  xid parse         parse kernel log lines and show how the fault policy classifies them
  xid policy        print the built-in starter fault policy
  version           print the version

Run "capledger <command> -h" for the flags of a command.
`

// streams are the command's standard input and outputs.
type streams struct {
	in       io.Reader
	out, err io.Writer
}

type commandFunc func(args []string, s streams) error

var commands = map[string]commandFunc{
	"agent":           cmdAgent,
	"probe":           cmdProbe,
	"sla compute":     cmdSLACompute,
	"sla verify":      cmdSLAVerify,
	"sla corroborate": cmdSLACorroborate,
	"contract check":  cmdContractCheck,
	"event emit":      cmdEventEmit,
	"events export":   cmdEventsExport,
	"evidence verify": cmdEvidenceVerify,
	"evidence append": cmdEvidenceAppend,
	"evidence gaps":   cmdEvidenceGaps,
	"bundle create":   cmdBundleCreate,
	"bundle verify":   cmdBundleVerify,
	"keygen":          cmdKeygen,
	"keyring add":     cmdKeyringAdd,
	"keyring import":  cmdKeyringImport,
	"keyring list":    cmdKeyringList,
	"xid parse":       cmdXIDParse,
	"xid policy":      cmdXIDPolicy,
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return 2
	}
	switch args[0] {
	case "version", "--version":
		fmt.Fprintf(stdout, "capledger %s (sla engine %s)\n", version, sla.EngineVersion)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usageText)
		return 0
	}
	s := streams{in: stdin, out: stdout, err: stderr}
	var fn commandFunc
	var rest []string
	if len(args) > 1 {
		if f, ok := commands[args[0]+" "+args[1]]; ok {
			fn, rest = f, args[2:]
		}
	}
	if fn == nil {
		if f, ok := commands[args[0]]; ok {
			fn, rest = f, args[1:]
		}
	}
	if fn == nil {
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", strings.Join(args[:min(2, len(args))], " "), usageText)
		return 2
	}
	err := fn(rest, s)
	if err == nil {
		return 0
	}
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	var ce codedError
	if errors.As(err, &ce) {
		fmt.Fprintln(stderr, "capledger:", ce.msg)
		return ce.code
	}
	var ue usageError
	if errors.As(err, &ue) {
		fmt.Fprintln(stderr, "capledger:", ue.msg)
		return 2
	}
	fmt.Fprintln(stderr, "capledger:", err)
	return 1
}

// codedError carries a specific exit code (1 = verification failed).
type codedError struct {
	code int
	msg  string
}

func (e codedError) Error() string { return e.msg }

// usageError is a mistake in the command line (exit code 2).
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usagef(format string, a ...any) error { return usageError{fmt.Sprintf(format, a...)} }

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func newFlags(name string, s streams) *flag.FlagSet {
	fs := flag.NewFlagSet("capledger "+name, flag.ContinueOnError)
	fs.SetOutput(s.err)
	return fs
}

// parseFlags parses args and rejects stray positional arguments, which are
// almost always a forgotten "--".
func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return usageError{err.Error()}
	}
	if fs.NArg() > 0 {
		return usagef("unexpected argument %q (flags start with --)", fs.Arg(0))
	}
	return nil
}

func required(pairs ...string) error {
	for i := 0; i+1 < len(pairs); i += 2 {
		if strings.TrimSpace(pairs[i+1]) == "" {
			return usagef("--%s is required", pairs[i])
		}
	}
	return nil
}

func parsePeriod(period, start, end string) (sla.Period, error) {
	if period != "" {
		t, err := time.Parse("2006-01", period)
		if err != nil {
			return sla.Period{}, usagef("--period must look like 2026-10: %v", err)
		}
		return sla.Period{Start: t.Unix(), End: t.AddDate(0, 1, 0).Unix()}, nil
	}
	if start == "" || end == "" {
		return sla.Period{}, usagef("give --period YYYY-MM or both --start and --end (RFC 3339)")
	}
	s, err := events.ParseTime(start)
	if err != nil {
		return sla.Period{}, usagef("--start: %v", err)
	}
	e, err := events.ParseTime(end)
	if err != nil {
		return sla.Period{}, usagef("--end: %v", err)
	}
	p := sla.Period{Start: s, End: e}
	return p, p.Validate()
}

// parseWindow is parsePeriod with optional ends (0 = open).
func parseWindow(period, since, until string) (int64, int64, error) {
	if period != "" {
		if since != "" || until != "" {
			return 0, 0, usagef("give --period or --since/--until, not both")
		}
		p, err := parsePeriod(period, "", "")
		return p.Start, p.End, err
	}
	var a, b int64
	var err error
	if since != "" {
		if a, err = events.ParseTime(since); err != nil {
			return 0, 0, usagef("--since: %v", err)
		}
	}
	if until != "" {
		if b, err = events.ParseTime(until); err != nil {
			return 0, 0, usagef("--until: %v", err)
		}
	}
	if a > 0 && b > 0 && b <= a {
		return 0, 0, usagef("--until must be after --since")
	}
	return a, b, nil
}

func loadEvents(paths []string) ([]events.Event, error) {
	var all []events.Event
	for _, p := range paths {
		evs, err := events.ReadFile(p)
		if err != nil {
			return nil, err
		}
		all = append(all, evs...)
	}
	if err := checkUniqueIDs(all); err != nil {
		return nil, err
	}
	events.Sort(all)
	return all, nil
}

func checkUniqueIDs(evs []events.Event) error {
	seen := make(map[string]bool, len(evs))
	for _, e := range evs {
		if seen[e.ID] {
			return fmt.Errorf("event id %q appears more than once", e.ID)
		}
		seen[e.ID] = true
	}
	return nil
}

func writeJSON(w io.Writer, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

func writeJSONLines(w io.Writer, evs []events.Event) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, ev := range evs {
		if err := enc.Encode(ev); err != nil {
			return err
		}
	}
	return nil
}

func checkFormat(f string) error {
	if f != "json" && f != "text" {
		return usagef("--format must be json or text, not %q", f)
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
