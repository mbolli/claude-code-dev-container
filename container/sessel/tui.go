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
	cSel    = lipgloss.AdaptiveColor{Light: "#ede9fe", Dark: "#2e2748"}

	sHeading = lipgloss.NewStyle().Bold(true).Foreground(cAccent)
	sLabel   = lipgloss.NewStyle().Foreground(cDim)
	sDim     = lipgloss.NewStyle().Foreground(cDim)
	sWho     = lipgloss.NewStyle().Bold(true)
	sWarn    = lipgloss.NewStyle().Foreground(cWarn).Bold(true)
	sLive    = lipgloss.NewStyle().Foreground(cLive)
	sSelRow  = lipgloss.NewStyle().Background(cSel)
	sBorder  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cDim)
	sFocus   = sBorder.BorderForeground(cAccent)
)

type tuiStyle struct{}

func (tuiStyle) Heading(s string) string { return sHeading.Render(s) }
func (tuiStyle) Label(s string) string   { return sLabel.Render(s) }
func (tuiStyle) Dim(s string) string     { return sDim.Render(s) }
func (tuiStyle) Who(s string) string     { return sWho.Render(s) }

type peekMsg struct {
	uuid  string
	mtime time.Time
	turns []Turn
	err   error
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

	mode    mode
	label   string // what the text input is for
	input   textinput.Model
	query   string
	pending []*Session // targets of a confirm
	summary string     // the confirm question, measured once

	peek      viewport.Model
	peekFocus bool
	peeks     map[string]peekMsg
	peekFor   string

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
	m := &model{st: st, marked: map[string]bool{}, peeks: map[string]peekMsg{}, input: in, peek: viewport.New(0, 0)}
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
	return tea.Tick(5*time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
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

func (m *model) refilter() {
	keep := ""
	if s := m.current(); s != nil {
		keep = s.UUID
	}
	q := strings.ToLower(m.query)
	m.rows = m.rows[:0]
	for _, s := range m.st.Sessions {
		if s.Scratch() && !m.showScratch {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(s.Title+" "+s.Repo()+" "+s.UUID+" "+strings.Join(s.Bridges, " ")), q) {
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
	s := m.current()
	if s == nil {
		return nil
	}
	m.peekFor = s.UUID
	if p, ok := m.peeks[s.UUID]; ok && p.mtime.Equal(s.Mtime) {
		m.setPeek()
		return nil
	}
	m.setPeek()
	uuid, file, mtime := s.UUID, s.File, s.Mtime
	return func() tea.Msg {
		turns, err := lastTurns(file, peekTurns)
		return peekMsg{uuid, mtime, turns, err}
	}
}

func (m *model) setPeek() {
	s := m.current()
	if s == nil {
		m.peek.SetContent(sDim.Render("no sessions"))
		return
	}
	width := m.peek.Width - 2
	p, ok := m.peeks[s.UUID]
	switch {
	case !ok:
		m.peek.SetContent(renderPeek(s, nil, width, tuiStyle{}) + sDim.Render("\nreading…"))
	case p.err != nil:
		m.peek.SetContent(sWarn.Render(p.err.Error()))
	default:
		m.peek.SetContent(renderPeek(s, p.turns, width, tuiStyle{}))
	}
	m.peek.GotoTop()
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
		if m.mode == browse {
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
		case "q", "ctrl+c":
			return m, tea.Quit
		}
		var cmd tea.Cmd
		m.peek, cmd = m.peek.Update(k)
		return m, cmd
	}

	m.note = ""
	switch k.String() {
	case "q", "ctrl+c", "esc":
		if k.String() == "esc" && m.query != "" {
			m.query = ""
			m.refilter()
			return m, m.loadPeek()
		}
		return m, tea.Quit
	case "up", "k":
		return m.move(-1)
	case "down", "j":
		return m.move(1)
	case "pgup":
		return m.move(-m.listHeight())
	case "pgdown":
		return m.move(m.listHeight())
	case "home", "g":
		return m.move(-len(m.rows))
	case "end", "G":
		return m.move(len(m.rows))
	case "tab", "right", "l":
		m.peekFocus = true
	case "/":
		m.startInput(filtering, m.query, "filter")
	case "a":
		m.showScratch = !m.showScratch
		m.refilter()
		return m, m.loadPeek()
	case "r":
		return m, reload()
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
				m.fail(fmt.Errorf("it is running and would write its own title back; use /rename inside it"))
			} else {
				m.startInput(renaming, s.Title, "rename")
			}
		}
	case "n":
		m.startInput(naming, "", "new project /develop/")
	case "enter":
		s := m.current()
		switch {
		case s == nil:
		case s.State == LiveOutside:
			m.pending, m.mode = []*Session{s}, confirmMove
		default:
			m.then = func() error { return Open(s) }
			return m, tea.Quit
		}
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
	switch k.String() {
	case "esc":
		m.mode = browse
		m.input.Blur()
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
			dir, err := NewProject(v)
			if err != nil {
				m.fail(err)
				return m, nil
			}
			// On the phone already; open a first session here too.
			m.then = func() error { return Start(dir) }
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

func (m *model) listWidth() int  { return max(40, m.w*2/5) }
func (m *model) listHeight() int { return max(1, m.h-3) }

func (m *model) layout() {
	m.peek.Width = max(20, m.w-m.listWidth()-4)
	m.peek.Height = max(1, m.h-3)
}

func (m *model) View() string {
	if m.w == 0 {
		return ""
	}
	list := m.viewList()
	pane := sBorder
	if m.peekFocus {
		pane = sFocus
	}
	// Clamp before boxing: lipgloss wraps a line wider than its box and treats
	// Height as a minimum, so one long peek line would push the footer, where
	// every prompt and confirm lives, off the screen.
	body := lipgloss.JoinHorizontal(lipgloss.Top,
		sBorder.Render(clamp(list, m.listWidth(), m.listHeight())),
		pane.Render(clamp(m.peek.View(), m.peek.Width, m.peek.Height)))
	return lipgloss.JoinVertical(lipgloss.Left, body, clamp(m.viewFooter(), m.w, 1))
}

func (m *model) viewList() string {
	h := m.listHeight()
	if m.cursor < m.top {
		m.top = m.cursor
	}
	if m.cursor >= m.top+h {
		m.top = m.cursor - h + 1
	}
	width := m.listWidth()
	repoW := 16
	titleW := max(8, width-repoW-12)
	var b strings.Builder
	for i := m.top; i < min(len(m.rows), m.top+h); i++ {
		s := m.rows[i]
		mark := " "
		if m.marked[s.UUID] {
			mark = sWarn.Render("✗")
		}
		state := marker(s.State)
		if s.State != Dead {
			state = sLive.Render(state)
		}
		line := fmt.Sprintf("%s%s %-4s %-*s %s", mark, state, age(s.Mtime),
			repoW, oneLine(s.Repo(), repoW), oneLine(s.Title, titleW))
		if i == m.cursor {
			line = sSelRow.Render(clamp(line, width, 1))
		}
		b.WriteString(line)
		if i < min(len(m.rows), m.top+h)-1 {
			b.WriteByte('\n')
		}
	}
	if len(m.rows) == 0 {
		b.WriteString(sDim.Render("no sessions match"))
	}
	return b.String()
}

func (m *model) viewFooter() string {
	switch m.mode {
	case filtering, renaming, naming:
		return " " + sHeading.Render(m.label+": ") + m.input.View() + sDim.Render("   enter ok · esc cancel")
	case confirmMove:
		s := m.pending[0]
		return " " + sWarn.Render(fmt.Sprintf("%q runs outside tmux (pid %d). Stop it and continue it in tmux? y/N", oneLine(s.Title, 40), s.PID))
	case confirmDelete:
		return " " + sWarn.Render(m.summary)
	}
	if m.note != "" {
		if m.isErr {
			return " " + sWarn.Render(m.note)
		}
		return " " + sLive.Render(m.note)
	}
	help := "↑↓ move · enter open · tab peek · / filter · space mark · d delete · R rename · n new project · a scratch · q quit"
	if m.peekFocus {
		help = "↑↓ pgup pgdn scroll · tab back · q quit"
	}
	info := fmt.Sprintf(" %d sessions", len(m.rows))
	if len(m.marked) > 0 {
		info += fmt.Sprintf(", %d marked", len(m.marked))
	}
	if m.query != "" {
		info += fmt.Sprintf(", filter %q", m.query)
	}
	return sDim.Render(info + " · " + help)
}
