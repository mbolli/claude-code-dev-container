package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

type mode int

const (
	browse mode = iota
	filtering
	renaming
	naming // new project
	confirmDelete
	confirmMove
)

var (
	cAccent = lipgloss.AdaptiveColor{Light: "#7c3aed", Dark: "#c4a7ff"}
	cDim    = lipgloss.AdaptiveColor{Light: "#6b7280", Dark: "#8b8fa3"}
	cWarn   = lipgloss.AdaptiveColor{Light: "#b91c1c", Dark: "#ff8a80"}
	cLive   = lipgloss.AdaptiveColor{Light: "#15803d", Dark: "#7ee2a0"}
	cBusy   = lipgloss.AdaptiveColor{Light: "#b45309", Dark: "#fcd34d"}
	cRepo   = lipgloss.AdaptiveColor{Light: "#0369a1", Dark: "#7dd3fc"}
	cCodex  = lipgloss.AdaptiveColor{Light: "#047857", Dark: "#6ee7b7"}
	cAgy    = lipgloss.AdaptiveColor{Light: "#b45309", Dark: "#fbbf24"}
	cSel    = lipgloss.AdaptiveColor{Light: "#ddd6fe", Dark: "#4a3d85"}

	sHeading = lipgloss.NewStyle().Bold(true).Foreground(cAccent)
	sKey     = lipgloss.NewStyle().Bold(true).Foreground(cAccent)
	sLabel   = lipgloss.NewStyle().Foreground(cDim)
	sDim     = lipgloss.NewStyle().Foreground(cDim)
	sWho     = lipgloss.NewStyle().Bold(true)
	sWarn    = lipgloss.NewStyle().Foreground(cWarn).Bold(true)
	sLive    = lipgloss.NewStyle().Foreground(cLive)
	sBusy    = lipgloss.NewStyle().Foreground(cBusy).Bold(true)
	sRepo    = lipgloss.NewStyle().Foreground(cRepo)
	sCodex   = lipgloss.NewStyle().Foreground(cCodex)
	sAgy     = lipgloss.NewStyle().Foreground(cAgy)
	sPill    = lipgloss.NewStyle().Bold(true).Foreground(cAccent)
	sBorder  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cDim)
	sFocus   = sBorder.BorderForeground(cAccent)
)

type tuiStyle struct{}

func (tuiStyle) Heading(s string) string { return sHeading.Render(s) }
func (tuiStyle) Label(s string) string   { return sLabel.Render(s) }
func (tuiStyle) Dim(s string) string     { return sDim.Render(s) }
func (tuiStyle) Who(s string) string     { return sWho.Render(s) }

func (tuiStyle) Pct(pct float64, s string) string {
	switch {
	case pct >= 95:
		return sWarn.Render(s)
	case pct >= 80:
		return sBusy.Render(s)
	}
	return sLive.Render(s)
}

type peekMsg struct {
	uuid  string
	mtime time.Time
	turns []Turn
	err   error
}
type artsMsg struct {
	uuid  string
	mtime time.Time
	arts  []Artifact
}
type reloadMsg struct {
	st  *Store
	err error
}
type doneMsg struct {
	note string
	err  error
}
type tickMsg time.Time

type model struct {
	st          *Store
	rows        []*Session
	cursor      int
	top         int
	marked      map[string]bool
	showScratch bool
	liveOnly    bool
	agentFilter string // "", "claude", "codex", "agy"

	mode           mode
	label          string // what the text input is for
	input          textinput.Model
	query          string
	preFilterQuery string
	pending        []*Session // targets of a confirm
	summary        string     // the confirm question, measured once

	peek      viewport.Model
	peekFocus bool
	peeks     map[string]peekMsg
	peekFor   string
	shownFor  string // the session or view key the pane currently shows
	showStats bool   // the right pane shows stats instead of the peek
	showHelp  bool   // the right pane shows the keyboard shortcuts & legend
	showAccts bool   // the right pane shows clauth accounts and usage windows
	arts      map[string]artsMsg

	note  string
	isErr bool
	w, h  int

	then func() error // run after the TUI exits: the tmux handoff
}

func runTUI() int {
	st, err := Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "sessel:", err)
		return 1
	}
	in := textinput.New()
	in.Prompt = ""
	m := &model{st: st, marked: map[string]bool{}, peeks: map[string]peekMsg{}, arts: map[string]artsMsg{}, input: in, peek: viewport.New(0, 0)}
	m.refilter()
	final, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "sessel:", err)
		return 1
	}
	if fm := final.(*model); fm.then != nil {
		if err := fm.then(); err != nil {
			fmt.Fprintln(os.Stderr, "sessel:", err)
			return 1
		}
	}
	return 0
}

func (m *model) Init() tea.Cmd { return tea.Batch(m.loadPeek(), tick()) }

// Live state changes under us (sessions start and stop), and a reload is a
// cache hit costing milliseconds, so the list refreshes itself.
func tick() tea.Cmd {
	return tea.Tick(2*time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func reload() tea.Cmd {
	return func() tea.Msg {
		st, err := Load()
		return reloadMsg{st, err}
	}
}

func (m *model) current() *Session {
	if m.cursor >= 0 && m.cursor < len(m.rows) {
		return m.rows[m.cursor]
	}
	return nil
}

func (m *model) hasActiveFilters() bool {
	return m.query != "" || m.agentFilter != "" || m.liveOnly || m.showScratch
}

func (m *model) refilter() {
	keep := ""
	if s := m.current(); s != nil {
		keep = s.UUID
	}
	q := strings.ToLower(strings.TrimSpace(m.query))
	m.rows = m.rows[:0]
	for _, s := range m.st.Sessions {
		if m.agentFilter != "" && s.AgentName() != m.agentFilter {
			continue
		}
		if m.liveOnly && s.State == Dead {
			continue
		}
		if s.Scratch() && !m.showScratch {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(s.AgentName()+" "+s.Profile+" "+s.Title+" "+s.Repo()+" "+s.Branch+" "+s.UUID+" "+strings.Join(s.Bridges, " ")), q) {
			continue
		}
		m.rows = append(m.rows, s)
	}
	m.cursor = 0
	for i, s := range m.rows {
		if s.UUID == keep {
			m.cursor = i
		}
	}
	for uuid := range m.marked {
		if m.st.Get(uuid) == nil {
			delete(m.marked, uuid)
		}
	}
}

func (m *model) loadPeek() tea.Cmd {
	if m.showHelp || m.showAccts {
		m.setPeek()
		return nil
	}
	s := m.current()
	if s == nil {
		m.setPeek()
		return nil
	}
	m.peekFor = s.UUID
	if m.showStats {
		if a, ok := m.arts[s.UUID]; ok && a.mtime.Equal(s.Mtime) {
			m.setPeek()
			return nil
		}
		m.setPeek()
		st, sess := m.st, s
		return func() tea.Msg { return artsMsg{sess.UUID, sess.Mtime, st.Footprint(sess)} }
	}
	if p, ok := m.peeks[s.UUID]; ok && p.mtime.Equal(s.Mtime) {
		m.setPeek()
		return nil
	}
	m.setPeek()
	sess, mtime := s, s.Mtime
	return func() tea.Msg {
		turns, err := lastTurnsFor(sess, peekTurns)
		return peekMsg{sess.UUID, mtime, turns, err}
	}
}

// setPeek redraws the scrollable body of the right pane. The session header
// stays pinned above the viewport so scrolling never hides the session's title
// or IDs. A live session's peek starts at the bottom and follows new turns
// while at the bottom.
func (m *model) setPeek() {
	m.peek.Width = m.rightWidth()
	m.peek.Height = m.viewportHeight(m.listHeight())
	width := max(20, m.peek.Width-2)

	if m.showHelp || m.showAccts {
		key, content := "__help__", ""
		if m.showAccts {
			key, content = "__accounts__", renderAccountsBody(m.st.Clauth, width, tuiStyle{})
		} else {
			content = renderHelpBody(width)
		}
		same := m.shownFor == key
		offset := m.peek.YOffset
		m.shownFor = key
		m.peek.SetContent(content)
		if !same {
			m.peek.GotoTop()
		} else {
			m.peek.SetYOffset(offset)
		}
		return
	}

	s := m.current()
	if s == nil {
		m.shownFor = ""
		m.peek.SetContent(sDim.Render("no sessions match the current filters"))
		return
	}
	viewKey := s.UUID
	if m.showStats {
		viewKey += ":stats"
	}
	same := m.shownFor == viewKey
	follow := !m.showStats && s.State != Dead && (!same || m.peek.AtBottom())
	offset := m.peek.YOffset
	m.shownFor = viewKey

	if m.showStats {
		var arts []Artifact
		if a, ok := m.arts[s.UUID]; ok {
			arts = a.arts
		}
		m.peek.SetContent(renderStatsBody(s, arts, m.st.Clauth, width, tuiStyle{}))
		if !same {
			m.peek.GotoTop()
		} else {
			m.peek.SetYOffset(offset)
		}
		return
	}
	p, ok := m.peeks[s.UUID]
	switch {
	case !ok:
		m.peek.SetContent(renderPeekBody(s, nil, width, tuiStyle{}) + sDim.Render("\nreading…"))
	case p.err != nil:
		m.peek.SetContent(sWarn.Render(p.err.Error()))
	default:
		m.peek.SetContent(renderPeekBody(s, p.turns, width, tuiStyle{}))
	}
	switch {
	case follow:
		m.peek.GotoBottom()
	case same:
		m.peek.SetYOffset(offset)
	default:
		m.peek.GotoTop()
	}
}

func (m *model) say(format string, a ...any) { m.note, m.isErr = fmt.Sprintf(format, a...), false }
func (m *model) fail(err error)              { m.note, m.isErr = err.Error(), true }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.layout()
		m.setPeek()
		return m, nil
	case tickMsg:
		if m.mode == browse || m.mode == filtering {
			return m, tea.Batch(reload(), tick())
		}
		return m, tick()
	case reloadMsg:
		if msg.err == nil {
			m.st = msg.st
			m.refilter()
			return m, m.loadPeek()
		}
		return m, nil
	case artsMsg:
		m.arts[msg.uuid] = msg
		if msg.uuid == m.peekFor {
			m.setPeek()
		}
		return m, nil
	case peekMsg:
		m.peeks[msg.uuid] = msg
		if msg.uuid == m.peekFor {
			m.setPeek()
		}
		return m, nil
	case doneMsg:
		m.pending, m.mode = nil, browse
		if msg.err != nil {
			m.fail(msg.err)
		} else {
			m.say("%s", msg.note)
		}
		return m, reload()
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m *model) openSelected() (tea.Model, tea.Cmd) {
	s := m.current()
	switch {
	case s == nil:
		return m, nil
	case s.State == LiveOutside && s.PID == 0:
		m.fail(Stop(s))
		return m, nil
	case s.State == LiveOutside:
		m.pending, m.mode = []*Session{s}, confirmMove
		return m, nil
	default:
		m.then = func() error { return Open(s) }
		return m, tea.Quit
	}
}

func (m *model) key(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.mode {
	case filtering, renaming, naming:
		return m.textKey(k)
	case confirmDelete, confirmMove:
		return m.confirmKey(k)
	}

	if m.peekFocus {
		switch k.String() {
		case "tab", "esc", "left", "h":
			m.peekFocus = false
			return m, nil
		case "?":
			m.showHelp = !m.showHelp
			m.showAccts = false
			m.setPeek()
			return m, nil
		case "u":
			m.showAccts = !m.showAccts
			m.showHelp = false
			return m, m.loadPeek()
		case "s":
			m.showHelp, m.showAccts = false, false
			m.showStats = !m.showStats
			return m, m.loadPeek()
		case "g", "home":
			m.peek.GotoTop()
			return m, nil
		case "G", "end":
			m.peek.GotoBottom()
			return m, nil
		case "q", "ctrl+c":
			return m, tea.Quit
		}
		var cmd tea.Cmd
		m.peek, cmd = m.peek.Update(k)
		return m, cmd
	}

	m.note = ""
	switch k.String() {
	case "?", "f1":
		m.showHelp = !m.showHelp
		m.showAccts = false
		return m, m.loadPeek()
	case "u":
		m.showAccts = !m.showAccts
		m.showHelp = false
		return m, m.loadPeek()
	case "q", "ctrl+c":
		return m, tea.Quit
	case "esc":
		if m.showHelp || m.showAccts {
			m.showHelp, m.showAccts = false, false
			return m, m.loadPeek()
		}
		if m.query != "" || m.agentFilter != "" || m.liveOnly {
			m.query, m.agentFilter, m.liveOnly = "", "", false
			m.refilter()
			m.say("filters cleared")
			return m, m.loadPeek()
		}
		return m, tea.Quit
	case "up", "k":
		return m.move(-1)
	case "down", "j":
		return m.move(1)
	case "pgup":
		return m.move(-m.listRowsHeight())
	case "pgdown":
		return m.move(m.listRowsHeight())
	case "home", "g":
		return m.move(-len(m.rows))
	case "end", "G":
		return m.move(len(m.rows))
	case "J":
		m.peek.LineDown(3)
		return m, nil
	case "K":
		m.peek.LineUp(3)
		return m, nil
	case "ctrl+d":
		m.peek.HalfViewDown()
		return m, nil
	case "ctrl+u":
		m.peek.HalfViewUp()
		return m, nil
	case "tab", "right":
		m.peekFocus = true
		return m, nil
	case "/":
		m.preFilterQuery = m.query
		m.startInput(filtering, m.query, "filter")
	case "f":
		m.agentFilter = nextAgentFilter(m.agentFilter)
		m.refilter()
		return m, m.loadPeek()
	case "l":
		m.liveOnly = !m.liveOnly
		m.refilter()
		return m, m.loadPeek()
	case "a":
		m.showScratch = !m.showScratch
		m.refilter()
		return m, m.loadPeek()
	case "r":
		m.say("reloaded")
		return m, reload()
	case "s":
		m.showHelp, m.showAccts = false, false
		m.showStats = !m.showStats
		return m, m.loadPeek()
	case " ":
		if s := m.current(); s != nil {
			if m.marked[s.UUID] {
				delete(m.marked, s.UUID)
			} else {
				m.marked[s.UUID] = true
			}
			return m.move(1)
		}
	case "d":
		m.askDelete()
	case "R":
		if s := m.current(); s != nil {
			if s.State != Dead {
				if s.AgentName() == AgentClaude {
					m.fail(fmt.Errorf("it is running and would write its own title back; use /rename inside it"))
				} else {
					m.fail(fmt.Errorf("it is running and would write its own title back; rename it inside the session"))
				}
			} else {
				m.startInput(renaming, s.Title, "rename")
			}
		}
	case "n":
		prompt := "new project /develop/ (name or agent:name)"
		if m.agentFilter != "" {
			prompt = fmt.Sprintf("new %s project /develop/ (name or agent:name)", m.agentFilter)
		}
		m.startInput(naming, "", prompt)
	case "enter":
		return m.openSelected()
	}
	return m, nil
}

func (m *model) move(d int) (tea.Model, tea.Cmd) {
	if len(m.rows) == 0 {
		return m, nil
	}
	m.cursor = max(0, min(len(m.rows)-1, m.cursor+d))
	return m, m.loadPeek()
}

func (m *model) startInput(md mode, value, prompt string) {
	m.mode = md
	m.label = prompt
	m.input.SetValue(value)
	m.input.CursorEnd()
	m.input.Focus()
}

func (m *model) textKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.mode == filtering {
		switch k.String() {
		case "up", "ctrl+p":
			return m.move(-1)
		case "down", "ctrl+n":
			return m.move(1)
		case "pgup":
			return m.move(-m.listRowsHeight())
		case "pgdown":
			return m.move(m.listRowsHeight())
		case "ctrl+d":
			m.peek.HalfViewDown()
			return m, nil
		case "ctrl+u":
			m.peek.HalfViewUp()
			return m, nil
		case "tab":
			m.query = strings.TrimSpace(m.input.Value())
			m.mode = browse
			m.input.Blur()
			m.refilter()
			return m, m.loadPeek()
		}
	}

	switch k.String() {
	case "esc":
		wasFiltering := m.mode == filtering
		m.mode = browse
		m.input.Blur()
		if wasFiltering {
			m.query = ""
			m.refilter()
			return m, m.loadPeek()
		}
		return m, nil
	case "enter":
		v := strings.TrimSpace(m.input.Value())
		md := m.mode
		m.mode = browse
		m.input.Blur()
		switch md {
		case filtering:
			m.query = v
			m.refilter()
			if m.current() != nil {
				return m.openSelected()
			}
			return m, m.loadPeek()
		case renaming:
			s := m.current()
			if s == nil || v == "" {
				return m, nil
			}
			if err := Rename(s, v); err != nil {
				m.fail(err)
				return m, nil
			}
			m.say("renamed to %q", v)
			return m, reload()
		case naming:
			if v == "" {
				return m, nil
			}
			defAgent := m.agentFilter
			if defAgent == "" {
				defAgent = AgentClaude
			}
			name, agent, err := parseNewSpec(v, defAgent)
			if err != nil {
				m.fail(err)
				return m, nil
			}
			dir, err := NewProject(name, agent)
			if err != nil {
				m.fail(err)
				return m, nil
			}
			// Open a first session here.
			m.then = func() error { return Start(dir, agent) }
			return m, tea.Quit
		}
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	if m.mode == filtering { // filter as you type
		m.query = m.input.Value()
		m.refilter()
		return m, tea.Batch(cmd, m.loadPeek())
	}
	return m, cmd
}

func (m *model) askDelete() {
	var targets []*Session
	for _, s := range m.st.Sessions {
		if m.marked[s.UUID] {
			targets = append(targets, s)
		}
	}
	if len(targets) == 0 {
		if s := m.current(); s != nil {
			targets = []*Session{s}
		}
	}
	if len(targets) == 0 {
		return
	}
	var files int
	var bytes int64
	running := 0
	for _, s := range targets {
		if s.State != Dead && s.PID == 0 {
			m.fail(Stop(s))
			return
		}
		f, b := totals(m.st.Footprint(s))
		files, bytes = files+f, bytes+b
		if s.State != Dead {
			running++
		}
	}
	what := fmt.Sprintf("%q", oneLine(targets[0].Title, 40))
	if len(targets) > 1 {
		what = fmt.Sprintf("%d sessions", len(targets))
	}
	stop := ""
	if running > 0 {
		stop = fmt.Sprintf(", stopping %d running first", running)
	}
	m.summary = fmt.Sprintf("Delete %s for good (%d files, %s%s)? claude.ai entries stay. y/N",
		what, files, humanBytes(bytes), stop)
	m.pending, m.mode = targets, confirmDelete
}

func (m *model) confirmKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if k.String() != "y" && k.String() != "Y" {
		m.pending, m.mode = nil, browse
		m.say("nothing changed")
		return m, nil
	}
	targets, st := m.pending, m.st
	if m.mode == confirmMove {
		s := targets[0]
		m.then = func() error { return Open(s) }
		return m, tea.Quit
	}
	m.say("deleting…")
	return m, func() tea.Msg {
		for _, s := range targets {
			if err := Stop(s); err != nil {
				return doneMsg{err: err}
			}
		}
		for _, s := range targets {
			if err := st.Delete(s); err != nil {
				return doneMsg{err: err}
			}
		}
		return doneMsg{note: fmt.Sprintf("deleted %d session(s)", len(targets))}
	}
}

// ---- layout ---------------------------------------------------------------

// clamp makes s exactly w cells wide and h lines tall.
func clamp(s string, w, h int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > h {
		lines = lines[:h]
	}
	for len(lines) < h {
		lines = append(lines, "")
	}
	for i, l := range lines {
		l = ansi.Truncate(l, w, "…")
		if pad := w - ansi.StringWidth(l); pad > 0 {
			l += strings.Repeat(" ", pad)
		}
		lines[i] = l
	}
	return strings.Join(lines, "\n")
}

func (m *model) listWidth() int      { return max(44, m.w*9/20) }
func (m *model) rightWidth() int     { return max(24, m.w-m.listWidth()-4) }
func (m *model) listHeight() int     { return max(1, m.h-3) }
func (m *model) listRowsHeight() int { return max(1, m.listHeight()-2) }

func (m *model) pinnedHeaderLines() int {
	if m.showHelp || m.showAccts {
		return 2
	}
	s := m.current()
	if s == nil {
		return 0
	}
	lines := 4 // title, metadata, uuid, divider bar
	if s.Bridge() != "" {
		lines++
	}
	lines += min(3, len(s.PRs))
	return lines
}

func (m *model) viewportHeight(boxH int) int {
	hdr := m.pinnedHeaderLines()
	if boxH-hdr < 4 {
		return max(1, boxH)
	}
	return max(1, boxH-hdr)
}

func (m *model) layout() {
	m.peek.Width = m.rightWidth()
	m.peek.Height = m.viewportHeight(m.listHeight())
}

func (m *model) View() string {
	if m.w == 0 {
		return ""
	}
	pane := sBorder
	if m.peekFocus {
		pane = sFocus
	}
	// Clamp before boxing: lipgloss wraps a line wider than its box and treats
	// Height as a minimum, so one long peek line would push the footer, where
	// every prompt and confirm lives, off the screen.
	footer := ansi.Wrap(m.viewFooter(), m.w, " ")
	fh := min(3, strings.Count(footer, "\n")+1)
	boxH := max(1, m.listHeight()-fh+1)
	list := m.viewList(boxH)
	right := m.viewRight(boxH)
	body := lipgloss.JoinHorizontal(lipgloss.Top,
		sBorder.Render(clamp(list, m.listWidth(), boxH)),
		pane.Render(clamp(right, m.rightWidth(), boxH)))
	return lipgloss.JoinVertical(lipgloss.Left, body, clamp(footer, m.w, fh))
}

func (m *model) viewRight(boxH int) string {
	width := m.rightWidth()
	if m.showHelp || m.showAccts {
		title := sHeading.Render("Keyboard Shortcuts & Status Legend")
		hint := sDim.Render("? or esc to close")
		label := "help"
		if m.showAccts {
			title = sHeading.Render("Accounts & Usage (clauth)")
			hint = sDim.Render("u or esc to close")
			label = "accounts"
		}
		gap := max(1, width-ansi.StringWidth(title)-ansi.StringWidth(hint))
		top := title + strings.Repeat(" ", gap) + hint
		div := m.viewRightDivider(label, width)
		m.peek.Height = max(1, boxH-2)
		return top + "\n" + div + "\n" + m.peek.View()
	}
	s := m.current()
	if s == nil {
		return m.peek.View()
	}
	hdr := renderHeader(s, width-2, tuiStyle{})
	modeLabel := "peek"
	if m.showStats {
		modeLabel = "stats"
	}
	div := m.viewRightDivider(modeLabel, width)
	hdrLines := strings.Count(hdr, "\n") + 2
	if boxH-hdrLines < 4 {
		m.peek.Height = max(1, boxH)
		return m.peek.View()
	}
	m.peek.Height = max(1, boxH-hdrLines)
	return hdr + "\n" + div + "\n" + m.peek.View()
}

func (m *model) viewRightDivider(modeLabel string, width int) string {
	left := sDim.Render("── ") + sPill.Render(modeLabel) + sDim.Render(" ")
	var rightParts []string
	switch {
	case m.showHelp, m.showAccts:
		rightParts = append(rightParts, "J/K or ctrl+d/u scroll")
	case m.peekFocus:
		rightParts = append(rightParts, "↑↓ pgup/pgdn scroll · tab back")
	case m.showStats:
		rightParts = append(rightParts, "s peek · ? help")
	default:
		rightParts = append(rightParts, "s stats · J/K scroll · ? help")
	}
	if m.peek.TotalLineCount() > m.peek.Height {
		switch {
		case m.peek.AtTop():
			rightParts = append(rightParts, "top ↓")
		case m.peek.AtBottom():
			rightParts = append(rightParts, "↑ bot")
		default:
			rightParts = append(rightParts, fmt.Sprintf("↕ %d%%", int(m.peek.ScrollPercent()*100)))
		}
	}
	right := sDim.Render(" " + strings.Join(rightParts, " · ") + " ──")
	fill := max(1, width-ansi.StringWidth(left)-ansi.StringWidth(right))
	return left + sDim.Render(strings.Repeat("─", fill)) + right
}

func shortAgentBadge(agent string) string {
	switch agent {
	case AgentCodex:
		return "cdx"
	case AgentAgy:
		return "agy"
	default:
		return "cla"
	}
}

func renderClauthPill(st *clauthStatus) string {
	if st == nil {
		return ""
	}
	var parts []string
	for _, harness := range []string{AgentClaude, AgentCodex} {
		for _, p := range st.Profiles {
			if !p.Active || p.Harness != harness {
				continue
			}
			badge := "cla"
			name := p.Name
			if harness == AgentCodex {
				badge = "cdx"
				name = strings.TrimPrefix(name, "codex-")
			}
			txt := badge + ":" + name
			if w := clauthWindowSummary(p.Windows); w != "" {
				txt += " " + w
			}
			if st.profileOverflowed(p) {
				parts = append(parts, sBusy.Render(txt+" ↪"))
			} else {
				parts = append(parts, sDim.Render(txt))
			}
			break
		}
	}
	return strings.Join(parts, sDim.Render(" · "))
}

func (m *model) viewListHeader(width int, repoW int) (string, string) {
	total := len(m.st.Sessions)
	shown := len(m.rows)
	countStr := fmt.Sprintf(" %d sessions", shown)
	if shown != total {
		countStr = fmt.Sprintf(" %d/%d sessions", shown, total)
	}
	left := sHeading.Render(countStr)

	var pills []string
	if m.agentFilter != "" {
		pills = append(pills, sPill.Render("agent:"+m.agentFilter))
	}
	if m.liveOnly {
		pills = append(pills, sLive.Render("● live"))
	}
	if m.showScratch {
		pills = append(pills, sPill.Render("+tmp"))
	}
	if m.query != "" {
		pills = append(pills, sPill.Render("/"+oneLine(m.query, 14)))
	}
	if len(m.marked) > 0 {
		pills = append(pills, sWarn.Render(fmt.Sprintf("%d marked", len(m.marked))))
	}
	right := ""
	if len(pills) > 0 {
		right = strings.Join(pills, sDim.Render(" · ")) + " "
	} else {
		liveCount := 0
		for _, s := range m.rows {
			if s.State != Dead {
				liveCount++
			}
		}
		if liveCount > 0 {
			right = sLive.Render(fmt.Sprintf("%d live", liveCount)) + " "
		}
		if cp := renderClauthPill(m.st.Clauth); cp != "" {
			cand := cp + " "
			if right != "" {
				cand = cp + sDim.Render(" · ") + right
			}
			if ansi.StringWidth(left)+ansi.StringWidth(cand)+2 <= width {
				right = cand
			}
		}
	}
	gap := max(1, width-ansi.StringWidth(left)-ansi.StringWidth(right))
	line1 := left + strings.Repeat(" ", gap) + right

	cols := fmt.Sprintf("   %-4s %-3s %-*s %s", "AGE", "AG", repoW, "REPO", "TITLE")
	line2 := sDim.Render(ansi.Truncate(cols, width, ""))
	return line1, line2
}

func (m *model) viewList(boxH int) string {
	width := m.listWidth()
	repoW := min(18, max(12, width/4))
	titleW := max(10, width-repoW-13)

	var b strings.Builder
	rowsH := boxH
	if boxH >= 4 {
		h1, h2 := m.viewListHeader(width, repoW)
		b.WriteString(h1)
		b.WriteByte('\n')
		b.WriteString(h2)
		b.WriteByte('\n')
		rowsH = boxH - 2
	}

	if m.cursor < m.top {
		m.top = m.cursor
	}
	if m.cursor >= m.top+rowsH {
		m.top = m.cursor - rowsH + 1
	}

	if len(m.rows) == 0 {
		msg := " no sessions match"
		if m.hasActiveFilters() {
			msg += " (press esc to clear filters)"
		}
		b.WriteString(sDim.Render(msg))
		return b.String()
	}

	end := min(len(m.rows), m.top+rowsH)
	for i := m.top; i < end; i++ {
		s := m.rows[i]
		sel := i == m.cursor
		// Every segment of the selected row carries the background itself: the
		// reset ending a coloured segment would otherwise end the highlight too.
		bg := func(st lipgloss.Style) lipgloss.Style {
			if sel {
				return st.Background(cSel)
			}
			return st
		}
		plain := bg(lipgloss.NewStyle())
		titleSt := plain
		if sel {
			titleSt = titleSt.Bold(true)
		}
		cur := plain.Render(" ")
		if sel {
			cur = bg(sHeading).Render("›")
		}
		mark := plain.Render(" ")
		if m.marked[s.UUID] {
			mark = bg(sWarn).Render("✗")
		}
		mk := marker(s)
		state := plain.Render(mk)
		switch {
		case s.Busy:
			state = bg(sBusy).Render(mk)
		case s.State == LiveTmux || (s.State == LiveOutside && s.Spawner == ""):
			state = bg(sLive).Render(mk)
		case s.State == LiveOutside:
			state = bg(sDim).Render(mk)
		}

		ageSeg := bg(sDim).Render(fmt.Sprintf(" %-4s ", age(s.Mtime)))
		ag := shortAgentBadge(s.AgentName())
		agStyle := sDim
		switch s.AgentName() {
		case AgentCodex:
			agStyle = sCodex
		case AgentAgy:
			agStyle = sAgy
		}
		agSeg := bg(agStyle).Render(fmt.Sprintf("%-3s ", ag))
		repoSeg := bg(sRepo).Render(fmt.Sprintf("%-*s ", repoW, oneLine(s.Repo(), repoW)))
		titleSeg := titleSt.Render(oneLine(s.Title, titleW))

		line := ansi.Truncate(cur+mark+state+ageSeg+agSeg+repoSeg+titleSeg, width, "…")
		if pad := width - ansi.StringWidth(line); pad > 0 {
			line += plain.Render(strings.Repeat(" ", pad))
		}
		b.WriteString(line)
		if i < end-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func (m *model) viewFooter() string {
	switch m.mode {
	case filtering:
		return " " + sHeading.Render("filter: ") + m.input.View() +
			sDim.Render("   ") +
			sKey.Render("↑↓") + sDim.Render(" select · ") +
			sKey.Render("enter") + sDim.Render(" open · ") +
			sKey.Render("tab") + sDim.Render(" keep filter · ") +
			sKey.Render("esc") + sDim.Render(" clear")
	case renaming, naming:
		return " " + sHeading.Render(m.label+": ") + m.input.View() +
			sDim.Render("   ") +
			sKey.Render("enter") + sDim.Render(" confirm · ") +
			sKey.Render("esc") + sDim.Render(" cancel")
	case confirmMove:
		return " " + sWarn.Render(MoveQuestion(m.pending[0], 40)+" y/N")
	case confirmDelete:
		return " " + sWarn.Render(m.summary)
	}
	if m.note != "" {
		if m.isErr {
			return " " + sWarn.Render(m.note)
		}
		return " " + sLive.Render(m.note)
	}

	pair := func(k, desc string) string {
		return sKey.Render(k) + sDim.Render(" "+desc)
	}
	sep := sDim.Render(" · ")

	if m.peekFocus {
		return " " + strings.Join([]string{
			pair("↑↓/jk", "scroll"),
			pair("pgup/pgdn", "page"),
			pair("g/G", "top/bottom"),
			pair("s", "peek/stats"),
			pair("tab/esc", "back to list"),
			pair("?", "help"),
			pair("q", "quit"),
		}, sep)
	}
	if m.showHelp || m.showAccts {
		closeKey, closeDesc := "?/esc", "close help"
		if m.showAccts {
			closeKey, closeDesc = "u/esc", "close accounts"
		}
		return " " + strings.Join([]string{
			pair("J/K", "scroll"),
			pair("ctrl+d/u", "half-page"),
			pair("↑↓", "select session"),
			pair(closeKey, closeDesc),
			pair("q", "quit"),
		}, sep)
	}

	view := "stats"
	if m.showStats {
		view = "peek"
	}
	items := []string{
		pair("↑↓", "move"),
		pair("enter", "open"),
		pair("/", "search"),
		pair("f", "agent"),
		pair("l", "live"),
		pair("s", view),
		pair("J/K", "scroll peek"),
		pair("space", "mark"),
		pair("d", "delete"),
		pair("R", "rename"),
		pair("n", "new"),
		pair("a", "tmp"),
		pair("u", "accounts"),
	}
	if m.hasActiveFilters() {
		items = append(items, pair("esc", "clear filters"))
	}
	items = append(items, pair("?", "help"), pair("q", "quit"))
	return " " + strings.Join(items, sep)
}

func renderHelpBody(width int) string {
	var b strings.Builder
	sec := func(title string) {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(sHeading.Render("── " + title + " ──"))
		b.WriteByte('\n')
	}
	row := func(keys, desc string) {
		kCol := fmt.Sprintf("  %-16s", keys)
		wrapped := wrap(desc, max(24, width-20), 8)
		lines := strings.Split(wrapped, "\n")
		b.WriteString(sKey.Render(kCol) + " " + lines[0] + "\n")
		for _, l := range lines[1:] {
			b.WriteString(strings.Repeat(" ", 19) + l + "\n")
		}
	}

	sec("Navigation & Opening Sessions")
	row("↑ / ↓, k / j", "Move up or down in the session list. The right pane updates immediately with the selected session's peek or stats.")
	row("pgup / pgdn", "Move up or down by one full page of sessions.")
	row("g / G, home/end", "Jump to the newest session at the top (g) or the oldest session at the bottom (G).")
	row("enter", "Open the selected session in tmux:")
	row("  stopped ( )", "Resumes the conversation in its own tmux session (<repo>-<uuid8>, <repo>@codex-<uuid8>, or <repo>@agy-<uuid8>) with --remote-control enabled for Claude and agy, then attaches your terminal.")
	row("  in tmux (●/◉)", "Attaches or switches your tmux client directly to the window and pane where the session is already running.")
	row("  outside (◦/◐)", "Asks y/N first, then stops the headless spawner or external process and resumes the same transcript in interactive tmux (the mobile app keeps the session under a new bridge link).")

	sec("Right Pane: Peek, Stats & Scrolling")
	row("s", "Switch the right pane between Conversation Peek (opening prompt + latest turns and collapsed tool runs) and Session Stats (disk size across all folders, prompts, turns, tool calls, runtime span, token usage, cache hits, models, bridge IDs, and PR links).")
	row("J / K (shift)", "Scroll the right pane down (J) or up (K) by 3 lines while keeping focus in the session list.")
	row("ctrl+d / ctrl+u", "Scroll the right pane down or up by half a page without leaving the session list.")
	row("tab, →", "Focus the right pane for full keyboard scrolling (↑/↓, j/k, pgup/pgdn, g/G). Press tab, ←, h, or esc to return to the list.")

	sec("Searching & Filtering")
	row("/", "Live search across title, repo, git branch, agent, transcript UUID, and phone session_01… / cse_01… bridge IDs. While typing: ↑/↓ moves the cursor, enter opens the selected session immediately, tab keeps the filter active for batch actions, and esc clears it.")
	row("f", "Cycle agent filter: all -> claude (cla) -> codex (cdx) -> agy (agy) -> all.")
	row("l", "Toggle live-only filter (show only currently running sessions).")
	row("a", "Toggle /tmp scratchpad and subagent probe sessions (hidden by default).")
	row("esc", "Close this help view, or clear any active /, f, or l filters. Press esc again with no active filters (or press q) to quit.")

	sec("Managing Sessions & Projects")
	row("space", "Mark or unmark the selected session (✗) for batch deletion and advance to the next row.")
	row("d", "Delete all marked sessions, or the selected session if none are marked. Shows total file count and disk footprint before asking y/N. Stops running sessions first and removes local transcripts, subagent outputs, file history, caches, locks, and SQLite rows (keeping ~/.claude/history.jsonl and project memories).")
	row("R", "Rename a stopped session (updates the Claude transcript, Codex SQLite/index, or agy summary DB). Running sessions must be renamed from inside the session so the running process does not overwrite the title.")
	row("n", "Create a new project in /develop/<name>, run git init, and start its first session in tmux (for Claude, also launches a phone Remote Control spawner). Prefix with <agent>:<name> (e.g. codex:my-tool or agy:my-app) to pick the agent.")
	row("r", "Rescan transcripts and refresh live process and tmux state immediately (also refreshes automatically every 2 seconds).")
	row("u", "Show every clauth account for Claude Code and Codex in fallback order: which one is active, plan, chain position, the threshold at which it hands over, and a bar plus reset time for each usage window (5h, 7d, and per-model weekly caps). The list header shows the active accounts in short form; an amber ↪ means the work account has taken over.")
	row("?, F1", "Toggle this keyboard shortcuts and status legend view.")
	row("q, ctrl+c", "Quit sessel.")

	sec("Row Markers & Badges Legend")
	b.WriteString("  " + sBusy.Render("◉") + "  " + sWho.Render("Busy (mid-reply)") + "   " + sDim.Render("Actively generating a response or running a tool right now.") + "\n")
	b.WriteString("  " + sLive.Render("●") + "  " + sWho.Render("Live in tmux") + "       " + sDim.Render("Interactive session in a tmux pane; enter attaches directly.") + "\n")
	b.WriteString("  " + sDim.Render("◦") + "  " + sWho.Render("Idle in spawner") + "    " + sDim.Render("Headless session held by a Claude Remote Control spawner for the phone.") + "\n")
	b.WriteString("  " + sLive.Render("◐") + "  " + sWho.Render("Live outside tmux") + "  " + sDim.Render("Running in another terminal or IDE; enter offers to move it into tmux.") + "\n")
	b.WriteString("  " + sWarn.Render("✗") + "  " + sWho.Render("Marked for delete") + "  " + sDim.Render("Toggled with space; press d to delete all marked sessions at once.") + "\n")
	b.WriteString("  " + sDim.Render("cla") + " / " + sCodex.Render("cdx") + " / " + sAgy.Render("agy") + "       " + sDim.Render("Agent badge: Claude Code, OpenAI Codex, or Google Antigravity.") + "\n")

	return b.String()
}

func nextAgentFilter(cur string) string {
	switch cur {
	case "":
		return AgentClaude
	case AgentClaude:
		return AgentCodex
	case AgentCodex:
		return AgentAgy
	default:
		return ""
	}
}
