package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/fardani235/k8s-copilot/internal/agent"
	"github.com/fardani235/k8s-copilot/internal/audit"
	"github.com/fardani235/k8s-copilot/internal/textutil"
	"github.com/fardani235/k8s-copilot/internal/tools"
)

type chatKind int

const (
	chatUser chatKind = iota
	chatAssistant
	chatTool
	chatToolErr
	chatNotice
	chatWarn
	chatGood
)

type chatItem struct {
	kind chatKind
	text string
}

// say appends to the transcript. Everything shown here passes through
// Sanitize: model output and cluster data must not be able to drive the
// terminal.
func (m *Model) say(kind chatKind, text string) {
	m.transcript = append(m.transcript, chatItem{kind, textutil.Sanitize(text)})
	if len(m.transcript) > 2000 {
		m.transcript = m.transcript[len(m.transcript)-2000:]
	}
	m.renderChat()
}

func (m *Model) renderChat() {
	w := m.chat.Width
	if w <= 0 {
		return
	}
	var b strings.Builder
	for i, it := range m.transcript {
		if i > 0 {
			b.WriteByte('\n')
		}
		switch it.kind {
		case chatUser:
			b.WriteString("\n" + stKey.Render("you") + "\n" + wrap(it.text, w))
		case chatAssistant:
			b.WriteString("\n" + stHeader.Render("copilot") + "\n" + wrap(it.text, w))
		case chatTool:
			b.WriteString(stDim.Render(wrap("· "+it.text, w)))
		case chatToolErr:
			b.WriteString(stWarn.Render(wrap("· "+it.text, w)))
		case chatNotice:
			b.WriteString(stDim.Render(wrap(it.text, w)))
		case chatWarn:
			b.WriteString(stBad.Render(wrap("! "+it.text, w)))
		case chatGood:
			b.WriteString(stGood.Render(wrap("✓ "+it.text, w)))
		}
	}
	m.chat.SetContent(b.String())
	m.chat.GotoBottom()
}

func (m *Model) viewCopilot(w, h int) string {
	title := "copilot"
	if m.deps.Agent != nil {
		title += stDim.Render(" · " + m.deps.Agent.ProviderName())
	}
	var b strings.Builder
	b.WriteString(clip(stHeader.Render(title), w) + "\n")

	if m.deps.Agent == nil {
		msg := stWarn.Render("The copilot is not available.") + "\n\n" + wrap(m.deps.AgentErr, w) +
			"\n\n" + stDim.Render(wrap("The browser works without it. Nothing can be changed in the cluster from here.", w))
		// A long reason in a short pane is cut, not allowed to push the
		// pane's frame off the screen.
		return b.String() + fitLines(msg, h-1)
	}
	if len(m.transcript) == 0 {
		m.chat.SetContent(stDim.Render(wrap(
			"Ask about what you are looking at — for example: \"why does this pod keep restarting?\"\n\n"+
				"I can read anything you can. I cannot change anything: when a fix is clear I will propose it, and it only happens if you approve.", w)))
	}
	b.WriteString(m.chat.View() + "\n")
	switch {
	case m.pending != nil:
		b.WriteString(clip(stBanner.Render("waiting for your decision — ctrl+p"), w))
	case m.busy:
		act := m.activity
		if act == "" {
			act = "thinking"
		}
		b.WriteString(clip(stWarn.Render(m.spin.View()+" "+act+"…")+stDim.Render("  esc cancels"), w))
	default:
		b.WriteString(stDim.Render(strings.Repeat("─", w)))
	}
	b.WriteString("\n" + m.input.View())
	return b.String()
}

// leaveCopilot returns focus to the browser. If the copilot was maximized, the
// split is restored first, so the browser is visible when it takes the
// keyboard — never a focused but hidden pane.
func (m *Model) leaveCopilot() {
	m.pane = paneBrowser
	m.input.Blur()
	m.copilotMax = false
	m.layout()
}

func (m *Model) onCopilotKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "tab":
		m.leaveCopilot()
		return nil
	case "esc":
		if m.pending != nil {
			// A proposal is waiting: esc must not throw it away. Deciding
			// (or quitting) is the way to end it.
			m.leaveCopilot()
			m.status = "a proposal is waiting for your decision — ctrl+p shows it"
			return nil
		}
		if m.busy && m.turnCancel != nil {
			m.turnCancel()
			m.say(chatNotice, "Cancelling…")
			return nil
		}
		m.leaveCopilot()
		return nil
	case "pgup", "pgdown", "ctrl+u", "ctrl+d":
		var cmd tea.Cmd
		m.chat, cmd = m.chat.Update(msg)
		return cmd
	case "ctrl+l":
		if m.deps.Agent == nil {
			return nil
		}
		if !m.deps.Agent.Reset() {
			m.status = "cannot start a new conversation while a request is running"
			return nil
		}
		m.transcript = nil
		m.renderChat()
		return nil
	case "enter":
		text := strings.TrimSpace(m.input.Value())
		if text == "" || m.deps.Agent == nil {
			return nil
		}
		if m.busy {
			m.status = "the copilot is still working — esc cancels the running request"
			return nil
		}
		m.input.SetValue("")
		return m.ask(text)
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return cmd
}

// ask starts an agent turn on its own goroutine. The UI stays live; results
// arrive as events.
func (m *Model) ask(text string) tea.Cmd {
	m.say(chatUser, text)
	m.busy, m.activity = true, ""
	ctx, cancel := context.WithCancel(m.ctx)
	m.turnCancel = cancel
	emit := func(ev agent.Event) {
		select {
		case m.events <- ev:
		case <-m.closed:
		}
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer cancel()
		m.deps.Agent.Run(ctx, text, emit)
	}()
	return m.spin.Tick
}

func (m *Model) onAgentEvent(ev agent.Event) {
	switch ev := ev.(type) {
	case agent.EventText:
		m.say(chatAssistant, strings.TrimSpace(ev.Text))
	case agent.EventToolStart:
		name := textutil.Truncate(textutil.OneLine(textutil.Sanitize(ev.Call.Name)), 40)
		m.activity = name
		if ev.Tier == tools.Mutate {
			m.activity = "validating proposal (" + name + ")"
		}
	case agent.EventToolEnd:
		m.activity = ""
		call := ev.Call.Name + "(" + compactArgs(ev.Call.Args) + ")"
		if ev.IsError {
			m.say(chatToolErr, call+" → "+textutil.Truncate(textutil.OneLine(ev.Result), 220))
		} else {
			m.say(chatTool, call+" → "+resultSummary(ev.Result))
		}
	case agent.EventProposalClosed:
		if m.pending != nil && m.pending.Proposal.ID == ev.ID {
			m.pending = nil
			m.modal = modalState{}
		}
		rec := ""
		if !ev.Audited {
			rec = " (NOT recorded in the audit trail)"
		}
		switch ev.Outcome {
		case audit.OutcomeApplying:
		case audit.OutcomeApplied:
			m.say(chatGood, "Applied with your approval: "+ev.Title+rec)
		case audit.OutcomeDeclined:
			m.say(chatNotice, "Declined, nothing changed: "+ev.Title+rec)
		case audit.OutcomeSuperseded:
			m.say(chatNotice, "Edited; re-validating: "+ev.Title+rec)
		case audit.OutcomeCancelled:
			m.say(chatNotice, "Proposal closed without a decision, nothing changed: "+ev.Title+rec)
		case audit.OutcomeDryRunRejected:
			m.say(chatWarn, "The API server rejected this in a dry-run, so it was never offered for approval: "+ev.Title+" — "+ev.Detail+rec)
		case audit.OutcomeFailed:
			m.say(chatWarn, "Approved, but applying it failed: "+ev.Title+" — "+ev.Detail+rec)
		}
	case agent.EventNotice:
		if ev.Warning {
			m.say(chatWarn, ev.Text)
		} else {
			m.say(chatNotice, ev.Text)
		}
	case agent.EventDone:
		m.busy, m.activity, m.turnCancel = false, "", nil
		m.pending, m.modal = nil, modalState{}
		switch {
		case ev.Err == nil:
		case errors.Is(ev.Err, context.Canceled):
			m.say(chatNotice, "Request cancelled. Nothing was changed.")
		default:
			m.say(chatWarn, fmt.Sprintf("The model request failed: %v\nNothing was changed in the cluster; the browser is unaffected. You can ask again.", ev.Err))
		}
		if ev.Usage.InputTokens+ev.Usage.OutputTokens > 0 {
			m.say(chatNotice, fmt.Sprintf("(%d tokens in, %d out)", ev.Usage.InputTokens, ev.Usage.OutputTokens))
		}
	}
}

// compactArgs renders tool arguments as "k=v, k=v" for the transcript.
func compactArgs(raw json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return textutil.Truncate(string(raw), 80)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		if k != "reason" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v, _ := json.Marshal(m[k])
		parts = append(parts, k+"="+textutil.Truncate(strings.Trim(string(v), `"`), 48))
	}
	return strings.Join(parts, ", ")
}

func resultSummary(s string) string {
	lines := strings.Count(s, "\n") + 1
	first, _, _ := strings.Cut(s, "\n")
	first = textutil.Truncate(textutil.OneLine(first), 120)
	if lines > 1 {
		return fmt.Sprintf("%s (+%d lines)", first, lines-1)
	}
	return first
}
