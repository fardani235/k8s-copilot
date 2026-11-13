package tui

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/fardani235/k8s-copilot/internal/agent"
	"github.com/fardani235/k8s-copilot/internal/approval"
	"github.com/fardani235/k8s-copilot/internal/llm"
)

// fillChat gives the copilot a transcript of n one-line entries.
func fillChat(u *ui, n int) {
	for i := 0; i < n; i++ {
		u.m.say(chatNotice, fmt.Sprintf("line %03d", i))
	}
}

func maxOffset(u *ui) int { return u.m.chat.TotalLineCount() - u.m.chat.Height }

func wheel(x, y int, b tea.MouseButton) tea.MouseMsg {
	return tea.MouseMsg{X: x, Y: y, Button: b, Action: tea.MouseActionPress}
}

// Task 3.1: the wheel scrolls the transcript when the pointer is over the
// copilot pane — whichever pane has focus, split or maximized — and moves
// nothing over the browser, over a hidden pane or under the approval dialog.
func TestCopilotWheelScrollsOnlyOverThePane(t *testing.T) {
	c := newCopilotUI(t)
	c.keys("c") // show the pane; the browser keeps focus
	fillChat(c.ui, 120)
	if !c.m.chat.AtBottom() {
		t.Fatal("a new transcript does not start at its end")
	}
	bottom := c.m.chat.YOffset
	paneX := c.m.width - c.m.copilotWidth()
	cursor, body := c.m.cursor, c.m.body.YOffset

	// Over the pane, focus on the browser: it scrolls, three lines a notch.
	c.send(wheel(paneX+5, 10, tea.MouseButtonWheelUp))
	if got := c.m.chat.YOffset; got != bottom-3 {
		t.Fatalf("wheel up over the pane: offset %d, want %d", got, bottom-3)
	}
	c.send(wheel(c.m.width-1, 10, tea.MouseButtonWheelDown))
	if got := c.m.chat.YOffset; got != bottom {
		t.Fatalf("wheel down over the pane: offset %d, want %d", got, bottom)
	}

	// Over the browser, the header and the footer: nothing moves.
	for _, at := range [][2]int{{5, 10}, {paneX - 1, 10}, {paneX + 5, 0}, {paneX + 5, c.m.height - 1}} {
		c.send(wheel(at[0], at[1], tea.MouseButtonWheelUp))
		if c.m.chat.YOffset != bottom {
			t.Fatalf("wheel at %v moved the transcript", at)
		}
	}
	if c.m.cursor != cursor || c.m.body.YOffset != body {
		t.Fatal("the wheel moved the browser")
	}
	// Not a wheel: nothing.
	c.send(tea.MouseMsg{X: paneX + 5, Y: 10, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	c.send(tea.MouseMsg{X: paneX + 5, Y: 10, Button: tea.MouseButtonWheelUp, Action: tea.MouseActionRelease})
	if c.m.chat.YOffset != bottom {
		t.Fatal("a click or a release moved the transcript")
	}

	// The approval dialog is modal: the wheel does nothing under it.
	c.m.pending = &approval.Request{}
	c.send(wheel(paneX+5, 10, tea.MouseButtonWheelUp))
	if c.m.chat.YOffset != bottom {
		t.Fatal("the wheel scrolled the transcript under the approval dialog")
	}
	c.m.pending = nil

	// Maximized: the whole width is the pane.
	c.keys("m")
	if !c.m.copilotMax {
		t.Fatal("not maximized")
	}
	bottom = c.m.chat.YOffset
	c.send(wheel(2, 10, tea.MouseButtonWheelUp))
	if got := c.m.chat.YOffset; got != bottom-3 {
		t.Fatalf("wheel over the maximized pane: offset %d, want %d", got, bottom-3)
	}
	c.keys("tab") // back to the split, focus on the browser
	if c.m.copilotMax {
		t.Fatal("still maximized")
	}

	// Hidden: nothing, wherever the pointer is.
	c.keys("c")
	if c.m.copilotOpen {
		t.Fatal("the pane is still open")
	}
	before := c.m.chat.YOffset
	for _, x := range []int{5, paneX + 5, c.m.width - 1} {
		c.send(wheel(x, 10, tea.MouseButtonWheelUp))
	}
	if c.m.chat.YOffset != before {
		t.Fatal("the wheel scrolled a hidden transcript")
	}
}

// Task 3.2: with the pane focused, the keys move the transcript by a line,
// a page, half a page or to an end; home and end stay with the input.
func TestCopilotScrollKeys(t *testing.T) {
	c := newCopilotUI(t)
	c.keys("tab")
	fillChat(c.ui, 120)
	h, bottom := c.m.chat.Height, maxOffset(c.ui)
	if c.m.chat.YOffset != bottom {
		t.Fatalf("offset %d, want the end %d", c.m.chat.YOffset, bottom)
	}
	steps := []struct {
		key  string
		want int
	}{
		{"up", bottom - 1},
		{"up", bottom - 2},
		{"down", bottom - 1},
		{"pgup", bottom - 1 - h},
		{"pgdown", bottom - 1},
		{"ctrl+u", bottom - 1 - h/2},
		{"ctrl+d", bottom - 1},
		{"ctrl+home", 0},
		{"up", 0},
		{"ctrl+end", bottom},
		{"down", bottom},
	}
	for _, s := range steps {
		c.keys(s.key)
		if got := c.m.chat.YOffset; got != s.want {
			t.Fatalf("after %s: offset %d, want %d", s.key, got, s.want)
		}
	}

	// The input's editing keys still edit.
	c.typeText("hello")
	c.keys("home")
	if c.m.input.Position() != 0 || c.m.chat.YOffset != bottom {
		t.Fatalf("home: caret %d, offset %d", c.m.input.Position(), c.m.chat.YOffset)
	}
	c.keys("end")
	if c.m.input.Position() != 5 || c.m.chat.YOffset != bottom {
		t.Fatalf("end: caret %d, offset %d", c.m.input.Position(), c.m.chat.YOffset)
	}
	if c.m.input.Value() != "hello" {
		t.Fatalf("a scroll key edited the input: %q", c.m.input.Value())
	}
}

// Task 3.3: new output follows a reader at the end, and leaves one who has
// scrolled up where they are.
func TestCopilotScrollIsNotYankedByNewOutput(t *testing.T) {
	c := newCopilotUI(t)
	c.keys("tab")
	fillChat(c.ui, 120)

	c.m.say(chatAssistant, "an answer arrives")
	if !c.m.chat.AtBottom() {
		t.Fatal("a reader at the end was left behind by new output")
	}

	c.keys("pgup", "pgup")
	at := c.m.chat.YOffset
	for i := 0; i < 5; i++ {
		c.m.say(chatTool, fmt.Sprintf("get_logs(…) → more output %d", i))
	}
	c.m.say(chatAssistant, "a long answer\nover\nseveral\nlines")
	if c.m.chat.YOffset != at {
		t.Fatalf("new output moved a scrolled-up view from %d to %d", at, c.m.chat.YOffset)
	}
	if strings.Contains(c.screen(), "a long answer") {
		t.Fatal("the new output was pulled into view")
	}

	// Scrolling back to the end resumes following.
	c.keys("ctrl+end")
	c.m.say(chatNotice, "after the end")
	if !c.m.chat.AtBottom() || !strings.Contains(c.screen(), "after the end") {
		t.Fatal("following did not resume at the end")
	}
}

// Task 3.3: sending a message returns the view to the end, so the question
// and its answer are visible.
func TestSendingReturnsToTheEnd(t *testing.T) {
	c := newCopilotUI(t, llm.Text("here is the answer"))
	c.keys("tab")
	fillChat(c.ui, 120)
	c.keys("ctrl+home")
	if c.m.chat.YOffset != 0 {
		t.Fatal("not at the top")
	}
	c.typeText("what now?")
	c.keys("enter")
	if !c.m.chat.AtBottom() || !strings.Contains(c.screen(), "what now?") {
		t.Fatalf("the question is not in view:\n%s", c.screen())
	}
	c.eventually("the answer", func() bool { return !c.m.busy })
	if !c.m.chat.AtBottom() || !strings.Contains(c.screen(), "here is the answer") {
		t.Fatalf("the answer is not in view:\n%s", c.screen())
	}
}

var rangeRE = regexp.MustCompile(`(\d+)–(\d+)/(\d+)`)

// copilotTitle is the copilot pane's title as plain text.
func copilotTitle(u *ui) string {
	w, h := u.m.copilotInner()
	first, _, _ := strings.Cut(ansi.Strip(u.m.viewCopilot(w, h)), "\n")
	return first
}

// Task 3.4: the title shows the visible range while the transcript is longer
// than the pane, follows the view, and is absent when it fits.
func TestCopilotScrollRangeIndicator(t *testing.T) {
	c := newCopilotUI(t)
	c.keys("tab")
	fillChat(c.ui, 5)
	if title := copilotTitle(c.ui); rangeRE.MatchString(title) {
		t.Fatalf("a range for a transcript that fits: %q", title)
	}

	fillChat(c.ui, 115)
	h, total := c.m.chat.Height, c.m.chat.TotalLineCount()
	want := fmt.Sprintf("%d–%d/%d", total-h+1, total, total)
	if title := copilotTitle(c.ui); !strings.Contains(title, "copilot · "+want) {
		t.Fatalf("title %q, want the range %s", title, want)
	}
	if !strings.Contains(c.screen(), want) {
		t.Fatal("the range is not on the screen")
	}

	c.keys("up", "up")
	want = fmt.Sprintf("%d–%d/%d", total-h-1, total-2, total)
	if title := copilotTitle(c.ui); !strings.Contains(title, want) {
		t.Fatalf("after scrolling: title %q, want %s", title, want)
	}
	c.keys("ctrl+home")
	want = fmt.Sprintf("1–%d/%d", h, total)
	if title := copilotTitle(c.ui); !strings.Contains(title, want) {
		t.Fatalf("at the top: title %q, want %s", title, want)
	}

	// No room in the title: the range is left out, not cut.
	first, _, _ := strings.Cut(ansi.Strip(c.m.viewCopilot(12, 20)), "\n")
	if strings.Contains(first, "–") || strings.Contains(first, "/") {
		t.Fatalf("a range in a title with no room for it: %q", first)
	}
}

// Task 3.4: at the narrowest supported terminal the copilot pane, range
// included, stays inside the screen, split and maximized.
func TestCopilotPaneFitsNarrowestTerminal(t *testing.T) {
	c := newCopilotUI(t)
	c.send(tea.WindowSizeMsg{Width: MinWidth, Height: MinHeight})
	c.keys("tab")
	fillChat(c.ui, 120)
	c.keys("pgup")

	check := func(what string) {
		t.Helper()
		lines := strings.Split(c.m.View(), "\n")
		if len(lines) != MinHeight {
			t.Fatalf("%s: %d lines, want %d:\n%s", what, len(lines), MinHeight, c.screen())
		}
		for i, l := range lines {
			if w := ansi.StringWidth(l); w > MinWidth {
				t.Fatalf("%s: line %d is %d wide:\n%s", what, i, w, c.screen())
			}
		}
		if !rangeRE.MatchString(c.screen()) {
			t.Fatalf("%s: no range shown:\n%s", what, c.screen())
		}
	}
	check("split")
	c.keys("tab", "m")
	check("maximized")
}

// Task 4.4: the reader's position survives a resize and maximize/restore;
// a reader at the end stays at the end.
func TestCopilotScrollSurvivesResizeAndMaximize(t *testing.T) {
	c := newCopilotUI(t)
	c.keys("c")
	fillChat(c.ui, 120)

	for _, size := range [][2]int{{100, 30}, {140, 50}, {MinWidth, MinHeight}, {120, 40}} {
		c.send(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		if !c.m.chat.AtBottom() {
			t.Fatalf("at %v: a reader at the end was left behind", size)
		}
	}
	c.keys("m", "tab")
	if !c.m.chat.AtBottom() {
		t.Fatal("maximize/restore left the end")
	}

	c.send(wheel(c.m.width-5, 10, tea.MouseButtonWheelUp))
	c.send(wheel(c.m.width-5, 10, tea.MouseButtonWheelUp))
	at := c.m.chat.YOffset
	c.keys("m", "tab")
	c.send(tea.WindowSizeMsg{Width: 100, Height: 30})
	c.send(tea.WindowSizeMsg{Width: 120, Height: 40})
	c.m.say(chatNotice, "more")
	if c.m.chat.AtBottom() || c.m.chat.YOffset != at {
		t.Fatalf("a scrolled-up view moved: offset %d, was %d", c.m.chat.YOffset, at)
	}
}

// Review finding: when the request's snapshot made a fresh read, an open
// health screen takes up that same observation rather than keep an older one.
func TestHealthScreenAdoptsTheSnapshotsRead(t *testing.T) {
	f := sickCluster()
	u := newUI(t, f, nil)
	u.keys("H")
	old := u.m.health.obs
	if old == nil {
		t.Fatal("the screen has no observation")
	}
	// What the snapshot would have read: a newer observation, for the
	// screen's own fallback.
	newer := f.HealthObservation(u.m.ctx, u.m.curNS, 0)
	if newer == old {
		t.Fatal("no new read")
	}
	u.send(agentEventMsg{agent.EventHealthSnapshot{}})
	if u.m.health.obs != old {
		t.Fatal("the screen moved before the snapshot was done")
	}
	if u.m.activity != "reading cluster health" {
		t.Fatalf("activity %q while the snapshot is taken", u.m.activity)
	}
	u.send(agentEventMsg{agent.EventHealthSnapshot{Done: true}})
	if u.m.health.obs != newer {
		t.Fatal("the screen did not take up the snapshot's observation")
	}
	if u.m.activity != "" {
		t.Fatalf("activity %q after the snapshot", u.m.activity)
	}
}

// Review finding: a scrolled-up view in a pane that grows is not left past
// the end of the transcript.
func TestCopilotScrollIsClampedWhenThePaneGrows(t *testing.T) {
	c := newCopilotUI(t)
	c.keys("tab")
	fillChat(c.ui, 120)
	c.keys("up", "up", "up")
	c.send(tea.WindowSizeMsg{Width: 120, Height: 60})
	if c.m.chat.PastBottom() {
		t.Fatalf("offset %d past the end (%d lines, height %d)", c.m.chat.YOffset, c.m.chat.TotalLineCount(), c.m.chat.Height)
	}
	at := c.m.chat.YOffset
	c.keys("up")
	if c.m.chat.YOffset != at-1 {
		t.Fatal("up does nothing after the pane grew")
	}
}

// firstVisible is the first transcript line in view, as plain text.
func firstVisible(u *ui) string {
	first, _, _ := strings.Cut(ansi.Strip(u.m.chat.View()), "\n")
	return strings.TrimSpace(first)
}

// Review finding: a resize re-wraps the transcript; the reader stays on the
// same text, not the same line number.
func TestCopilotScrollKeepsItsPlaceAcrossRewrap(t *testing.T) {
	c := newCopilotUI(t)
	c.keys("tab")
	for i := 0; i < 60; i++ {
		c.m.say(chatNotice, fmt.Sprintf("answer %03d %s", i, strings.Repeat("word ", 20)))
	}
	c.keys("pgup", "pgup", "pgup")
	want := firstVisible(c.ui)
	if !strings.HasPrefix(want, "answer ") {
		t.Fatalf("first visible line %q", want)
	}
	for _, size := range [][2]int{{80, 40}, {200, 40}, {120, 40}} {
		c.send(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		if got := firstVisible(c.ui); got[:len("answer 000")] != want[:len("answer 000")] {
			t.Fatalf("at %v the view moved from %q to %q", size, want, got)
		}
	}
	c.keys("tab", "m") // maximize: wider again
	if got := firstVisible(c.ui); got[:len("answer 000")] != want[:len("answer 000")] {
		t.Fatalf("maximized, the view moved from %q to %q", want, got)
	}
}

// Review finding: once the transcript is full, dropping its oldest entries
// does not move a scrolled-up reader off the text they are reading.
func TestCopilotScrollSurvivesTrimming(t *testing.T) {
	c := newCopilotUI(t)
	c.keys("tab")
	fillChat(c.ui, 2000)
	c.keys("ctrl+home")
	for i := 0; i < 10; i++ {
		c.keys("down")
	}
	if got := firstVisible(c.ui); got != "line 010" {
		t.Fatalf("first visible %q", got)
	}
	for i := 0; i < 5; i++ {
		c.m.say(chatTool, "more")
	}
	if got := firstVisible(c.ui); got != "line 010" {
		t.Fatalf("trimming moved the view to %q", got)
	}
	// Once the line being read is itself dropped, the view is at the oldest
	// line left, not somewhere further on.
	for i := 0; i < 20; i++ {
		c.m.say(chatTool, "more")
	}
	if c.m.chat.YOffset != 0 || firstVisible(c.ui) != "line 025" {
		t.Fatalf("offset %d, first visible %q", c.m.chat.YOffset, firstVisible(c.ui))
	}
}
