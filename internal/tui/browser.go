package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/fardani235/k8s-copilot/internal/kube"
	"github.com/fardani235/k8s-copilot/internal/textutil"
)

func lipWidth(s string) int { return lipgloss.Width(s) }

const allNamespacesLabel = "(all namespaces)"

func (m *Model) onListKey(key string) tea.Cmd {
	page := m.listRows()
	switch key {
	case "up", "k":
		m.moveCursor(-1)
	case "down", "j":
		m.moveCursor(1)
	case "pgup", "ctrl+b":
		m.moveCursor(-page)
	case "pgdown", "ctrl+f", " ":
		m.moveCursor(page)
	case "home", "g":
		m.moveCursor(-len(m.rows))
	case "end", "G":
		m.moveCursor(len(m.rows))
	case "r":
		m.status = "refreshing…"
		return tea.Batch(m.loadList(), m.loadNamespaces(), m.loadTypes(true))
	case "n":
		m.openNamespacePicker()
	case "t", ":":
		m.openTypePicker()
	case "/":
		m.filtering = true
		m.filterIn.SetValue(m.filter)
		m.filterIn.CursorEnd()
		return m.filterIn.Focus()
	case "enter", "d":
		return m.openSubject(vDetail)
	case "l":
		return m.openSubject(vLogs)
	case "e":
		return m.openSubject(vEvents)
	}
	return nil
}

func (m *Model) moveCursor(delta int) {
	m.cursor = clamp(m.cursor+delta, 0, len(m.rows)-1)
}

func (m *Model) listRows() int {
	_, h := m.browserInner()
	return max(h-3, 1)
}

func (m *Model) openNamespacePicker() {
	if !m.curType.IsZero() && !m.curType.Namespaced {
		m.status = m.curType.String() + " is cluster-scoped: there is no namespace to choose"
		return
	}
	items := []pickItem{{label: allNamespacesLabel, search: "all namespaces *", value: -1}}
	for i, n := range m.namespaces {
		items = append(items, pickItem{label: n, search: strings.ToLower(n), value: i})
	}
	title := "Namespace"
	if m.nsErr != nil {
		// Without permission to list namespaces the user can still type one.
		title = "Namespace — cannot list namespaces: " + textutil.OneLine(textutil.Sanitize(kube.Reason(m.nsErr)))
		if m.curNS != kube.AllNamespaces {
			items = append(items, pickItem{label: m.curNS, search: strings.ToLower(m.curNS), value: -2})
		}
	}
	cur := m.curNS
	if cur == kube.AllNamespaces {
		cur = allNamespacesLabel
	}
	p := newPicker(title, items, cur)
	m.pick, m.pickK = &p, "ns"
}

func (m *Model) openTypePicker() {
	if m.typesErr != nil {
		m.status = "resource types are unavailable: " + textutil.OneLine(textutil.Sanitize(m.typesErr.Error()))
		return
	}
	items := make([]pickItem, 0, len(m.types))
	for i, t := range m.types {
		scope := "cluster-scoped"
		if t.Namespaced {
			scope = "namespaced"
		}
		detail := t.Kind + " · " + t.APIVersion() + " · " + scope
		if len(t.ShortNames) > 0 {
			detail += " · " + strings.Join(t.ShortNames, ",")
		}
		items = append(items, pickItem{
			label: t.String(), detail: detail, value: i,
			search: strings.ToLower(t.String() + " " + t.Kind + " " + strings.Join(t.ShortNames, " ")),
		})
	}
	title := fmt.Sprintf("Resource type (%d served by this cluster)", len(m.types))
	if m.typesWarn != "" {
		title += " — " + m.typesWarn
	}
	p := newPicker(title, items, m.curType.String())
	m.pick, m.pickK, m.pickTypes = &p, "type", m.types
}

func (m *Model) onPickerKey(msg tea.KeyMsg) tea.Cmd {
	chosen, done, cmd := m.pick.update(msg)
	if !done {
		return cmd
	}
	kind := m.pickK
	m.pick = nil
	if chosen == nil {
		return nil
	}
	switch kind {
	case "ns":
		if chosen.value == -1 {
			m.curNS = kube.AllNamespaces
		} else {
			m.curNS = chosen.label
		}
	case "type":
		m.curType = m.pickTypes[chosen.value]
		m.filter = ""
	}
	m.table, m.rows, m.listErr = nil, nil, nil
	m.cursor, m.offset = 0, 0
	return m.loadList()
}

func (m *Model) onFilterKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "enter":
		m.filtering = false
		m.filterIn.Blur()
		return nil
	case "esc":
		m.filtering = false
		m.filterIn.Blur()
		m.setFilter("")
		return nil
	}
	var cmd tea.Cmd
	m.filterIn, cmd = m.filterIn.Update(msg)
	m.setFilter(m.filterIn.Value())
	return cmd
}

// setFilter applies the filter input to whichever listing is on screen: the
// resource listing, or the metrics screen's.
func (m *Model) setFilter(v string) {
	if m.view == vMetrics {
		if v != m.metrics.filter {
			m.metrics.filter = v
			m.rebuildMetrics(m.metrics.selectedKey())
		}
		return
	}
	if v != m.filter {
		m.filter = v
		m.refilter()
	}
}

// refilter recomputes the visible rows.
func (m *Model) refilter() {
	m.rows = m.rows[:0]
	if m.table == nil {
		return
	}
	q := strings.ToLower(strings.TrimSpace(m.filter))
	for i, r := range m.table.Rows {
		if q == "" || strings.Contains(strings.ToLower(strings.Join(r.Cells, " ")), q) {
			m.rows = append(m.rows, i)
		}
	}
	m.cursor = clamp(m.cursor, 0, len(m.rows)-1)
}

func (m *Model) selectedRow() (kube.Row, bool) {
	if m.table == nil || m.cursor < 0 || m.cursor >= len(m.rows) {
		return kube.Row{}, false
	}
	return m.table.Rows[m.rows[m.cursor]], true
}

func (m *Model) selectedKey() (ns, name string) {
	if r, ok := m.selectedRow(); ok {
		return r.Namespace, r.Name
	}
	return "", ""
}

// selectKey puts the cursor back on the same resource after a refresh.
func (m *Model) selectKey(ns, name string) {
	if name == "" {
		return
	}
	for i, idx := range m.rows {
		r := m.table.Rows[idx]
		if r.Name == name && r.Namespace == ns {
			m.cursor = i
			return
		}
	}
}

func (m *Model) scopeLabel() string {
	switch {
	case !m.curType.Namespaced:
		return "cluster-wide"
	case m.curNS == kube.AllNamespaces:
		return "all namespaces"
	default:
		return "namespace " + m.curNS
	}
}

func (m *Model) viewList(w, h int) string {
	var b strings.Builder

	// Line 1: what is listed, and its state.
	switch {
	case m.typesErr != nil:
		return stBad.Render("Cannot discover resource types") + "\n\n" + wrap(textutil.Sanitize(m.typesErr.Error()), w) +
			"\n\n" + stDim.Render("No type list is shown because it could not be trusted to be complete. Press r to retry.")
	case m.curType.IsZero():
		return stDim.Render("Discovering what this cluster serves…")
	}
	head := stHeader.Render(m.curType.String()) + stDim.Render(" · "+m.scopeLabel())
	if m.table != nil {
		head += stDim.Render(fmt.Sprintf(" · %d", len(m.rows)))
		if len(m.rows) != len(m.table.Rows) {
			head += stDim.Render(fmt.Sprintf(" of %d", len(m.table.Rows)))
		}
		if m.table.More {
			head += stWarn.Render(fmt.Sprintf(" · first %d only", kube.DefaultListLimit))
		}
	}
	if m.filtering {
		head += "  " + m.filterIn.View()
	} else if m.filter != "" {
		head += stWarn.Render("  /" + m.filter)
	}
	if m.typesWarn != "" {
		head += stWarn.Render("  ⚠ " + m.typesWarn)
	}
	b.WriteString(clip(head, w) + "\n")

	// States that replace the table.
	switch {
	case m.listErr != nil && kube.IsDenied(m.listErr):
		b.WriteString("\n" + stBad.Render("Permission denied") + "\n\n")
		b.WriteString(wrap(fmt.Sprintf("You are not allowed to list %s (%s). This is an authorization failure, not an empty list.", m.curType.String(), m.scopeLabel()), w) + "\n\n")
		b.WriteString(stDim.Render(wrap(textutil.Sanitize(kube.Reason(m.listErr)), w)))
		return b.String()
	case m.listErr != nil:
		b.WriteString("\n" + stBad.Render("Cannot list "+m.curType.String()) + "\n\n")
		b.WriteString(wrap(textutil.Sanitize(kube.Reason(m.listErr)), w) + "\n\n" + stDim.Render("Press r to retry."))
		return b.String()
	case m.table == nil:
		b.WriteString("\n" + stDim.Render("Loading…"))
		return b.String()
	case len(m.table.Rows) == 0:
		b.WriteString("\n" + stDim.Render(fmt.Sprintf("No %s found (%s).", m.curType.String(), m.scopeLabel())))
		return b.String()
	case len(m.rows) == 0:
		b.WriteString("\n" + stDim.Render(fmt.Sprintf("Nothing matches the filter %q. Press esc to clear it.", m.filter)))
		return b.String()
	}

	// Column widths from the visible content.
	cols := m.table.Columns
	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = lipWidth(c)
	}
	for _, idx := range m.rows {
		for i, c := range m.table.Rows[idx].Cells {
			if i < len(widths) {
				widths[i] = max(widths[i], min(lipWidth(c), 56))
			}
		}
	}
	line := func(cells []string) string {
		var sb strings.Builder
		for i := range cols {
			c := ""
			if i < len(cells) {
				c = textutil.Sanitize(cells[i])
			}
			sb.WriteString(fit(c, widths[i]))
			if i < len(cols)-1 {
				sb.WriteString("  ")
			}
		}
		return sb.String()
	}
	b.WriteString(stColHead.Render(clip(line(cols), w)) + "\n")

	rows := max(h-2, 1)
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+rows {
		m.offset = m.cursor - rows + 1
	}
	m.offset = clamp(m.offset, 0, max(len(m.rows)-rows, 0))
	for i := m.offset; i < len(m.rows) && i < m.offset+rows; i++ {
		row := m.table.Rows[m.rows[i]]
		text := line(row.Cells)
		switch {
		case i == m.cursor:
			b.WriteString(stSelected.Render(fit(text, w)))
		default:
			b.WriteString(rowStyle(row).Render(clip(text, w)))
		}
		if i < m.offset+rows-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// rowStyle tints rows whose cells say something is wrong. Purely cosmetic.
func rowStyle(r kube.Row) lipgloss.Style {
	for _, c := range r.Cells {
		switch {
		case strings.Contains(c, "BackOff"), strings.Contains(c, "Error"), strings.Contains(c, "Failed"),
			strings.Contains(c, "Evicted"), strings.Contains(c, "OOMKilled"), strings.Contains(c, "NotReady"),
			strings.HasPrefix(c, "Err"), strings.HasPrefix(c, "Invalid"):
			return stBad
		case c == "Pending", c == "Terminating", c == "ContainerCreating", c == "Unknown", strings.HasPrefix(c, "Init:"):
			return stWarn
		}
	}
	return lipgloss.NewStyle()
}
