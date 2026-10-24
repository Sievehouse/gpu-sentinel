package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const (
	yamlContract   = "../../examples/contracts/hgx-16-node.yaml"
	jsonContract   = "../../examples/contracts/hgx-16-node.json"
	providerEvents = "../../examples/events/hgx-16-node-2026-10.jsonl"
	tenantEvents   = "../../examples/events/hgx-16-node-2026-10-tenant.jsonl"
)

func mustRun(t *testing.T, args ...string) string {
	t.Helper()
	out, code := capledger(t, args...)
	if code != 0 {
		t.Fatalf("capledger %s: exit %d\n%s", strings.Join(args, " "), code, out)
	}
	return out
}

func wantExit(t *testing.T, want int, args ...string) string {
	t.Helper()
	out, code := capledger(t, args...)
	if code != want {
		t.Fatalf("capledger %s: exit %d, want %d\n%s", strings.Join(args, " "), code, want, out)
	}
	return out
}

// jsonLines returns the lines of out that hold JSON objects (stdout and
// stderr are captured together).
func jsonLines(out string) []string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "{") {
			lines = append(lines, l)
		}
	}
	return lines
}

func TestVersion(t *testing.T) {
	if out := mustRun(t, "version"); !strings.Contains(out, "sla engine 0.2.0") {
		t.Fatalf("version: %s", out)
	}
}

func TestYAMLContractGivesTheSameStatement(t *testing.T) {
	out := mustRun(t, "sla", "compute", "--contract", yamlContract, "--events", providerEvents, "--period", "2026-10")
	if !strings.Contains(out, `"statementSha256": "d6280aea2086f9bf153922421dffbc4d44fbe17cc1106ba5184cad29f99323fc"`) {
		t.Fatalf("the YAML contract must reproduce the v0.1 statement:\n%s", out)
	}
	digest := func(path string) string {
		for _, line := range strings.Split(mustRun(t, "contract", "check", "--contract", path), "\n") {
			if strings.HasPrefix(line, "digest") {
				return line
			}
		}
		t.Fatalf("no digest for %s", path)
		return ""
	}
	if a, b := digest(yamlContract), digest(jsonContract); a != b {
		t.Fatalf("digests differ:\n%s\n%s", a, b)
	}
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	os.WriteFile(bad, []byte("apiVersion: capacity-ledger/v1alpha1\nkind: ContractSLA\nmetadata:\n  id: x\n  version: one\n"), 0o644)
	if out := wantExit(t, 1, "contract", "check", "--contract", bad); !strings.Contains(out, "line 5") {
		t.Fatalf("expected a line number: %s", out)
	}
}

func TestKeyringImportBindsKeysToNodes(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "gpu-003.jsonl")
	keyDir := filepath.Join(dir, "keys-gpu-003")
	kr := filepath.Join(dir, "keyring.json")

	out := mustRun(t, "agent", "--node", "gpu-003", "--source", "../../examples/samples/kern.log", "--once", "--log", logPath, "--key-dir", keyDir)
	if !strings.Contains(out, "created signing key") {
		t.Fatalf("agent should create a per-node key: %s", out)
	}
	mustRun(t, "event", "emit", "--log", logPath, "--key-dir", keyDir, "--node", "gpu-003", "--type", "up", "--ts", "2026-10-03T06:40:00Z", "--cause", "repaired")

	out = mustRun(t, "keyring", "import", "--keyring", kr, "--log", logPath, "--owner", "Example Neocloud", "--role", "provider-agent")
	if !strings.Contains(out, "nodes gpu-003") || !strings.Contains(out, "1 key(s) added") {
		t.Fatalf("import: %s", out)
	}
	if out := mustRun(t, "keyring", "import", "--keyring", kr, "--log", logPath, "--owner", "Example Neocloud", "--role", "provider-agent"); !strings.Contains(out, "already trusted") {
		t.Fatalf("second import: %s", out)
	}
	if out := mustRun(t, "keyring", "list", "--keyring", kr); !strings.Contains(out, "provider-agent") || !strings.Contains(out, "gpu-003") {
		t.Fatalf("list: %s", out)
	}
	mustRun(t, "evidence", "verify", "--log", logPath, "--keyring", kr)
	if lines := jsonLines(mustRun(t, "events", "export", "--log", logPath, "--keyring", kr)); len(lines) != 2 {
		t.Fatalf("expected two events, got %d", len(lines))
	}

	// The same key now reports for another node: a stolen node key cannot
	// speak for the rest of the cluster.
	mustRun(t, "event", "emit", "--log", logPath, "--key-dir", keyDir, "--node", "gpu-004", "--type", "down", "--ts", "2026-10-04T00:00:00Z", "--cause", "forged")
	if out := wantExit(t, 1, "evidence", "verify", "--log", logPath, "--keyring", kr, "--format", "text"); !strings.Contains(out, "may speak only for gpu-003") {
		t.Fatalf("binding not enforced: %s", out)
	}
	wantExit(t, 1, "events", "export", "--log", logPath, "--keyring", kr)
	// --pub keeps the version 0.1 meaning: trusted for every node.
	mustRun(t, "evidence", "verify", "--log", logPath, "--pub", filepath.Join(keyDir, "capledger.pub.pem"))
}

func TestGapsFromHeartbeats(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.jsonl")
	keyDir := filepath.Join(dir, "keys")
	for _, ts := range []string{"00:00", "00:01", "00:02", "00:30", "00:31"} {
		hb := filepath.Join(dir, "hb.json")
		os.WriteFile(hb, []byte(`{"node":"gpu-001","ts":"2026-10-01T`+ts+`:00Z","intervalSeconds":60,"role":"agent"}`), 0o644)
		mustRun(t, "evidence", "append", "--log", logPath, "--key-dir", keyDir, "--kind", "heartbeat", "--payload", hb)
	}
	pub := filepath.Join(keyDir, "capledger.pub.pem")
	window := []string{"--since", "2026-10-01T00:00:00Z", "--until", "2026-10-01T01:00:00Z"}

	out := mustRun(t, append([]string{"evidence", "gaps", "--log", logPath, "--pub", pub, "--max-silence", "5m", "--format", "json"}, window...)...)
	var gs []struct {
		Node, Start, End, Reason string
		Seconds                  int64
	}
	if err := json.Unmarshal([]byte(out), &gs); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(gs) != 2 || gs[0].Start != "2026-10-01T00:03:00Z" || gs[0].End != "2026-10-01T00:30:00Z" || gs[0].Seconds != 1620 ||
		gs[1].Start != "2026-10-01T00:32:00Z" || gs[1].Seconds != 1680 {
		t.Fatalf("gaps %+v", gs)
	}

	out = mustRun(t, append([]string{"events", "export", "--log", logPath, "--pub", pub, "--derive-gaps", "5m"}, window...)...)
	lines := jsonLines(out)
	if len(lines) != 4 || !strings.Contains(lines[0], `"id":"gap:gpu-001:2026-10-01T00:03:00Z"`) || !strings.Contains(lines[0], `"type":"unmonitored_start"`) ||
		!strings.Contains(lines[1], `"type":"unmonitored_end"`) {
		t.Fatalf("export:\n%s", out)
	}

	out = mustRun(t, append([]string{"evidence", "gaps", "--log", logPath, "--pub", pub, "--gap-role", "tenant-probe"}, window...)...)
	if !strings.Contains(out, "no silences") {
		t.Fatalf("role filter: %s", out)
	}
	wantExit(t, 2, "events", "export", "--log", logPath, "--pub", pub, "--period", "2026-10")
	wantExit(t, 2, "evidence", "gaps", "--log", logPath, "--pub", pub, "--period", "2026-10", "--since", "2026-10-01T00:00:00Z")
}

func TestCorroborate(t *testing.T) {
	out := mustRun(t, "sla", "corroborate", "--contract", yamlContract, "--events-a", providerEvents, "--events-b", tenantEvents, "--period", "2026-10")
	if !strings.Contains(out, "Agreement  93.63%") || !strings.Contains(out, "5 node(s) in dispute") {
		t.Fatalf("corroborate:\n%s", out)
	}
	wantExit(t, 2, "sla", "corroborate", "--contract", yamlContract, "--events-a", providerEvents, "--period", "2026-10")
}

func TestBundleCreateVerifyExtract(t *testing.T) {
	dir := t.TempDir()
	keyDir := filepath.Join(dir, "harness-keys")
	pub := filepath.Join(keyDir, "capledger.pub.pem")
	logPath := filepath.Join(dir, "harness.jsonl")
	os.WriteFile(filepath.Join(dir, "report.json"), []byte(`{"summary":{"accept":4}}`+"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "report.md"), []byte("# Acceptance\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "raw", "gpu-001"), 0o755)
	os.WriteFile(filepath.Join(dir, "raw", "gpu-001", "dcgm.txt"), []byte("PASS\n"), 0o644)
	b := filepath.Join(dir, "acceptance.tar.gz")

	create := []string{"bundle", "create", "--out", b, "--type", "acceptance", "--subject", "example-hgx-32gpu", "--key-dir", keyDir,
		"--file", filepath.Join(dir, "report.json"), "--file", "human/report.md=" + filepath.Join(dir, "report.md"), "--dir", filepath.Join(dir, "raw"),
		"--created", "2026-10-07T12:00:00Z", "--log", logPath}
	out := mustRun(t, create...)
	if !strings.Contains(out, "3 files") || !strings.Contains(out, "recorded in") {
		t.Fatalf("create: %s", out)
	}
	wantExit(t, 1, create...) // refuses to overwrite

	out = mustRun(t, "bundle", "verify", "--bundle", b, "--pub", pub)
	for _, want := range []string{"status     OK", "raw/gpu-001/dcgm.txt", "human/report.md", "report.json", "created    2026-10-07T12:00:00Z"} {
		if !strings.Contains(out, want) {
			t.Fatalf("verify output lacks %q:\n%s", want, out)
		}
	}
	x := filepath.Join(dir, "x")
	mustRun(t, "bundle", "verify", "--bundle", b, "--pub", pub, "--extract", x)
	if got, err := os.ReadFile(filepath.Join(x, "human", "report.md")); err != nil || string(got) != "# Acceptance\n" {
		t.Fatalf("extracted %q %v", got, err)
	}
	if out := mustRun(t, "evidence", "verify", "--log", logPath, "--pub", pub); !strings.Contains(out, `"bundle"`) {
		t.Fatalf("bundle entry missing from the log: %s", out)
	}

	// A key that expired before the bundle was made is not trusted for it.
	kr := filepath.Join(dir, "keyring.json")
	mustRun(t, "keyring", "add", "--keyring", kr, "--pub", pub, "--owner", "Acceptance team", "--role", "harness", "--not-after", "2026-10-01T00:00:00Z")
	if out := wantExit(t, 1, "bundle", "verify", "--bundle", b, "--keyring", kr); !strings.Contains(out, "after its notAfter") {
		t.Fatalf("validity window not enforced: %s", out)
	}

	raw, _ := os.ReadFile(b)
	raw[len(raw)/2] ^= 0xff
	bad := filepath.Join(dir, "bad.tar.gz")
	os.WriteFile(bad, raw, 0o644)
	wantExit(t, 1, "bundle", "verify", "--bundle", bad, "--pub", pub)
}

func TestAgentReplaysAMDLog(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "mi355-02.jsonl")
	keyDir := filepath.Join(dir, "keys")
	mustRun(t, "agent", "--vendor", "amd", "--node", "mi355-02", "--source", "../../examples/samples/kern-amd.log", "--once", "--log", logPath, "--key-dir", keyDir)
	out := mustRun(t, "events", "export", "--log", logPath, "--pub", filepath.Join(keyDir, "capledger.pub.pem"))
	lines := jsonLines(out)
	if len(lines) != 1 || !strings.Contains(lines[0], `"cause":"amdgpu:ras_uncorrectable:umc"`) || !strings.Contains(lines[0], `"ts":"2026-10-07T11:02:45Z"`) {
		t.Fatalf("expected one down event from the uncorrectable RAS error:\n%s", out)
	}
}

func TestProbeWithAMDSysfs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake sysfs tree needs ':' in file names")
	}
	dir := t.TempDir()
	root := filepath.Join(dir, "sys")
	for _, n := range []string{"0000:05:00.0", "0000:65:00.0"} {
		os.MkdirAll(filepath.Join(root, "bus", "pci", "drivers", "amdgpu", n), 0o755)
	}
	logPath := filepath.Join(dir, "tenant.jsonl")
	keyDir := filepath.Join(dir, "probe-keys")
	common := []string{"probe", "--node", "mi355-02", "--vendor", "amd", "--sys-root", root, "--log", logPath, "--key-dir", keyDir, "--interval", "1ms"}

	out := mustRun(t, append(common, "--expected-gpus", "2", "--cycles", "3")...)
	if !strings.Contains(out, "after 3 checks: 3 heartbeats, 0 observations, 0 events") {
		t.Fatalf("healthy node: %s", out)
	}
	out = mustRun(t, append(common, "--expected-gpus", "3", "--cycles", "2", "--fail-after", "2")...)
	if !strings.Contains(out, "2 observations, 1 events") || !strings.Contains(out, "node is down") {
		t.Fatalf("missing GPU: %s", out)
	}
	pub := filepath.Join(keyDir, "capledger.pub.pem")
	out = mustRun(t, "events", "export", "--log", logPath, "--pub", pub)
	if !strings.Contains(out, `"cause":"tenant:gpu-missing:2/3"`) {
		t.Fatalf("export: %s", out)
	}
	kr := filepath.Join(dir, "tenant-keyring.json")
	mustRun(t, "keyring", "import", "--keyring", kr, "--log", logPath, "--owner", "Example Lab", "--role", "tenant-probe")
	mustRun(t, "evidence", "verify", "--log", logPath, "--keyring", kr)
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"sla", "compute", "--events", providerEvents, "--period", "2026-10"},
		{"sla", "compute", "--contract", yamlContract, "--events", providerEvents, "--period", "2026-10", "stray"},
		{"probe", "--log", "x.jsonl", "--key-dir", "k"},
		{"evidence", "verify", "--log", "x.jsonl"},
		{"bundle", "verify", "--bundle", "x.tar.gz", "--pub", "p.pem", "--format", "yaml"},
		{"agent", "--vendor", "intel", "--log", "x.jsonl", "--key", "k.pem"},
	} {
		wantExit(t, 2, args...)
	}
}
