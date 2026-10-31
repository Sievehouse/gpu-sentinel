package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
