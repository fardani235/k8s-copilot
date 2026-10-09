package metrics

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// NoData is how an unknown reading is written, on the screen and to the
// model alike. It is never "0".
const NoData = "—"

// FormatCPU writes CPU the way Kubernetes does: millicores below one core
// ("431m"), cores above ("1.93", "8"). A fraction of a millicore is rounded
// up, as kubectl top does, so that something measured as using a little is
// not written as using nothing.
func FormatCPU(a Amount) string {
	const milli = 1_000_000 // nanocores
	if !a.OK {
		return NoData
	}
	m := (a.V + milli - 1) / milli
	if m < 1000 {
		return strconv.FormatInt(m, 10) + "m"
	}
	return trimFloat(float64(a.V)/(1000*milli), 2)
}

// FormatMem writes bytes in binary units: "431Mi", "11.2Gi".
func FormatMem(a Amount) string {
	const ki, mi, gi, ti = 1 << 10, 1 << 20, 1 << 30, 1 << 40
	v := float64(a.V)
	switch {
	case !a.OK:
		return NoData
	case a.V == 0:
		return "0"
	case a.V < mi:
		return trimFloat(v/ki, 0) + "Ki"
	case a.V < gi:
		return trimFloat(v/mi, 0) + "Mi"
	case a.V < ti:
		return trimFloat(v/gi, 1) + "Gi"
	}
	return trimFloat(v/ti, 1) + "Ti"
}

func trimFloat(v float64, decimals int) string {
	s := strconv.FormatFloat(v, 'f', decimals, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

// Percent is used/of in whole percent, rounded to nearest. It is undefined
// (false) when either side is unknown or there is nothing to divide by.
func Percent(used, of Amount) (int, bool) {
	if !used.OK || !of.OK || of.V <= 0 || used.V < 0 {
		return 0, false
	}
	// In floating point: the quantities are 64-bit and multiplying one by a
	// hundred need not fit.
	return int(math.Min(math.Round(float64(used.V)/float64(of.V)*100), 999_999)), true
}

// Heat is how close a reading is to its bound.
type Heat int

const (
	Calm Heat = iota
	// Elevated is ElevatedAt percent of the bound or more.
	Elevated
	// High is HighAt percent of the bound or more: for memory against a
	// limit an OOM kill is near, for CPU against a limit the container is
	// being throttled, for a node there is little left to schedule into.
	High
)

// The thresholds are the same for the screen's colours and for what the
// copilot is told is "HIGH".
const (
	ElevatedAt = 75
	HighAt     = 90
)

// HeatOf classifies a percentage of a bound.
func HeatOf(pct int) Heat {
	switch {
	case pct >= HighAt:
		return High
	case pct >= ElevatedAt:
		return Elevated
	}
	return Calm
}

func (h Heat) String() string {
	switch h {
	case High:
		return "HIGH"
	case Elevated:
		return "elevated"
	}
	return ""
}

// --- when a source is missing -------------------------------------------------

func (s SourceStatus) where() string {
	switch {
	case s.Source == NodeMetrics || s.Source == NodeObjects:
		return ""
	case s.Scope == "":
		return " across all namespaces"
	}
	return " in namespace " + s.Scope
}

// Headline says in a few words what is wrong with the source.
func (s SourceStatus) Headline() string {
	api := s.Source.isMetricsAPI()
	switch s.State {
	case OK:
		return string(s.Source) + ": ok"
	case Absent:
		if api {
			return "Metrics are not available on this cluster"
		}
		return "The cluster does not serve " + string(s.Source)
	case Unavailable:
		if api {
			return "The metrics API is not answering"
		}
		return "The API server cannot list " + string(s.Source) + " right now"
	case Denied:
		return "Permission denied reading " + string(s.Source) + s.where()
	case Unauthenticated:
		return "The cluster did not accept your credentials"
	case TimedOut:
		if api {
			return "The metrics API did not answer in time"
		}
		return "Listing " + string(s.Source) + " timed out"
	}
	return "Cannot read " + string(s.Source)
}

// Explain says what the failure means for the person looking at the screen
// (or for the model about to answer them). The same sentences are used in
// both places.
func (s SourceStatus) Explain() string {
	api := s.Source.isMetricsAPI()
	switch s.State {
	case OK:
		return ""
	case Absent:
		if api {
			return "The API server does not serve the metrics API (metrics.k8s.io), so there are no CPU or memory readings to show. " +
				"That usually means metrics-server, or an equivalent, is not installed. " +
				"k8s-copilot only reads what the cluster already offers: it installs nothing."
		}
	case Unavailable:
		if api {
			return "The metrics API (metrics.k8s.io) is registered on this cluster but nothing healthy is answering behind it — " +
				"typically metrics-server is down, still starting, or cannot reach the kubelets. " +
				"Usage is unknown until it recovers; it is not zero."
		}
	case Denied:
		if api {
			return fmt.Sprintf("Your kubeconfig user is not allowed to read %s%s. "+
				"This is an authorization failure, not an idle cluster: the readings exist, you may not see them.", s.Source, s.where())
		}
		return fmt.Sprintf("Your kubeconfig user is not allowed to list %s%s. This is an authorization failure, not an absence of %s.", s.Source, s.where(), s.Source)
	case Unauthenticated:
		return fmt.Sprintf("The API server rejected the kubeconfig credentials when asked for %s — typically an expired token or certificate. "+
			"This is an authentication failure, not an idle cluster: nothing was read.", s.Source)
	case TimedOut:
		if api {
			return "The metrics API did not answer in time. Usage is unknown for now; it is not zero."
		}
	}
	return fmt.Sprintf("Reading %s%s failed.", s.Source, s.where())
}

// --- freshness ----------------------------------------------------------------

// StaleAfter is the sample age beyond which a reading is flagged as old.
// metrics-server samples every 15s by default, so anything past this has
// missed several rounds.
const StaleAfter = 90 * time.Second

// Freshness describes how old the readings are.
type Freshness struct {
	// Text is ready to show: "sampled 8s ago · 15s window".
	Text string
	// Short says the same in as few cells as possible: "8s old".
	Short string
	// Warn is set when the readings should not be taken as current: the
	// sample is old, or this snapshot is standing in for a failed refresh.
	Warn bool
}

// Freshness reports the age of the newest sample. The sample's timestamp is
// the cluster's; the age is measured against the local clock.
func (s *Snapshot) Freshness(now time.Time) Freshness {
	if s.Held != nil {
		return Freshness{Warn: true,
			Text: fmt.Sprintf("refresh failing since %s ago — showing the readings from %s ago",
				Age(now.Sub(s.Held.FailedAt)), Age(now.Sub(s.TakenAt))),
			Short: "NOT CURRENT · " + Age(now.Sub(s.TakenAt)) + " old"}
	}
	if s.SampleTime.IsZero() {
		age := Age(now.Sub(s.TakenAt))
		return Freshness{Text: "read " + age + " ago", Short: "read " + age + " ago"}
	}
	age := now.Sub(s.SampleTime)
	f := Freshness{Text: fmt.Sprintf("sampled %s ago", Age(age)), Short: Age(age) + " old"}
	if s.Window > 0 {
		f.Text += fmt.Sprintf(" · %s window", Age(s.Window))
	}
	if age > StaleAfter {
		f.Warn = true
		f.Text += " · OLD"
		f.Short = "OLD · " + f.Short
	}
	return f
}

// Age writes a duration compactly: "8s", "2m10s", "3h".
func Age(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		m, s := int(d.Minutes()), int(d.Seconds())%60
		if s == 0 {
			return fmt.Sprintf("%dm", m)
		}
		return fmt.Sprintf("%dm%ds", m, s)
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}

// ScopeText names a snapshot's scope.
func ScopeText(namespace string) string {
	if namespace == "" {
		return "all namespaces"
	}
	return "namespace " + namespace
}
