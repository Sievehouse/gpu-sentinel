package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Sievehouse/gpu-sentinel/internal/contract"
	"github.com/Sievehouse/gpu-sentinel/internal/events"
	"github.com/Sievehouse/gpu-sentinel/internal/keyring"
	"github.com/Sievehouse/gpu-sentinel/internal/sla"
)

func cmdSLACompute(args []string, s streams) error {
	fs := newFlags("sla compute", s)
	contractPath := fs.String("contract", "", "contract spec (YAML or JSON)")
	var eventPaths stringList
	fs.Var(&eventPaths, "events", "events file (JSON Lines); repeat for several files")
	period := fs.String("period", "", "calendar month in UTC, e.g. 2026-10")
	start := fs.String("start", "", "period start (RFC 3339), instead of --period")
	end := fs.String("end", "", "period end, exclusive (RFC 3339)")
	out := fs.String("out", "", "write the statement JSON to this file")
	format := fs.String("format", "json", "stdout format: json or text")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := required("contract", *contractPath); err != nil {
		return err
	}
	if err := checkFormat(*format); err != nil {
		return err
	}
	if len(eventPaths) == 0 {
		return usagef("--events is required")
	}
	p, err := parsePeriod(*period, *start, *end)
	if err != nil {
		return err
	}
	spec, err := contract.Load(*contractPath)
	if err != nil {
		return err
	}
	evs, err := loadEvents(eventPaths)
	if err != nil {
		return err
	}
	st, err := sla.Compute(spec, evs, p)
	if err != nil {
		return err
	}
	pretty, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	pretty = append(pretty, '\n')
	if *out != "" {
		if err := os.WriteFile(*out, pretty, 0o644); err != nil {
			return err
		}
	}
	switch {
	case *format == "text":
		_, err = io.WriteString(s.out, st.Text())
	case *out == "":
		_, err = s.out.Write(pretty)
	default:
		fmt.Fprintf(s.out, "wrote %s (statementSha256 %s)\n", *out, st.StatementSHA256)
	}
	return err
}

func cmdSLAVerify(args []string, s streams) error {
	fs := newFlags("sla verify", s)
	stmtPath := fs.String("statement", "", "statement JSON to check")
	contractPath := fs.String("contract", "", "contract spec; with --events, replay the computation")
	var eventPaths, sigPaths stringList
	fs.Var(&eventPaths, "events", "events file; repeat for several files")
	fs.Var(&sigPaths, "signature", "statement signature (from sla sign); repeat for several parties")
	keyringPath := fs.String("keyring", "", "keyring whose roles say who may sign as which party (needed with --signature)")
	require := fs.String("require", "", "parties that must all have accepted, comma-separated, e.g. provider,customer")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := required("statement", *stmtPath); err != nil {
		return err
	}
	parties := splitList(*require)
	if (len(sigPaths) > 0 || len(parties) > 0) && *keyringPath == "" {
		return usagef("--signature and --require need --keyring: the keyring says which key may sign as which party")
	}
	st, err := sla.ReadStatement(*stmtPath)
	if err != nil {
		return err
	}
	switch {
	case *contractPath == "" && len(eventPaths) == 0:
		if err := st.CheckSeal(); err != nil {
			return codedError{1, err.Error()}
		}
		fmt.Fprintf(s.out, "seal OK: %s\n", st.StatementSHA256)
		if len(sigPaths) == 0 && len(parties) == 0 {
			fmt.Fprintln(s.out, "(pass --contract and --events to replay the computation as well)")
		}
	case *contractPath == "" || len(eventPaths) == 0:
		return usagef("replay needs both --contract and --events")
	default:
		spec, err := contract.Load(*contractPath)
		if err != nil {
			return err
		}
		evs, err := loadEvents(eventPaths)
		if err != nil {
			return err
		}
		if err := sla.Replay(spec, evs, st); err != nil {
			return codedError{1, "replay failed: " + err.Error()}
		}
		fmt.Fprintf(s.out, "replay OK: same inputs, same statement %s (engine semantics %s)\n", st.StatementSHA256, st.EngineVersion)
	}
	if len(sigPaths) == 0 && len(parties) == 0 {
		return nil
	}
	kr, err := keyring.Load(*keyringPath)
	if err != nil {
		return err
	}
	sigs := make([]*sla.Signature, 0, len(sigPaths))
	for _, p := range sigPaths {
		sg, err := sla.ReadSignature(p)
		if err != nil {
			return err
		}
		sigs = append(sigs, sg)
	}
	rep := sla.CheckCoSigned(st, sigs, sigPaths, kr.SignerAt, parties)
	for _, r := range rep.Signatures {
		status := "OK"
		if !r.Valid {
			status = "INVALID: " + r.Problem
		}
		note := ""
		if r.Note != "" {
			note = " \"" + r.Note + "\""
		}
		fmt.Fprintf(s.out, "%-10s %-8s key %s at %s%s  %s\n", r.Party, r.Decision, r.KeyID, r.SignedAt, note, status)
	}
	if len(parties) > 0 {
		fmt.Fprintf(s.out, "required %s: accepted by %s; disputed by %s; missing %s\n", strings.Join(parties, ", "),
			orNone(rep.AcceptedBy), orNone(rep.DisputedBy), orNone(rep.Missing))
	}
	if !rep.OK {
		return codedError{1, "statement is not co-signed as required"}
	}
	if len(parties) > 0 {
		fmt.Fprintf(s.out, "co-signed: every required party accepted statement %s\n", st.StatementSHA256)
	}
	return nil
}

func cmdSLASign(args []string, s streams) error {
	fs := newFlags("sla sign", s)
	stmtPath := fs.String("statement", "", "sealed statement JSON to sign")
	party := fs.String("party", "", "the party you sign for; must be your key's keyring role: provider, customer, financier ...")
	decision := fs.String("decision", sla.DecisionAccept, "accept or dispute")
	note := fs.String("note", "", "what is disputed, or any remark (required for a dispute)")
	keyPath := fs.String("key", "", "private signing key (PEM)")
	keyDir := fs.String("key-dir", "", "directory holding the party's signing key (created on first use)")
	out := fs.String("out", "", "signature file (default: STATEMENT.PARTY.sig.json next to the statement)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := required("statement", *stmtPath, "party", *party); err != nil {
		return err
	}
	st, err := sla.ReadStatement(*stmtPath)
	if err != nil {
		return err
	}
	priv, err := signingKey(*keyPath, *keyDir, s)
	if err != nil {
		return err
	}
	sg, err := sla.Sign(st, *party, *decision, *note, priv, time.Now())
	if err != nil {
		return err
	}
	if *out == "" {
		*out = strings.TrimSuffix(*stmtPath, ".json") + "." + *party + ".sig.json"
	}
	b, err := json.MarshalIndent(sg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, append(b, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(s.out, "signed statement %s as %s (%s) with key %s: %s\n", short(st.StatementSHA256), *party, *decision, sg.KeyID, *out)
	return nil
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func orNone(v []string) string {
	if len(v) == 0 {
		return "none"
	}
	return strings.Join(v, ", ")
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func cmdSLACorroborate(args []string, s streams) error {
	fs := newFlags("sla corroborate", s)
	contractPath := fs.String("contract", "", "contract spec (YAML or JSON)")
	var aPaths, bPaths, bothPaths stringList
	fs.Var(&aPaths, "events-a", "events recorded by side A (usually the provider); repeat for several files")
	fs.Var(&bPaths, "events-b", "events recorded by side B (usually the tenant probe); repeat for several files")
	fs.Var(&bothPaths, "events", "both parties' events in one set, split by the contract's sources (instead of --events-a and --events-b)")
	labelA := fs.String("label-a", "provider", "name of side A")
	labelB := fs.String("label-b", "tenant", "name of side B")
	period := fs.String("period", "", "calendar month in UTC, e.g. 2026-10")
	start := fs.String("start", "", "period start (RFC 3339), instead of --period")
	end := fs.String("end", "", "period end, exclusive (RFC 3339)")
	out := fs.String("out", "", "write the report JSON to this file")
	format := fs.String("format", "text", "stdout format: text or json")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := required("contract", *contractPath); err != nil {
		return err
	}
	if err := checkFormat(*format); err != nil {
		return err
	}
	split := len(bothPaths) > 0
	switch {
	case split && (len(aPaths) > 0 || len(bPaths) > 0):
		return usagef("give --events, or --events-a and --events-b, not both")
	case !split && (len(aPaths) == 0 || len(bPaths) == 0):
		return usagef("--events-a and --events-b are both required (or --events with a contract that lists sources)")
	}
	p, err := parsePeriod(*period, *start, *end)
	if err != nil {
		return err
	}
	spec, err := contract.Load(*contractPath)
	if err != nil {
		return err
	}
	var evsA, evsB []events.Event
	if split {
		all, err := loadEvents(bothPaths)
		if err != nil {
			return err
		}
		if evsA, evsB, err = sla.SplitBySide(spec, all); err != nil {
			return err
		}
		*labelA, *labelB = contract.SideProvider, contract.SideTenant
	} else {
		if evsA, err = loadEvents(aPaths); err != nil {
			return fmt.Errorf("%s: %w", *labelA, err)
		}
		if evsB, err = loadEvents(bPaths); err != nil {
			return fmt.Errorf("%s: %w", *labelB, err)
		}
	}
	c, err := sla.Corroborate(spec, evsA, evsB, *labelA, *labelB, p)
	if err != nil {
		return err
	}
	if *out != "" {
		b, err := json.MarshalIndent(c, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(*out, append(b, '\n'), 0o644); err != nil {
			return err
		}
	}
	if *format == "json" {
		return writeJSON(s.out, c)
	}
	_, err = io.WriteString(s.out, c.Text())
	return err
}

func cmdContractCheck(args []string, s streams) error {
	fs := newFlags("contract check", s)
	contractPath := fs.String("contract", "", "contract spec (YAML or JSON)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := required("contract", *contractPath); err != nil {
		return err
	}
	spec, err := contract.Load(*contractPath)
	if err != nil {
		return codedError{1, err.Error()}
	}
	digest, err := spec.Digest()
	if err != nil {
		return err
	}
	c := spec.Contract
	unmonitored := c.Measurement.Unmonitored
	if unmonitored == "" {
		unmonitored = contract.UnmonitoredReport + " (default)"
	}
	fmt.Fprintf(s.out, "contract    %s (version %d)\n", c.Metadata.ID, c.Metadata.Version)
	fmt.Fprintf(s.out, "nodes       %d x %d GPUs, %d rack(s)\n", len(c.Cluster.Nodes), c.Cluster.GPUsPerNode, len(c.Cluster.Racks))
	fmt.Fprintf(s.out, "credits     %d tier(s), monthly fee %s %s\n", len(c.Credits.Tiers), orDash(c.Credits.MonthlyFee), c.Credits.Currency)
	fmt.Fprintf(s.out, "unmonitored %s\n", unmonitored)
	if src := c.Sources; src != nil {
		unlisted := src.Unlisted
		if unlisted == "" {
			unlisted = contract.UnlistedReject + " (default)"
		}
		fmt.Fprintf(s.out, "sources     provider %s; tenant %s; unlisted %s\n", sideText(src.Provider), sideText(src.Tenant), unlisted)
	} else {
		fmt.Fprintf(s.out, "sources     any (no sources block)\n")
	}
	if r := c.Measurement.Reconcile; r != nil {
		fmt.Fprintf(s.out, "reconcile   count %s, start %s, end %s\n", r.Count, r.Start, r.End)
	}
	fmt.Fprintf(s.out, "digest      %s\n", digest)
	return nil
}

func sideText(side *contract.SourceSide) string {
	if side == nil {
		return "-"
	}
	var parts []string
	if len(side.Roles) > 0 {
		parts = append(parts, "roles "+strings.Join(side.Roles, ","))
	}
	if len(side.Sources) > 0 {
		parts = append(parts, "sources "+strings.Join(side.Sources, ","))
	}
	return strings.Join(parts, " + ")
}

func orDash(v string) string {
	if v == "" {
		return "-"
	}
	return v
}
