package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/fardani235/k2stui/internal/approval"
	"github.com/fardani235/k2stui/internal/textutil"
)

// armDelay is how long the dialog ignores keys after it appears. It exists
// so that keystrokes already on their way — the user was typing or
// navigating when the proposal popped up — cannot answer it. It only ever
// delays input; nothing happens when it elapses.
const armDelay = 700 * time.Millisecond

// confirmDelay is the least time between the approve chord and the
// confirming enter, so a single bounce of keys cannot do both.
const confirmDelay = 250 * time.Millisecond

type modalState struct {
	shownAt time.Time
	hidden  bool // dismissed with esc; the proposal is still waiting
	details bool // show the exact API request
	// confirming is the second step of approval: y was pressed, enter
	// confirms, anything else backs out.
	confirming bool
	confirmAt  time.Time
	editing    bool
	editErr    string
	// decided is set once a decision has been sent, while the loop applies.
	decided string
	scroll  int
}

func (m *Model) openProposal(req *approval.Request) {
	if !m.busy {
		// The turn that asked is already over; this request is dead.
		req.Decide(approval.Decision{Action: approval.Reject})
		return
	}
	m.pending = req
	m.modal = modalState{shownAt: time.Now()}
}

func (m *Model) onModalKey(msg tea.KeyMsg) tea.Cmd {
	md := &m.modal
	key := msg.String()

	if md.decided != "" {
		return nil // already answered; waiting for the loop to report back
	}
	if md.editing {
		return m.onEditorKey(msg)
	}
	if time.Since(md.shownAt) < armDelay {
		return nil
	}

	if md.confirming {
		md.confirming = false
		if key == "enter" && time.Since(md.confirmAt) >= confirmDelay {
			md.decided = "approved — applying…"
			m.pending.Decide(approval.Decision{Action: approval.Approve})
		}
		return nil
	}

	// Approve and reject are chords: no run of ordinary typing — a question
	// the user was still writing when the dialog appeared — can produce them.
	switch key {
	case "ctrl+y":
		md.confirming, md.confirmAt = true, time.Now()
	case "ctrl+n":
		md.decided = "rejected"
		m.pending.Decide(approval.Decision{Action: approval.Reject})
	case "e":
		md.editing, md.editErr = true, ""
		var pretty bytes.Buffer
		if json.Indent(&pretty, m.pending.Proposal.Args, "", "  ") != nil {
			pretty.Reset()
			pretty.Write(m.pending.Proposal.Args)
		}
		m.editor.SetValue(pretty.String())
		m.editor.SetWidth(clamp(m.width-12, 30, 100))
		m.editor.SetHeight(clamp(m.height-14, 4, 16))
		return m.editor.Focus()
	case "x":
		md.details = !md.details
		md.scroll = 0
	case "esc":
		md.hidden = true
		m.status = "the proposal is still waiting — nothing happens until you decide (press p)"
	case "up", "k":
		md.scroll = max(md.scroll-1, 0)
	case "down", "j":
		md.scroll++
	case "pgup":
		md.scroll = max(md.scroll-10, 0)
	case "pgdown", " ":
		md.scroll += 10
	}
	return nil
}

func (m *Model) onEditorKey(msg tea.KeyMsg) tea.Cmd {
	md := &m.modal
	switch msg.String() {
	case "esc":
		md.editing = false
		m.editor.Blur()
		md.shownAt = time.Now()
		return nil
	case "ctrl+s":
		raw := []byte(strings.TrimSpace(m.editor.Value()))
		var obj map[string]any
		if err := json.Unmarshal(raw, &obj); err != nil {
			md.editErr = "not a valid JSON object: " + err.Error()
			return nil
		}
		md.editing = false
		m.editor.Blur()
		md.decided = "edited — re-validating…"
		m.pending.Decide(approval.Decision{Action: approval.Edit, EditedArgs: raw})
		return nil
	}
	var cmd tea.Cmd
	m.editor, cmd = m.editor.Update(msg)
	return cmd
}

func (m *Model) viewModal() string {
	p := m.pending.Proposal
	md := &m.modal
	w := clamp(m.width-8, 40, 104)
	inner := w - 6

	var b strings.Builder
	line := func(label, value string) {
		const lw = 10
		value = wrap(value, inner-lw-1)
		value = strings.ReplaceAll(value, "\n", "\n"+strings.Repeat(" ", lw+1))
		b.WriteString(stDim.Render(fit(label, lw)) + " " + value + "\n")
	}
	san := textutil.Sanitize

	b.WriteString(stBanner.Render("APPROVAL NEEDED") + stDim.Render("  nothing has been changed") + "\n\n")
	b.WriteString(stBold.Render(wrap(san(p.Title), inner)) + "\n\n")

	target := p.Target.Kind + " " + p.Target.Name
	if p.Target.Namespace != "" {
		target = p.Target.Kind + " " + p.Target.Namespace + "/" + p.Target.Name
	}
	line("Action", san(p.Tool))
	line("Target", san(target)+"  ("+san(p.Target.APIVersion)+")")
	line("Cluster", san(p.Target.Context)+"  "+san(p.Target.Server))
	for i, c := range p.Changes {
		label := "Change"
		if i > 0 {
			label = ""
		}
		line(label, fmt.Sprintf("%s:  %s  →  %s", san(c.Field), stBad.Render(san(c.Before)), stGood.Render(san(c.After))))
	}
	line("Dry-run", stGood.Render("✓ ")+p.DryRun)
	if p.Reversible {
		line("Undo", p.Reversibility)
	} else {
		line("Undo", stWarn.Render(p.Reversibility))
	}
	for _, wn := range p.Warnings {
		line("⚠", stWarn.Render(san(wn)))
	}
	b.WriteString("\n")
	line("You asked", san(textutil.Truncate(textutil.OneLine(p.Intent), 400)))
	line("Its reason", san(textutil.Truncate(textutil.OneLine(p.ModelReason), 600))+stDim.Render("  (the model's words, not verified)"))

	if md.details {
		b.WriteString("\n" + stDim.Render("Exact request sent to the API server if you approve:") + "\n")
		b.WriteString(wrap(san(p.Request), inner) + "\n")
		b.WriteString(stDim.Render("Tool arguments: ") + wrap(san(string(p.Args)), inner) + "\n")
	}

	content := strings.TrimRight(b.String(), "\n")

	var foot string
	switch {
	case md.decided != "":
		foot = stWarn.Render(md.decided)
	case md.editing:
		foot = stDim.Render("Edit the arguments. The edited proposal is validated again and shown again before anything happens.") + "\n" +
			m.editor.View() + "\n"
		if md.editErr != "" {
			foot += stBad.Render(wrap(md.editErr, inner)) + "\n"
		}
		foot += hints("ctrl+s", "submit for re-validation", "esc", "back")
	case md.confirming:
		foot = stBanner.Render("Apply this change to the cluster?") + "  " + hints("enter", "yes, apply", "any other key", "go back")
	default:
		foot = hints("ctrl+y", "approve", "ctrl+n", "reject", "e", "edit", "x", "exact request", "esc", "hide (keeps waiting)")
		foot += "\n" + stDim.Render("There is no timeout. Nothing happens until you decide.")
	}

	// Keep the decision keys on screen: scroll the body if it is too tall.
	// The scroll hint has its own line, so no content line is ever covered.
	footH := strings.Count(foot, "\n") + 1
	avail := m.height - 4 - footH - 2
	lines := strings.Split(content, "\n")
	if len(lines) > avail && avail > 3 {
		md.scroll = clamp(md.scroll, 0, len(lines)-avail)
		more := len(lines) - avail - md.scroll
		lines = lines[md.scroll : md.scroll+avail]
		hint := "↑↓ scroll · end of proposal"
		if more > 0 {
			hint = fmt.Sprintf("↑↓ scroll · %d more line(s) below", more)
		}
		content = strings.Join(lines, "\n") + "\n" + stWarn.Render(hint)
	} else {
		md.scroll = 0
	}

	box := stModal.Width(w - 2).Render(content + "\n\n" + foot)
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
}
