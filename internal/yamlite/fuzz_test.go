package yamlite

import "testing"

// FuzzUnmarshal checks that no input makes the decoder panic.
// Run longer with: go test -fuzz=FuzzUnmarshal ./internal/yamlite
func FuzzUnmarshal(f *testing.F) {
	f.Add(sample)
	f.Add("a: [1, {b: 'c'}]\n- x\n")
	f.Add("- - a\n")
	f.Add("? k\n")
	f.Add("a: \"\\u12\"\n")
	f.Add("{a: [b, {c: d}}\n")
	f.Fuzz(func(t *testing.T, src string) {
		var d doc
		_ = Unmarshal([]byte(src), &d)
		var g any
		_ = Unmarshal([]byte(src), &g)
	})
}
