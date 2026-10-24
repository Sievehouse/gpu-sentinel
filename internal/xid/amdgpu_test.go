package xid

import (
	"bufio"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The sample lines follow upstream amdgpu driver message formats; they are
// synthetic, not captured from a production host.
func TestParseAMDSample(t *testing.T) {
	f, err := os.Open("../../examples/samples/kern-amd.log")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	policy, err := DefaultPolicy()
	if err != nil {
		t.Fatal(err)
	}
	type got struct {
		class, block, device, ts string
		count                    int64
		action                   Action
	}
	var all []got
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		o, ok := ParseLine(sc.Text())
		if !ok {
			continue
		}
		all = append(all, got{o.Class, o.Block, o.Device, o.TS, o.Count, policy.Classify(o).Action})
	}
	want := []got{
		{AMDRASCorrectable, "umc", "0000:05:00.0", "2026-10-07T09:20:11Z", 3, Observe},
		{AMDRingTimeout, "gfx_0.0.0", "0000:65:00.0", "2026-10-07T09:41:37Z", 0, Review},
		{AMDGPUReset, "", "0000:65:00.0", "2026-10-07T09:41:38Z", 0, Review},
		{AMDGPUResetSucceeded, "", "0000:65:00.0", "2026-10-07T09:41:52Z", 0, Observe},
		{AMDRASUncorrectable, "umc", "0000:05:00.0", "2026-10-07T11:02:45Z", 1, NodeDown},
		{AMDPageFault, "", "0000:05:00.0", "2026-10-07T11:02:46Z", 0, App},
	}
	if !reflect.DeepEqual(all, want) {
		t.Fatalf("got  %+v\nwant %+v", all, want)
	}
	if _, ok := ParseLine("amdgpu 0000:05:00.0: amdgpu: GPU reset(3) failed"); !ok {
		t.Fatal("reset failure not recognised")
	}
}

func fakeRAS(t *testing.T, root, card, pci, block, content string) {
	t.Helper()
	devDir := filepath.Join(root, "devices", "pci0000:00", pci)
	if err := os.MkdirAll(filepath.Join(devDir, "ras"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(devDir, "ras", block+"_err_count"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cardDir := filepath.Join(root, "class", "drm", card)
	if err := os.MkdirAll(cardDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(cardDir, "device")
	if _, err := os.Lstat(link); err != nil {
		if err := os.Symlink(devDir, link); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRASCountersAndTracker(t *testing.T) {
	root := t.TempDir()
	fakeRAS(t, root, "card1", "0000:05:00.0", "umc", "ue: 0\nce: 2\n")
	fakeRAS(t, root, "card1", "0000:05:00.0", "gfx", "ue: 0\nce: 0\n")
	fakeRAS(t, root, "card2", "0000:65:00.0", "umc", "ue: 0\nce: 0\n")
	first, err := ReadRASCounters(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 3 || first[0].Device != "0000:05:00.0" || first[0].Block != "gfx" {
		t.Fatalf("counters %+v", first)
	}
	tr := NewRASTracker()
	if obs := tr.Update(first); len(obs) != 0 {
		t.Fatalf("baseline produced observations: %+v", obs)
	}
	fakeRAS(t, root, "card1", "0000:05:00.0", "umc", "ue: 1\nce: 5\n")
	second, _ := ReadRASCounters(root)
	obs := tr.Update(second)
	if len(obs) != 2 || obs[0].Class != AMDRASUncorrectable || obs[0].Count != 1 || obs[1].Class != AMDRASCorrectable || obs[1].Count != 3 {
		t.Fatalf("deltas %+v", obs)
	}
	// A counter that goes down (driver reload) is re-baselined, not reported.
	fakeRAS(t, root, "card1", "0000:05:00.0", "umc", "ue: 0\nce: 0\n")
	third, _ := ReadRASCounters(root)
	if obs := tr.Update(third); len(obs) != 0 {
		t.Fatalf("reset counter reported: %+v", obs)
	}
	// No amdgpu RAS support: no counters, no error.
	if none, err := ReadRASCounters(t.TempDir()); err != nil || len(none) != 0 {
		t.Fatalf("empty sysfs: %v %v", none, err)
	}
}

func TestCountAMDGPUs(t *testing.T) {
	root := t.TempDir()
	drv := filepath.Join(root, "bus", "pci", "drivers", "amdgpu")
	for _, name := range []string{"0000:05:00.0", "0000:65:00.0", "bind", "module", "new_id"} {
		if err := os.MkdirAll(filepath.Join(drv, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	n, err := CountAMDGPUs(root)
	if err != nil || n != 2 {
		t.Fatalf("got %d, %v", n, err)
	}
}

func TestPolicyRejectsUnknownAMDClass(t *testing.T) {
	_, err := ParsePolicy([]byte(`{"version":"x","xid":{},"amd":{"gpu_on_fire":{"action":"node_down"}},"default":{"action":"review"}}`))
	if err == nil {
		t.Fatal("unknown AMD class accepted")
	}
}
