package yamlite

import (
	"reflect"
	"strings"
	"testing"
)

type tier struct {
	Below   string `json:"availabilityBelow"`
	Percent int    `json:"creditPercent"`
}

type rack struct {
	Name  string   `json:"name"`
	Nodes []string `json:"nodes"`
}

type doc struct {
	ID      string            `json:"id"`
	Version int               `json:"version"`
	Enabled bool              `json:"enabled"`
	Ratio   float64           `json:"ratio"`
	Nodes   []string          `json:"nodes"`
	Racks   []rack            `json:"racks"`
	Tiers   []tier            `json:"tiers"`
	Labels  map[string]string `json:"labels"`
	Rule    *struct {
		N int `json:"n"`
	} `json:"rule"`
	Extra any `json:"extra"`
}

const sample = `---
# leading comment
id: "contract-1"   # trailing comment
version: 3
enabled: true
ratio: 0.25
nodes: [gpu-001, "gpu-002", 003,
        gpu-#4, 'it''s']
racks:
  - name: rack-a
    nodes: [a1, a2]
  - name: rack-b
    nodes:
      - b1
      - b2
tiers:
- { availabilityBelow: "99.9", creditPercent: 5 }
- {availabilityBelow: 99.5, creditPercent: 10}
labels:
  owner: Lab's team # an apostrophe in plain text does not hide this comment
  url: https://example.com/a:b
rule:
  n: 3
extra: {a: [1, 2.5, x, true, null], b: "q"}
`

func TestDecodeSample(t *testing.T) {
	var d doc
	if err := Unmarshal([]byte(sample), &d); err != nil {
		t.Fatal(err)
	}
	if d.ID != "contract-1" || d.Version != 3 || !d.Enabled || d.Ratio != 0.25 {
		t.Fatalf("scalars wrong: %+v", d)
	}
	wantNodes := []string{"gpu-001", "gpu-002", "003", "gpu-#4", "it's"}
	if !reflect.DeepEqual(d.Nodes, wantNodes) {
		t.Fatalf("nodes %q, want %q", d.Nodes, wantNodes)
	}
	wantRacks := []rack{{"rack-a", []string{"a1", "a2"}}, {"rack-b", []string{"b1", "b2"}}}
	if !reflect.DeepEqual(d.Racks, wantRacks) {
		t.Fatalf("racks %+v", d.Racks)
	}
	// Unquoted 99.5 decoded into a string keeps its exact text.
	if !reflect.DeepEqual(d.Tiers, []tier{{"99.9", 5}, {"99.5", 10}}) {
		t.Fatalf("tiers %+v", d.Tiers)
	}
	if d.Labels["owner"] != "Lab's team" || d.Labels["url"] != "https://example.com/a:b" {
		t.Fatalf("labels %q", d.Labels)
	}
	if d.Rule == nil || d.Rule.N != 3 {
		t.Fatalf("rule %+v", d.Rule)
	}
	extra := d.Extra.(map[string]any)
	if !reflect.DeepEqual(extra["a"], []any{int64(1), 2.5, "x", true, nil}) || extra["b"] != "q" {
		t.Fatalf("extra %#v", extra)
	}
}

func TestEmptyValuesAndCompactSequence(t *testing.T) {
	var d doc
	src := "id:\nnodes:\n- a\n- b\nrule:\n"
	if err := Unmarshal([]byte(src), &d); err != nil {
		t.Fatal(err)
	}
	if d.ID != "" || !reflect.DeepEqual(d.Nodes, []string{"a", "b"}) || d.Rule != nil {
		t.Fatalf("got %+v", d)
	}
}

func TestErrorsNameTheLine(t *testing.T) {
	cases := map[string]struct {
		src, want string
	}{
		"duplicate key":    {"id: a\nversion: 1\nid: b\n", "line 3: duplicate key \"id\" (first used on line 1)"},
		"unknown field":    {"id: a\nverison: 1\n", "line 2: document: unknown field \"verison\""},
		"tab indent":       {"racks:\n\t- name: a\n", "line 2: tabs are not allowed"},
		"anchor":           {"id: &x a\n", "line 1: unsupported YAML feature \"&\""},
		"block scalar":     {"id: |\n  text\n", "line 1: unsupported YAML feature \"|\""},
		"colon in value":   {"id: a: b\n", "line 1: plain value \"a: b\" contains \": \""},
		"unclosed flow":    {"nodes: [a, b,\n", "line 1: flow collection is never closed"},
		"bad int":          {"version: three\n", "line 1: version: expected a whole number"},
		"quoted bool":      {"enabled: \"true\"\n", "line 1: enabled: expected true or false"},
		"multi-doc":        {"id: a\n---\nid: b\n", "line 2: several YAML documents"},
		"wrong shape":      {"nodes: a\n", "line 1: nodes: expected a sequence"},
		"bad indentation":  {"id: a\n  version: 1\n", "line 2: unexpected indentation"},
		"nested seq":       {"nodes:\n  - - a\n", "nested inline sequences"},
		"unterminated":     {"id: \"abc\n", "unterminated quoted scalar"},
		"flow dup key":     {"extra: {a: 1, a: 2}\n", "duplicate key \"a\""},
		"empty flow entry": {"nodes: [a, , b]\n", "empty entry in flow collection"},
		"seq under map":    {"id: a\n- b\n", "line 2: a sequence item"},
		"int overflow":     {"version: 99999999999999999999\n", "out of range"},
	}
	for name, c := range cases {
		var d doc
		err := Unmarshal([]byte(c.src), &d)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got error %v, want it to contain %q", name, err, c.want)
		}
	}
}

func TestCRLFAndBOM(t *testing.T) {
	var d doc
	if err := Unmarshal([]byte("\ufeffid: a\r\nversion: 2\r\n"), &d); err != nil {
		t.Fatal(err)
	}
	if d.ID != "a" || d.Version != 2 {
		t.Fatalf("got %+v", d)
	}
}

func TestEscapes(t *testing.T) {
	var m map[string]string
	src := `a: "tab\tquote\" slash\\ é"` + "\nb: 'single ''quoted'' # not a comment'\n"
	if err := Unmarshal([]byte(src), &m); err != nil {
		t.Fatal(err)
	}
	if m["a"] != "tab\tquote\" slash\\ é" || m["b"] != "single 'quoted' # not a comment" {
		t.Fatalf("got %q", m)
	}
}

func TestDepthLimits(t *testing.T) {
	var g any
	deepFlow := "a: " + strings.Repeat("[", 100) + strings.Repeat("]", 100) + "\n"
	if err := Unmarshal([]byte(deepFlow), &g); err == nil || !strings.Contains(err.Error(), "nested more than") {
		t.Fatalf("deep flow: %v", err)
	}
	var b strings.Builder
	for i := 0; i < 100; i++ {
		b.WriteString(strings.Repeat(" ", i) + "k:\n")
	}
	if err := Unmarshal([]byte(b.String()), &g); err == nil || !strings.Contains(err.Error(), "nested more than") {
		t.Fatalf("deep blocks: %v", err)
	}
	long := "nodes: [a,\n" + strings.Repeat("  b,\n", 20000) + "  c]\n"
	var d doc
	if err := Unmarshal([]byte(long), &d); err != nil || len(d.Nodes) != 20002 {
		t.Fatalf("long flow: %v, %d nodes", err, len(d.Nodes))
	}
}
