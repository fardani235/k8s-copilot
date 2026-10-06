package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

var (
	colAccent = lipgloss.AdaptiveColor{Light: "25", Dark: "39"}
	colDim    = lipgloss.AdaptiveColor{Light: "244", Dark: "243"}
	colWarn   = lipgloss.AdaptiveColor{Light: "130", Dark: "214"}
	colBad    = lipgloss.AdaptiveColor{Light: "160", Dark: "203"}
	colGood   = lipgloss.AdaptiveColor{Light: "28", Dark: "78"}
	colSelBg  = lipgloss.AdaptiveColor{Light: "254", Dark: "237"}

	stHeader   = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	stDim      = lipgloss.NewStyle().Foreground(colDim)
	stWarn     = lipgloss.NewStyle().Foreground(colWarn)
	stBad      = lipgloss.NewStyle().Foreground(colBad)
	stGood     = lipgloss.NewStyle().Foreground(colGood)
	stBold     = lipgloss.NewStyle().Bold(true)
	stColHead  = lipgloss.NewStyle().Bold(true).Foreground(colDim)
	stSelected = lipgloss.NewStyle().Background(colSelBg).Bold(true)
	stKey      = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	stBanner   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(colWarn).Padding(0, 1)

	stPane        = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colDim)
	stPaneFocused = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colAccent)
	stModal       = lipgloss.NewStyle().Border(lipgloss.DoubleBorder()).BorderForeground(colWarn).Padding(1, 2)
)

// fit truncates s to w cells (with an ellipsis) and pads it to exactly w.
func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	s = ansi.Truncate(s, w, "…")
	if pad := w - lipgloss.Width(s); pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	return s
}

// clip truncates without padding.
func clip(s string, w int) string {
	if w <= 0 {
		return ""
	}
	return ansi.Truncate(s, w, "…")
}

// wrap hard-wraps text to w cells, keeping existing newlines.
func wrap(s string, w int) string {
	if w < 4 {
		w = 4
	}
	return ansi.Wrap(s, w, " -/,")
}

// hints renders "key action · key action".
func hints(pairs ...string) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, stKey.Render(pairs[i])+" "+stDim.Render(pairs[i+1]))
	}
	return strings.Join(parts, stDim.Render(" · "))
}

func clamp(v, lo, hi int) int {
	if hi < lo {
		hi = lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
