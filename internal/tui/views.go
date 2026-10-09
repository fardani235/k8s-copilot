package tui

import (
	"bufio"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/fardani235/k8s-copilot/internal/audit"
	"github.com/fardani235/k8s-copilot/internal/kube"
	"github.com/fardani235/k8s-copilot/internal/textutil"
)

// openSubject opens a sub-view for the selected row (from the list) or for
// the current subject (from another sub-view).
func (m *Model) openSubject(v viewID) tea.Cmd {
	if m.view == vList {
		row, ok := m.selectedRow()
		if !ok {
			m.status = "nothing is selected"
			return nil
		}
		m.subj = subject{typ: m.curType, namespace: row.Namespace, name: row.Name}
		m.subjErr = nil
	}
	if v == vLogs && !(m.subj.typ.Group == "" && m.subj.typ.Resource == "pods") {
		m.status = "logs belong to pods: select a pod (press t and choose pods)"
		return nil
	}
	m.push(v)
	m.subjSeq++
	seq, s := m.subjSeq, m.subj
	m.body.SetContent(stDim.Render("Loading…"))
	m.body.GotoTop()
	if v == vLogs {
		m.logs.reset()
	}
	if s.obj != nil {
		return m.onSubjectLoaded()
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
		defer cancel()
		obj, err := m.deps.Cluster.Get(ctx, s.typ, s.namespace, s.name)
		return subjectMsg{seq, obj, err}
	}
}

// onSubjectLoaded continues opening the current sub-view once the object is
// there (or failed to load).
func (m *Model) onSubjectLoaded() tea.Cmd {
	if m.subjErr != nil {
		msg := "Cannot load " + m.subj.title()
		if kube.IsDenied(m.subjErr) {
			msg = "Permission denied reading " + m.subj.title()
		}
		text := stBad.Render(msg) + "\n\n" + wrap(textutil.Sanitize(kube.Reason(m.subjErr)), m.body.Width)
		m.body.SetContent(text)
		if m.view == vLogs {
			m.logs.err, m.logs.done = m.subjErr, true
		}
		return nil
	}
	switch m.view {
	case vDetail:
		m.renderDetail()
	case vEvents:
		return m.loadEvents()
	case vLogs:
		m.logs.containers = kube.Containers(m.subj.obj)
		found := m.logs.want == ""
		for i, c := range m.logs.containers {
			if c == m.logs.want {
				m.logs.ci, found = i, true
			}
		}
		if !found {
			// Do not show another container's logs as if they were the one
			// that was asked for.
			m.status = fmt.Sprintf("container %s is not in this pod's spec — showing %s", textutil.Sanitize(m.logs.want), m.logs.container())
		}
		return m.startLogs()
	}
	return nil
}

func (m *Model) renderDetail() {
	if m.subj.obj == nil {
		return
	}
	w := m.body.Width
	var b strings.Builder
	keyW := 0
	fields := kube.Summarize(m.subj.obj, time.Now())
	for _, f := range fields {
		keyW = max(keyW, len(f.Key))
	}
	keyW = min(keyW, 24)
	for _, f := range fields {
		val := wrap(textutil.Sanitize(f.Value), max(w-keyW-2, 10))
		val = strings.ReplaceAll(val, "\n", "\n"+strings.Repeat(" ", keyW+2))
		b.WriteString(stDim.Render(fit(textutil.Sanitize(f.Key), keyW)) + "  " + val + "\n")
	}
	b.WriteString("\n" + stDim.Render("─── YAML "+strings.Repeat("─", max(w-9, 0))) + "\n")
	for _, line := range strings.Split(textutil.Sanitize(kube.YAML(m.subj.obj)), "\n") {
		b.WriteString(clip(line, w) + "\n")
	}
	m.body.SetContent(b.String())
}

func (m *Model) loadEvents() tea.Cmd {
	seq, s := m.subjSeq, m.subj
	ref := kube.ObjectRef{Kind: s.typ.Kind, Namespace: s.namespace, Name: s.name}
	if s.obj != nil {
		ref.UID = string(s.obj.GetUID())
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
		defer cancel()
		evs, err := m.deps.Cluster.Events(ctx, ref)
		return eventsMsg{seq, evs, err}
	}
}

func renderEvents(evs []kube.Event, err error, w int) string {
	switch {
	case err != nil && kube.IsDenied(err):
		return stBad.Render("Permission denied reading events") + "\n\n" + wrap(textutil.Sanitize(kube.Reason(err)), w)
	case err != nil:
		return stBad.Render("Cannot read events") + "\n\n" + wrap(textutil.Sanitize(kube.Reason(err)), w)
	case len(evs) == 0:
		return stDim.Render("No events for this resource. (Kubernetes keeps events for about an hour.)")
	}
	var b strings.Builder
	now := time.Now()
	b.WriteString(stColHead.Render(fmt.Sprintf("%-8s %-8s %-22s %s", "AGE", "TYPE", "REASON", "MESSAGE")) + "\n")
	for _, e := range evs {
		reason := e.Reason
		if e.Count > 1 {
			reason += fmt.Sprintf(" x%d", e.Count)
		}
		head := fmt.Sprintf("%-8s %-8s %-22s ", kube.Age(e.Time, now), clip(textutil.Sanitize(e.Type), 8), clip(textutil.Sanitize(reason), 22))
		msg := wrap(textutil.OneLine(textutil.Sanitize(e.Message)), max(w-len(head), 16))
		msg = strings.ReplaceAll(msg, "\n", "\n"+strings.Repeat(" ", len(head)))
		line := head + msg
		if e.Type == "Warning" {
			line = stWarn.Render(line)
		}
		b.WriteString(line + "\n")
	}
	b.WriteString(stDim.Render("(most recent last)"))
	return b.String()
}

func (m *Model) onBodyKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "l":
		if m.view == vDetail || m.view == vEvents {
			return m.openSubject(vLogs)
		}
	case "e":
		if m.view == vDetail {
			return m.openSubject(vEvents)
		}
	case "r":
		switch m.view {
		case vEvents:
			return m.loadEvents()
		case vAudit:
			return m.loadAudit()
		case vDetail:
			seq, s := m.subjSeq, m.subj
			return func() tea.Msg {
				ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
				defer cancel()
				obj, err := m.deps.Cluster.Get(ctx, s.typ, s.namespace, s.name)
				return subjectMsg{seq, obj, err}
			}
		}
	case "g", "home":
		m.body.GotoTop()
		return nil
	case "G", "end":
		m.body.GotoBottom()
		return nil
	}
	var cmd tea.Cmd
	m.body, cmd = m.body.Update(msg)
	return cmd
}

// --- audit trail view ---------------------------------------------------------

func (m *Model) loadAudit() tea.Cmd {
	path := m.deps.AuditPath
	return func() tea.Msg {
		rep, err := audit.Verify(path)
		if err != nil {
			return auditMsg{text: "Cannot read the audit trail: " + textutil.Sanitize(err.Error())}
		}
		return auditMsg{text: FormatAudit(path, rep)}
	}
}

// FormatAudit renders the trail for reading: integrity first, then each
// gated action — what was asked, what was decided, what happened.
func FormatAudit(path string, rep audit.Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Audit trail: %s\n", path)
	if rep.OK() {
		fmt.Fprintf(&b, "Integrity: OK — %d entries, hash chain intact\n", len(rep.Entries))
	} else {
		fmt.Fprintf(&b, "Integrity: FAILED — %d problem(s)\n", len(rep.Problems))
		for _, p := range rep.Problems {
			fmt.Fprintf(&b, "  ! %s\n", p)
		}
	}
	for _, n := range rep.Notes {
		fmt.Fprintf(&b, "  note: %s\n", n)
	}
	if len(rep.Entries) == 0 {
		b.WriteString("\nNo gated actions have been recorded yet.\n")
		return b.String()
	}
	for _, e := range rep.Entries {
		target := e.Target.Kind + " " + e.Target.Name
		if e.Target.Namespace != "" {
			target = e.Target.Kind + " " + e.Target.Namespace + "/" + e.Target.Name
		}
		fmt.Fprintf(&b, "\n#%d  %s  %s  %s  [%s]\n", e.Seq, e.Time.Local().Format("2006-01-02 15:04:05"), e.Tool, target, e.Target.Context)
		fmt.Fprintf(&b, "  you asked:  %s\n", textutil.OneLine(textutil.Sanitize(e.Intent)))
		if e.ModelReason != "" {
			fmt.Fprintf(&b, "  model said: %s\n", textutil.OneLine(textutil.Sanitize(e.ModelReason)))
		}
		for _, c := range e.Changes {
			fmt.Fprintf(&b, "  change:     %s: %s → %s\n", c.Field, textutil.Sanitize(c.Before), textutil.Sanitize(c.After))
		}
		dry := e.DryRun.Result
		if e.DryRun.Message != "" {
			dry += " — " + textutil.OneLine(textutil.Sanitize(e.DryRun.Message))
		}
		fmt.Fprintf(&b, "  dry-run:    %s\n", dry)
		dec := e.Decision.Action
		if e.Decision.By != "" {
			dec += " by " + e.Decision.By
		}
		fmt.Fprintf(&b, "  decision:   %s\n", dec)
		out := e.Outcome.Status
		if e.Outcome.Error != "" {
			out += " — " + textutil.OneLine(textutil.Sanitize(e.Outcome.Error))
		} else if e.Outcome.Detail != "" {
			out += " — " + textutil.OneLine(textutil.Sanitize(e.Outcome.Detail))
		}
		fmt.Fprintf(&b, "  outcome:    %s\n", out)
	}
	// The file may have been edited by someone else: nothing in it gets to
	// drive the terminal.
	return textutil.Sanitize(b.String())
}

func helpText() string {
	return strings.TrimSpace(`
k8s-copilot browses the cluster of your current kubeconfig context and has a
copilot that can investigate for you.

BROWSER
  ↑ ↓ j k, pgup pgdn, g G   move
  enter                     detail: key fields and YAML
  l                         logs (pods)
  e                         events for the selected resource
  n                         choose namespace (or all namespaces)
  t                         choose resource type: anything the cluster serves
  /                         filter the listing
  r                         refresh now
  esc                       back
  M                         metrics: how loaded the cluster is right now
  A                         audit trail
  q, ctrl+c                 quit

METRICS (M)
  Current CPU and memory from the cluster's metrics API, read-only, set
  against node capacity and pod requests and limits. It refreshes itself.
  1 2 3, ← →                nodes / namespaces / pods
  enter                     drill down: a node's or namespace's pods, then a
                            pod's containers
  esc                       back up one level, then out
  s                         sort by cpu, memory or name
  /                         filter by name
  l  e  d                   logs, events, detail of the selected row
  r                         read again now
  —  means there is no reading. It never means zero. If the metrics API is
  missing, broken or not permitted, the screen says which, instead of
  showing an empty table.
  %CPU/R %MEM/R are usage as a share of requests, %CPU/L %MEM/L of limits.
  Rows turn amber at 75% of a hard bound (allocatable, limit), red at 90%.

COPILOT
  c                         show / hide the pane
  m                         maximize the pane, filling the body (from the
                            browser); esc or tab restores the split
  tab                       move between browser and copilot
  enter                     send your question
  esc                       cancel a running request / back to the browser
  ctrl+l                    start a new conversation
  The copilot always knows what you are looking at: namespace, type and the
  selected resource are sent with each question.

CHANGES
  The copilot can read anything you can. It cannot change anything.
  It can only PROPOSE one of four small changes: scale, rollout restart,
  set labels, set annotations. Each proposal is first validated by the API
  server (dry-run), then shown to you, and then it waits — for as long as you
  like. Nothing is applied unless you approve.
  ctrl+y then enter         approve
  ctrl+n                    reject
  e                         edit the proposal (it is re-validated and shown again)
  x                         show the exact API request
  esc                       hide the dialog; the proposal keeps waiting (ctrl+p reopens)
  The decision keys are chords so that nothing you were in the middle of
  typing can answer the dialog.
  Every proposal and its outcome is recorded in the audit trail (A).
`)
}

// --- logs ---------------------------------------------------------------------

const (
	maxLogLines  = 5000
	logTailLines = 500
)

type logState struct {
	lines      []string
	containers []string
	ci         int
	// want is the container to start on, when the logs were opened from a
	// particular container (the metrics screen); empty for the pod's first.
	want     string
	follow   bool
	previous bool
	done     bool
	err      error
	seq      int
	cancel   context.CancelFunc
	ch       chan logChunk
	vp       viewport.Model
}

type logChunk struct {
	line string
	err  error
}

type logMsg struct {
	seq   int
	lines []string
	err   error
	done  bool
}

func (l *logState) stop() {
	if l.cancel != nil {
		l.cancel()
		l.cancel = nil
	}
}

func (l *logState) reset() {
	l.stop()
	l.lines, l.containers, l.ci, l.done, l.err = nil, nil, 0, false, nil
	l.follow, l.previous, l.want = false, false, ""
}

func (l *logState) container() string {
	if l.ci < len(l.containers) {
		return l.containers[l.ci]
	}
	return ""
}

// startLogs (re)opens the stream for the current container and options.
func (m *Model) startLogs() tea.Cmd {
	l := &m.logs
	l.stop()
	l.seq++
	l.lines, l.done, l.err = nil, false, nil
	ctx, cancel := context.WithCancel(m.ctx)
	l.cancel = cancel
	ch := make(chan logChunk, 1024)
	l.ch = ch
	seq, s := l.seq, m.subj
	opts := kube.LogOptions{Container: l.container(), Follow: l.follow, Previous: l.previous, TailLines: logTailLines}
	m.refreshLogView()

	go func() {
		defer close(ch)
		send := func(c logChunk) bool {
			select {
			case ch <- c:
				return true
			case <-ctx.Done():
				return false
			}
		}
		rc, err := m.deps.Cluster.Logs(ctx, s.namespace, s.name, opts)
		if err != nil {
			send(logChunk{err: err})
			return
		}
		defer rc.Close()
		sc := bufio.NewScanner(rc)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			if !send(logChunk{line: sc.Text()}) {
				return
			}
		}
		if err := sc.Err(); err != nil && ctx.Err() == nil {
			send(logChunk{err: err})
		}
	}()
	return waitLog(ch, seq)
}

// waitLog blocks for the next line, then takes whatever else is already
// buffered so a burst becomes one update.
func waitLog(ch chan logChunk, seq int) tea.Cmd {
	return func() tea.Msg {
		c, ok := <-ch
		if !ok {
			return logMsg{seq: seq, done: true}
		}
		msg := logMsg{seq: seq}
		for {
			if c.err != nil {
				msg.err = c.err
				return msg
			}
			msg.lines = append(msg.lines, c.line)
			if len(msg.lines) >= 500 {
				return msg
			}
			select {
			case c, ok = <-ch:
				if !ok {
					msg.done = true
					return msg
				}
			default:
				return msg
			}
		}
	}
}

func (m *Model) onLog(msg logMsg) tea.Cmd {
	l := &m.logs
	if msg.seq != l.seq {
		return nil
	}
	atBottom := l.vp.AtBottom() || len(l.lines) == 0
	for _, line := range msg.lines {
		l.lines = append(l.lines, textutil.Sanitize(line))
	}
	if over := len(l.lines) - maxLogLines; over > 0 {
		l.lines = append(l.lines[:0:0], l.lines[over:]...)
	}
	if msg.err != nil {
		l.err, l.done = msg.err, true
	}
	if msg.done {
		l.done = true
	}
	m.refreshLogView()
	if atBottom {
		l.vp.GotoBottom()
	}
	if l.done {
		return nil
	}
	return waitLog(l.ch, l.seq)
}

func (m *Model) refreshLogView() {
	l := &m.logs
	w := l.vp.Width
	var b strings.Builder
	for i, line := range l.lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(clip(line, w))
	}
	switch {
	case l.err != nil:
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		head := "Cannot read logs"
		if kube.IsDenied(l.err) {
			head = "Permission denied reading logs"
		}
		b.WriteString(stBad.Render(head) + "\n" + wrap(textutil.Sanitize(kube.Reason(l.err)), w))
	case len(l.lines) == 0 && l.done:
		which := "This container"
		if l.previous {
			which = "The previous instance of this container"
		}
		b.WriteString(stDim.Render(which + " has produced no log output (or has not started)."))
	case len(l.lines) == 0 && l.follow:
		b.WriteString(stDim.Render("Following — no log output yet…"))
	case len(l.lines) == 0:
		b.WriteString(stDim.Render("Loading…"))
	}
	l.vp.SetContent(b.String())
}

func (m *Model) onLogsKey(msg tea.KeyMsg) tea.Cmd {
	l := &m.logs
	switch msg.String() {
	case "f":
		l.follow = !l.follow
		if l.follow {
			l.previous = false // a terminated instance has nothing to follow
		}
		return m.startLogs()
	case "v":
		l.previous = !l.previous
		if l.previous {
			l.follow = false
		}
		return m.startLogs()
	case "s":
		if len(l.containers) < 2 {
			m.status = "this pod has a single container"
			return nil
		}
		l.ci = (l.ci + 1) % len(l.containers)
		return m.startLogs()
	case "r":
		return m.startLogs()
	case "e":
		return m.openSubject(vEvents)
	case "g", "home":
		l.vp.GotoTop()
		return nil
	case "G", "end":
		l.vp.GotoBottom()
		return nil
	}
	var cmd tea.Cmd
	l.vp, cmd = l.vp.Update(msg)
	return cmd
}

func (m *Model) viewLogs(w, _ int) string {
	l := &m.logs
	title := stHeader.Render("logs · " + m.subj.title())
	state := []string{}
	if c := l.container(); c != "" {
		state = append(state, fmt.Sprintf("container %s (%d/%d)", c, l.ci+1, len(l.containers)))
	}
	if l.previous {
		state = append(state, "previous instance")
	}
	switch {
	case l.follow && !l.done:
		state = append(state, stGood.Render("following"))
	case l.follow:
		state = append(state, "stream ended")
	default:
		state = append(state, fmt.Sprintf("last %d lines", logTailLines))
	}
	if len(l.lines) >= maxLogLines {
		state = append(state, fmt.Sprintf("showing newest %d", maxLogLines))
	}
	return clip(title, w) + "\n" + clip(stDim.Render(strings.Join(state, " · ")), w) + "\n" + l.vp.View()
}
