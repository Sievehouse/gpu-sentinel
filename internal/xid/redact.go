package xid

import "regexp"

// Redaction removes process names and ids from kernel fault lines before
// they are recorded. NVIDIA XID lines carry "pid=48211, name=python3", and
// amdgpu page faults "process python3 pid 48211 thread python3 pid 48211":
// enough to tell which customer job was running on a GPU.
//
// Redaction is deterministic and keeps everything the fault policy and the
// SLA engine use (the code, the device, the rest of the message), so a
// party holding the original kernel log can redact it the same way and
// compare it with the recorded line.
var redactions = []struct {
	re   *regexp.Regexp
	with string
}{
	// NVIDIA: "pid=48211, name=python3, ..." and "pid=48211, ..."
	{regexp.MustCompile(`\bpid=\d+, name=[^,]*`), "pid=<redacted>, name=<redacted>"},
	{regexp.MustCompile(`\bpid=\d+`), "pid=<redacted>"},
	{regexp.MustCompile(`\bname=[^,]*`), "name=<redacted>"},
	// amdgpu: "process python3 pid 48211 thread python3 pid 48211" and
	// "Process info: process python3 pid 48211"
	{regexp.MustCompile(`\b(process|thread) \S+ pid \d+`), "$1 <redacted> pid <redacted>"},
	{regexp.MustCompile(`\b(comm|task)[=:]\s*\S+`), "$1=<redacted>"},
}

// RedactText applies the redactions to one line of text.
func RedactText(s string) string {
	for _, r := range redactions {
		s = r.re.ReplaceAllString(s, r.with)
	}
	return s
}

// Redact returns o with process names and ids removed from its message and
// raw line. It marks the observation as redacted only if something changed.
func Redact(o Observation) Observation {
	msg, raw := RedactText(o.Message), RedactText(o.Raw)
	if msg != o.Message || raw != o.Raw {
		o.Message, o.Raw, o.Redacted = msg, raw, true
	}
	return o
}
