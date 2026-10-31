package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	reconciledContract = "../../examples/contracts/hgx-16-node-reconciled.yaml"
	bothEvents         = "../../examples/events/hgx-16-node-2026-10-both.jsonl"
)

func TestExportAttestsSigners(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "probe.jsonl")
	keyDir := filepath.Join(dir, "keys")
	kr := filepath.Join(dir, "keyring.json")
	mustRun(t, "event", "emit", "--log", logPath, "--key-dir", keyDir, "--role", "tenant-probe", "--node", "gpu-003", "--type", "down",
		"--ts", "2026-10-03T04:13:00Z", "--cause", "tenant:gpu-missing:7/8", "--source", "tenant-probe")
	// A writer cannot attest itself: signer fields in a payload are dropped.
	forged := filepath.Join(dir, "forged.json")
	os.WriteFile(forged, []byte(`{"id":"x-1","ts":"2026-10-03T06:41:00Z","node":"gpu-003","type":"up","source":"tenant-probe","signerRole":"provider-agent","signerKey":"0000000000000000"}`), 0o644)
	mustRun(t, "evidence", "append", "--log", logPath, "--key-dir", keyDir, "--kind", "event", "--payload", forged)
	mustRun(t, "keyring", "import", "--keyring", kr, "--log", logPath, "--owner", "Example Lab", "--role", "tenant-probe")

	plain := jsonLines(mustRun(t, "events", "export", "--log", logPath, "--keyring", kr))
	if len(plain) != 2 || strings.Contains(strings.Join(plain, "\n"), "signer") {
		t.Fatalf("export without --signers must carry no signer fields:\n%s", strings.Join(plain, "\n"))
	}
	attested := jsonLines(mustRun(t, "events", "export", "--log", logPath, "--keyring", kr, "--signers"))
	for _, l := range attested {
		if !strings.Contains(l, `"signerRole":"tenant-probe"`) || strings.Contains(l, "provider-agent") || strings.Contains(l, "0000000000000000") {
			t.Fatalf("bad attestation: %s", l)
		}
	}
	wantExit(t, 2, "events", "export", "--log", logPath, "--no-verify", "--signers")
}

func TestReconciledContractCLI(t *testing.T) {
	out := mustRun(t, "contract", "check", "--contract", reconciledContract)
	for _, want := range []string{"version 2", "reconcile   count either, start provider, end tenant", "tenant roles tenant-probe + sources tenant-probe", "unlisted reject"} {
		if !strings.Contains(out, want) {
			t.Fatalf("contract check missing %q:\n%s", want, out)
		}
	}
	out = mustRun(t, "sla", "compute", "--contract", reconciledContract, "--events", bothEvents, "--period", "2026-10", "--format", "text")
	if !strings.Contains(out, "Availability  99.6946%") || !strings.Contains(out, "[node seen:tenant tenant:gpu-missing:6/8]") {
		t.Fatalf("compute:\n%s", out)
	}
	out = mustRun(t, "sla", "corroborate", "--contract", reconciledContract, "--events", bothEvents, "--period", "2026-10")
	if !strings.Contains(out, "Agreement  93.63%") || !strings.Contains(out, "5 node(s) in dispute") {
		t.Fatalf("corroborate:\n%s", out)
	}
	wantExit(t, 2, "sla", "corroborate", "--contract", reconciledContract, "--events", bothEvents, "--events-a", bothEvents, "--period", "2026-10")
	// The version 1 contract lists no sources, so it cannot split one file.
	wantExit(t, 1, "sla", "corroborate", "--contract", yamlContract, "--events", bothEvents, "--period", "2026-10")
}

func TestCoSigningCLI(t *testing.T) {
	dir := t.TempDir()
	stmt := filepath.Join(dir, "statement.json")
	kr := filepath.Join(dir, "keyring.json")
	mustRun(t, "sla", "compute", "--contract", yamlContract, "--events", providerEvents, "--period", "2026-10", "--out", stmt)
	for _, p := range []struct{ dir, owner, role string }{
		{"provider-signer", "Example Neocloud", "provider"},
		{"customer-signer", "Example Lab", "customer"},
	} {
		mustRun(t, "keygen", "--out", filepath.Join(dir, p.dir))
		mustRun(t, "keyring", "add", "--keyring", kr, "--pub", filepath.Join(dir, p.dir, "capledger.pub.pem"), "--owner", p.owner, "--role", p.role)
	}
	out := mustRun(t, "sla", "sign", "--statement", stmt, "--party", "provider", "--key-dir", filepath.Join(dir, "provider-signer"))
	provSig := filepath.Join(dir, "statement.provider.sig.json")
	if !strings.Contains(out, provSig) {
		t.Fatalf("sign: %s", out)
	}
	out = wantExit(t, 1, "sla", "verify", "--statement", stmt, "--signature", provSig, "--keyring", kr, "--require", "provider,customer")
	if !strings.Contains(out, "missing customer") {
		t.Fatalf("missing party not reported:\n%s", out)
	}
	mustRun(t, "sla", "sign", "--statement", stmt, "--party", "customer", "--key-dir", filepath.Join(dir, "customer-signer"))
	custSig := filepath.Join(dir, "statement.customer.sig.json")
	out = mustRun(t, "sla", "verify", "--statement", stmt, "--contract", yamlContract, "--events", providerEvents,
		"--signature", provSig, "--signature", custSig, "--keyring", kr, "--require", "provider,customer")
	if !strings.Contains(out, "replay OK") || !strings.Contains(out, "co-signed: every required party accepted") {
		t.Fatalf("co-signed:\n%s", out)
	}
	// The provider's key cannot sign as the customer.
	forged := filepath.Join(dir, "forged.sig.json")
	mustRun(t, "sla", "sign", "--statement", stmt, "--party", "customer", "--key-dir", filepath.Join(dir, "provider-signer"), "--out", forged)
	out = wantExit(t, 1, "sla", "verify", "--statement", stmt, "--signature", provSig, "--signature", forged, "--keyring", kr, "--require", "provider,customer")
	if !strings.Contains(out, `whose keyring role is "provider"`) {
		t.Fatalf("forged party not caught:\n%s", out)
	}
	wantExit(t, 2, "sla", "verify", "--statement", stmt, "--signature", provSig)
	wantExit(t, 1, "sla", "sign", "--statement", stmt, "--party", "customer", "--decision", "dispute", "--key-dir", filepath.Join(dir, "customer-signer"))
}
