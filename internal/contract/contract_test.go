package contract

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestExampleContractsLoad(t *testing.T) {
	for _, p := range []string{"../../examples/contracts/hgx-16-node.json", "../../examples/contracts/nvl72-2-racks.json"} {
		spec, err := Load(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if spec.MinIncident != 300 || spec.MaintenanceNotice != 72*3600 || spec.ExclusionCap != 8*3600 {
			t.Fatalf("%s: durations parsed wrongly: %+v", p, spec)
		}
		if _, err := spec.Digest(); err != nil {
			t.Fatal(err)
		}
	}
}

// The YAML authoring file and its JSON twin must be the same contract, down
// to the digest that statements fingerprint.
func TestYAMLAndJSONExamplesAreIdentical(t *testing.T) {
	j, err := Load("../../examples/contracts/hgx-16-node.json")
	if err != nil {
		t.Fatal(err)
	}
	y, err := Load("../../examples/contracts/hgx-16-node.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dj, _ := j.Digest()
	dy, _ := y.Digest()
	if dj != dy {
		t.Fatalf("digests differ: json %s, yaml %s", dj, dy)
	}
	// Sniffing without an extension also works.
	b, _ := os.ReadFile("../../examples/contracts/hgx-16-node.yaml")
	s, err := Read(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := s.Digest(); d != dj {
		t.Fatal("sniffed YAML gives a different digest")
	}
}

func TestYAMLErrorsCarryLineNumbers(t *testing.T) {
	src := "apiVersion: capacity-ledger/v1alpha1\nkind: ContractSLA\nmetadata: {id: x, version: 1}\ncluster:\n  gpusPerNode: eight\n"
	_, err := Decode([]byte(src), "yaml")
	if err == nil || !strings.Contains(err.Error(), "line 5: cluster.gpusPerNode: expected a whole number") {
		t.Fatalf("got %v", err)
	}
}

func TestRejectsInvalidContracts(t *testing.T) {
	base := `{"apiVersion":"capacity-ledger/v1alpha1","kind":"ContractSLA","metadata":{"id":"x","version":1},
	"cluster":{"gpusPerNode":8,"nodes":["a","b"]%s},"measurement":{%s},"exclusions":{},"credits":{"capPercent":0,"tiers":[%s]}}`
	cases := map[string][3]string{
		"rack node outside cluster": {`,"racks":[{"name":"r","nodes":["a","z"]}]`, ``, ``},
		"rack rule without racks":   {``, `"rackRule":{"nodesDownThreshold":3}`, ``},
		"bad duration":              {``, `"minIncident":"five minutes"`, ``},
		"float-like threshold":      {``, ``, `{"availabilityBelow":"99.9e0","creditPercent":5}`},
		"threshold above 100":       {``, ``, `{"availabilityBelow":"100.5","creditPercent":5}`},
	}
	for name, parts := range cases {
		doc := strings.Replace(base, "%s", parts[0], 1)
		doc = strings.Replace(doc, "%s", parts[1], 1)
		doc = strings.Replace(doc, "%s", parts[2], 1)
		if _, err := Read(strings.NewReader(doc)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	dup := strings.Replace(strings.Replace(strings.Replace(base, "%s", "", 3), `["a","b"]`, `["a","a"]`, 1), "%s", "", -1)
	if _, err := Read(strings.NewReader(dup)); err == nil {
		t.Error("duplicate node: expected an error")
	}
}
