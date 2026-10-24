package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/Sievehouse/capacity-ledger/internal/contract"
	"github.com/Sievehouse/capacity-ledger/internal/sla"
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
	var eventPaths stringList
	fs.Var(&eventPaths, "events", "events file; repeat for several files")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := required("statement", *stmtPath); err != nil {
		return err
	}
	st, err := sla.ReadStatement(*stmtPath)
	if err != nil {
		return err
	}
	if *contractPath == "" && len(eventPaths) == 0 {
		if err := st.CheckSeal(); err != nil {
			return codedError{1, err.Error()}
		}
		fmt.Fprintf(s.out, "seal OK: %s\n(pass --contract and --events to replay the computation as well)\n", st.StatementSHA256)
		return nil
	}
	if *contractPath == "" || len(eventPaths) == 0 {
		return usagef("replay needs both --contract and --events")
	}
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
	return nil
}

func cmdSLACorroborate(args []string, s streams) error {
	fs := newFlags("sla corroborate", s)
	contractPath := fs.String("contract", "", "contract spec (YAML or JSON)")
	var aPaths, bPaths stringList
	fs.Var(&aPaths, "events-a", "events recorded by side A (usually the provider); repeat for several files")
	fs.Var(&bPaths, "events-b", "events recorded by side B (usually the tenant probe); repeat for several files")
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
	if len(aPaths) == 0 || len(bPaths) == 0 {
		return usagef("--events-a and --events-b are both required")
	}
	p, err := parsePeriod(*period, *start, *end)
	if err != nil {
		return err
	}
	spec, err := contract.Load(*contractPath)
	if err != nil {
		return err
	}
	evsA, err := loadEvents(aPaths)
	if err != nil {
		return fmt.Errorf("%s: %w", *labelA, err)
	}
	evsB, err := loadEvents(bPaths)
	if err != nil {
		return fmt.Errorf("%s: %w", *labelB, err)
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
	fmt.Fprintf(s.out, "digest      %s\n", digest)
	return nil
}

func orDash(v string) string {
	if v == "" {
		return "-"
	}
	return v
}
