package xid

import "testing"

func TestRedact(t *testing.T) {
	cases := []struct{ in, want string }{
		{"NVRM: Xid (PCI:0000:5d:00): 94, pid=48211, name=python3, Contained ECC error.", "NVRM: Xid (PCI:0000:5d:00): 94, pid=<redacted>, name=<redacted>, Contained ECC error."},
		{"NVRM: Xid (PCI:0000:3b:00): 79, pid=0, GPU has fallen off the bus.", "NVRM: Xid (PCI:0000:3b:00): 79, pid=<redacted>, GPU has fallen off the bus."},
		{"NVRM: Xid (PCI:0000:3b:00): 79, GPU has fallen off the bus.", "NVRM: Xid (PCI:0000:3b:00): 79, GPU has fallen off the bus."},
		{"amdgpu: [gfxhub] page fault (src_id:0 ring:24 vmid:3 pasid:32769, for process python3 pid 48211 thread python3 pid 48211)",
			"amdgpu: [gfxhub] page fault (src_id:0 ring:24 vmid:3 pasid:32769, for process <redacted> pid <redacted> thread <redacted> pid <redacted>)"},
		{"amdgpu: Process info: process train.py pid 7 thread train.py pid 8", "amdgpu: Process info: process <redacted> pid <redacted> thread <redacted> pid <redacted>"},
	}
	for _, c := range cases {
		if got := RedactText(c.in); got != c.want {
			t.Errorf("RedactText(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
		if again := RedactText(RedactText(c.in)); again != c.want {
			t.Errorf("redaction is not idempotent: %q", again)
		}
	}
	line := "2026-10-03T05:02:10+0000 gpu-003 kernel: NVRM: Xid (PCI:0000:5d:00): 94, pid=48211, name=python3, Contained ECC error."
	o, ok := ParseLine(line)
	if !ok {
		t.Fatal("not parsed")
	}
	r := Redact(o)
	if !r.Redacted || r.Code != 94 || r.Device != o.Device || r.TS != o.TS {
		t.Fatalf("redaction changed the fault: %+v", r)
	}
	clean, _ := ParseLine("2026-10-03T04:11:58+0000 gpu-003 kernel: NVRM: Xid (PCI:0000:3b:00): 79, GPU has fallen off the bus.")
	if Redact(clean).Redacted {
		t.Fatal("a line without process details is marked redacted")
	}
}
