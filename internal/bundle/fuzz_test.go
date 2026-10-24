package bundle

import (
	"bytes"
	"testing"
)

// FuzzVerify feeds mutated bundles to Verify. It must never panic, and a
// bundle that verifies must list exactly the files it holds.
func FuzzVerify(f *testing.F) {
	var buf bytes.Buffer
	if _, err := Create(&buf, Options{Type: TypeAcceptance, Subject: "fuzz", Created: created}, sample(), key(1)); err != nil {
		f.Fatal(err)
	}
	f.Add(buf.Bytes())
	f.Add([]byte{})
	f.Add([]byte{0x1f, 0x8b})
	tr := trust(key(1))
	f.Fuzz(func(t *testing.T, b []byte) {
		rep := Verify(bytes.NewReader(b), tr)
		if rep.OK && (len(rep.Files) == 0 || len(rep.Problems) != 0) {
			t.Fatalf("inconsistent report: %+v", rep)
		}
	})
}
