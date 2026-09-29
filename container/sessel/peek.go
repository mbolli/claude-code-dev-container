package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// A turn is one line of the peek: something you typed, something Claude said,
// or a tool call it made.
type Turn struct {
	Who  string // "you", "claude", "tool"
	Text string
}

const (
	peekTurns = 14
	peekTail  = 4 << 20
)

// seekPastNewline positions f just after the first newline at or after off.
func seekPastNewline(f *os.File, off int64) error {
	buf := make([]byte, 64<<10)
	for {
		n, err := f.ReadAt(buf, off)
		if i := bytes.IndexByte(buf[:n], '\n'); i >= 0 {
			_, serr := f.Seek(off+int64(i)+1, io.SeekStart)
			return serr
		}
		if err != nil {
			return err
		}
		off += int64(n)
	}
}

func cmdPeek(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: sessel peek <id>")
	}
	s, err := mustLoad().Resolve(args[0])
	if err != nil {
		return err
	}
	turns, err := lastTurns(s.File, peekTurns)
	if err != nil {
		return err
	}
	fmt.Print(renderPeek(s, turns, 100, plainStyle{}))
	return nil
}

// lastTurns keeps a rolling window of the last n turns. Oversized entries are
// tool results, which the peek shows only as the call that produced them.
func lastTurns(path string, n int) ([]Turn, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// Only the tail: the last turns are there, the opening prompt comes from
	// the cache, and a live session's peek is re-read every couple of seconds.
	// Start after the first newline, not mid-line.
	if fi, err := f.Stat(); err == nil && fi.Size() > peekTail {
		if err := seekPastNewline(f, fi.Size()-peekTail); err != nil {
			return nil, err
		}
	}
	var ring []Turn
	push := func(t Turn) {
		ring = append(ring, t)
		if len(ring) > n {
			ring = ring[len(ring)-n:]
		}
	}
	r := bufio.NewReaderSize(f, chunk)
	for {
		line, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = r.ReadSlice('\n')
			}
			continue
		}
		if len(line) > 0 {
			for _, t := range turnsOf(line) {
				push(t)
			}
		}
		if err != nil {
			return ring, nil
		}
	}
}

func turnsOf(line []byte) []Turn {
	var v struct {
		Type    string
		IsMeta  bool `json:"isMeta"`
		Message struct {
			Content json.RawMessage
		}
	}
	if json.Unmarshal(line, &v) != nil || v.IsMeta {
		return nil
	}
	switch v.Type {
	case "user":
		if t := userText(line); t != "" {
			return []Turn{{"you", t}}
		}
	case "assistant":
		var blocks []struct {
			Type  string
			Text  string
			Name  string
			Input map[string]any
		}
		if json.Unmarshal(v.Message.Content, &blocks) != nil {
			return nil
		}
		var out []Turn
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if t := strings.TrimSpace(b.Text); t != "" {
					out = append(out, Turn{"claude", t})
				}
			case "tool_use":
				out = append(out, Turn{"tool", toolSummary(b.Name, b.Input)})
			}
		}
		return out
	}
	return nil
}

// toolSummary is the call on one line: the tool and its most telling argument.
func toolSummary(name string, in map[string]any) string {
	for _, k := range []string{"command", "file_path", "pattern", "url", "query", "description", "prompt"} {
		if s, ok := in[k].(string); ok && s != "" {
			return fmt.Sprintf("%s(%s)", name, oneLine(s, 90))
		}
	}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if s, ok := in[k].(string); ok && s != "" {
			return fmt.Sprintf("%s(%s)", name, oneLine(s, 90))
		}
	}
	return name
}

// style lets the CLI print plain text and the TUI colour the same content.
type style interface {
	Heading(string) string
	Label(string) string
	Dim(string) string
	Who(string) string
}

type plainStyle struct{}

func (plainStyle) Heading(s string) string { return s }
func (plainStyle) Label(s string) string   { return s }
func (plainStyle) Dim(s string) string     { return s }
func (plainStyle) Who(s string) string     { return s }

func renderPeek(s *Session, turns []Turn, width int, st style) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }

	w("%s\n", st.Heading(s.Title))
	w("%s %s   %s %s   %s %s ago   %s %s\n",
		st.Label("repo"), s.Repo(), st.Label("branch"), orDash(s.Branch),
		st.Label("age"), age(s.Mtime), st.Label("state"), stateWords(s))
	w("%s %s\n", st.Label("uuid"), s.UUID)
	if rc := s.Bridge(); rc != "" {
		more := ""
		if len(s.Bridges) > 1 {
			more = st.Dim(fmt.Sprintf("  (+%d older)", len(s.Bridges)-1))
		}
		w("%s %s%s\n", st.Label("phone"), rc, more)
	}
	for _, pr := range s.PRs {
		w("%s #%d %s\n", st.Label("PR"), pr.Number, pr.URL)
	}

	w("\n%s\n", st.Label("── opening prompt ──"))
	if s.FirstPrompt != "" {
		w("%s\n", wrap(s.FirstPrompt, width, 12))
	} else {
		w("%s\n", st.Dim("(none recorded)"))
	}

	w("\n%s\n", st.Label("── where it stopped ──"))
	if len(turns) == 0 {
		w("%s\n", st.Dim("(nothing yet)"))
	}
	for _, t := range turns {
		switch t.Who {
		case "tool":
			w("%s\n", st.Dim("  ⏺ "+oneLine(t.Text, width-4)))
		default:
			// Indent continuation lines past "claude: " so they don't read as a
			// new speaker.
			w("%s %s\n", st.Who(t.Who+":"), strings.ReplaceAll(wrap(t.Text, width-8, 6), "\n", "\n        "))
		}
	}
	return b.String()
}

func stateWords(s *Session) string {
	switch s.State {
	case LiveTmux:
		return "running in tmux " + s.Tmux
	case LiveOutside:
		return fmt.Sprintf("running outside tmux (pid %d)", s.PID)
	}
	return "not running"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// wrap folds text to width and keeps at most maxLines, so one long reply
// cannot push the rest of the peek out of view.
func wrap(s string, width, maxLines int) string {
	if width < 20 {
		width = 20
	}
	var lines []string
	for _, para := range strings.Split(strings.TrimSpace(s), "\n") {
		words := strings.Fields(para)
		if len(words) == 0 {
			continue
		}
		cur := ""
		for _, word := range words {
			if cur != "" && len([]rune(cur))+1+len([]rune(word)) > width {
				lines = append(lines, cur)
				cur = word
			} else if cur == "" {
				cur = word
			} else {
				cur += " " + word
			}
		}
		lines = append(lines, cur)
	}
	if len(lines) > maxLines {
		lines = append(lines[:maxLines], "…")
	}
	return strings.Join(lines, "\n")
}

// renderStats is the other face of the right pane: numbers instead of text.
// arts is nil while the footprint is still being measured.
func renderStats(s *Session, arts []Artifact, width int, st style) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	row := func(k, v string) { w("%s %s\n", st.Label(fmt.Sprintf("%-17s", k)), v) }
	x := s.Stats

	w("%s\n", st.Heading(s.Title))
	row("repo", s.Repo()+"   "+st.Label("branch")+" "+orDash(s.Branch))
	row("state", stateWords(s))
	row("uuid", s.UUID)
	for i, br := range s.Bridges {
		if i == 0 {
			row("phone", br)
		} else {
			row("", st.Dim(br+"  (older)"))
		}
	}

	w("\n%s\n", st.Label("── size ──"))
	if fi, err := os.Stat(s.File); err == nil {
		row("transcript", fmt.Sprintf("%s   %d entries", humanBytes(fi.Size()), s.Lines))
	}
	if arts == nil {
		row("on disk in total", st.Dim("measuring…"))
	} else {
		files, bytes := totals(arts)
		row("on disk in total", fmt.Sprintf("%s   %d files in %d places", humanBytes(bytes), files, len(arts)))
	}

	w("\n%s\n", st.Label("── length ──"))
	row("your prompts", fmt.Sprint(x.Prompts))
	row("Claude's turns", fmt.Sprint(x.Turns))
	row("tool calls", fmt.Sprint(x.ToolCalls))
	first, firstOK := parseTS(x.FirstAt)
	last, lastOK := parseTS(x.LastAt)
	if firstOK {
		row("started", first.Local().Format("2006-01-02 15:04")+st.Dim("   "+age(first)+" ago"))
	}
	if lastOK {
		row("last activity", last.Local().Format("2006-01-02 15:04")+st.Dim("   "+age(last)+" ago"))
	}
	if firstOK && lastOK {
		row("span", span(last.Sub(first)))
	}

	w("\n%s\n", st.Label("── tokens ──"))
	row("input", humanCount(x.InTokens))
	row("output", humanCount(x.OutTokens))
	row("cache read", humanCount(x.CacheRead))
	row("cache write", humanCount(x.CacheWrite))
	if len(x.Models) > 0 {
		row("models", strings.Join(x.Models, ", "))
	}

	if len(s.PRs) > 0 {
		w("\n%s\n", st.Label("── pull requests ──"))
		for _, pr := range s.PRs {
			w("#%d %s\n", pr.Number, pr.URL)
		}
	}
	return b.String()
}

func parseTS(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil
}

func span(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd %dh", int(d.Hours()/24), int(d.Hours())%24)
}

func humanCount(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.1fB", float64(n)/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}
