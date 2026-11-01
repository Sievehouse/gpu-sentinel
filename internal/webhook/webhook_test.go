package webhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sievehouse/gpu-sentinel/internal/events"
)

const secret = "a-shared-secret-of-enough-length"

func mapping(t *testing.T, mutate func(*Mapping)) *Mapping {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "secret.txt"), []byte(secret+"\n"), 0o600)
	m := &Mapping{
		APIVersion: APIVersion, Kind: Kind, Source: "jira-repairs", dir: dir,
		Auth:   Auth{HMACSecretFile: "secret.txt", TokenSHA256: tokenHash("bearer-token")},
		Fields: Fields{Node: "issue.fields.node", Status: "issue.fields.status.name", Time: "issue.fields.updated", Ticket: "issue.key", NoticeTS: "issue.fields.created"},
		Statuses: map[string]events.Type{
			"In Repair": events.Down, "Customer Caused": events.Down, "Repaired": events.Up,
			"In Maintenance": events.MaintenanceStart, "Done": events.MaintenanceEnd,
		},
		CustomerCaused: []string{"Customer Caused"},
	}
	if mutate != nil {
		mutate(m)
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	return m
}

func tokenHash(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func delivery(key, node, status, updated string) []byte {
	b, _ := json.Marshal(map[string]any{"issue": map[string]any{"key": key, "fields": map[string]any{
		"node": node, "status": map[string]any{"name": status}, "updated": updated, "created": "2026-10-10T00:00:00.000+0000",
	}}})
	return b
}

func sign(body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestValidate(t *testing.T) {
	for name, mutate := range map[string]func(*Mapping){
		"no auth":        func(m *Mapping) { m.Auth = Auth{} },
		"bad token hash": func(m *Mapping) { m.Auth.TokenSHA256 = "xyz" },
		"short secret":   func(m *Mapping) { os.WriteFile(filepath.Join(m.dir, "secret.txt"), []byte("short"), 0o600) },
		"missing secret": func(m *Mapping) { m.Auth.HMACSecretFile = "nope.txt" },
		"bad type":       func(m *Mapping) { m.Statuses["Weird"] = "unmonitored_start" },
		"no statuses":    func(m *Mapping) { m.Statuses = nil },
		"no node":        func(m *Mapping) { m.Fields.Node = "" },
		"bad source":     func(m *Mapping) { m.Source = "Jira Repairs" },
		"unknown cust":   func(m *Mapping) { m.CustomerCaused = []string{"Nope"} },
		"wrong kind":     func(m *Mapping) { m.Kind = "LedgerConfig" },
	} {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "secret.txt"), []byte(secret), 0o600)
		m := &Mapping{APIVersion: APIVersion, Kind: Kind, Source: "jira", dir: dir,
			Auth:     Auth{HMACSecretFile: "secret.txt"},
			Fields:   Fields{Node: "n", Status: "s"},
			Statuses: map[string]events.Type{"x": events.Down}}
		mutate(m)
		if err := m.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLookupAndTimes(t *testing.T) {
	var doc any
	dec := json.NewDecoder(strings.NewReader(`{"a":{"b":[{"c":"x"},{"c":42}],"t":true,"n":null}}`))
	dec.UseNumber()
	dec.Decode(&doc)
	for path, want := range map[string]string{"a.b.0.c": "x", "a.b.1.c": "42", "a.t": "true"} {
		if got, ok := Lookup(doc, path); !ok || got != want {
			t.Errorf("Lookup(%s) = %q %v", path, got, ok)
		}
	}
	for _, path := range []string{"a.n", "a.b", "a.b.9.c", "a.x", "a.b.z"} {
		if _, ok := Lookup(doc, path); ok {
			t.Errorf("Lookup(%s) found a value", path)
		}
	}
	want := time.Date(2026, 10, 3, 4, 12, 0, 0, time.UTC).Unix()
	for _, s := range []string{"2026-10-03T04:12:00Z", "2026-10-03T04:12:00.000+0000", "2026-10-03T09:42:00+05:30", "2026-10-03 04:12:00", "1791000000"} {
		got, err := ParseTime(s)
		if err != nil {
			t.Errorf("ParseTime(%s): %v", s, err)
			continue
		}
		if s != "1791000000" && got != want {
			t.Errorf("ParseTime(%s) = %d", s, got)
		}
	}
	if ms, _ := ParseTime("1791000000123"); ms != 1791000000 {
		t.Errorf("milliseconds: %d", ms)
	}
	if _, err := ParseTime("yesterday"); err == nil {
		t.Error("bad time accepted")
	}
}

type recorder struct{ evs []events.Event }

func (r *recorder) record(ev events.Event) (string, error) {
	ev.ID = "e" + string(rune('0'+len(r.evs)))
	r.evs = append(r.evs, ev)
	return ev.ID, nil
}

func post(t *testing.T, url string, body []byte, header map[string]string) (int, Result) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var res Result
	json.NewDecoder(resp.Body).Decode(&res)
	return resp.StatusCode, res
}

func TestReceiver(t *testing.T) {
	m := mapping(t, nil)
	rec := &recorder{}
	rcv := NewReceiver(m, []string{"gpu-003", "gpu-010"}, rec.record, nil)
	rcv.SetClock(func() time.Time { return time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC) })
	srv := httptest.NewServer(rcv)
	defer srv.Close()
	signed := func(body []byte) map[string]string { return map[string]string{"X-Hub-Signature-256": sign(body)} }

	open := delivery("OPS-1", "gpu-003", "In Repair", "2026-10-03T04:20:00.000+0000")
	if code, _ := post(t, srv.URL, open, nil); code != http.StatusUnauthorized {
		t.Fatalf("unsigned: %d", code)
	}
	if code, _ := post(t, srv.URL, open, map[string]string{"X-Hub-Signature-256": sign([]byte("other"))}); code != http.StatusUnauthorized {
		t.Fatalf("wrong signature: %d", code)
	}
	code, res := post(t, srv.URL, open, signed(open))
	if code != http.StatusCreated || !res.Recorded || res.TS != "2026-10-03T04:20:00Z" {
		t.Fatalf("down: %d %+v", code, res)
	}
	// Redelivery changes nothing.
	if code, res := post(t, srv.URL, open, signed(open)); code != http.StatusOK || res.Recorded {
		t.Fatalf("redelivery: %d %+v", code, res)
	}
	// A delivery dated before the node's last event is late or replayed: refused.
	stale := delivery("OPS-1", "gpu-003", "Repaired", "2026-10-03T03:00:00.000+0000")
	if code, res := post(t, srv.URL, stale, signed(stale)); code != http.StatusOK || res.Recorded || !strings.Contains(res.Reason, "before gpu-003's last recorded event") {
		t.Fatalf("stale delivery: %d %+v", code, res)
	}
	// A bearer token works too.
	closeBody := delivery("OPS-1", "gpu-003", "Repaired", "2026-10-03T06:40:00.000+0000")
	code, res = post(t, srv.URL, closeBody, map[string]string{"Authorization": "Bearer bearer-token"})
	if code != http.StatusCreated || res.TS != "2026-10-03T06:40:00Z" {
		t.Fatalf("up: %d %+v", code, res)
	}
	if code, _ := post(t, srv.URL, closeBody, map[string]string{"Authorization": "Bearer wrong"}); code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", code)
	}
	// Replaying the original "In Repair" delivery cannot reopen the outage.
	if code, res := post(t, srv.URL, open, signed(open)); code != http.StatusOK || res.Recorded {
		t.Fatalf("replayed delivery: %d %+v", code, res)
	}
	// Unmapped statuses are acknowledged and ignored.
	other := delivery("OPS-2", "gpu-003", "Triage", "2026-10-04T00:00:00Z")
	if code, res := post(t, srv.URL, other, signed(other)); code != http.StatusAccepted || res.Recorded {
		t.Fatalf("unmapped: %d %+v", code, res)
	}
	// Nodes outside the contract are refused.
	stray := delivery("OPS-3", "gpu-099", "In Repair", "2026-10-04T00:00:00Z")
	if code, _ := post(t, srv.URL, stray, signed(stray)); code != http.StatusUnprocessableEntity {
		t.Fatalf("stray node: %d", code)
	}
	// Bad JSON and missing fields.
	if code, _ := post(t, srv.URL, []byte("{"), signed([]byte("{"))); code != http.StatusBadRequest {
		t.Fatalf("bad json: %d", code)
	}
	noNode := []byte(`{"issue":{"fields":{"status":{"name":"In Repair"}}}}`)
	if code, _ := post(t, srv.URL, noNode, signed(noNode)); code != http.StatusBadRequest {
		t.Fatalf("no node: %d", code)
	}
	// A future time is dated now; maintenance carries its notice.
	maint := delivery("OPS-4", "gpu-010", "In Maintenance", "2030-01-01T00:00:00Z")
	if code, res := post(t, srv.URL, maint, signed(maint)); code != http.StatusCreated || res.TS != "2026-10-20T00:00:00Z" {
		t.Fatalf("future: %d %+v", code, res)
	}
	// Customer-caused statuses get a customer cause.
	cust := delivery("OPS-5", "gpu-003", "Customer Caused", "2026-10-21T00:00:00Z")
	post(t, srv.URL, cust, signed(cust))

	if len(rec.evs) != 4 {
		t.Fatalf("want 4 events, got %+v", rec.evs)
	}
	if rec.evs[2].NoticeTS != "2026-10-10T00:00:00Z" || rec.evs[2].Type != events.MaintenanceStart {
		t.Fatalf("maintenance: %+v", rec.evs[2])
	}
	if rec.evs[3].Cause != "customer:ticket:OPS-5" || rec.evs[0].Cause != "ticket:OPS-1" || rec.evs[0].Source != "jira-repairs" {
		t.Fatalf("causes: %+v", rec.evs)
	}

	// GET /healthz, and no other methods.
	resp, _ := http.Get(srv.URL + "/healthz")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz: %d", resp.StatusCode)
	}
	resp, _ = http.Get(srv.URL + "/")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /: %d", resp.StatusCode)
	}
	big := bytes.Repeat([]byte("x"), MaxBody+10)
	if code, _ := post(t, srv.URL, big, signed(big)); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("big: %d", code)
	}
}

func TestResume(t *testing.T) {
	m := mapping(t, nil)
	rec := &recorder{}
	rcv := NewReceiver(m, nil, rec.record, nil)
	err := rcv.Resume([]events.Event{
		{ID: "a", TS: "2026-10-03T04:20:00Z", Node: "gpu-003", Type: events.Down},
		{ID: "b", TS: "2026-10-14T02:00:00Z", Node: "gpu-010", Type: events.MaintenanceStart},
		{ID: "c", TS: "2026-10-14T05:00:00Z", Node: "gpu-010", Type: events.MaintenanceEnd},
	})
	if err != nil {
		t.Fatal(err)
	}
	if open := rcv.Open(); len(open) != 1 || len(open["gpu-003"]) != 1 {
		t.Fatalf("open: %v", open)
	}
	d, _ := m.Read(delivery("OPS-1", "gpu-003", "In Repair", "2026-10-05T00:00:00Z"))
	if _, res := rcv.Apply(d); res.Recorded {
		t.Fatal("a down already open before the restart was recorded again")
	}
	d, _ = m.Read(delivery("OPS-1", "gpu-003", "Repaired", "2026-10-05T00:00:00Z"))
	if _, res := rcv.Apply(d); !res.Recorded {
		t.Fatal("recovery after restart not recorded")
	}
}

func TestLoadMappingExample(t *testing.T) {
	m, err := LoadMapping("../../examples/webhooks/jira-repairs.json")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile("../../examples/webhooks/01-repair-opened.json")
	d, err := m.Read(body)
	if err != nil || d.Node != "gpu-003" || d.Type != events.Down || d.Cause() != "ticket:OPS-4711" {
		t.Fatalf("example: %+v %v", d, err)
	}
}
