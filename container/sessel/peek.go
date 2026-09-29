package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

// A turn is one line of the peek: something you typed, something Claude said,
// or a tool call it made.
type Turn struct {
	Who  string // "you", "claude", "tool"
	Text string
}

const peekTurns = 14

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
