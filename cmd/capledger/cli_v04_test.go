package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Sievehouse/gpu-sentinel/internal/evidence"
	"github.com/Sievehouse/gpu-sentinel/internal/ledger"
)

const (
	nodesFault = "../../examples/kubernetes/nodes-2026-10-03T0415.json"
	nodesOK    = "../../examples/kubernetes/nodes-2026-10-03T0645.json"
)

func TestWatchKubernetes(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "k8s.jsonl")
	keyDir := filepath.Join(dir, "keys")
	kr := filepath.Join(dir, "keyring.json")
	watch := func(view, at string) string {
		return mustRun(t, "watch", "kubernetes", "--from", view, "--at", at, "--once", "--down-condition", "*", "--expected-gpus", "8",
			"--log", logPath, "--key-dir", keyDir)
	}
	out := watch(nodesFault, "2026-10-03T04:15:00Z")
	for _, want := range []string{
		"2026-10-03T04:12:00Z gpu-003 down k8s:NotReady:GpuHealthCheckFailed+k8s:SysLogsXIDError:79",
		"2026-10-03T04:15:00Z gpu-004 down k8s:gpus:7/8",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("first view: missing %q in\n%s", want, out)
		}
	}
	// A restart with the same view repeats nothing.
	if out := watch(nodesFault, "2026-10-03T04:20:00Z"); !strings.Contains(out, "2 node(s) open as down") || strings.Contains(out, "Z gpu-") {
		t.Fatalf("repeated view recorded:\n%s", out)
	}
	out = watch(nodesOK, "2026-10-03T06:45:00Z")
	if !strings.Contains(out, "2026-10-03T06:40:00Z gpu-003 up k8s:healthy") || !strings.Contains(out, "2026-10-03T06:45:00Z gpu-004 up k8s:healthy") {
		t.Fatalf("repair view:\n%s", out)
	}
	// The watcher speaks for every node, like the Slurm watcher.
	mustRun(t, "keyring", "import", "--keyring", kr, "--log", logPath, "--owner", "Example Neocloud", "--role", "provider-integration", "--bind-nodes=false")
	evs := jsonLines(mustRun(t, "events", "export", "--log", logPath, "--keyring", kr, "--signers"))
	if len(evs) != 4 {
		t.Fatalf("want 4 events, got:\n%s", strings.Join(evs, "\n"))
	}
	for _, l := range evs {
		if !strings.Contains(l, `"signerRole":"provider-integration"`) || !strings.Contains(l, `"source":"kubernetes-node-watcher"`) {
			t.Fatalf("event: %s", l)
		}
	}
	// Scoped to the contract, nodes the view does not list are down.
	scoped := filepath.Join(dir, "scoped.jsonl")
	out = mustRun(t, "watch", "kubernetes", "--from", nodesOK, "--at", "2026-10-03T06:45:00Z", "--once", "--contract", yamlContract,
		"--log", scoped, "--key-dir", keyDir)
	if n := strings.Count(out, "k8s:node-missing"); n != 13 {
		t.Fatalf("want 13 missing nodes, got %d:\n%s", n, out)
	}
	wantExit(t, 2, "watch", "kubernetes", "--from", nodesOK, "--at", "2026-10-03T06:45:00Z", "--log", scoped, "--key-dir", keyDir)
	wantExit(t, 2, "watch", "kubernetes", "--from", nodesOK, "--once", "--down-condition", "[", "--log", scoped, "--key-dir", keyDir)
	wantExit(t, 2, "watch", "kubernetes", "--from", "kubectl", "--api-server", "https://x", "--once", "--log", scoped, "--key-dir", keyDir)
	wantExit(t, 1, "watch", "kubernetes", "--from", filepath.Join(dir, "missing.json"), "--once", "--log", scoped, "--key-dir", keyDir)
}

func TestAgentRedacts(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct{ vendor, sample, secret string }{
		{"nvidia", "../../examples/samples/kern.log", "python3"},
		{"amd", "../../examples/samples/kern-amd.log", "python3"},
	} {
		logPath := filepath.Join(dir, c.vendor+".jsonl")
		mustRun(t, "agent", "--vendor", c.vendor, "--node", "n1", "--source", c.sample, "--once", "--redact", "--log", logPath, "--key-dir", filepath.Join(dir, c.vendor))
		raw, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), c.secret) || strings.Contains(string(raw), "48211") {
			t.Fatalf("%s: process details survived redaction:\n%s", c.vendor, raw)
		}
		if !strings.Contains(string(raw), `"redacted":true`) {
			t.Fatalf("%s: redacted observations are not marked", c.vendor)
		}
		mustRun(t, "evidence", "verify", "--log", logPath, "--pub", filepath.Join(dir, c.vendor, "capledger.pub.pem"))
	}
	// The same events come out with or without redaction.
	plain := filepath.Join(dir, "plain.jsonl")
	mustRun(t, "agent", "--node", "n1", "--source", "../../examples/samples/kern.log", "--once", "--log", plain, "--key-dir", filepath.Join(dir, "plain"))
	a := mustRun(t, "events", "export", "--log", plain, "--no-verify")
	b := mustRun(t, "events", "export", "--log", filepath.Join(dir, "nvidia.jsonl"), "--no-verify")
	if strip(a) != strip(b) {
		t.Fatalf("redaction changed the events:\n%s\n%s", a, b)
	}
	out := mustRun(t, "xid", "parse", "--file", "../../examples/samples/kern.log", "--redact")
	if strings.Contains(out, "python3") || !strings.Contains(out, "pid=<redacted>, name=<redacted>") {
		t.Fatalf("xid parse --redact:\n%s", out)
	}
}

// strip removes the fields that differ between two logs written with
// different keys (ids and evidence hashes).
func strip(s string) string {
	var out []string
	for _, l := range jsonLines(s) {
		var keep []string
		for _, f := range strings.Split(l, ",") {
			if strings.HasPrefix(f, `{"id"`) || strings.HasPrefix(f, `"evidence"`) {
				continue
			}
			keep = append(keep, f)
		}
		out = append(out, strings.Join(keep, ","))
	}
	return strings.Join(out, "\n")
}

// TestLedgerSeveralContracts hosts two contracts from a configuration
// directory, each with its own keyring, and uses one through its /c/ URL.
func TestLedgerSeveralContracts(t *testing.T) {
	dir := t.TempDir()
	at := func(p ...string) string { return filepath.Join(append([]string{dir}, p...)...) }
	os.MkdirAll(at("configs", "keyrings"), 0o755)
	hashRe := regexp.MustCompile(`tokenSha256\s+([0-9a-f]{64})`)
	for _, lab := range []string{"lab-a", "lab-b"} {
		id := "neocloud--" + lab + "--2026"
		mustRun(t, "event", "emit", "--log", at(lab+"-gpu-003.jsonl"), "--key-dir", at(lab+"-agent"), "--role", "provider-agent",
			"--node", "gpu-003", "--type", "down", "--ts", "2026-10-03T04:12:00Z", "--cause", "xid:79", "--source", "capledger-agent")
		mustRun(t, "keyring", "import", "--keyring", at("configs", "keyrings", lab+".json"), "--log", at(lab+"-gpu-003.jsonl"),
			"--owner", "Example Neocloud", "--role", "provider-agent")
		cfg := ledger.Config{APIVersion: ledger.APIVersion, Kind: ledger.ConfigKind, Contract: id, Keyring: "keyrings/" + lab + ".json"}
		for _, p := range []struct{ name, role, write string }{{"example-neocloud", "provider", "provider-agent"}, {lab, "customer", "tenant-probe"}} {
			out := mustRun(t, "ledger", "token", "--out", at(lab+"-"+p.name+".token"))
			cfg.Parties = append(cfg.Parties, ledger.Party{Name: p.name, Role: p.role, TokenSHA256: hashRe.FindStringSubmatch(out)[1], WriteRoles: []string{p.write}})
		}
		b, _ := json.MarshalIndent(cfg, "", "  ")
		os.WriteFile(at("configs", lab+".json"), b, 0o644)
	}
	contracts, err := loadLedgerContracts(nil, at("configs"), "")
	if err != nil || len(contracts) != 2 {
		t.Fatalf("load: %v", err)
	}
	witness, _, _ := evidence.LoadOrCreateKey(at("witness"))
	hub := ledger.NewHub(at("data"), witness, nil)
	if _, _, err := hub.Apply(contracts); err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(hub.Handler())
	defer hs.Close()
	urlA := hs.URL + "/c/neocloud--lab-a--2026"
	out := mustRun(t, "ledger", "push", "--log", at("lab-a-gpu-003.jsonl"), "--server", urlA, "--token-file", at("lab-a-example-neocloud.token"))
	if !strings.Contains(out, "pushed 2 entries") {
		t.Fatalf("push: %s", out)
	}
	// Lab B's log is signed by a key only lab B's keyring holds.
	wantExit(t, 1, "ledger", "push", "--log", at("lab-b-gpu-003.jsonl"), "--server", urlA, "--token-file", at("lab-a-example-neocloud.token"))
	// Lab B's token does not open lab A's contract.
	wantExit(t, 1, "ledger", "status", "--server", urlA, "--token-file", at("lab-b-example-neocloud.token"))
	if out := mustRun(t, "ledger", "status", "--server", urlA, "--token-file", at("lab-a-lab-a.token")); !strings.Contains(out, "neocloud--lab-a--2026") {
		t.Fatalf("status: %s", out)
	}

	// A configuration without a keyring needs --keyring.
	plain := at("plain.json")
	cfgA, _ := ledger.LoadConfig(at("configs", "lab-a.json"))
	cfgA.Keyring = ""
	b, _ := json.Marshal(cfgA)
	os.WriteFile(plain, b, 0o644)
	if _, err := loadLedgerContracts([]string{plain}, "", ""); err == nil || !strings.Contains(err.Error(), "names no keyring") {
		t.Fatalf("contract without a keyring: %v", err)
	}
	if got, err := loadLedgerContracts([]string{plain}, "", at("configs", "keyrings", "lab-a.json")); err != nil || len(got) != 1 {
		t.Fatalf("--keyring fallback: %v", err)
	}
	wantExit(t, 2, "ledger", "serve", "--config", plain, "--config-dir", at("configs"), "--data", at("d"), "--key-dir", at("w"))
	wantExit(t, 2, "ledger", "serve", "--data", at("d"), "--key-dir", at("w"))
}
