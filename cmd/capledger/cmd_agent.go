package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Sievehouse/gpu-sentinel/internal/agent"
	"github.com/Sievehouse/gpu-sentinel/internal/keyring"
	"github.com/Sievehouse/gpu-sentinel/internal/xid"
)

func cmdAgent(args []string, s streams) error {
	fs := newFlags("agent", s)
	hostname, _ := os.Hostname()
	node := fs.String("node", hostname, "node name as listed in the contract")
	vendor := fs.String("vendor", "nvidia", "GPU vendor: nvidia or amd")
	source := fs.String("source", "kmsg", "kmsg (/dev/kmsg), a log file path, or - for stdin")
	once := fs.Bool("once", false, "read the source to the end and exit (replay mode: no heartbeats or polling)")
	fromStart := fs.Bool("from-start", false, "when following, start at the beginning instead of the end")
	logPath := fs.String("log", "", "evidence log to append to")
	keyPath := fs.String("key", "", "private signing key (PEM)")
	keyDir := fs.String("key-dir", "", "directory for this node's own signing key, created on first use (default "+defaultAgentKeyDir+" unless --key is given)")
	policyPath := fs.String("policy", "", "fault policy JSON (default: built-in starter policy)")
	suppress := fs.Duration("suppress", 10*time.Minute, "minimum gap between down events for this node")
	expected := fs.Int("expected-gpus", 0, "if > 0, count GPUs every --smi-interval and record a down event when fewer are visible")
	smiEvery := fs.Duration("smi-interval", time.Minute, "GPU count interval (nvidia-smi, or sysfs for AMD)")
	heartbeat := fs.Duration("heartbeat", time.Minute, "heartbeat interval; 0 disables heartbeats, so gap detection sees the node as unmonitored")
	rasEvery := fs.Duration("ras-interval", time.Minute, "AMD only: RAS error counter interval; 0 disables")
	sysRoot := fs.String("sys-root", "/sys", "sysfs root, for AMD GPU counts and RAS counters")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *vendor != "nvidia" && *vendor != "amd" {
		return usagef("--vendor must be nvidia or amd")
	}
	if *heartbeat != 0 && *heartbeat < time.Second {
		return usagef("--heartbeat must be at least 1s (or 0 to disable)")
	}
	if *keyPath == "" && *keyDir == "" {
		*keyDir = defaultAgentKeyDir
	}
	priv, err := signingKey(*keyPath, *keyDir, s)
	if err != nil {
		return err
	}
	w, err := openLog(*logPath, priv, *node, keyring.RoleProviderAgent)
	if err != nil {
		return err
	}
	policy, err := loadPolicy(*policyPath)
	if err != nil {
		return err
	}
	a, err := agent.New(agent.Config{Node: *node, Policy: policy, Writer: w, Suppress: *suppress, Log: s.err})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(s.err, "capledger agent %s: node %s, vendor %s, key %s, policy %s, source %s\n", version, *node, *vendor, w.KeyID(), policy.Version, *source)
	if !*once {
		if *expected > 0 {
			go every(ctx, *smiEvery, func() {
				n, err := countGPUs(ctx, *vendor, *sysRoot)
				if err != nil {
					fmt.Fprintf(s.err, "GPU count failed: %v\n", err)
					return
				}
				if err := a.HandleGPUCount(n, *expected); err != nil {
					fmt.Fprintf(s.err, "recording GPU count failed: %v\n", err)
				}
			})
		}
		if *vendor == "amd" && *rasEvery > 0 {
			go every(ctx, *rasEvery, func() {
				readings, err := xid.ReadRASCounters(*sysRoot)
				if err != nil {
					fmt.Fprintf(s.err, "reading RAS counters: %v\n", err)
				}
				if err := a.HandleRAS(readings); err != nil {
					fmt.Fprintf(s.err, "recording RAS counters failed: %v\n", err)
				}
			})
		}
		if *heartbeat > 0 {
			go every(ctx, *heartbeat, func() {
				if err := a.Heartbeat(*heartbeat); err != nil {
					fmt.Fprintf(s.err, "writing heartbeat failed: %v\n", err)
				}
			})
		}
	}

	switch {
	case *source == "-":
		err = a.Run(ctx, s.in)
	case *once:
		f, ferr := os.Open(*source)
		if ferr != nil {
			return ferr
		}
		err = a.Run(ctx, f)
		f.Close()
	case *source == "kmsg":
		err = agent.FollowKmsg(ctx, *fromStart, a.HandleLine)
	default:
		err = agent.FollowFile(ctx, *source, *fromStart, 500*time.Millisecond, a.HandleLine)
	}
	st := a.Stats()
	fmt.Fprintf(s.err, "processed %d lines: %d observations, %d events, %d suppressed, %d heartbeats\n", st.Lines, st.Observations, st.Events, st.Suppressed, st.Heartbeats)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// every runs fn at once and then every d until ctx ends.
func every(ctx context.Context, d time.Duration, fn func()) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		fn()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// countGPUs is a coarse liveness check: a GPU that falls off the bus usually
// disappears from nvidia-smi, or from the amdgpu driver's device list. When
// nvidia-smi fails but still lists some GPUs, the listed ones are counted;
// when it lists none, the failure is reported as an error rather than as
// zero GPUs, because it may come from the tool rather than the hardware.
func countGPUs(ctx context.Context, vendor, sysRoot string) (int, error) {
	if vendor == "amd" {
		return xid.CountAMDGPUs(sysRoot)
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "nvidia-smi", "--query-gpu=index,pci.bus_id", "--format=csv,noheader").Output()
	n := agent.CountGPUs(string(out))
	var exitErr *exec.ExitError
	if err != nil && !(errors.As(err, &exitErr) && n > 0) {
		return 0, fmt.Errorf("nvidia-smi: %v", err)
	}
	return n, nil
}

func cmdXIDParse(args []string, s streams) error {
	fs := newFlags("xid parse", s)
	policyPath := fs.String("policy", "", "fault policy JSON (default: built-in starter policy)")
	file := fs.String("file", "", "kernel log file (default: stdin)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	policy, err := loadPolicy(*policyPath)
	if err != nil {
		return err
	}
	in := s.in
	if *file != "" {
		f, err := os.Open(*file)
		if err != nil {
			return err
		}
		defer f.Close()
		in = f
	}
	data, err := io.ReadAll(in)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(s.out)
	enc.SetEscapeHTML(false)
	for _, line := range strings.Split(string(data), "\n") {
		o, ok := xid.ParseLine(line)
		if !ok {
			continue
		}
		r := policy.Classify(o)
		if err := enc.Encode(map[string]any{"observation": o, "action": r.Action, "summary": r.Summary}); err != nil {
			return err
		}
	}
	return nil
}

func cmdXIDPolicy(args []string, s streams) error {
	fs := newFlags("xid policy", s)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	_, err := s.out.Write(xid.DefaultPolicyJSON())
	return err
}

func loadPolicy(path string) (xid.Policy, error) {
	if path == "" {
		return xid.DefaultPolicy()
	}
	return xid.LoadPolicy(path)
}
