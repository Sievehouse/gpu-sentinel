package k8s

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sievehouse/gpu-sentinel/internal/events"
	"github.com/Sievehouse/gpu-sentinel/internal/evidence"
)

func ts(s string) int64 {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t.Unix()
}

type nodeOpt func(*Node)

func node(name string, opts ...nodeOpt) Node {
	var n Node
	n.Metadata.Name = name
	n.Status.Conditions = []Condition{{Type: "Ready", Status: "True", Reason: "KubeletReady", LastTransitionTime: "2026-09-01T00:00:00Z"}}
	n.Status.Allocatable = map[string]string{"nvidia.com/gpu": "8"}
	for _, o := range opts {
		o(&n)
	}
	return n
}

func cond(typ, status, reason, msg, at string) nodeOpt {
	return func(n *Node) {
		for i, c := range n.Status.Conditions {
			if c.Type == typ {
				n.Status.Conditions[i] = Condition{typ, status, reason, msg, at}
				return
			}
		}
		n.Status.Conditions = append(n.Status.Conditions, Condition{typ, status, reason, msg, at})
	}
}

func taint(key, effect string) nodeOpt {
	return func(n *Node) { n.Spec.Taints = append(n.Spec.Taints, Taint{Key: key, Value: "true", Effect: effect}) }
}

func gpus(v string) nodeOpt { return func(n *Node) { n.Status.Allocatable["nvidia.com/gpu"] = v } }

func label(k, v string) nodeOpt {
	return func(n *Node) {
		if n.Metadata.Labels == nil {
			n.Metadata.Labels = map[string]string{}
		}
		n.Metadata.Labels[k] = v
	}
}

func list(ns ...Node) *NodeList { return &NodeList{Kind: "List", Items: ns} }

func TestAssess(t *testing.T) {
	nvs := Rules{NotReady: true, DownConditions: []string{"*"}, DegradedConditions: []string{"GpuThermalWatch"}, ExpectedGPUs: 8}
	cases := []struct {
		name          string
		rules         Rules
		n             Node
		down, degrade string
	}{
		{"healthy", nvs, node("a"), "", ""},
		{"not ready", nvs, node("a", cond("Ready", "False", "KubeletNotReady", "PLEG is not healthy", "2026-10-03T04:12:00Z")), "k8s:NotReady:KubeletNotReady", ""},
		{"unknown", nvs, node("a", cond("Ready", "Unknown", "NodeStatusUnknown", "", "")), "k8s:NotReady:NodeStatusUnknown", ""},
		{"nvsentinel fatal", nvs, node("a", cond("GpuMemWatch", "True", "HardwareFailure", "[DCGM_FR_FAULTY_MEMORY] GPU memory failure detected on GPU 0 - RecommendedAction: RESTART_VM", "")), "k8s:GpuMemWatch:DCGM_FR_FAULTY_MEMORY", ""},
		{"nvsentinel passed", nvs, node("a", cond("GpuMemWatch", "False", "HealthCheckPassed", "GPU memory health check passed", "")), "", ""},
		{"wildcard skips kubelet pressure", nvs, node("a", cond("DiskPressure", "True", "KubeletHasDiskPressure", "", "")), "", ""},
		{"exact name matches kubelet condition", Rules{DownConditions: []string{"DiskPressure"}}, node("a", cond("DiskPressure", "True", "KubeletHasDiskPressure", "", "")), "k8s:DiskPressure:KubeletHasDiskPressure", ""},
		{"degraded condition", nvs, node("a", cond("GpuThermalWatch", "True", "Throttled", "", "")), "", "k8s:GpuThermalWatch:Throttled"},
		{"lost gpu", nvs, node("a", gpus("7")), "k8s:gpus:7/8", ""},
		{"lost gpu degrades", Rules{ExpectedGPUs: 8, GPUsDegrade: true}, node("a", gpus("6")), "", "k8s:gpus:6/8"},
		{"no gpu resource", Rules{ExpectedGPUs: 8, GPUResource: "amd.com/gpu"}, node("a"), "k8s:gpus:0/8", ""},
		{"taint", Rules{DownTaints: []string{"nvidia.com/gpu-*"}}, node("a", taint("nvidia.com/gpu-xid-error", "NoSchedule")), "k8s:taint:nvidia.com/gpu-xid-error", ""},
		{"other taint", Rules{DownTaints: []string{"nvidia.com/gpu-*"}}, node("a", taint("dedicated", "NoSchedule")), "", ""},
		{"cordon ignored by default", nvs, func() Node { n := node("a"); n.Spec.Unschedulable = true; return n }(), "", ""},
		{"cordon counts", Rules{Cordoned: true}, func() Node { n := node("a"); n.Spec.Unschedulable = true; return n }(), "k8s:cordoned", ""},
		{"no ready condition", nvs, func() Node { n := node("a"); n.Status.Conditions = nil; return n }(), "k8s:NotReady:no-ready-condition", ""},
		{"two causes", nvs, node("a", cond("Ready", "False", "GpuHealthCheckFailed", "", ""), cond("SysLogsXIDError", "True", "", "[79] GPU has fallen off the bus", "")), "k8s:NotReady:GpuHealthCheckFailed+k8s:SysLogsXIDError:79", ""},
	}
	for _, c := range cases {
		h := c.rules.Assess(c.n)
		if got := Cause(h.Down); got != c.down {
			t.Errorf("%s: down %q, want %q", c.name, got, c.down)
		}
		if got := Cause(h.Degraded); got != c.degrade {
			t.Errorf("%s: degraded %q, want %q", c.name, got, c.degrade)
		}
	}
}

func TestRulesValidate(t *testing.T) {
	if err := (Rules{DownConditions: []string{"["}}).Validate(); err == nil {
		t.Fatal("bad pattern accepted")
	}
	if err := (Rules{DownTaints: []string{" "}}).Validate(); err == nil {
		t.Fatal("empty pattern accepted")
	}
}

func TestWatcherOutage(t *testing.T) {
	w, err := NewWatcher(Rules{NotReady: true, DownConditions: []string{"*"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t0 := ts("2026-10-03T04:00:00Z")
	if tr, _ := w.Step(list(node("gpu-003"), node("gpu-004")), t0); len(tr) != 0 {
		t.Fatalf("healthy cluster gave %v", tr)
	}
	// The fault began at 04:12, the watcher sees it at 04:12:30: the event
	// is dated by the condition.
	bad := node("gpu-003", cond("SysLogsXIDError", "True", "", "[79] GPU has fallen off the bus", "2026-10-03T04:12:00Z"))
	tr, _ := w.Step(list(bad, node("gpu-004")), ts("2026-10-03T04:12:30Z"))
	if len(tr) != 1 || tr[0].Type != events.Down || tr[0].TS != ts("2026-10-03T04:12:00Z") || tr[0].Cause != "k8s:SysLogsXIDError:79" {
		t.Fatalf("down: %+v", tr)
	}
	// Still down: nothing new, even with another cause added.
	worse := node("gpu-003", cond("SysLogsXIDError", "True", "", "[79]", "2026-10-03T04:12:00Z"), cond("Ready", "False", "GpuHealthCheckFailed", "", "2026-10-03T04:13:00Z"))
	if tr, _ := w.Step(list(worse, node("gpu-004")), ts("2026-10-03T04:13:30Z")); len(tr) != 0 {
		t.Fatalf("repeat: %+v", tr)
	}
	// Repaired: the recovery is dated by the latest condition change.
	fixed := node("gpu-003", cond("SysLogsXIDError", "False", "HealthCheckPassed", "", "2026-10-03T06:40:00Z"), cond("Ready", "True", "KubeletReady", "", "2026-10-03T06:39:00Z"))
	tr, _ = w.Step(list(fixed, node("gpu-004")), ts("2026-10-03T06:41:00Z"))
	if len(tr) != 1 || tr[0].Type != events.Up || tr[0].TS != ts("2026-10-03T06:40:00Z") {
		t.Fatalf("up: %+v", tr)
	}
	if d, g := w.Open(); len(d)+len(g) != 0 {
		t.Fatalf("open after repair: %v %v", d, g)
	}
}

func TestWatcherDatesAreClamped(t *testing.T) {
	w, _ := NewWatcher(Rules{DownTaints: []string{"repair"}, NotReady: true}, nil)
	now := ts("2026-10-05T10:00:00Z")
	// A taint has no transition time: the down event is dated now.
	tr, _ := w.Step(list(node("a", taint("repair", "NoSchedule"))), now)
	if len(tr) != 1 || tr[0].TS != now {
		t.Fatalf("down: %+v", tr)
	}
	// The taint goes; Ready last changed weeks ago, so it did not end the
	// outage: the recovery is dated by the view, not pulled back.
	later := ts("2026-10-05T12:00:00Z")
	tr, _ = w.Step(list(node("a")), later)
	if len(tr) != 1 || tr[0].Type != events.Up || tr[0].TS != later {
		t.Fatalf("up: %+v", tr)
	}
	// A condition dated in the future is dated now.
	w2, _ := NewWatcher(DefaultRules(), nil)
	tr, _ = w2.Step(list(node("b", cond("Ready", "False", "X", "", "2030-01-01T00:00:00Z"))), now)
	if len(tr) != 1 || tr[0].TS != now {
		t.Fatalf("future: %+v", tr)
	}
}

func TestWatcherScopeAndMissing(t *testing.T) {
	w, _ := NewWatcher(DefaultRules(), []string{"gpu-001", "gpu-002"})
	now := ts("2026-10-01T00:00:00Z")
	tr, _ := w.Step(list(node("gpu-001"), node("cpu-001", cond("Ready", "False", "X", "", ""))), now)
	if len(tr) != 1 || tr[0].Node != "gpu-002" || tr[0].Cause != "k8s:node-missing" {
		t.Fatalf("missing: %+v", tr)
	}
	tr, _ = w.Step(list(node("gpu-001"), node("gpu-002")), now+60)
	if len(tr) != 1 || tr[0].Type != events.Up || tr[0].Node != "gpu-002" {
		t.Fatalf("back: %+v", tr)
	}
	// Without a scope, a node that disappears after being seen is down.
	w2, _ := NewWatcher(DefaultRules(), nil)
	w2.Step(list(node("x"), node("y")), now)
	tr, _ = w2.Step(list(node("x")), now+60)
	if len(tr) != 1 || tr[0].Node != "y" || tr[0].Type != events.Down {
		t.Fatalf("vanished: %+v", tr)
	}
}

func TestWatcherNameLabel(t *testing.T) {
	r := DefaultRules()
	r.NameLabel = "example.com/contract-node"
	w, _ := NewWatcher(r, nil)
	a := node("ip-10-0-0-1", label(r.NameLabel, "gpu-001"), cond("Ready", "False", "X", "", ""))
	tr, _ := w.Step(list(a, node("unlabelled", cond("Ready", "False", "X", "", ""))), 100)
	if len(tr) != 1 || tr[0].Node != "gpu-001" {
		t.Fatalf("label: %+v", tr)
	}
	b := node("ip-10-0-0-2", label(r.NameLabel, "gpu-001"))
	if _, err := w.Step(list(a, b), 200); err == nil {
		t.Fatal("two nodes with one name accepted")
	}
}

func TestWatcherDegraded(t *testing.T) {
	w, _ := NewWatcher(Rules{NotReady: true, ExpectedGPUs: 8, GPUsDegrade: true}, nil)
	tr, _ := w.Step(list(node("a", gpus("7"))), 100)
	if len(tr) != 1 || tr[0].Type != events.DegradedStart || tr[0].Cause != "k8s:gpus:7/8" {
		t.Fatalf("start: %+v", tr)
	}
	tr, _ = w.Step(list(node("a", gpus("7"), cond("Ready", "False", "X", "", ""))), 200)
	if len(tr) != 1 || tr[0].Type != events.Down {
		t.Fatalf("down while degraded: %+v", tr)
	}
	tr, _ = w.Step(list(node("a")), 300)
	if len(tr) != 2 || tr[0].Type != events.Up || tr[1].Type != events.DegradedEnd {
		t.Fatalf("recovery: %+v", tr)
	}
}

func TestResume(t *testing.T) {
	dir := t.TempDir()
	_, priv, _ := ed25519.GenerateKey(nil)
	logPath := filepath.Join(dir, "k8s.jsonl")
	lw, err := evidence.OpenWriter(logPath, priv)
	if err != nil {
		t.Fatal(err)
	}
	for i, ev := range []events.Event{
		{TS: "2026-10-03T04:12:00Z", Node: "gpu-003", Type: events.Down, Cause: "k8s:x"},
		{TS: "2026-10-04T00:00:00Z", Node: "gpu-004", Type: events.Down, Cause: "k8s:x"},
		{TS: "2026-10-04T01:00:00Z", Node: "gpu-004", Type: events.Up},
	} {
		ev.ID = string(rune('a' + i))
		if _, err := lw.Append(evidence.KindEvent, ev); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := evidence.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	w, _ := NewWatcher(DefaultRules(), nil)
	if err := w.Resume(entries); err != nil {
		t.Fatal(err)
	}
	if d, _ := w.Open(); len(d) != 1 || d[0] != "gpu-003" {
		t.Fatalf("open after resume: %v", d)
	}
	// gpu-003 is still down after the restart: no second down event; when
	// it recovers, the up is not dated before the recorded down.
	tr, _ := w.Step(list(node("gpu-003", cond("Ready", "False", "X", "", "2026-10-03T04:12:00Z")), node("gpu-004")), ts("2026-10-05T00:00:00Z"))
	if len(tr) != 0 {
		t.Fatalf("after resume: %+v", tr)
	}
	tr, _ = w.Step(list(node("gpu-003"), node("gpu-004")), ts("2026-10-05T01:00:00Z"))
	if len(tr) != 1 || tr[0].Type != events.Up || tr[0].TS != ts("2026-10-05T01:00:00Z") {
		t.Fatalf("recovery after resume: %+v", tr)
	}
}

func TestParseNodeList(t *testing.T) {
	b, _ := json.Marshal(node("solo"))
	if _, err := ParseNodeList(b); err == nil {
		t.Fatal("object without kind accepted")
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	m["kind"] = "Node"
	b, _ = json.Marshal(m)
	l, err := ParseNodeList(b)
	if err != nil || len(l.Items) != 1 || l.Items[0].Metadata.Name != "solo" {
		t.Fatalf("single node: %v %+v", err, l)
	}
	if _, err := ParseNodeList([]byte(`{"kind":"List","items":[{"metadata":{}}]}`)); err == nil {
		t.Fatal("nameless node accepted")
	}
}

func TestSources(t *testing.T) {
	dir := t.TempDir()
	b, _ := json.Marshal(list(node("gpu-001")))
	f := filepath.Join(dir, "nodes.json")
	os.WriteFile(f, b, 0o644)
	l, err := FileSource{Path: f}.Nodes(context.Background())
	if err != nil || len(l.Items) != 1 {
		t.Fatalf("file: %v", err)
	}

	tokenFile := filepath.Join(dir, "token")
	os.WriteFile(tokenFile, []byte("secret-token\n"), 0o600)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/nodes" || r.Header.Get("Authorization") != "Bearer secret-token" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.URL.Query().Get("labelSelector") != "gpu=true" {
			http.Error(w, "selector", http.StatusBadRequest)
			return
		}
		w.Write([]byte(strings.Replace(string(b), `"List"`, `"NodeList"`, 1)))
	}))
	defer srv.Close()
	ca := filepath.Join(dir, "ca.crt")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o644)
	api, err := NewAPISource(srv.URL, tokenFile, ca, "gpu=true")
	if err != nil {
		t.Fatal(err)
	}
	l, err = api.Nodes(context.Background())
	if err != nil || len(l.Items) != 1 {
		t.Fatalf("api: %v", err)
	}
	os.WriteFile(tokenFile, []byte("rotated"), 0o600)
	if _, err := api.Nodes(context.Background()); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("rotated token not re-read: %v", err)
	}
	if _, err := NewAPISource("http://insecure", "", "", ""); err == nil {
		t.Fatal("plain http accepted")
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	if _, err := InCluster(""); err == nil {
		t.Fatal("in-cluster outside a pod")
	}
}
