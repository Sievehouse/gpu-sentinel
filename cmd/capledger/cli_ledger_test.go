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
	"github.com/Sievehouse/gpu-sentinel/internal/keyring"
	"github.com/Sievehouse/gpu-sentinel/internal/ledger"
)

// TestLedgerCLI runs a whole month through the ledger: both parties push
// their logs, the witness checkpoints them, the statement is published and
// co-signed, and a financier reads the feed once both parties consent.
func TestLedgerCLI(t *testing.T) {
	dir := t.TempDir()
	at := func(p ...string) string { return filepath.Join(append([]string{dir}, p...)...) }
	kr := at("keyring.json")

	// Evidence on both sides, each with its own key.
	mustRun(t, "event", "emit", "--log", at("gpu-003.jsonl"), "--key-dir", at("agent"), "--role", "provider-agent",
		"--node", "gpu-003", "--type", "down", "--ts", "2026-10-03T04:12:00Z", "--cause", "xid:79", "--source", "capledger-agent")
	mustRun(t, "event", "emit", "--log", at("probe-gpu-003.jsonl"), "--key-dir", at("probe"), "--role", "tenant-probe",
		"--node", "gpu-003", "--type", "down", "--ts", "2026-10-03T04:13:00Z", "--cause", "tenant:gpu-missing:7/8", "--source", "tenant-probe")
	mustRun(t, "keyring", "import", "--keyring", kr, "--log", at("gpu-003.jsonl"), "--owner", "Example Neocloud", "--role", "provider-agent")
	mustRun(t, "keyring", "import", "--keyring", kr, "--log", at("probe-gpu-003.jsonl"), "--owner", "Example Lab", "--role", "tenant-probe")
	for _, p := range []struct{ dir, owner, role string }{
		{"provider-signer", "Example Neocloud", "provider"}, {"customer-signer", "Example Lab", "customer"},
		{"witness", "Example Ledger Operator", "witness"},
	} {
		mustRun(t, "keygen", "--out", at(p.dir))
		mustRun(t, "keyring", "add", "--keyring", kr, "--pub", at(p.dir, "capledger.pub.pem"), "--owner", p.owner, "--role", p.role)
	}

	// The ledger's configuration: one token per party.
	hashRe := regexp.MustCompile(`tokenSha256\s+([0-9a-f]{64})`)
	cfg := ledger.Config{APIVersion: ledger.APIVersion, Kind: ledger.ConfigKind, Contract: "example-neocloud--example-lab--2026"}
	for _, p := range []struct {
		name, role string
		write      []string
	}{
		{"example-neocloud", "provider", []string{"provider-agent"}}, {"example-lab", "customer", []string{"tenant-probe"}},
		{"example-insurer", "financier", nil},
	} {
		out := mustRun(t, "ledger", "token", "--out", at(p.name+".token"))
		cfg.Parties = append(cfg.Parties, ledger.Party{Name: p.name, Role: p.role, TokenSHA256: hashRe.FindStringSubmatch(out)[1], WriteRoles: p.write})
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	os.WriteFile(at("ledger.json"), b, 0o644)
	loaded, err := ledger.LoadConfig(at("ledger.json"))
	if err != nil {
		t.Fatal(err)
	}
	krl, _ := keyring.Load(kr)
	store, err := ledger.OpenStore(at("data"))
	if err != nil {
		t.Fatal(err)
	}
	witness, err := evidence.LoadPrivateKey(at("witness", "capledger.key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	srv, err := ledger.NewServer(store, loaded, krl, witness, nil)
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()
	as := func(party string, args ...string) []string {
		return append(args, "--server", hs.URL, "--token-file", at(party+".token"))
	}

	if out := mustRun(t, as("example-neocloud", "ledger", "push", "--log", at("gpu-003.jsonl"))...); !strings.Contains(out, "pushed 2 entries") {
		t.Fatalf("push: %s", out)
	}
	if out := mustRun(t, as("example-neocloud", "ledger", "push", "--log", at("gpu-003.jsonl"))...); !strings.Contains(out, "up to date") {
		t.Fatalf("second push: %s", out)
	}
	mustRun(t, as("example-lab", "ledger", "push", "--log", at("probe-gpu-003.jsonl"))...)
	if out := wantExit(t, 1, as("example-insurer", "ledger", "push", "--log", at("gpu-003.jsonl"))...); !strings.Contains(out, "HTTP 403") {
		t.Fatalf("financier push: %s", out)
	}

	// The customer fetches the provider's log and checks it against the
	// witness's checkpoint.
	mustRun(t, as("example-lab", "ledger", "pull", "--all", "--out-dir", at("pulled"))...)
	mustRun(t, as("example-lab", "ledger", "checkpoint", "--create", "--out", at("cp.json"))...)
	out := mustRun(t, "evidence", "verify", "--log", at("pulled", "gpu-003.jsonl"), "--log", at("pulled", "probe-gpu-003.jsonl"),
		"--keyring", kr, "--checkpoint", at("cp.json"), "--format", "text")
	if !strings.Contains(out, "signed by Example Ledger Operator (witness)") || strings.Contains(out, "FAILED") {
		t.Fatalf("verify pulled logs:\n%s", out)
	}
	// More entries arrive; pull extends the local copy.
	mustRun(t, "event", "emit", "--log", at("gpu-003.jsonl"), "--key-dir", at("agent"), "--node", "gpu-003", "--type", "up", "--ts", "2026-10-03T06:40:00Z")
	mustRun(t, as("example-neocloud", "ledger", "push", "--log", at("gpu-003.jsonl"))...)
	if out := mustRun(t, as("example-lab", "ledger", "pull", "--name", "gpu-003", "--out-dir", at("pulled"))...); !strings.Contains(out, "fetched 1 entries") {
		t.Fatalf("incremental pull: %s", out)
	}

	// The monthly statement, signed by both parties.
	stmt := at("statement.json")
	mustRun(t, "sla", "compute", "--contract", yamlContract, "--events", providerEvents, "--period", "2026-10", "--out", stmt)
	mustRun(t, "sla", "sign", "--statement", stmt, "--party", "provider", "--key-dir", at("provider-signer"))
	mustRun(t, "sla", "sign", "--statement", stmt, "--party", "customer", "--key-dir", at("customer-signer"))
	mustRun(t, as("example-neocloud", "ledger", "publish", "--statement", stmt, "--signature", at("statement.provider.sig.json"))...)
	if out := mustRun(t, as("example-lab", "ledger", "publish", "--signature", at("statement.customer.sig.json"))...); !strings.Contains(out, "co-signed by customer, provider") {
		t.Fatalf("publish: %s", out)
	}

	// The financier waits for both consents.
	wantExit(t, 1, as("example-insurer", "ledger", "feed", "--out-dir", at("feed"))...)
	if out := mustRun(t, as("example-neocloud", "ledger", "consent", "--financier", "example-insurer", "--party", "provider", "--key-dir", at("provider-signer"))...); !strings.Contains(out, "waiting for customer") {
		t.Fatalf("consent: %s", out)
	}
	mustRun(t, as("example-lab", "ledger", "consent", "--financier", "example-insurer", "--party", "customer", "--key-dir", at("customer-signer"))...)
	out = mustRun(t, as("example-insurer", "ledger", "feed", "--out-dir", at("feed"))...)
	if !strings.Contains(out, "1 statement(s), 1 checkpoint(s)") {
		t.Fatalf("feed: %s", out)
	}
	sigs, _ := filepath.Glob(at("feed", "statements", "*.sig.json"))
	stmts, _ := filepath.Glob(at("feed", "statements", "2026-10-*[0-9a-f].json"))
	if len(sigs) != 2 || len(stmts) != 1 {
		t.Fatalf("feed files: %v %v", sigs, stmts)
	}
	// The financier checks the co-signatures itself.
	mustRun(t, "sla", "verify", "--statement", stmts[0], "--signature", sigs[0], "--signature", sigs[1], "--keyring", kr, "--require", "provider,customer")

	if out := mustRun(t, as("example-neocloud", "ledger", "status", "--witness-pub", at("witness-from-ledger.pem"))...); !strings.Contains(out, "customer, provider") || !strings.Contains(out, "probe-gpu-003  example-lab") {
		t.Fatalf("status:\n%s", out)
	}
	got, _ := os.ReadFile(at("witness-from-ledger.pem"))
	want, _ := os.ReadFile(at("witness", "capledger.pub.pem"))
	if string(got) != string(want) {
		t.Fatalf("witness key from the ledger differs:\n%s\n%s", got, want)
	}
	wantExit(t, 2, "ledger", "push", "--log", at("gpu-003.jsonl"))
}
