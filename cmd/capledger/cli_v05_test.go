package main

import (
	"path/filepath"
	"strings"
	"testing"
)

const jiraMapping = "../../examples/webhooks/jira-repairs.json"

func TestWatchWebhookReplay(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "jira.jsonl")
	keyDir := filepath.Join(dir, "keys")
	d := func(name string) string { return "../../examples/webhooks/" + name }
	out := mustRun(t, "watch", "webhook", "--mapping", jiraMapping, "--log", logPath, "--key-dir", keyDir,
		"--replay", d("01-repair-opened.json"), "--replay", d("02-repair-redelivered.json"), "--replay", d("03-repair-closed.json"))
	for _, want := range []string{"2026-10-03T04:20:00Z gpu-003 down", "not recorded: down is already open for gpu-003", "2026-10-03T06:40:00Z gpu-003 up"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	// A restart remembers that nothing is open, so a second close is not recorded.
	out = mustRun(t, "watch", "webhook", "--mapping", jiraMapping, "--log", logPath, "--key-dir", keyDir, "--replay", d("03-repair-closed.json"))
	if !strings.Contains(out, "no open down for gpu-003") {
		t.Fatalf("after restart:\n%s", out)
	}
	mustRun(t, "watch", "webhook", "--mapping", jiraMapping, "--log", logPath, "--key-dir", keyDir,
		"--replay", d("04-maintenance-started.json"), "--replay", d("05-maintenance-done.json"), "--contract", yamlContract)
	evs := jsonLines(mustRun(t, "events", "export", "--log", logPath, "--pub", filepath.Join(keyDir, "capledger.pub.pem")))
	if len(evs) != 4 || !strings.Contains(evs[2], `"noticeTs":"2026-10-10T00:00:00Z"`) || !strings.Contains(evs[0], `"source":"jira-repairs"`) {
		t.Fatalf("events:\n%s", strings.Join(evs, "\n"))
	}
	wantExit(t, 2, "watch", "webhook", "--log", logPath, "--key-dir", keyDir)
	wantExit(t, 1, "watch", "webhook", "--mapping", d("01-repair-opened.json"), "--log", logPath, "--key-dir", keyDir, "--replay", d("01-repair-opened.json"))
}

func TestSLACompare(t *testing.T) {
	dir := t.TempDir()
	stmt := filepath.Join(dir, "statement.json")
	mustRun(t, "sla", "compute", "--contract", yamlContract, "--events", providerEvents, "--period", "2026-10", "--out", stmt)
	report := "../../examples/pilot/provider-report-2026-10.csv"
	out := mustRun(t, "sla", "compare", "--statement", stmt, "--provider-report", report, "--contract", yamlContract)
	for _, want := range []string{
		"statement   availability 99.7021%, downtime 127680 node-seconds, credit 5% = 15000.00 USD",
		"provider    availability 99.9160%, downtime 36000 node-seconds, credit 0% = 0.00 USD",
		"Flagged     5 node(s) differ by more than 300 s",
		"-64800  <-",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	js := mustRun(t, "sla", "compare", "--statement", stmt, "--provider-report", report, "--tolerance", "2h", "--format", "json")
	if !strings.Contains(js, `"flaggedNodes": 2`) {
		t.Fatalf("json:\n%s", js)
	}
	wantExit(t, 2, "sla", "compare", "--statement", stmt)
	wantExit(t, 1, "sla", "compare", "--statement", stmt, "--provider-report", stmt)
}
