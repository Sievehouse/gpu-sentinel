package xid

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// AMD observation classes. The kernel-log patterns below follow message
// strings in the upstream amdgpu driver (amdgpu_ras.c, amdgpu_device.c,
// amdgpu_job.c, gmc). Driver versions differ: validate them against your
// own logs before relying on them for SLA decisions.
const (
	AMDRASUncorrectable  = "ras_uncorrectable"
	AMDRASCorrectable    = "ras_correctable"
	AMDGPUReset          = "gpu_reset"
	AMDGPUResetSucceeded = "gpu_reset_succeeded"
	AMDGPUResetFailed    = "gpu_reset_failed"
	AMDRingTimeout       = "ring_timeout"
	AMDPageFault         = "page_fault"
)

// AMDClasses lists every class a policy may map.
var AMDClasses = []string{AMDGPUReset, AMDGPUResetFailed, AMDGPUResetSucceeded, AMDPageFault, AMDRASCorrectable, AMDRASUncorrectable, AMDRingTimeout}

func knownAMDClass(c string) bool {
	for _, k := range AMDClasses {
		if k == c {
			return true
		}
	}
	return false
}

var (
	amdLineRe    = regexp.MustCompile(`amdgpu ([0-9A-Fa-f]{4}:[0-9A-Fa-f]{2}:[0-9A-Fa-f]{2}\.[0-9A-Fa-f]): (.*)$`)
	amdUERe      = regexp.MustCompile(`(\d+) uncorrectable hardware errors? (?:was |were )?detected in (?:the )?(\S+) block`)
	amdCERe      = regexp.MustCompile(`(\d+) correctable hardware errors? (?:was |were )?detected in (?:the )?(\S+) block`)
	amdFailRe    = regexp.MustCompile(`(?i)GPU reset(?:\(\d+\))? failed|GPU Recovery Failed|ASIC reset failed`)
	amdSuccessRe = regexp.MustCompile(`GPU reset(?:\(\d+\))? succeeded`)
	amdBeginRe   = regexp.MustCompile(`GPU reset begin`)
	amdRingRe    = regexp.MustCompile(`ring (\S+) timeout`)
	amdFaultRe   = regexp.MustCompile(`page fault`)
)

// parseAMDLine recognises amdgpu kernel messages that matter for an SLA.
// Other amdgpu messages are not observations.
func parseAMDLine(line string) (Observation, bool) {
	m := amdLineRe.FindStringSubmatch(line)
	if m == nil {
		return Observation{}, false
	}
	msg := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(m[2]), "amdgpu:"))
	o := Observation{Kind: "amdgpu", Device: strings.ToLower(m[1]), Message: msg, Raw: line}
	switch {
	case amdUERe.MatchString(msg):
		sub := amdUERe.FindStringSubmatch(msg)
		o.Class, o.Block = AMDRASUncorrectable, sub[2]
		o.Count, _ = strconv.ParseInt(sub[1], 10, 64)
	case amdCERe.MatchString(msg):
		sub := amdCERe.FindStringSubmatch(msg)
		o.Class, o.Block = AMDRASCorrectable, sub[2]
		o.Count, _ = strconv.ParseInt(sub[1], 10, 64)
	case amdFailRe.MatchString(msg):
		o.Class = AMDGPUResetFailed
	case amdSuccessRe.MatchString(msg):
		o.Class = AMDGPUResetSucceeded
	case amdBeginRe.MatchString(msg):
		o.Class = AMDGPUReset
	case amdRingRe.MatchString(msg):
		o.Class = AMDRingTimeout
		o.Block = amdRingRe.FindStringSubmatch(msg)[1]
	case amdFaultRe.MatchString(msg):
		o.Class = AMDPageFault
	default:
		return Observation{}, false
	}
	if tm := tsRe.FindStringSubmatch(line); tm != nil {
		if t, ok := parseLogTime(tm[1]); ok {
			o.TS = t.UTC().Format(time.RFC3339)
		}
	}
	return o, true
}
