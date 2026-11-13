package health

import (
	"fmt"
	"strings"
	"time"
)

// Text writes the report for the model, in at most maxBytes when that can
// be done without losing a class's state (maxBytes <= 0: no limit).
//
// Everything that qualifies the findings comes first: the age, the summary,
// every class's state with what it covers. If the whole does not fit, it is
// findings that are left out — counted, per class — never a class's state:
// that is the part that stops "nothing is wrong" from being read off an
// empty list.
func (r *Report) Text(now time.Time, maxBytes int) string {
	head := r.textHead(now, false)
	if maxBytes > 0 && len(head) > maxBytes {
		// Even the class states alone do not fit: keep every one of them,
		// each shortened, rather than drop any.
		head = r.textHead(now, true)
	}

	lines := make([]string, len(r.Findings))
	// sum[n] is the length of the first n lines, so that the cut is found
	// without rebuilding the text for every finding left out.
	sum := make([]int, len(lines)+1)
	for i := range r.Findings {
		lines[i] = r.Findings[i].Text(now) + "\n"
		sum[i+1] = sum[i] + len(lines[i])
	}
	if len(lines) == 0 {
		return head
	}
	intro := func(n int) string {
		if n < len(lines) {
			return fmt.Sprintf("\nFindings — showing %d of %d to fit; every class's state above is complete. Not shown: %s.\n", n, len(lines), r.omitted(n))
		}
		return fmt.Sprintf("\nFindings (%d):\n", len(lines))
	}
	fits := func(n int) bool { return len(head)+len(intro(n))+sum[n] <= maxBytes }
	shown := len(lines)
	if maxBytes > 0 && !fits(shown) {
		// The largest n that fits: the size grows with n (the intro's few
		// bytes of difference are dwarfed by a line), so a binary search.
		lo, hi := 0, len(lines)-1
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if fits(mid) {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		shown = lo
	}
	return head + intro(shown) + strings.Join(lines[:shown], "")
}

// textHead is everything above the findings. compact shortens each class's
// line and leaves out what each covers — for a budget too small for the full
// version; every class's state is still there.
func (r *Report) textHead(now time.Time, compact bool) string {
	var head strings.Builder
	fmt.Fprintf(&head, "Cluster health, read %s ago (the age of the oldest read behind it). These are the same findings the user sees on the health screen.\n", r.Age(now))
	head.WriteString(r.Summary() + "\n\nClasses:\n")
	for i := range r.Classes {
		c := &r.Classes[i]
		if compact {
			fmt.Fprintf(&head, "- %s\n", clean(c.Line(), 240))
		} else {
			fmt.Fprintf(&head, "- %s Covers: %s.\n", c.Line(), c.ID.Covers())
		}
	}
	head.WriteString("\nReading this: a class marked NOT WATCHED was not checked, and \"watched in part\" covers only what was read. Never describe the cluster as healthy in a respect that was not watched; say it was not checked and why.\n")
	if !r.AllClear() && !compact {
		head.WriteString(NotCovered + "\n")
	}
	return head.String()
}

// omitted says how many findings of each class are left out when only the
// first n are shown.
func (r *Report) omitted(n int) string {
	count := map[ClassID]int{}
	for _, f := range r.Findings[n:] {
		count[f.Class]++
	}
	var parts []string
	for _, id := range ClassOrder {
		if count[id] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", id, count[id]))
		}
	}
	return strings.Join(parts, ", ")
}

// Text is the finding on one line, for the model.
func (f *Finding) Text(now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "- [%s, %s] %s", f.Class, f.Severity, f.Object())
	if f.Container != "" {
		b.WriteString(" container " + f.Container)
	}
	b.WriteString(" — " + f.Reason)
	if age := f.Age(now); age != "" {
		if f.Class == Events {
			b.WriteString(", last " + age + " ago")
		} else {
			b.WriteString(", for " + age)
		}
	}
	if f.Class == Events && f.Count > 1 {
		fmt.Fprintf(&b, ", %d times", f.Count)
	}
	if f.Message != "" {
		b.WriteString(": " + f.Message)
	}
	return b.String()
}
