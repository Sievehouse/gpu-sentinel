package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Sievehouse/gpu-sentinel/internal/contract"
	"github.com/Sievehouse/gpu-sentinel/internal/events"
	"github.com/Sievehouse/gpu-sentinel/internal/evidence"
	"github.com/Sievehouse/gpu-sentinel/internal/k8s"
	"github.com/Sievehouse/gpu-sentinel/internal/keyring"
)

const defaultWatcherKeyDir = "/var/lib/capledger/watcher-keys"

func cmdWatchKubernetes(args []string, s streams) error {
	fs := newFlags("watch kubernetes", s)
	from := fs.String("from", "", "where to read nodes: in-cluster (the pod's service account), kubectl, or a file saved from \"kubectl get nodes -o json\" (default: in-cluster inside a pod, otherwise kubectl)")
	kubectl := fs.String("kubectl", "kubectl", "kubectl binary, for --from kubectl")
	kubeContext := fs.String("context", "", "kubeconfig context, for --from kubectl")
	apiServer := fs.String("api-server", "", "API server URL instead of the in-cluster one (with --token-file and --ca-file)")
	tokenFile := fs.String("token-file", "", "bearer token file, for --api-server")
	caFile := fs.String("ca-file", "", "CA certificate file, for --api-server (default: system roots)")
	selector := fs.String("selector", "", "label selector, e.g. nvidia.com/gpu.present=true (not applied to files)")
	contractPath := fs.String("contract", "", "record only the contract's nodes, and count contract nodes missing from the cluster as down")
	nameLabel := fs.String("name-label", "", "node label whose value is the contract's node name (default: the Kubernetes node name)")
	notReady := fs.Bool("not-ready", true, "a node whose Ready condition is not True is down")
	cordoned := fs.Bool("cordoned", false, "a cordoned (unschedulable) node is down")
	var downConds, degradedConds, downTaints, degradedTaints stringList
	fs.Var(&downConds, "down-condition", "condition type, or pattern such as '*' (path.Match), whose status True means down; repeatable. '*' covers NVSentinel and node-problem-detector conditions; kubelet conditions must be named exactly")
	fs.Var(&degradedConds, "degraded-condition", "condition type or pattern whose status True means degraded; repeatable; an exact name beats a pattern")
	fs.Var(&downTaints, "down-taint", "taint key or pattern, such as 'nvidia.com/gpu-*', whose presence means down; repeatable")
	fs.Var(&degradedTaints, "degraded-taint", "taint key or pattern whose presence means degraded; repeatable")
	expected := fs.Int("expected-gpus", 0, "if > 0, a node with fewer allocatable GPUs is down")
	gpuResource := fs.String("gpu-resource", "", "extended resource that counts GPUs (default: nvidia.com/gpu or amd.com/gpu, whichever the node reports)")
	gpusDegrade := fs.Bool("gpus-degrade", false, "with --expected-gpus: missing GPUs make the node degraded instead of down")
	interval := fs.Duration("interval", 30*time.Second, "time between views of the cluster")
	once := fs.Bool("once", false, "take one view, record the transitions and exit")
	at := fs.String("at", "", "with --once: when the view was taken (RFC 3339), for replaying saved views; default now")
	logPath := fs.String("log", "", "evidence log to append to (the watcher's own)")
	keyPath := fs.String("key", "", "private signing key (PEM)")
	keyDir := fs.String("key-dir", "", "directory for the watcher's own signing key, created on first use (default "+defaultWatcherKeyDir+" unless --key is given)")
	source := fs.String("source", "kubernetes-node-watcher", "source name recorded in events")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *interval < time.Second {
		return usagef("--interval must be at least 1s")
	}
	now := func() int64 { return time.Now().Unix() }
	if *at != "" {
		if !*once {
			return usagef("--at goes with --once")
		}
		t, err := events.ParseTime(*at)
		if err != nil {
			return usagef("--at: %v", err)
		}
		now = func() int64 { return t }
	}
	rules := k8s.Rules{
		NotReady: *notReady, Cordoned: *cordoned,
		DownConditions: downConds, DegradedConditions: degradedConds,
		DownTaints: downTaints, DegradedTaints: degradedTaints,
		ExpectedGPUs: *expected, GPUResource: *gpuResource, GPUsDegrade: *gpusDegrade,
		NameLabel: *nameLabel,
	}
	if err := rules.Validate(); err != nil {
		return usagef("%v", err)
	}
	src, err := nodeSource(*from, *kubectl, *kubeContext, *apiServer, *tokenFile, *caFile, *selector)
	if err != nil {
		return err
	}
	var scope []string
	if *contractPath != "" {
		spec, err := contract.Load(*contractPath)
		if err != nil {
			return err
		}
		scope = spec.SortedNodes()
	}
	watcher, err := k8s.NewWatcher(rules, scope)
	if err != nil {
		return err
	}
	if *keyPath == "" && *keyDir == "" {
		*keyDir = defaultWatcherKeyDir
	}
	priv, err := signingKey(*keyPath, *keyDir, s)
	if err != nil {
		return err
	}
	w, err := openLog(*logPath, priv, "", keyring.RoleProviderIntegration)
	if err != nil {
		return err
	}
	if err := resumeWatcher(watcher, *logPath, w.KeyID()); err != nil {
		return err
	}
	down, degraded := watcher.Open()
	fmt.Fprintf(s.err, "capledger watch kubernetes %s: %s, key %s, %d node(s) open as down and %d as degraded from the log\n",
		version, src, w.KeyID(), len(down), len(degraded))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	poll := func() error {
		list, err := src.Nodes(ctx)
		if err != nil {
			// A failed view is not evidence that nodes are down; the
			// watcher's silence shows in its log's timestamps.
			return err
		}
		trs, err := watcher.Step(list, now())
		if err != nil {
			return err
		}
		for _, tr := range trs {
			ev := events.Event{
				ID: fmt.Sprintf("%s-%d", w.KeyID(), w.NextSeq()), Seq: w.NextSeq(), TS: events.FormatTime(tr.TS),
				Node: tr.Node, Type: tr.Type, Cause: tr.Cause, Note: tr.Note, Source: *source,
			}
			if err := ev.Normalize(); err != nil {
				return err
			}
			if _, err := w.Append(evidence.KindEvent, ev); err != nil {
				return err
			}
			fmt.Fprintf(s.out, "%s %s %s %s\n", ev.TS, ev.Node, ev.Type, ev.Cause)
		}
		return nil
	}
	if *once {
		return poll()
	}
	t := time.NewTicker(*interval)
	defer t.Stop()
	for {
		if err := poll(); err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			fmt.Fprintf(s.err, "%s: %v\n", time.Now().UTC().Format(time.RFC3339), err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func nodeSource(from, kubectl, kubeContext, apiServer, tokenFile, caFile, selector string) (k8s.Source, error) {
	if apiServer != "" {
		if from != "" {
			return nil, usagef("give --from or --api-server, not both")
		}
		return k8s.NewAPISource(apiServer, tokenFile, caFile, selector)
	}
	if tokenFile != "" || caFile != "" {
		return nil, usagef("--token-file and --ca-file go with --api-server")
	}
	if from == "" {
		from = "kubectl"
		if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
			from = "in-cluster"
		}
	}
	switch from {
	case "in-cluster":
		return k8s.InCluster(selector)
	case "kubectl":
		return k8s.KubectlSource{Bin: kubectl, Context: kubeContext, Selector: selector}, nil
	}
	return k8s.FileSource{Path: from}, nil
}

// resumeWatcher restores the watcher's state from the events its own key
// wrote to the log.
func resumeWatcher(w *k8s.Watcher, path, keyID string) error {
	entries, err := evidence.ReadFile(path)
	if err != nil {
		return err
	}
	own := entries[:0]
	for _, e := range entries {
		if e.KeyID == keyID {
			own = append(own, e)
		}
	}
	return w.Resume(own)
}
