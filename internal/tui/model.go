// Package tui is the terminal interface: the resource browser, the copilot
// pane beside it, and the approval dialog.
//
// It only ever reads from the cluster. The single thing it can do towards a
// change is hand the human's decision back to the approval gate.
package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/fardani235/k8s-copilot/internal/agent"
	"github.com/fardani235/k8s-copilot/internal/approval"
	"github.com/fardani235/k8s-copilot/internal/kube"
	"github.com/fardani235/k8s-copilot/internal/textutil"
)

// Minimum usable terminal size.
const (
	MinWidth  = 60
	MinHeight = 16
)

// FocusStore is the browser's current focus, readable from the agent's
// goroutine. The UI overwrites it after every update, so a request always
// sees what is on screen at the moment it is sent.
type FocusStore struct {
	mu sync.RWMutex
	f  agent.Focus
}

// Get returns the latest focus.
func (s *FocusStore) Get() agent.Focus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.f
}

func (s *FocusStore) set(f agent.Focus) {
	s.mu.Lock()
	s.f = f
	s.mu.Unlock()
}

// Deps is everything the UI is given.
type Deps struct {
	Cluster *kube.Cluster
	// Agent is nil when the copilot cannot start (no API key, …); AgentErr
	// then says why and what to do. The browser works regardless.
	Agent    *agent.Agent
	AgentErr string
	Gate     *approval.Gate
	Focus    *FocusStore

	AuditPath string
	AuditNote string // start-up warning about the audit trail, if any

	Refresh   time.Duration
	Namespace string // initial namespace
	Version   string
}

type viewID int

const (
	vList viewID = iota
	vDetail
	vLogs
	vEvents
	vAudit
	vHelp
)

func (v viewID) String() string {
	return [...]string{"list", "detail", "logs", "events", "audit trail", "help"}[v]
}

type paneID int

const (
	paneBrowser paneID = iota
	paneCopilot
)

// subject is the resource a detail/logs/events view is about.
type subject struct {
	typ       kube.ResourceType
	namespace string
	name      string
	obj       *unstructured.Unstructured // nil until fetched
}

func (s subject) title() string {
	if s.namespace != "" {
		return fmt.Sprintf("%s %s/%s", s.typ.Kind, s.namespace, s.name)
	}
	return fmt.Sprintf("%s %s", s.typ.Kind, s.name)
}

// Model is the Bubble Tea model.
type Model struct {
	deps   Deps
	ctx    context.Context
	cancel context.CancelFunc

	width, height int
	ready         bool

	// navigation
	view  viewID
	stack []viewID
	pane  paneID
	pick  *picker
	pickK string // "ns" or "type"
	// pickTypes is the type list the open picker was built from; m.types can
	// be replaced by a refresh while the picker is showing.
	pickTypes []kube.ResourceType

	status string // transient footer message

	// cluster data
	types      []kube.ResourceType
	typesErr   error
	typesWarn  string
	namespaces []string
	nsErr      error

	curNS   string // kube.AllNamespaces = all
	curType kube.ResourceType

	// listing
	table     *kube.Table
	listErr   error
	loading   bool
	listSeq   int
	listedAt  time.Time
	cursor    int
	offset    int
	filter    string
	filtering bool
	filterIn  textinput.Model
	rows      []int // visible row indexes into table.Rows

	// sub-views
	subj     subject
	subjErr  error
	subjSeq  int
	body     viewport.Model // detail / events / audit / help
	logs     logState
	auditTxt string

	// copilot
	copilotOpen bool
	// copilotMax gives the copilot the whole body instead of the split. It is
	// entered with m from the browser; leaving the copilot restores the split.
	copilotMax bool
	transcript []chatItem
	chat       viewport.Model
	input      textinput.Model
	busy       bool
	spin       spinner.Model
	turnCancel context.CancelFunc
	events     chan agent.Event
	closed     chan struct{}
	wg         sync.WaitGroup
	shutdown   sync.Once
	activity   string

	// approval
	pending *approval.Request
	modal   modalState
	editor  textarea.Model
}

// New builds the model.
func New(deps Deps) *Model {
	ctx, cancel := context.WithCancel(context.Background())
	in := textinput.New()
	in.Prompt = "› "
	in.Placeholder = "ask about what you're looking at…"
	in.CharLimit = 4000

	fi := textinput.New()
	fi.Prompt = "/"

	ed := textarea.New()
	ed.ShowLineNumbers = false
	ed.CharLimit = 8000

	sp := spinner.New()
	sp.Spinner = spinner.MiniDot

	ns := deps.Namespace
	if ns == "" {
		ns = deps.Cluster.Info.Namespace
	}
	m := &Model{
		deps: deps, ctx: ctx, cancel: cancel,
		curNS: ns, input: in, filterIn: fi, editor: ed, spin: sp,
		events: make(chan agent.Event, 256), closed: make(chan struct{}),
		body: viewport.New(0, 0), chat: viewport.New(0, 0),
	}
	m.logs.vp = viewport.New(0, 0)
	if deps.AuditNote != "" {
		m.say(chatWarn, deps.AuditNote)
	}
	m.publishFocus()
	return m
}

// Shutdown cancels anything in flight and waits briefly for the agent loop
// to finish, so that a proposal left pending is recorded as cancelled before
// the process exits. Call it after the program has stopped.
func (m *Model) Shutdown() {
	m.shutdown.Do(func() {
		m.cancel()
		close(m.closed)
		done := make(chan struct{})
		go func() { m.wg.Wait(); close(done) }()
		// Normally immediate. The long limit is for a change that was
		// approved a moment before quitting: it is allowed to finish so its
		// outcome gets recorded.
		select {
		case <-done:
		case <-time.After(70 * time.Second):
		}
	})
}

// --- messages ---------------------------------------------------------------

type (
	typesMsg struct {
		types []kube.ResourceType
		warn  *kube.DiscoveryWarning
		err   error
	}
	nsMsg struct {
		names []string
		err   error
	}
	listMsg struct {
		seq   int
		table *kube.Table
		err   error
	}
	subjectMsg struct {
		seq int
		obj *unstructured.Unstructured
		err error
	}
	eventsMsg struct {
		seq    int
		events []kube.Event
		err    error
	}
	auditMsg      struct{ text string }
	tickMsg       struct{}
	agentEventMsg struct{ ev agent.Event }
	proposalMsg   struct{ req *approval.Request }
)

func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.loadTypes(false), m.loadNamespaces(), m.listenAgent()}
	if m.deps.Gate != nil {
		cmds = append(cmds, m.listenGate())
	}
	if m.deps.Refresh > 0 {
		cmds = append(cmds, m.tick())
	}
	return tea.Batch(cmds...)
}

func (m *Model) tick() tea.Cmd {
	return tea.Tick(m.deps.Refresh, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m *Model) listenAgent() tea.Cmd {
	return func() tea.Msg {
		select {
		case ev := <-m.events:
			return agentEventMsg{ev}
		case <-m.closed:
			return nil
		}
	}
}

func (m *Model) listenGate() tea.Cmd {
	return func() tea.Msg {
		select {
		case req := <-m.deps.Gate.Requests():
			return proposalMsg{req}
		case <-m.closed:
			return nil
		}
	}
}

func (m *Model) loadTypes(refresh bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
		defer cancel()
		types, warn, err := m.deps.Cluster.Types(ctx, refresh)
		return typesMsg{types, warn, err}
	}
}

func (m *Model) loadNamespaces() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
		defer cancel()
		names, err := m.deps.Cluster.Namespaces(ctx)
		return nsMsg{names, err}
	}
}

func (m *Model) loadList() tea.Cmd {
	if m.curType.IsZero() {
		return nil
	}
	m.listSeq++
	seq, t, ns := m.listSeq, m.curType, m.curNS
	m.loading = true
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
		defer cancel()
		tbl, err := m.deps.Cluster.List(ctx, t, ns, 0)
		return listMsg{seq, tbl, err}
	}
}

// --- update -----------------------------------------------------------------

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	model, cmd := m.update(msg)
	m.publishFocus()
	return model, cmd
}

func (m *Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height, m.ready = msg.Width, msg.Height, true
		m.layout()
		return m, nil

	case tea.KeyMsg:
		return m.onKey(msg)

	case typesMsg:
		m.types, m.typesErr, m.typesWarn = msg.types, msg.err, ""
		if msg.warn != nil {
			m.typesWarn = textutil.Sanitize(msg.warn.Error())
		}
		if msg.err != nil || !m.curType.IsZero() {
			return m, nil
		}
		t, err := kube.ResolveIn(m.types, "pods")
		if err != nil {
			t = m.types[0]
		}
		m.curType = t
		return m, m.loadList()

	case nsMsg:
		m.namespaces, m.nsErr = msg.names, msg.err
		return m, nil

	case listMsg:
		if msg.seq != m.listSeq {
			return m, nil // a newer request superseded this one
		}
		m.loading = false
		m.listErr = msg.err
		if msg.err == nil {
			keepNS, keepName := m.selectedKey()
			m.table = msg.table
			m.listedAt = time.Now()
			m.refilter()
			m.selectKey(keepNS, keepName)
		} else {
			m.table = nil
			m.rows = nil
		}
		return m, nil

	case tickMsg:
		var cmd tea.Cmd
		if m.view == vList && m.pick == nil && !m.loading && !m.curType.IsZero() {
			cmd = m.loadList()
		}
		return m, tea.Batch(cmd, m.tick())

	case subjectMsg:
		if msg.seq != m.subjSeq {
			return m, nil
		}
		m.subj.obj, m.subjErr = msg.obj, msg.err
		return m, m.onSubjectLoaded()

	case eventsMsg:
		if msg.seq != m.subjSeq || m.view != vEvents {
			return m, nil
		}
		m.body.SetContent(renderEvents(msg.events, msg.err, m.body.Width))
		m.body.GotoBottom()
		return m, nil

	case auditMsg:
		m.auditTxt = msg.text
		if m.view == vAudit {
			m.body.SetContent(wrap(m.auditTxt, m.body.Width))
			m.body.GotoBottom()
		}
		return m, nil

	case logMsg:
		return m, m.onLog(msg)

	case agentEventMsg:
		m.onAgentEvent(msg.ev)
		return m, m.listenAgent()

	case proposalMsg:
		m.openProposal(msg.req)
		return m, m.listenGate()

	case spinner.TickMsg:
		if !m.busy {
			return m, nil
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	}

	// Cursor blink and similar housekeeping for whichever input is active.
	var cmd tea.Cmd
	switch {
	case m.pending != nil && m.modal.editing:
		m.editor, cmd = m.editor.Update(msg)
	case m.pick != nil:
		m.pick.input, cmd = m.pick.input.Update(msg)
	case m.filtering:
		m.filterIn, cmd = m.filterIn.Update(msg)
	case m.pane == paneCopilot:
		m.input, cmd = m.input.Update(msg)
	}
	return m, cmd
}

func (m *Model) onKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if key == "ctrl+c" {
		return m, tea.Quit
	}
	m.status = ""

	// The approval dialog owns the keyboard while it is showing.
	if m.pending != nil && !m.modal.hidden {
		return m, m.onModalKey(msg)
	}
	// Reopening a hidden proposal works from anywhere.
	if key == "ctrl+p" && m.pending != nil {
		m.modal.hidden = false
		m.modal.shownAt = time.Now()
		return m, nil
	}
	if m.pick != nil {
		return m, m.onPickerKey(msg)
	}
	if m.filtering {
		return m, m.onFilterKey(msg)
	}
	if m.pane == paneCopilot && m.copilotOpen {
		return m, m.onCopilotKey(msg)
	}

	// Keys that work in every browser view.
	switch key {
	case "q":
		return m, tea.Quit
	case "tab":
		m.copilotOpen = true
		m.pane = paneCopilot
		m.layout()
		return m, m.input.Focus()
	case "c":
		m.copilotOpen = !m.copilotOpen
		if !m.copilotOpen {
			m.copilotMax = false
		}
		m.layout()
		return m, nil
	case "m":
		return m, m.toggleMaximize()
	case "p":
		if m.pending != nil {
			m.modal.hidden = false
			m.modal.shownAt = time.Now()
		} else {
			m.status = "no proposal is waiting"
		}
		return m, nil
	case "?":
		if m.view != vHelp {
			m.push(vHelp)
			m.body.SetContent(wrap(helpText(), m.body.Width))
			m.body.GotoTop()
		}
		return m, nil
	case "A":
		if m.view != vAudit {
			m.push(vAudit)
			m.body.SetContent("Reading the audit trail…")
			return m, m.loadAudit()
		}
		return m, nil
	case "esc", "backspace":
		if m.view != vList {
			return m, m.pop()
		}
		if key == "esc" && m.filter != "" {
			m.filter = ""
			m.refilter()
		}
		return m, nil
	}

	switch m.view {
	case vList:
		return m, m.onListKey(key)
	case vLogs:
		return m, m.onLogsKey(msg)
	default:
		return m, m.onBodyKey(msg)
	}
}

func (m *Model) push(v viewID) {
	m.stack = append(m.stack, m.view)
	m.view = v
	m.layout()
}

func (m *Model) pop() tea.Cmd {
	if m.view == vLogs {
		m.logs.stop()
	}
	if n := len(m.stack); n > 0 {
		m.view = m.stack[n-1]
		m.stack = m.stack[:n-1]
	} else {
		m.view = vList
	}
	m.layout()
	// The sub-views share one viewport: put the returning view's content back.
	switch m.view {
	case vList:
		return m.loadList()
	case vDetail:
		m.renderDetail()
	case vEvents:
		return m.loadEvents()
	case vAudit:
		return m.loadAudit()
	case vHelp:
		m.body.SetContent(wrap(helpText(), m.body.Width))
	case vLogs:
		return m.startLogs()
	}
	return nil
}

// publishFocus tells the copilot what is on screen.
func (m *Model) publishFocus() {
	if m.deps.Focus == nil {
		return
	}
	f := agent.Focus{
		Context:       m.deps.Cluster.Info.Context,
		Namespace:     m.curNS,
		AllNamespaces: m.curNS == kube.AllNamespaces,
		View:          m.view.String(),
	}
	if !m.curType.IsZero() {
		f.Type, f.Kind, f.Namespaced = m.curType.String(), m.curType.Kind, m.curType.Namespaced
		if !m.curType.Namespaced {
			f.AllNamespaces, f.Namespace = false, "(not applicable: cluster-scoped type)"
		}
	}
	if m.view == vDetail || m.view == vLogs || m.view == vEvents {
		f.Selected, f.SelectedNamespace = m.subj.name, m.subj.namespace
		f.Type, f.Kind, f.Namespaced = m.subj.typ.String(), m.subj.typ.Kind, m.subj.typ.Namespaced
	} else if row, ok := m.selectedRow(); ok {
		f.Selected, f.SelectedNamespace = row.Name, row.Namespace
	}
	m.deps.Focus.set(f)
}

// layout sizes the viewports for the current terminal and pane arrangement.
func (m *Model) layout() {
	bw, bh := m.browserInner()
	m.body.Width, m.body.Height = bw, bh-1
	m.logs.vp.Width, m.logs.vp.Height = bw, bh-2
	cw, ch := m.copilotInner()
	m.chat.Width, m.chat.Height = cw, ch-3
	m.input.Width = cw - 4
	m.renderChat()
}

// copilotWidth is the outer width of the copilot pane (0 when hidden).
func (m *Model) copilotWidth() int {
	if !m.copilotOpen {
		return 0
	}
	if m.copilotMax {
		return m.width
	}
	return clamp(m.width*42/100, 34, 80)
}

func (m *Model) browserInner() (w, h int) {
	h = m.height - 2 - 2
	if m.copilotMax {
		// Maximized: the browser is not rendered, so it gets no width.
		return 0, h
	}
	return m.width - m.copilotWidth() - 2, h
}

func (m *Model) copilotInner() (w, h int) {
	return m.copilotWidth() - 2, m.height - 2 - 2
}

// toggleMaximize gives the copilot the whole body, or restores the split. It
// is entered with m from the browser. While the copilot input is focused, m is
// an ordinary character, so the way back is esc or tab (see leaveCopilot).
func (m *Model) toggleMaximize() tea.Cmd {
	if m.copilotMax {
		m.copilotMax = false
		m.pane = paneBrowser
		m.input.Blur()
		m.layout()
		return nil
	}
	m.copilotOpen = true
	m.copilotMax = true
	m.pane = paneCopilot
	m.layout()
	return m.input.Focus()
}

// --- view -------------------------------------------------------------------

func (m *Model) View() string {
	if !m.ready {
		return "starting…"
	}
	if m.width < MinWidth || m.height < MinHeight {
		return fmt.Sprintf("Terminal too small for k8s-copilot.\nIt needs at least %d×%d; this one is %d×%d.\nEnlarge the window, or press q to quit.",
			MinWidth, MinHeight, m.width, m.height)
	}
	if m.pending != nil && !m.modal.hidden {
		return m.viewModal()
	}

	if m.copilotMax {
		cw, ch := m.copilotInner()
		cstyle := stPane
		if m.pane == paneCopilot {
			cstyle = stPaneFocused
		}
		body := cstyle.Width(cw).Height(ch).MaxHeight(ch + 2).MaxWidth(cw + 2).Render(m.viewCopilot(cw, ch))
		return m.viewHeader() + "\n" + body + "\n" + m.viewFooter()
	}

	bw, bh := m.browserInner()
	style := stPane
	if m.pane == paneBrowser {
		style = stPaneFocused
	}
	body := style.Width(bw).Height(bh).MaxHeight(bh + 2).MaxWidth(bw + 2).Render(m.viewBrowser(bw, bh))
	if m.copilotOpen {
		cw, ch := m.copilotInner()
		cstyle := stPane
		if m.pane == paneCopilot {
			cstyle = stPaneFocused
		}
		body = joinH(body, cstyle.Width(cw).Height(ch).MaxHeight(ch+2).MaxWidth(cw+2).Render(m.viewCopilot(cw, ch)))
	}
	return m.viewHeader() + "\n" + body + "\n" + m.viewFooter()
}

func joinH(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	n := len(al)
	if len(bl) > n {
		n = len(bl)
	}
	var out []string
	for i := 0; i < n; i++ {
		var l, r string
		if i < len(al) {
			l = al[i]
		}
		if i < len(bl) {
			r = bl[i]
		}
		out = append(out, l+r)
	}
	return strings.Join(out, "\n")
}

func (m *Model) viewHeader() string {
	info := m.deps.Cluster.Info
	left := stHeader.Render("k8s-copilot") + stDim.Render("  ctx ") + info.Context + stDim.Render("  ns ") + m.nsLabel()
	if !m.curType.IsZero() {
		left += stDim.Render("  type ") + m.curType.String()
	}
	right := ""
	if m.pending != nil {
		right = stBanner.Render("APPROVAL WAITING — press ctrl+p")
	} else if m.busy {
		right = stWarn.Render(m.spin.View() + " copilot working")
	}
	gap := m.width - lipWidth(left) - lipWidth(right)
	if gap < 1 {
		return clip(left, m.width-lipWidth(right)-1) + " " + right
	}
	return left + strings.Repeat(" ", gap) + right
}

func (m *Model) nsLabel() string {
	if !m.curType.IsZero() && !m.curType.Namespaced {
		return stDim.Render("(cluster-scoped)")
	}
	if m.curNS == kube.AllNamespaces {
		return "(all)"
	}
	return m.curNS
}

func (m *Model) viewFooter() string {
	if m.status != "" {
		return clip(stWarn.Render(m.status), m.width)
	}
	var h string
	switch {
	case m.pick != nil:
		h = hints("↑↓", "move", "enter", "select", "esc", "cancel")
	case m.filtering:
		h = hints("enter", "apply", "esc", "clear")
	case m.pane == paneCopilot && m.copilotOpen:
		if m.busy {
			h = hints("esc", "cancel request", "tab", "browser", "pgup/pgdn", "scroll", "ctrl+c", "quit")
		} else {
			h = hints("enter", "send", "tab/esc", "browser", "pgup/pgdn", "scroll", "ctrl+l", "new conversation", "ctrl+c", "quit")
		}
	case m.view == vList:
		h = hints("↑↓", "move", "enter", "detail", "l", "logs", "e", "events", "n", "namespace", "t", "type", "/", "filter", "r", "refresh", "c", "copilot", "m", "maximize", "tab", "ask", "?", "help", "q", "quit")
	case m.view == vLogs:
		h = hints("f", "follow", "s", "container", "v", "previous", "↑↓", "scroll", "esc", "back", "m", "maximize", "tab", "ask", "q", "quit")
	case m.view == vDetail:
		h = hints("↑↓", "scroll", "l", "logs", "e", "events", "esc", "back", "m", "maximize", "tab", "ask", "q", "quit")
	default:
		h = hints("↑↓", "scroll", "r", "reload", "esc", "back", "m", "maximize", "tab", "ask", "q", "quit")
	}
	return clip(h, m.width)
}

func (m *Model) viewBrowser(w, h int) string {
	if m.pick != nil {
		return m.pick.view(w, h)
	}
	switch m.view {
	case vList:
		return m.viewList(w, h)
	case vLogs:
		return m.viewLogs(w, h)
	default:
		title := m.view.String()
		if m.view == vDetail || m.view == vEvents {
			title = m.view.String() + " · " + m.subj.title()
		}
		return clip(stHeader.Render(title), w) + "\n" + m.body.View()
	}
}
