package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Sievehouse/gpu-sentinel/internal/keyring"
	"github.com/Sievehouse/gpu-sentinel/internal/probe"
)

func cmdProbe(args []string, s streams) error {
	fs := newFlags("probe", s)
	hostname, _ := os.Hostname()
	node := fs.String("node", hostname, "node name as listed in the contract")
	vendor := fs.String("vendor", "nvidia", "GPU vendor: nvidia or amd")
	expected := fs.Int("expected-gpus", 0, "GPUs this node must show (required)")
	interval := fs.Duration("interval", time.Minute, "time between checks; also the heartbeat interval")
	failAfter := fs.Int("fail-after", 3, "consecutive bad checks before a down event")
	recoverAfter := fs.Int("recover-after", 2, "consecutive good checks before an up event")
	queryTimeout := fs.Duration("query-timeout", 30*time.Second, "timeout for one nvidia-smi query")
	benchCmd := fs.String("bench-cmd", "", "optional benchmark command, split on spaces; its last printed number is the result")
	benchFloor := fs.Float64("bench-floor", 0, "lowest acceptable benchmark result; below it for --fail-after runs is a degraded event")
	benchEvery := fs.Duration("bench-every", 10*time.Minute, "time between benchmark runs")
	benchTimeout := fs.Duration("bench-timeout", 5*time.Minute, "benchmark timeout")
	sysRoot := fs.String("sys-root", "/sys", "sysfs root, for AMD GPU counts")
	source := fs.String("source", "tenant-probe", "recorded as the source of events")
	logPath := fs.String("log", "", "the tenant's evidence log")
	keyPath := fs.String("key", "", "private signing key (PEM)")
	keyDir := fs.String("key-dir", "", "directory for the probe's own signing key, created on first use (instead of --key)")
	cycles := fs.Int("cycles", 0, "stop after this many checks (0 = run until stopped)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *expected < 1 {
		return usagef("--expected-gpus is required")
	}
	if *interval < time.Second && *cycles == 0 {
		return usagef("--interval must be at least 1s")
	}
	if *cycles < 0 {
		return usagef("--cycles cannot be negative")
	}
	priv, err := signingKey(*keyPath, *keyDir, s)
	if err != nil {
		return err
	}
	w, err := openLog(*logPath, priv, *node, keyring.RoleTenantProbe)
	if err != nil {
		return err
	}
	p, err := probe.New(probe.Config{
		Node: *node, Vendor: *vendor, ExpectedGPUs: *expected, FailAfter: *failAfter, RecoverAfter: *recoverAfter,
		Interval: *interval, QueryTimeout: *queryTimeout,
		BenchCmd: strings.Fields(*benchCmd), BenchFloor: *benchFloor, BenchEvery: *benchEvery, BenchTimeout: *benchTimeout,
		SysRoot: *sysRoot, Source: *source, Writer: w, Log: s.err,
	})
	if err != nil {
		return usageError{err.Error()}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(s.err, "capledger probe %s: node %s, vendor %s, expecting %d GPUs, key %s\n", version, *node, *vendor, *expected, w.KeyID())

	t := time.NewTicker(max(*interval, time.Millisecond))
	defer t.Stop()
	for i := 1; ; i++ {
		// A check runs to completion even if a stop signal arrives meanwhile,
		// so that shutting the probe down is never recorded as a failed check.
		if err := p.Cycle(context.Background()); err != nil {
			return err
		}
		if (*cycles > 0 && i >= *cycles) || !wait(ctx, t.C) {
			break
		}
	}
	st := p.Stats()
	state := "up"
	if p.Down() {
		state = "down"
	}
	fmt.Fprintf(s.err, "probe stopped after %d checks: %d heartbeats, %d observations, %d events, %d benchmark runs; node is %s\n",
		st.Cycles, st.Heartbeats, st.Observations, st.Events, st.BenchRuns, state)
	return nil
}

// wait blocks until the next tick and reports false if ctx ended first.
func wait(ctx context.Context, tick <-chan time.Time) bool {
	select {
	case <-ctx.Done():
		return false
	case <-tick:
		return true
	}
}
