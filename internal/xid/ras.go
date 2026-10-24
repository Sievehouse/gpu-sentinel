package xid

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// RASCounter is one amdgpu RAS error counter read from sysfs, for example
// /sys/class/drm/card1/device/ras/umc_err_count, whose content is
//
//	ue: 0
//	ce: 3
type RASCounter struct {
	Device string `json:"device"` // PCI address
	Card   string `json:"card"`
	Block  string `json:"block"`
	UE     int64  `json:"ue"`
	CE     int64  `json:"ce"`
}

func (c RASCounter) key() string { return c.Device + "/" + c.Block }

// ReadRASCounters reads every amdgpu RAS counter under sysRoot (normally
// "/sys"). Hosts without amdgpu RAS support return no counters, not an error.
func ReadRASCounters(sysRoot string) ([]RASCounter, error) {
	files, err := filepath.Glob(filepath.Join(sysRoot, "class", "drm", "card*", "device", "ras", "*_err_count"))
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []RASCounter
	for _, f := range files {
		cardDir := filepath.Dir(filepath.Dir(filepath.Dir(f)))
		card := filepath.Base(cardDir)
		device := card
		if target, err := filepath.EvalSymlinks(filepath.Join(cardDir, "device")); err == nil {
			device = filepath.Base(target)
		}
		block := strings.TrimSuffix(filepath.Base(f), "_err_count")
		c := RASCounter{Device: strings.ToLower(device), Card: card, Block: block}
		if seen[c.key()] {
			continue
		}
		if err := readUECE(f, &c); err != nil {
			return nil, err
		}
		seen[c.key()] = true
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out, nil
}

func readUECE(path string, c *RASCounter) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	got := 0
	for sc.Scan() {
		name, value, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return fmt.Errorf("%s: %q is not a count", path, sc.Text())
		}
		switch strings.TrimSpace(name) {
		case "ue":
			c.UE, got = n, got+1
		case "ce":
			c.CE, got = n, got+1
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if got != 2 {
		return fmt.Errorf("%s: expected \"ue:\" and \"ce:\" lines", path)
	}
	return nil
}

// RASTracker turns successive counter readings into observations. The first
// reading of a counter is its baseline; a counter that goes down (driver
// reload) is re-baselined, not reported.
type RASTracker struct {
	last map[string]RASCounter
}

// NewRASTracker returns an empty tracker.
func NewRASTracker() *RASTracker { return &RASTracker{last: map[string]RASCounter{}} }

// Update compares a reading with the previous one and returns one
// observation per counter that increased.
func (t *RASTracker) Update(readings []RASCounter) []Observation {
	var out []Observation
	for _, c := range readings {
		prev, ok := t.last[c.key()]
		t.last[c.key()] = c
		if !ok || c.UE < prev.UE || c.CE < prev.CE {
			continue
		}
		if d := c.UE - prev.UE; d > 0 {
			out = append(out, Observation{Kind: "amdgpu", Class: AMDRASUncorrectable, Block: c.Block, Count: d, Device: c.Device,
				Message: fmt.Sprintf("%d new uncorrectable errors in %s (sysfs counter %d -> %d)", d, c.Block, prev.UE, c.UE)})
		}
		if d := c.CE - prev.CE; d > 0 {
			out = append(out, Observation{Kind: "amdgpu", Class: AMDRASCorrectable, Block: c.Block, Count: d, Device: c.Device,
				Message: fmt.Sprintf("%d new correctable errors in %s (sysfs counter %d -> %d)", d, c.Block, prev.CE, c.CE)})
		}
	}
	return out
}

var pciAddrRe = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-9a-f]$`)

// CountAMDGPUs counts PCI functions bound to the amdgpu driver under sysRoot.
// Partitioned GPUs can expose more functions; set the expected count to what
// a healthy node shows.
func CountAMDGPUs(sysRoot string) (int, error) {
	entries, err := os.ReadDir(filepath.Join(sysRoot, "bus", "pci", "drivers", "amdgpu"))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if pciAddrRe.MatchString(strings.ToLower(e.Name())) {
			n++
		}
	}
	return n, nil
}
