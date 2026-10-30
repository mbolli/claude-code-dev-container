// Antigravity (agy) indexes conversations in ~/.gemini/antigravity-cli/conversation_summaries.db
// and writes compact JSONL transcripts under brain/<uuid>/.system_generated/logs/transcript.jsonl.
// A running session holds presence/<uuid>.lock and conversations/<uuid>.db open.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func agyHome() string {
	if h := os.Getenv("AGY_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".gemini", "antigravity-cli")
}

type agySummaryRow struct {
	ConversationID   string `json:"conversation_id"`
	Title            string `json:"title"`
	Preview          string `json:"preview"`
	StepCount        int    `json:"step_count"`
	LastModifiedTime string `json:"last_modified_time"`
	WorkspaceURIs    string `json:"workspace_uris"`
	Status           string `json:"status"`
	NotFullyIdle     int    `json:"not_fully_idle"`
}

// loadAgySessions reads top-level conversations from conversation_summaries.db
// and scans their compact transcript.jsonl files for token and turn counts.
func loadAgySessions(cache, next map[string]cacheEntry, mu *sync.Mutex) ([]*Session, map[string]bool) {
	home := agyHome()
	db := filepath.Join(home, "conversation_summaries.db")
	const q = `SELECT
		conversation_id,
		COALESCE(title, '') AS title,
		COALESCE(preview, '') AS preview,
		COALESCE(step_count, 0) AS step_count,
		COALESCE(last_modified_time, '') AS last_modified_time,
		COALESCE(workspace_uris, '[]') AS workspace_uris,
		COALESCE(status, '') AS status,
		COALESCE(not_fully_idle, 0) AS not_fully_idle
	FROM conversation_summaries
	WHERE COALESCE(nesting_depth, 0) = 0
	  AND COALESCE(parent_conversation_id, '') = '';`

	rows, err := sqliteQueryJSON[agySummaryRow](db, q)
	if err != nil || len(rows) == 0 {
		return nil, nil
	}

	paths := make([]string, len(rows))
	for i, r := range rows {
		paths[i] = filepath.Join(home, "brain", r.ConversationID, ".system_generated", "logs", "transcript.jsonl")
	}
	metas, stats := scanPaths(paths, parseAgyLine, cache, next, mu)

	var out []*Session
	busyByUUID := map[string]bool{}
	for i, r := range rows {
		if !uuidRe.MatchString(r.ConversationID) {
			continue
		}
		m := metas[i]
		fi := stats[i]
		file := paths[i]
		if fi == nil {
			convDB := filepath.Join(home, "conversations", r.ConversationID+".db")
			if dbFi, err := os.Stat(convDB); err == nil {
				file = convDB
				fi = dbFi
			}
		}

		cwd := parseWorkspaceURI(r.WorkspaceURIs)
		if cwd == "" {
			cwd = m.Cwd
		}
		branch := gitBranchOf(cwd)
		firstPrompt := m.FirstPrompt
		if firstPrompt == "" {
			firstPrompt = strings.TrimSpace(r.Preview)
		}
		mtime := parseAgyTime(r.LastModifiedTime)
		if fi != nil && (mtime.IsZero() || fi.ModTime().After(mtime)) {
			mtime = fi.ModTime()
		}
		lines := m.Lines
		if lines == 0 && r.StepCount > 0 {
			lines = r.StepCount
		}

		s := &Session{
			UUID:        r.ConversationID,
			Agent:       AgentAgy,
			File:        file,
			Cwd:         cwd,
			Branch:      branch,
			FirstPrompt: firstPrompt,
			Mtime:       mtime,
			Lines:       lines,
			Stats:       m.Stats,
		}
		s.Title, s.TitleSource = agyTitle(r.Title, firstPrompt)
		s.StateName = s.State.String()
		busyByUUID[s.UUID] = r.NotFullyIdle != 0 || r.Status == "CASCADE_RUN_STATUS_RUNNING"
		out = append(out, s)
	}
	return out, busyByUUID
}

func agyTitle(title, firstPrompt string) (string, string) {
	if t := strings.TrimSpace(title); t != "" {
		return oneLine(t, 80), "ai"
	}
	if firstPrompt != "" {
		return oneLine(firstPrompt, 80), "prompt"
	}
	return "(untitled)", "none"
}

// parseWorkspaceURI decodes the first file:// URI from agy's JSON array column.
func parseWorkspaceURI(raw string) string {
	var uris []string
	if json.Unmarshal([]byte(raw), &uris) != nil || len(uris) == 0 {
		return ""
	}
	u, err := url.Parse(uris[0])
	if err != nil {
		return strings.TrimPrefix(uris[0], "file://")
	}
	if u.Path != "" {
		return u.Path
	}
	return ""
}

// parseAgyTime parses SQLite and RFC3339 timestamps stored in conversation_summaries.db.
func parseAgyTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05Z07:00",
		time.RFC3339Nano,
		time.RFC3339,
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// attachAgyLive matches running agy processes to sessions via open file
// descriptors, preferring the holder of presence/<uuid>.lock over
// conversations/<uuid>.db when multiple processes have the database open.
func attachAgyLive(sessions []*Session, busyByUUID map[string]bool, procs []agentProc) {
	if len(sessions) == 0 {
		return
	}
	home := agyHome()
	byUUID := make(map[string]*Session, len(sessions))
	for _, s := range sessions {
		byUUID[s.UUID] = s
	}
	hasLock := map[string]bool{}

	for _, p := range procs {
		if p.comm != "agy" {
			continue
		}
		for _, target := range p.fds {
			uuid, isLock := agyUUIDFromFD(target, home)
			if uuid == "" {
				continue
			}
			s := byUUID[uuid]
			if s == nil {
				continue
			}
			better := s.PID == 0 || (isLock && !hasLock[uuid]) || (isLock == hasLock[uuid] && p.pid < s.PID)
			if !better {
				continue
			}
			hasLock[uuid] = hasLock[uuid] || isLock
			s.PID = p.pid
			s.procStart = p.procStart
			s.Busy = busyByUUID[uuid]
			if p.tmux != "" {
				s.State, s.Tmux = LiveTmux, p.tmux
			} else {
				s.State, s.Tmux = LiveOutside, ""
			}
			s.StateName = s.State.String()
		}
	}
}

// agyUUIDFromFD returns the conversation UUID and whether the fd is the
// exclusive presence lock rather than the shared conversation SQLite file.
func agyUUIDFromFD(target, home string) (uuid string, isPresenceLock bool) {
	target = strings.TrimSuffix(target, " (deleted)")
	if !within(target, home) {
		return "", false
	}
	switch {
	case strings.Contains(target, "/presence/") && strings.HasSuffix(target, ".lock"):
		id := strings.TrimSuffix(filepath.Base(target), ".lock")
		if uuidRe.MatchString(id) {
			return id, true
		}
	case strings.Contains(target, "/conversations/") && strings.HasSuffix(target, ".db"):
		id := strings.TrimSuffix(filepath.Base(target), ".db")
		if uuidRe.MatchString(id) {
			return id, false
		}
	}
	return "", false
}

var (
	kAgyUser    = []byte(`"type":"USER_INPUT"`)
	kAgyPlanner = []byte(`"type":"PLANNER_RESPONSE"`)
)

type agyTranscriptStep struct {
	StepIndex       int     `json:"step_index"`
	Type            string  `json:"type"`
	CreatedAt       string  `json:"created_at"`
	Content         string  `json:"content"`
	InputTokens     int64   `json:"input_tokens"`
	CacheReadTokens int64   `json:"cache_read_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	ToolCalls       []agyTC `json:"tool_calls"`
}

type agyTC struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

// parseAgyLine counts user prompts, planner replies, tool calls and tokens
// from one line of an agy transcript.jsonl file.
func parseAgyLine(line []byte, m *meta, whole bool) {
	if !whole {
		return
	}
	if !bytes.Contains(line, kAgyUser) && !bytes.Contains(line, kAgyPlanner) {
		return
	}
	var v agyTranscriptStep
	if json.Unmarshal(line, &v) != nil {
		return
	}
	st := &m.Stats
	switch v.Type {
	case "USER_INPUT":
		if mdl := extractAgyModel(v.Content); mdl != "" {
			st.Models = appendUnique(st.Models, mdl)
		}
		txt := extractAgyUserRequest(v.Content)
		if txt == "" {
			return
		}
		st.Prompts++
		if m.FirstPrompt == "" {
			m.FirstPrompt = txt
		}
		touchTS(st, v.CreatedAt)
	case "PLANNER_RESPONSE":
		if strings.TrimSpace(v.Content) != "" || len(v.ToolCalls) > 0 {
			st.Turns++
		}
		st.ToolCalls += len(v.ToolCalls)
		st.InTokens += v.InputTokens
		st.OutTokens += v.OutputTokens
		st.CacheRead += v.CacheReadTokens
		touchTS(st, v.CreatedAt)
	}
}

// agyTurnsOf converts one transcript.jsonl step into peek turns, unquoting
// string-encoded tool call arguments.
func agyTurnsOf(line []byte) []Turn {
	if !bytes.Contains(line, kAgyUser) && !bytes.Contains(line, kAgyPlanner) {
		return nil
	}
	var v agyTranscriptStep
	if json.Unmarshal(line, &v) != nil {
		return nil
	}
	switch v.Type {
	case "USER_INPUT":
		if txt := extractAgyUserRequest(v.Content); txt != "" {
			return []Turn{{"you", txt}}
		}
	case "PLANNER_RESPONSE":
		var out []Turn
		if txt := strings.TrimSpace(v.Content); txt != "" {
			out = append(out, Turn{"agy", txt})
		}
		for _, tc := range v.ToolCalls {
			for k, val := range tc.Args {
				if s, ok := val.(string); ok && len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
					var unq string
					if json.Unmarshal([]byte(s), &unq) == nil {
						tc.Args[k] = unq
					}
				}
			}
			out = append(out, Turn{"tool", toolSummary(tc.Name, tc.Args)})
		}
		return out
	}
	return nil
}

// extractAgyUserRequest strips the <USER_REQUEST> wrapper and trailing
// <ADDITIONAL_METADATA> / <USER_SETTINGS_CHANGE> blocks injected by agy.
func extractAgyUserRequest(content string) string {
	const openTag = "<USER_REQUEST>"
	const closeTag = "</USER_REQUEST>"
	if i := strings.Index(content, openTag); i >= 0 {
		rest := content[i+len(openTag):]
		if j := strings.Index(rest, closeTag); j >= 0 {
			return strings.TrimSpace(rest[:j])
		}
		return strings.TrimSpace(rest)
	}
	return strings.TrimSpace(content)
}

// extractAgyModel parses the model name from agy's <USER_SETTINGS_CHANGE> notice.
func extractAgyModel(content string) string {
	const marker = "Model Selection` from "
	i := strings.Index(content, marker)
	if i < 0 {
		return ""
	}
	rest := content[i+len(marker):]
	_, after, ok := strings.Cut(rest, " to ")
	if !ok {
		return ""
	}
	model, _, ok := strings.Cut(after, ".")
	if !ok {
		return ""
	}
	return strings.TrimSpace(model)
}

// agyFootprint lists the files and summary row a real delete removes. Non-UUID
// session IDs are rejected so Join(home, "brain", "") can never resolve to the
// brain directory itself.
func agyFootprint(s *Session) []Artifact {
	if !uuidRe.MatchString(s.UUID) {
		return nil
	}
	home := agyHome()
	candidates := []string{
		filepath.Join(home, "conversations", s.UUID+".db"),
		filepath.Join(home, "conversations", s.UUID+".db-wal"),
		filepath.Join(home, "conversations", s.UUID+".db-shm"),
		filepath.Join(home, "brain", s.UUID),
		filepath.Join(home, "annotations", s.UUID+".pbtxt"),
		filepath.Join(home, "presence", s.UUID+".lock"),
	}
	var out []Artifact
	for _, p := range candidates {
		if !within(p, home) {
			continue
		}
		if a, ok := measure(p); ok {
			out = append(out, a)
		}
	}
	if db := filepath.Join(home, "conversation_summaries.db"); fileExists(db) {
		out = append(out, Artifact{Path: db + " (conversation_summaries row)"})
	}
	return out
}

// agy has no delete or rename CLI subcommand, so deleteAgy and renameAgy edit
// its files and conversation_summaries.db directly.
func deleteAgy(s *Session) error {
	if !uuidRe.MatchString(s.UUID) {
		return fmt.Errorf("invalid agy session id: %q", s.UUID)
	}
	for _, a := range agyFootprint(s) {
		if a.Files == 0 {
			continue
		}
		if err := os.RemoveAll(a.Path); err != nil {
			return err
		}
	}
	db := filepath.Join(agyHome(), "conversation_summaries.db")
	if fileExists(db) {
		sql := fmt.Sprintf("DELETE FROM conversation_summaries WHERE conversation_id = %s;", sqlQuote(s.UUID))
		if err := sqliteExec(db, sql); err != nil {
			return err
		}
	}
	return nil
}

func renameAgy(s *Session, title string) error {
	db := filepath.Join(agyHome(), "conversation_summaries.db")
	sql := fmt.Sprintf("UPDATE conversation_summaries SET title = %s WHERE conversation_id = %s;", sqlQuote(title), sqlQuote(s.UUID))
	return sqliteExec(db, sql)
}
