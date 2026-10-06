package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// pickItem is one choice in a picker.
type pickItem struct {
	label  string // shown
	detail string // shown dimmed after the label
	search string // lower-case text the filter matches
	value  int    // index into the caller's data; -1 for specials
}

// picker is a filterable single-choice list (namespaces, resource types).
type picker struct {
	title  string
	items  []pickItem
	shown  []int // indexes into items after filtering
	cursor int
	input  textinput.Model
}

func newPicker(title string, items []pickItem, current string) picker {
	in := textinput.New()
	in.Prompt = "filter: "
	in.Focus()
	p := picker{title: title, items: items, input: in}
	p.refilter()
	for i, idx := range p.shown {
		if items[idx].label == current {
			p.cursor = i
		}
	}
	return p
}

func (p *picker) refilter() {
	q := strings.ToLower(strings.TrimSpace(p.input.Value()))
	p.shown = p.shown[:0]
	for i, it := range p.items {
		if q == "" || strings.Contains(it.search, q) {
			p.shown = append(p.shown, i)
		}
	}
	p.cursor = clamp(p.cursor, 0, len(p.shown)-1)
}

// update handles a key. chosen is non-nil when the user picked an item; done
// is true when the picker should close.
func (p *picker) update(msg tea.KeyMsg) (chosen *pickItem, done bool, cmd tea.Cmd) {
	switch msg.String() {
	case "esc":
		return nil, true, nil
	case "enter":
		if len(p.shown) == 0 {
			return nil, false, nil
		}
		it := p.items[p.shown[p.cursor]]
		return &it, true, nil
	case "up", "ctrl+p":
		p.cursor = clamp(p.cursor-1, 0, len(p.shown)-1)
	case "down", "ctrl+n":
		p.cursor = clamp(p.cursor+1, 0, len(p.shown)-1)
	case "pgup":
		p.cursor = clamp(p.cursor-10, 0, len(p.shown)-1)
	case "pgdown":
		p.cursor = clamp(p.cursor+10, 0, len(p.shown)-1)
	default:
		before := p.input.Value()
		p.input, cmd = p.input.Update(msg)
		if p.input.Value() != before {
			p.cursor = 0
			p.refilter()
		}
	}
	return nil, false, cmd
}

func (p *picker) view(w, h int) string {
	var b strings.Builder
	b.WriteString(stHeader.Render(p.title) + "\n")
	b.WriteString(p.input.View() + "\n\n")
	rows := h - 5
	if rows < 1 {
		rows = 1
	}
	start := 0
	if p.cursor >= rows {
		start = p.cursor - rows + 1
	}
	if len(p.shown) == 0 {
		b.WriteString(stDim.Render("  no match") + "\n")
	}
	for i := start; i < len(p.shown) && i < start+rows; i++ {
		it := p.items[p.shown[i]]
		line := "  " + it.label
		if it.detail != "" {
			line += "  " + stDim.Render(it.detail)
		}
		if i == p.cursor {
			line = stSelected.Render(fit("› "+it.label+"  "+it.detail, w))
		}
		b.WriteString(clip(line, w) + "\n")
	}
	b.WriteString("\n" + hints("↑↓", "move", "enter", "select", "esc", "cancel", "type", "to filter"))
	return b.String()
}
