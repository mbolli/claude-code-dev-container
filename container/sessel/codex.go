// Codex keeps thread metadata in ~/.codex/state_5.sqlite (table threads) and
// writes turn-by-turn transcripts to ~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl.
// While a thread is active, either the client or the shared app-server daemon
// holds ~/.codex/thread-writer-locks/<uuid>.lock open.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// codexBin is the Codex CLI executable name, overridden in tests to exercise
// both CLI delegation and the missing-binary fallback.
var codexBin = "codex"

func codexHome() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex")
}

type codexHomeSpec struct {
	Home    string
	Profile string
}

// codexHomes returns ~/.codex plus any ~/.clauth/profiles/<name>/codex-home
// directories created by `clauth start <codex-profile>`.
func codexHomes() []codexHomeSpec {
	primary := codexHome()
	out := []codexHomeSpec{{Home: primary}}
	dirs, _ := filepath.Glob(filepath.Join(clauthHome(), "profiles", "*", "codex-home"))
	for _, d := range dirs {
		if filepath.Clean(d) == filepath.Clean(primary) {
			continue
		}
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			out = append(out, codexHomeSpec{
				Home:    d,
				Profile: filepath.Base(filepath.Dir(d)),
			})
		}
	}
	return out
}

func codexHomeFor(s *Session) string {
	if s != nil {
		for _, h := range codexHomes() {
			if s.File != "" && within(s.File, h.Home) {
				return h.Home
			}
		}
		if s.Profile != "" {
			dir := filepath.Join(clauthHome(), "profiles", s.Profile, "codex-home")
			if fileExists(codexStateDB(dir)) {
				return dir
			}
		}
	}
	return codexHome()
}

// codexStateDB locates state_5.sqlite, falling back to the highest-numbered
// state_*.sqlite if a newer schema migration bumped the filename.
func codexStateDB(home string) string {
	p := filepath.Join(home, "state_5.sqlite")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	matches, _ := filepath.Glob(filepath.Join(home, "state_*.sqlite"))
	if len(matches) == 0 {
		return p
	}
	sort.Strings(matches)
	return matches[len(matches)-1]
}

type codexThreadRow struct {
	ID               string `json:"id"`
	RolloutPath      string `json:"rollout_path"`
	CreatedAt        int64  `json:"created_at"`
	UpdatedAt        int64  `json:"updated_at"`
	Cwd              string `json:"cwd"`
	Title            string `json:"title"`
	Name             string `json:"name"`
	FirstUserMessage string `json:"first_user_message"`
	Model            string `json:"model"`
	TokensUsed       int64  `json:"tokens_used"`
	GitBranch        string `json:"git_branch"`
}

// loadCodexSessions reads non-archived top-level threads from state_5.sqlite
// across ~/.codex and per-profile clauth codex-homes, merging them with cached
// rollout transcript stats.
func loadCodexSessions(cache, next map[string]cacheEntry, mu *sync.Mutex) []*Session {
	const q = `SELECT
		id,
		COALESCE(rollout_path, '') AS rollout_path,
		created_at,
		updated_at,
		COALESCE(cwd, '') AS cwd,
		COALESCE(title, '') AS title,
		COALESCE(name, '') AS name,
		COALESCE(first_user_message, '') AS first_user_message,
		COALESCE(model, '') AS model,
		COALESCE(tokens_used, 0) AS tokens_used,
		COALESCE(git_branch, '') AS git_branch
	FROM threads
	WHERE archived = 0
	  AND COALESCE(attached_thread_parent_id, '') = '';`

	var rows []codexThreadRow
	var rowProfiles []string
	seen := map[string]bool{}
	for _, h := range codexHomes() {
		db := codexStateDB(h.Home)
		if !fileExists(db) {
			continue
		}
		rs, err := sqliteQueryJSON[codexThreadRow](db, q)
		if err != nil {
			continue
		}
		for _, r := range rs {
			if seen[r.ID] {
				continue
			}
			seen[r.ID] = true
			rows = append(rows, r)
			rowProfiles = append(rowProfiles, h.Profile)
		}
	}
	if len(rows) == 0 {
		return nil
	}

	paths := make([]string, len(rows))
	for i, r := range rows {
		paths[i] = r.RolloutPath
	}
	metas, stats := scanPaths(paths, parseCodexLine, cache, next, mu)

	var out []*Session
	for i, r := range rows {
		if !uuidRe.MatchString(r.ID) {
			continue
		}
		m := metas[i]
		fi := stats[i]
		cwd := r.Cwd
		if cwd == "" {
			cwd = m.Cwd
		}
		branch := r.GitBranch
		if branch == "" {
			branch = m.Branch
		}
		if branch == "" {
			branch = gitBranchOf(cwd)
		}
		firstPrompt := m.FirstPrompt
		if firstPrompt == "" {
			firstPrompt = strings.TrimSpace(r.FirstUserMessage)
		}
		mtime := time.Unix(r.UpdatedAt, 0)
		if fi != nil && fi.ModTime().After(mtime) {
			mtime = fi.ModTime()
		}
		st := m.Stats
		if r.Model != "" {
			st.Models = appendUnique(st.Models, r.Model)
		}
		if st.InTokens == 0 && st.OutTokens == 0 && r.TokensUsed > 0 {
			st.InTokens = r.TokensUsed
		}
		if st.FirstAt == "" && r.CreatedAt > 0 {
			st.FirstAt = time.Unix(r.CreatedAt, 0).UTC().Format(time.RFC3339)
		}
		if st.LastAt == "" && r.UpdatedAt > 0 {
			st.LastAt = time.Unix(r.UpdatedAt, 0).UTC().Format(time.RFC3339)
		}

		s := &Session{
			UUID:        r.ID,
			Agent:       AgentCodex,
			Profile:     rowProfiles[i],
			File:        r.RolloutPath,
			Cwd:         cwd,
			Branch:      branch,
			FirstPrompt: firstPrompt,
			Mtime:       mtime,
			Lines:       m.Lines,
			Stats:       st,
		}
		s.Title, s.TitleSource = codexTitle(r, firstPrompt)
		s.StateName = s.State.String()
		out = append(out, s)
	}
	return out
}

// codexTitle prefers a user-assigned thread name over the generated title.
func codexTitle(r codexThreadRow, firstPrompt string) (string, string) {
	if name := strings.TrimSpace(r.Name); name != "" {
		return name, "custom"
	}
	if t := strings.TrimSpace(r.Title); t != "" {
		return oneLine(t, 80), "ai"
	}
	if firstPrompt != "" {
		return oneLine(firstPrompt, 80), "prompt"
	}
	return "(untitled)", "none"
}

type codexClientProc struct {
	pid       int
	procStart string
	args      []string
	open      map[string]bool
	tmux      string
}

// attachCodexLive matches running codex TUI clients to threads when confirmed
// by an open lock or rollout file. A thread whose writer lock is held by the
// app-server daemon with no confirmed client is marked Busy and LiveOutside
// with PID 0 so Stop, Open and Delete refuse to touch it.
func attachCodexLive(sessions []*Session, procs []agentProc) {
	if len(sessions) == 0 {
		return
	}
	homes := codexHomes()
	byUUID := make(map[string]*Session, len(sessions))
	byPrefix := make(map[string]*Session, len(sessions))
	byRollout := make(map[string]string, len(sessions))
	for _, s := range sessions {
		byUUID[s.UUID] = s
		if len(s.UUID) >= 8 {
			byPrefix[s.UUID[:8]] = s
		}
		if s.File != "" {
			byRollout[filepath.Clean(s.File)] = s.UUID
		}
	}

	locked := map[string]bool{}
	var clients []codexClientProc

	for _, p := range procs {
		if p.comm != "codex" {
			continue
		}
		open := map[string]bool{}
		for _, target := range p.fds {
			id, isLock := codexUUIDFromFD(target, homes, byRollout)
			if id == "" {
				continue
			}
			open[id] = true
			if isLock {
				locked[id] = true
			}
		}
		if len(p.args) == 0 || isCodexSubcommand(p.args[1:]) {
			continue
		}
		clients = append(clients, codexClientProc{
			pid:       p.pid,
			procStart: p.procStart,
			args:      p.args,
			open:      open,
			tmux:      p.tmux,
		})
	}

	usedPID := map[int]bool{}
	assign := func(s *Session, c codexClientProc) {
		usedPID[c.pid] = true
		s.PID = c.pid
		s.procStart = c.procStart
		if c.tmux != "" {
			s.State, s.Tmux = LiveTmux, c.tmux
		} else {
			s.State = LiveOutside
		}
		s.StateName = s.State.String()
	}
	// confirmed reports whether a client's command-line or tmux UUID is still
	// backed by an open lock or rollout file (and the client has not switched
	// to a different thread's file).
	confirmed := func(c codexClientProc, uuid string) bool {
		if c.open[uuid] {
			return true
		}
		return len(c.open) == 0 && locked[uuid]
	}

	// Pass 0: client directly holds a single thread's lock or rollout file.
	for _, c := range clients {
		if len(c.open) == 1 {
			for uuid := range c.open {
				if s := byUUID[uuid]; s != nil && s.State == Dead {
					assign(s, c)
				}
			}
		}
	}

	// Pass 1: explicit UUID on cmdline (`codex resume <uuid>`), confirmed by lock or rollout.
	for _, c := range clients {
		if usedPID[c.pid] {
			continue
		}
		for _, a := range c.args[1:] {
			if s := byUUID[a]; s != nil && s.State == Dead && confirmed(c, s.UUID) {
				assign(s, c)
				break
			}
		}
	}

	// Pass 2: tmux session named `<repo>@codex-<uuid8>` or `<repo>-codex-<uuid8>`, confirmed by lock or rollout.
	for _, c := range clients {
		if usedPID[c.pid] || c.tmux == "" {
			continue
		}
		sessName, _, _ := strings.Cut(c.tmux, ":")
		for _, sep := range []string{"@codex-", "-codex-"} {
			if idx := strings.LastIndex(sessName, sep); idx >= 0 {
				prefix := sessName[idx+len(sep):]
				if s := byPrefix[prefix]; s != nil && s.State == Dead && confirmed(c, s.UUID) {
					assign(s, c)
					break
				}
			}
		}
	}

	for _, s := range sessions {
		s.Locked = locked[s.UUID]
		if s.Locked && s.State == Dead {
			s.Busy = true
			s.State = LiveOutside
			s.StateName = s.State.String()
		}
	}
}

// codexUUIDFromFD extracts a thread UUID from an open lock or rollout path.
func codexUUIDFromFD(target string, homes []codexHomeSpec, byRollout map[string]string) (uuid string, isLock bool) {
	inHome := false
	for _, h := range homes {
		if within(target, h.Home) {
			inHome = true
			break
		}
	}
	if !inHome {
		return "", false
	}
	if strings.Contains(target, "/thread-writer-locks/") && strings.HasSuffix(target, ".lock") {
		id := strings.TrimSuffix(filepath.Base(target), ".lock")
		if uuidRe.MatchString(id) {
			return id, true
		}
	}
	if id := byRollout[filepath.Clean(target)]; id != "" {
		return id, false
	}
	return "", false
}

// codexLockHeld reports whether any running codex process currently holds the
// writer lock for uuid.
func codexLockHeld(uuid string) bool {
	if !uuidRe.MatchString(uuid) {
		return false
	}
	wants := map[string]bool{}
	for _, h := range codexHomes() {
		wants[filepath.Clean(filepath.Join(h.Home, "thread-writer-locks", uuid+".lock"))] = true
	}
	for _, p := range scanAgentProcs(tmuxPanes{}) {
		if p.comm != "codex" {
			continue
		}
		for _, fd := range p.fds {
			if wants[filepath.Clean(fd)] {
				return true
			}
		}
	}
	return false
}

// isCodexSubcommand checks the first non-flag argument of a codex command line
// so daemons and one-shot commands are skipped while interactive clients (no
// subcommand, `resume`, `fork`) are kept.
func isCodexSubcommand(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return false
		}
		if strings.HasPrefix(a, "-") {
			if !strings.Contains(a, "=") && codexFlagTakesValue(a) {
				i++
			}
			continue
		}
		switch a {
		case "agents", "exec", "e", "review", "login", "logout", "mcp", "mcp-server",
			"plugin", "app-server", "remote-control", "completion", "update", "doctor",
			"sandbox", "debug", "apply", "a", "queue", "archive", "unarchive", "delete",
			"migrate-rollouts", "migrate-nfs-db", "cloud", "exec-server", "features", "help":
			return true
		default:
			return false
		}
	}
	return false
}

func codexFlagTakesValue(flag string) bool {
	switch flag {
	case "-c", "--config", "--enable", "--disable", "--remote", "--remote-auth-token-env",
		"-i", "--image", "-m", "--model", "--local-provider", "-p", "--profile",
		"-s", "--sandbox", "-C", "--cd", "--add-dir", "-a", "--ask-for-approval":
		return true
	}
	return false
}

var (
	kCodexMeta  = []byte(`"type":"session_meta"`)
	kCodexTurn  = []byte(`"type":"turn_context"`)
	kCodexItem  = []byte(`"type":"response_item"`)
	kCodexUsage = []byte(`"type":"token_usage_record"`)
	kCodexEvent = []byte(`"type":"event_msg"`)
)

type codexTokenUsage struct {
	InputTokens       int64 `json:"input_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens"`
	CacheWriteTokens  int64 `json:"cache_write_input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
}

func applyCodexUsage(st *Stats, u codexTokenUsage, ts string) {
	if u.InputTokens > 0 || u.OutputTokens > 0 {
		st.InTokens = u.InputTokens
		st.OutTokens = u.OutputTokens
		st.CacheRead = u.CachedInputTokens
		st.CacheWrite = u.CacheWriteTokens
	}
	touchTS(st, ts)
}

// parseCodexLine extracts cwd, models, token totals and message counts from
// one line of a Codex rollout JSONL file.
func parseCodexLine(line []byte, m *meta, whole bool) {
	if m.Cwd == "" {
		if v := field(line, kCwdKey); v != "" {
			m.Cwd = v
		}
	}
	if !whole {
		return
	}
	switch {
	case bytes.Contains(line, kCodexMeta):
		var v struct {
			Timestamp string `json:"timestamp"`
			Payload   struct {
				Cwd              string `json:"cwd"`
				BaseInstructions struct {
					Provenance struct {
						Model string `json:"model"`
					} `json:"provenance"`
				} `json:"base_instructions"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &v) == nil {
			if m.Cwd == "" && v.Payload.Cwd != "" {
				m.Cwd = v.Payload.Cwd
			}
			if mdl := v.Payload.BaseInstructions.Provenance.Model; mdl != "" {
				m.Stats.Models = appendUnique(m.Stats.Models, mdl)
			}
			touchTS(&m.Stats, v.Timestamp)
		}
	case bytes.Contains(line, kCodexTurn):
		var v struct {
			Timestamp string `json:"timestamp"`
			Payload   struct {
				Cwd   string `json:"cwd"`
				Model string `json:"model"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &v) == nil {
			if m.Cwd == "" && v.Payload.Cwd != "" {
				m.Cwd = v.Payload.Cwd
			}
			if v.Payload.Model != "" {
				m.Stats.Models = appendUnique(m.Stats.Models, v.Payload.Model)
			}
			touchTS(&m.Stats, v.Timestamp)
		}
	case bytes.Contains(line, kCodexUsage):
		var v struct {
			Timestamp string `json:"timestamp"`
			Payload   struct {
				ThreadTokenUsage codexTokenUsage `json:"thread_token_usage"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &v) == nil {
			applyCodexUsage(&m.Stats, v.Payload.ThreadTokenUsage, v.Timestamp)
		}
	case bytes.Contains(line, kCodexEvent):
		if !bytes.Contains(line, []byte(`"token_count"`)) {
			return
		}
		var v struct {
			Timestamp string `json:"timestamp"`
			Payload   struct {
				Type string `json:"type"`
				Info struct {
					TotalTokenUsage codexTokenUsage `json:"total_token_usage"`
				} `json:"info"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &v) == nil && v.Payload.Type == "token_count" {
			applyCodexUsage(&m.Stats, v.Payload.Info.TotalTokenUsage, v.Timestamp)
		}
	case bytes.Contains(line, kCodexItem):
		countCodexItem(line, m)
	}
}

type codexResponseItem struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Payload   struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		Role      string          `json:"role"`
		Name      string          `json:"name"`
		Arguments string          `json:"arguments"`
		Input     string          `json:"input"`
		Content   json.RawMessage `json:"content"`
		Action    struct {
			Command []string `json:"command"`
		} `json:"action"`
	} `json:"payload"`
}

func countCodexItem(line []byte, m *meta) {
	var v codexResponseItem
	if json.Unmarshal(line, &v) != nil || v.Type != "response_item" {
		return
	}
	st := &m.Stats
	switch v.Payload.Type {
	case "message":
		switch v.Payload.Role {
		case "user":
			txt := codexMessageText(v.Payload.Content, true)
			if txt == "" {
				return
			}
			st.Prompts++
			if m.FirstPrompt == "" {
				m.FirstPrompt = txt
			}
			touchTS(st, v.Timestamp)
		case "assistant":
			txt := codexMessageText(v.Payload.Content, false)
			if txt == "" {
				return
			}
			if v.Payload.ID != "" && v.Payload.ID == st.LastMsgID {
				return
			}
			if v.Payload.ID != "" {
				st.LastMsgID = v.Payload.ID
			}
			st.Turns++
			touchTS(st, v.Timestamp)
		}
	case "function_call", "custom_tool_call", "local_shell_call", "web_search_call":
		st.ToolCalls++
		touchTS(st, v.Timestamp)
	}
}

// codexTurnsOf converts one rollout response_item line into peek turns.
func codexTurnsOf(line []byte) []Turn {
	if !bytes.Contains(line, kCodexItem) {
		return nil
	}
	var v codexResponseItem
	if json.Unmarshal(line, &v) != nil || v.Type != "response_item" {
		return nil
	}
	switch v.Payload.Type {
	case "message":
		switch v.Payload.Role {
		case "user":
			if t := codexMessageText(v.Payload.Content, true); t != "" {
				return []Turn{{"you", t}}
			}
		case "assistant":
			if t := codexMessageText(v.Payload.Content, false); t != "" {
				return []Turn{{"codex", t}}
			}
		}
	case "function_call", "custom_tool_call", "local_shell_call", "web_search_call":
		return []Turn{{"tool", codexToolSummary(v)}}
	}
	return nil
}

// codexMessageText extracts user or assistant text blocks from a response_item,
// skipping harness-injected instructions on user messages.
func codexMessageText(raw json.RawMessage, user bool) string {
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	wantType := "output_text"
	if user {
		wantType = "input_text"
	}
	var out []string
	for _, p := range parts {
		if p.Type != wantType && p.Type != "text" {
			continue
		}
		t := strings.TrimSpace(p.Text)
		if t == "" || (user && isCodexInjectedBlock(t)) {
			continue
		}
		out = append(out, t)
	}
	return strings.Join(out, "\n")
}

func isCodexInjectedBlock(t string) bool {
	return strings.HasPrefix(t, "# AGENTS.md instructions") ||
		strings.HasPrefix(t, "<environment_context>") ||
		strings.HasPrefix(t, "<permissions") ||
		strings.HasPrefix(t, "<collaboration_mode>") ||
		strings.HasPrefix(t, "<subagent_notification>") ||
		strings.HasPrefix(t, "<INSTRUCTIONS>")
}

func codexToolSummary(v codexResponseItem) string {
	name := v.Payload.Name
	if name == "" {
		name = v.Payload.Type
	}
	if len(v.Payload.Action.Command) > 0 {
		return fmt.Sprintf("%s(%s)", name, oneLine(strings.Join(v.Payload.Action.Command, " "), 90))
	}
	if v.Payload.Arguments != "" {
		var in map[string]any
		if json.Unmarshal([]byte(v.Payload.Arguments), &in) == nil {
			return toolSummary(name, in)
		}
		return fmt.Sprintf("%s(%s)", name, oneLine(v.Payload.Arguments, 90))
	}
	if v.Payload.Input != "" {
		return fmt.Sprintf("%s(%s)", name, oneLine(v.Payload.Input, 90))
	}
	return name
}

func touchTS(st *Stats, ts string) {
	if ts == "" {
		return
	}
	if st.FirstAt == "" {
		st.FirstAt = ts
	}
	st.LastAt = ts
}

// codexFootprint returns the files and SQLite rows a Codex delete removes.
// Non-UUID session IDs are rejected so Join(home, dir, "") can never resolve
// to a parent directory.
func codexFootprint(s *Session) []Artifact {
	if !uuidRe.MatchString(s.UUID) {
		return nil
	}
	home := codexHomeFor(s)
	candidates := []string{
		s.File,
		filepath.Join(home, "tui-thread-reference-capabilities", s.UUID),
		filepath.Join(home, "thread-writer-locks", s.UUID+".lock"),
	}
	snaps, _ := filepath.Glob(filepath.Join(home, "shell_snapshots", s.UUID+".*"))
	candidates = append(candidates, snaps...)

	var out []Artifact
	for _, p := range candidates {
		if p == "" || !within(p, home) {
			continue
		}
		if a, ok := measure(p); ok {
			out = append(out, a)
		}
	}
	if db := codexStateDB(home); fileExists(db) {
		out = append(out, Artifact{Path: db + " (threads row)"})
	}
	if histDB := filepath.Join(home, "thread_history_1.sqlite"); fileExists(histDB) {
		out = append(out, Artifact{Path: histDB + " (thread_turns rows)"})
	}
	if logsDB := filepath.Join(home, "logs_2.sqlite"); fileExists(logsDB) {
		out = append(out, Artifact{Path: logsDB + " (logs rows)"})
	}
	return out
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// deleteCodex delegates to `codex delete --force <id>` when the codex binary
// is available, and falls back to removing the footprint files and SQLite rows
// directly only when the binary is not installed.
func deleteCodex(s *Session) error {
	if !uuidRe.MatchString(s.UUID) {
		return fmt.Errorf("invalid codex session id: %q", s.UUID)
	}
	home := codexHomeFor(s)
	if path, err := exec.LookPath(codexBin); err == nil {
		cmd := exec.Command(path, "delete", "--force", s.UUID)
		cmd.Env = append(os.Environ(), "CODEX_HOME="+home)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("codex delete %s: %s", s.UUID[:8], strings.TrimSpace(string(out)))
		}
		return nil
	}
	for _, a := range codexFootprint(s) {
		if a.Files == 0 {
			continue
		}
		if err := os.RemoveAll(a.Path); err != nil {
			return err
		}
	}
	q := sqlQuote(s.UUID)
	if db := codexStateDB(home); fileExists(db) {
		sql := fmt.Sprintf("PRAGMA foreign_keys = ON; DELETE FROM threads WHERE id = %s;", q)
		if err := sqliteExec(db, sql); err != nil {
			return err
		}
	}
	if histDB := filepath.Join(home, "thread_history_1.sqlite"); fileExists(histDB) {
		sql := fmt.Sprintf(
			"DELETE FROM thread_items WHERE thread_id = %s; "+
				"DELETE FROM thread_realtime_items WHERE thread_id = %s; "+
				"DELETE FROM thread_turns WHERE thread_id = %s; "+
				"DELETE FROM thread_history_projection_state WHERE thread_id = %s;",
			q, q, q, q,
		)
		_ = sqliteExec(histDB, sql)
	}
	if logsDB := filepath.Join(home, "logs_2.sqlite"); fileExists(logsDB) {
		_ = sqliteExec(logsDB, fmt.Sprintf("DELETE FROM logs WHERE thread_id = %s;", q))
	}
	return nil
}

// renameCodex sets the user-assigned name column on the thread in state_5.sqlite.
func renameCodex(s *Session, title string) error {
	db := codexStateDB(codexHomeFor(s))
	sql := fmt.Sprintf("UPDATE threads SET name = %s WHERE id = %s;", sqlQuote(title), sqlQuote(s.UUID))
	return sqliteExec(db, sql)
}
