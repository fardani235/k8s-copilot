// Package health is the single model of "is anything broken right now?".
//
// The health screen and the copilot's get_findings tool are both renderers of
// what this package produces: the same Report, the same findings, the same
// sentences for a class that was not watched. Neither classifies anything of
// its own, so they cannot disagree about what is wrong or about why something
// was not looked at.
//
// The package does no I/O and keeps no state between calls. internal/kube
// reads the cluster and hands the typed objects, with the outcome of each
// read, to Observe; Observe returns a picture of now.
//
// One rule runs through everything here, the same one internal/metrics
// follows: a class that was not read is not a class with no problems. It
// produces no findings and is never described as clear, on the screen or to
// the model.
package health

import (
	"fmt"
	"strings"
	"time"

	"github.com/fardani235/k8s-copilot/internal/metrics"
	"github.com/fardani235/k8s-copilot/internal/textutil"
)

// --- thresholds (D5): stated wherever they decide something ----------------

const (
	// RestartThreshold: a container restarted more times than this is
	// "restarting repeatedly", whatever its state now.
	RestartThreshold = 5
	// NotReadyGrace is how long a running pod may have no ready container
	// before that is a finding. Shorter is a pod starting up.
	NotReadyGrace = 5 * time.Minute
	// EventWindow is how far back Warning events are read.
	EventWindow = 15 * time.Minute
	// MaxEventFindings caps the distinct warnings reported; the events class
	// says how many there were in all.
	MaxEventFindings = 50
	// ListCap is the most objects one read returns (kube.DefaultListLimit).
	// A read cut there says so, and its class is never "clear".
	ListCap = 500
)

// Bounds on the text a finding carries. Everything a finding says comes from
// the cluster, and anything in the cluster can be written by anyone.
const (
	maxName    = 253 // a DNS subdomain
	maxReason  = 128
	maxMessage = 400
)

// --- classes and their states ---------------------------------------------

// ClassID names one class of findings.
type ClassID string

const (
	Workloads ClassID = "workloads"
	Nodes     ClassID = "nodes"
	Rollouts  ClassID = "rollouts"
	Usage     ClassID = "usage"
	Events    ClassID = "events"
)

// ClassOrder is the order classes are listed in, everywhere.
var ClassOrder = []ClassID{Workloads, Nodes, Rollouts, Usage, Events}

// Tag is the class's three-letter label for narrow panes.
func (c ClassID) Tag() string {
	switch c {
	case Workloads:
		return "WRK"
	case Nodes:
		return "NOD"
	case Rollouts:
		return "ROL"
	case Usage:
		return "USE"
	case Events:
		return "EVT"
	}
	return "???"
}

// Covers says what the class looks for, with the thresholds that decide it.
func (c ClassID) Covers() string {
	switch c {
	case Workloads:
		return fmt.Sprintf("containers in CrashLoopBackOff or restarted more than %d times; images that cannot be pulled; containers stuck waiting (CreateContainerConfigError, RunContainerError, …) for over %s; running pods with no ready container for over %s",
			RestartThreshold, metrics.Age(NotReadyGrace), metrics.Age(NotReadyGrace))
	case Nodes:
		return "nodes that are NotReady or report no Ready condition, or report memory or disk pressure"
	case Rollouts:
		return fmt.Sprintf("Deployments that report they are not progressing; StatefulSets and DaemonSets short of ready replicas with a pod not ready for over %s; paused and completed rollouts are not reported",
			metrics.Age(NotReadyGrace))
	case Usage:
		return fmt.Sprintf("nodes at %d%% or more of allocatable CPU or memory, containers at %d%% or more of their limit (the metrics screen's red)",
			metrics.HighAt, metrics.HighAt)
	case Events:
		return fmt.Sprintf("Warning events in the last %s, identical ones grouped", metrics.Age(EventWindow))
	}
	return ""
}

// ClassState says whether a class was looked at.
type ClassState int

const (
	// Watched: the class's reads answered; its findings are what was found.
	Watched ClassState = iota
	// Forbidden: the cluster refused a read the class depends on (403), or
	// did not accept the credentials (401).
	Forbidden
	// Unavailable: a read failed some other way — not served, not
	// answering, timed out.
	Unavailable
	// NotApplicable: the source the class needs does not exist here (no
	// metrics API) or was not part of this observation.
	NotApplicable
)

// String is the state as a word or two. Every state has its own; none of them
// is "ok" or "clear".
func (s ClassState) String() string {
	switch s {
	case Watched:
		return "watched"
	case Forbidden:
		return "forbidden"
	case Unavailable:
		return "unavailable"
	case NotApplicable:
		return "not applicable"
	}
	return "unknown"
}

// Gap is one read a partly watched class could not make.
type Gap struct {
	Source string
	State  ClassState
	Why    string // sanitised
}

// Class is one class's state in a report.
type Class struct {
	ID    ClassID
	State ClassState
	// Why explains a state other than Watched, in the words the screen and
	// the model both use. Sanitised.
	Why string
	// Scope is the namespace the class was limited to; empty for the whole
	// cluster. ScopeWhy is the refusal that limited it.
	Scope    string
	ScopeWhy string
	// Truncated names the reads that were cut at ListCap, as sentences.
	Truncated []string
	// Limits are other reasons the class saw less than everything it is
	// about ("no reading for 2 nodes"), as sentences.
	Limits []string
	// Gaps are reads a Watched class could not make: it is watched in part.
	Gaps []Gap
	// Findings counts the findings in the report for this class. Total is
	// how many there were before MaxEventFindings cut them (events only).
	Findings, Total int
}

// Complete reports whether the class saw everything it is about: watched,
// cluster-wide, nothing cut, nothing missing. Only a complete class with no
// findings may be called clear.
func (c *Class) Complete() bool {
	return c.State == Watched && c.Scope == "" && len(c.Truncated) == 0 && len(c.Limits) == 0 && len(c.Gaps) == 0
}

// Clear reports whether the class may be described as having no problems.
func (c *Class) Clear() bool { return c.Complete() && c.Findings == 0 }

// Line is the class in one sentence: its state, what it covers if limited,
// and its count. It is what the screen's class lines and the tool's result
// both print.
func (c *Class) Line() string {
	name := string(c.ID)
	if c.State != Watched {
		return fmt.Sprintf("%s: NOT WATCHED (%s) — %s Its problems are unknown, not absent.", name, c.State, c.Why)
	}
	var head string
	switch {
	case c.Clear():
		head = "watched, no findings"
	case c.Findings == 0:
		head = "watched in part, no findings among what was read"
	case c.Complete():
		head = "watched, " + plural(c.Findings, "finding")
	default:
		head = "watched in part, " + plural(c.Findings, "finding") + " among what was read"
	}
	return name + ": " + strings.Join(append([]string{head}, c.Notes()...), "; ") + "."
}

// Notes are what qualifies a watched class: how much of it is shown, the
// namespace it was limited to, the reads that were cut or not made.
func (c *Class) Notes() []string {
	var parts []string
	if c.Total > c.Findings {
		parts = append(parts, fmt.Sprintf("showing %d of %d", c.Findings, c.Total))
	}
	if c.Scope != "" {
		parts = append(parts, fmt.Sprintf("namespace %s only, because %s", c.Scope, c.ScopeWhy))
	}
	parts = append(parts, c.Truncated...)
	parts = append(parts, c.Limits...)
	for _, g := range c.Gaps {
		parts = append(parts, fmt.Sprintf("%s not read (%s: %s)", g.Source, g.State, g.Why))
	}
	return parts
}

// Brief is the class's state in as few cells as can still be told apart:
// "clear", "3", "part 2", "forbidden", "unavailable", "n/a". For the
// narrowest panes.
func (c *Class) Brief() string {
	switch {
	case c.State == NotApplicable:
		return "n/a"
	case c.State != Watched:
		return c.State.String()
	case c.Clear():
		return "clear"
	case c.Complete():
		return fmt.Sprint(c.Findings)
	}
	return fmt.Sprintf("part %d", c.Findings)
}

// Short is the class's state in a few cells: "watched", "watched in part",
// "forbidden", …
func (c *Class) Short() string {
	switch {
	case c.State != Watched:
		return c.State.String()
	case c.Complete():
		return "watched"
	}
	return "watched in part"
}

// --- findings ---------------------------------------------------------------

// Severity orders findings within a class.
type Severity int

const (
	Warning Severity = iota
	Critical
)

func (s Severity) String() string {
	if s == Critical {
		return "critical"
	}
	return "warning"
}

// Finding is one thing that is wrong. Every string in it has been sanitised
// and bounded: it is safe to draw and safe to hand to the model.
type Finding struct {
	Class    ClassID
	Severity Severity
	// The object it concerns: what the browser opens for detail, logs and
	// events. Namespace is empty for a cluster-scoped object.
	Kind      string
	Namespace string
	Name      string
	// Container is set for findings about one container.
	Container string
	// Reason is the short cause ("CrashLoopBackOff", "MemoryPressure",
	// "ProgressDeadlineExceeded", "CPU at limit").
	Reason string
	// Message is what was observed, in a sentence.
	Message string
	// Since is when the condition began, as far as the cluster says; zero if
	// it does not say. For events, the last occurrence.
	Since time.Time
	// Count is how often: restarts, repeated warnings.
	Count int
}

// Key identifies the finding across refreshes, for keeping it selected.
func (f *Finding) Key() string {
	return strings.Join([]string{string(f.Class), f.Kind, f.Namespace, f.Name, f.Container, f.Reason}, "\x00")
}

// Object is "Kind ns/name" or "Kind name".
func (f *Finding) Object() string {
	if f.Namespace != "" {
		return f.Kind + " " + f.Namespace + "/" + f.Name
	}
	return f.Kind + " " + f.Name
}

// Age says how long the condition has held, or how long ago an event last
// occurred; "" when the cluster does not say.
func (f *Finding) Age(now time.Time) string {
	if f.Since.IsZero() {
		return ""
	}
	return metrics.Age(now.Sub(f.Since))
}

// Search is the text the filter matches: object, namespace, class, reason.
func (f *Finding) Search() string {
	return strings.ToLower(strings.Join([]string{string(f.Class), f.Kind, f.Namespace, f.Name, f.Container, f.Reason}, " "))
}

// --- the report ---------------------------------------------------------------

// Report is the classification of one observation.
type Report struct {
	Classes  []Class // in ClassOrder
	Findings []Finding
	// ReadAt is when the oldest read behind the report was received, by the
	// local clock.
	ReadAt time.Time
}

// Class returns one class's state.
func (r *Report) Class(id ClassID) *Class {
	for i := range r.Classes {
		if r.Classes[i].ID == id {
			return &r.Classes[i]
		}
	}
	return nil
}

// Age is how old the oldest read behind the report is.
func (r *Report) Age(now time.Time) string { return metrics.Age(now.Sub(r.ReadAt)) }

// AllClear reports whether every class saw everything and found nothing.
func (r *Report) AllClear() bool {
	for i := range r.Classes {
		if !r.Classes[i].Clear() {
			return false
		}
	}
	return len(r.Classes) > 0
}

// Unwatched lists the classes that were not watched.
func (r *Report) Unwatched() []*Class {
	var out []*Class
	for i := range r.Classes {
		if r.Classes[i].State != Watched {
			out = append(out, &r.Classes[i])
		}
	}
	return out
}

// Summary is the report in one sentence.
func (r *Report) Summary() string {
	var watched, partial, not []string
	for i := range r.Classes {
		c := &r.Classes[i]
		switch {
		case c.State != Watched:
			not = append(not, fmt.Sprintf("%s (%s)", c.ID, c.State))
		case c.Complete():
			watched = append(watched, string(c.ID))
		default:
			partial = append(partial, string(c.ID))
		}
	}
	if r.AllClear() {
		return "No findings. Every class was watched across the whole cluster and none reported a problem: " + strings.Join(watched, ", ") + ". " + NotCovered
	}
	var b strings.Builder
	if len(r.Findings) == 0 {
		b.WriteString("No findings among what was read.")
	} else {
		b.WriteString(plural(len(r.Findings), "finding") + ".")
	}
	if len(watched) > 0 {
		b.WriteString(" Watched: " + strings.Join(watched, ", ") + ".")
	}
	if len(partial) > 0 {
		b.WriteString(" Watched in part: " + strings.Join(partial, ", ") + ".")
	}
	if len(not) > 0 {
		b.WriteString(" NOT watched: " + strings.Join(not, ", ") + " — problems there are unknown, not absent.")
	}
	return b.String()
}

// NotCovered is what no class looks at. The screen and the tool both say it,
// so that "no findings" is not read as more than it is.
const NotCovered = "Not covered by any class: failed Jobs or CronJobs, Pending pods that cannot be scheduled or are stuck in ContainerCreating, StatefulSets or DaemonSets whose pods were never created, PersistentVolumeClaims, HorizontalPodAutoscalers, PodDisruptionBudgets, Service endpoints."

func plural(n int, one string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %ss", n, one)
}

// clean makes text from the cluster safe and bounded: no control sequences,
// one line, at most n runes with a marker where it was cut.
func clean(s string, n int) string {
	s = textutil.OneLine(textutil.Sanitize(s))
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
