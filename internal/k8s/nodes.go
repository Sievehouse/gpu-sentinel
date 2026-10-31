// Package k8s turns the health that Kubernetes reports for its nodes into
// SLA events: node conditions (Ready, and the conditions that health
// monitors such as NVIDIA NVSentinel or node-problem-detector set), taints,
// cordons and the number of allocatable GPUs.
//
// The watcher is an integration on the provider's side, like the Slurm
// watcher: it records what the provider's own control plane says about each
// node, so that repairs close the incidents the kernel-log agent opens. It
// decides nothing about the contract; the SLA engine does.
package k8s

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// NodeList is the part of a Kubernetes NodeList (or "kubectl get nodes -o
// json" output) that the watcher reads.
type NodeList struct {
	Kind  string `json:"kind"`
	Items []Node `json:"items"`
}

// Node is the part of a Kubernetes Node object that the watcher reads.
type Node struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Unschedulable bool    `json:"unschedulable"`
		Taints        []Taint `json:"taints"`
	} `json:"spec"`
	Status struct {
		Conditions  []Condition       `json:"conditions"`
		Allocatable map[string]string `json:"allocatable"`
		Capacity    map[string]string `json:"capacity"`
	} `json:"status"`
}

// Taint is a node taint.
type Taint struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	Effect    string `json:"effect"`
	TimeAdded string `json:"timeAdded"`
}

// Condition is a node condition.
type Condition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason"`
	Message            string `json:"message"`
	LastTransitionTime string `json:"lastTransitionTime"`
}

// ParseNodeList reads a NodeList, a List of nodes (kubectl) or a single Node.
func ParseNodeList(b []byte) (*NodeList, error) {
	var l NodeList
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, fmt.Errorf("node list: %w", err)
	}
	switch l.Kind {
	case "NodeList", "List":
	case "Node":
		var n Node
		if err := json.Unmarshal(b, &n); err != nil {
			return nil, fmt.Errorf("node: %w", err)
		}
		l.Items = []Node{n}
	default:
		return nil, fmt.Errorf("expected a NodeList, List or Node, not kind %q", l.Kind)
	}
	for i, n := range l.Items {
		if n.Metadata.Name == "" {
			return nil, fmt.Errorf("node %d has no name", i)
		}
	}
	return &l, nil
}

// builtinConditions are set by the kubelet. Wildcard patterns never match
// them; name one exactly to use it (Ready is governed by Rules.NotReady).
var builtinConditions = map[string]bool{
	"Ready": true, "MemoryPressure": true, "DiskPressure": true, "PIDPressure": true, "NetworkUnavailable": true,
}

// Rules say which node states count as down and which as degraded.
type Rules struct {
	// NotReady makes a node whose Ready condition is not True down.
	NotReady bool
	// Cordoned makes a node marked unschedulable down. Off by default:
	// operators cordon for many reasons, and health monitors that cordon
	// on a fault (NVSentinel) also set a condition.
	Cordoned bool
	// DownConditions and DegradedConditions are condition types, or
	// patterns such as "*" or "Gpu*" (path.Match syntax), whose status
	// True means down or degraded. NVSentinel and node-problem-detector set
	// a condition to True while a fault lasts.
	DownConditions     []string
	DegradedConditions []string
	// DownTaints and DegradedTaints are taint keys, or patterns such as
	// "nvidia.com/gpu-*", whose presence means down or degraded.
	DownTaints     []string
	DegradedTaints []string
	// GPUResource is the extended resource that counts GPUs, such as
	// nvidia.com/gpu or amd.com/gpu. Empty means whichever of the two the
	// node reports.
	GPUResource string
	// ExpectedGPUs, if positive, makes a node with fewer allocatable GPUs
	// down (or degraded, with GPUsDegrade).
	ExpectedGPUs int
	GPUsDegrade  bool
	// NameLabel, if set, names the node by this label's value instead of
	// metadata.name, for clusters whose node names differ from the
	// contract's.
	NameLabel string
}

// DefaultRules counts a node that is not Ready as down.
func DefaultRules() Rules { return Rules{NotReady: true} }

// Validate checks the patterns.
func (r Rules) Validate() error {
	for _, set := range [][]string{r.DownConditions, r.DegradedConditions, r.DownTaints, r.DegradedTaints} {
		for _, p := range set {
			if strings.TrimSpace(p) == "" {
				return errors.New("empty condition or taint pattern")
			}
			if _, err := path.Match(p, ""); err != nil {
				return fmt.Errorf("pattern %q: %w", p, err)
			}
		}
	}
	if r.ExpectedGPUs < 0 {
		return errors.New("expected GPUs must not be negative")
	}
	return nil
}

// Name returns the node's name for the contract, or "" if NameLabel is set
// and the node does not carry it.
func (r Rules) Name(n Node) string {
	if r.NameLabel == "" {
		return n.Metadata.Name
	}
	return n.Metadata.Labels[r.NameLabel]
}

// Reason is one thing that makes a node down or degraded.
type Reason struct {
	// Cause is short and stable, e.g. "k8s:NotReady:KubeletNotReady" or
	// "k8s:SysLogsXIDError". It becomes the event's cause.
	Cause string
	// Detail is free text, e.g. a condition message.
	Detail string
	// Since is when the condition last changed (Unix seconds), or 0.
	Since int64
}

// Health is a node's state under the rules.
type Health struct {
	Down     []Reason
	Degraded []Reason
	// Changes are the transition times (Unix seconds) of every condition
	// the rules look at, used to date recoveries.
	Changes []int64
}

// IsDown and IsDegraded report the verdict.
func (h Health) IsDown() bool     { return len(h.Down) > 0 }
func (h Health) IsDegraded() bool { return len(h.Degraded) > 0 }

// Assess applies the rules to one node.
func (r Rules) Assess(n Node) Health {
	var h Health
	for _, c := range n.Status.Conditions {
		since := parseTime(c.LastTransitionTime)
		if c.Type == "Ready" {
			// Ready is the only condition whose healthy status is True, and
			// only the NotReady rule looks at it.
			if !r.NotReady {
				continue
			}
			if since > 0 {
				h.Changes = append(h.Changes, since)
			}
			if c.Status != "True" {
				reason := c.Reason
				if reason == "" {
					reason = "status-" + c.Status
				}
				h.Down = append(h.Down, Reason{Cause: "k8s:NotReady:" + slug(reason), Detail: c.Message, Since: since})
			}
			continue
		}
		// An exact name is more specific than a pattern, so
		// --down-condition '*' --degraded-condition GpuThermalWatch makes
		// thermal trouble degraded and every other fault down.
		dn, dg := matchCondition(r.DownConditions, c.Type), matchCondition(r.DegradedConditions, c.Type)
		down := dn > 0 && dn >= dg
		degraded := dg > dn
		if !down && !degraded {
			continue
		}
		if since > 0 {
			h.Changes = append(h.Changes, since)
		}
		if c.Status != "True" {
			continue
		}
		cause := "k8s:" + slug(c.Type)
		if code := errorCode(c.Message); code != "" {
			cause += ":" + code
		} else if c.Reason != "" {
			cause += ":" + slug(c.Reason)
		}
		re := Reason{Cause: cause, Detail: c.Message, Since: since}
		if down {
			h.Down = append(h.Down, re)
		} else {
			h.Degraded = append(h.Degraded, re)
		}
	}
	if r.NotReady && !hasCondition(n, "Ready") {
		h.Down = append(h.Down, Reason{Cause: "k8s:NotReady:no-ready-condition"})
	}
	for _, t := range n.Spec.Taints {
		since := parseTime(t.TimeAdded)
		switch {
		case matchAny(r.DownTaints, t.Key):
			h.Down = append(h.Down, Reason{Cause: "k8s:taint:" + t.Key, Detail: strings.Trim(t.Value+" "+t.Effect, " "), Since: since})
		case matchAny(r.DegradedTaints, t.Key):
			h.Degraded = append(h.Degraded, Reason{Cause: "k8s:taint:" + t.Key, Detail: strings.Trim(t.Value+" "+t.Effect, " "), Since: since})
		}
	}
	if r.Cordoned && n.Spec.Unschedulable {
		h.Down = append(h.Down, Reason{Cause: "k8s:cordoned"})
	}
	if r.ExpectedGPUs > 0 {
		have, res := r.gpus(n)
		if have < r.ExpectedGPUs {
			re := Reason{Cause: fmt.Sprintf("k8s:gpus:%d/%d", have, r.ExpectedGPUs), Detail: res + " allocatable"}
			if r.GPUsDegrade {
				h.Degraded = append(h.Degraded, re)
			} else {
				h.Down = append(h.Down, re)
			}
		}
	}
	sortReasons(h.Down)
	sortReasons(h.Degraded)
	return h
}

// matchCondition returns 2 if a pattern names typ exactly, 1 if a wildcard
// pattern matches it, and 0 otherwise.
func matchCondition(patterns []string, typ string) int {
	best := 0
	for _, p := range patterns {
		if p == typ {
			return 2
		}
		if builtinConditions[typ] || !strings.ContainsAny(p, "*?[") {
			continue
		}
		if ok, _ := path.Match(p, typ); ok {
			best = 1
		}
	}
	return best
}

func matchAny(patterns []string, s string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, s); ok {
			return true
		}
	}
	return false
}

func hasCondition(n Node, typ string) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == typ {
			return true
		}
	}
	return false
}

// gpus returns the allocatable GPU count and the resource it came from.
func (r Rules) gpus(n Node) (int, string) {
	resources := []string{r.GPUResource}
	if r.GPUResource == "" {
		resources = []string{"nvidia.com/gpu", "amd.com/gpu"}
	}
	for _, res := range resources {
		if v, ok := n.Status.Allocatable[res]; ok {
			c, err := strconv.Atoi(v)
			if err != nil {
				return 0, res + " (unreadable: " + v + ")"
			}
			return c, res
		}
	}
	return 0, strings.Join(resources, " or ") + " (not reported)"
}

func sortReasons(rs []Reason) {
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].Cause < rs[j].Cause })
}

// Cause joins the causes of reasons into one event cause, such as
// "k8s:NotReady:KubeletNotReady+k8s:SysLogsXIDError:79".
func Cause(rs []Reason) string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Cause)
	}
	return strings.Join(out, "+")
}

// Detail joins the details of reasons into one event note.
func Detail(rs []Reason) string {
	var out []string
	for _, r := range rs {
		if r.Detail != "" {
			out = append(out, r.Cause+": "+r.Detail)
		}
	}
	s := strings.Join(out, "; ")
	if len(s) > 500 {
		s = s[:497] + "..."
	}
	return s
}

// errorCodeRe finds the first bracketed error code in an NVSentinel message,
// such as "[DCGM_FR_FAULTY_MEMORY] GPU memory failure ...".
var errorCodeRe = regexp.MustCompile(`^\s*\[([A-Za-z0-9_]+)`)

func errorCode(msg string) string {
	if m := errorCodeRe.FindStringSubmatch(msg); m != nil {
		return m[1]
	}
	return ""
}

var slugRe = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

func slug(s string) string {
	s = slugRe.ReplaceAllString(strings.TrimSpace(s), "-")
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

func parseTime(s string) int64 {
	if s == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0
	}
	return t.Unix()
}
