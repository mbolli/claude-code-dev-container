package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A conversation has two ids. The transcript uuid is what `claude --resume`
// takes. The Remote Control id (session_01..., cse_01..., or the tail of a
// claude.ai/code URL) is what the phone shows, and it changes every time the
// conversation is re-bridged. The registry in ~/.claude/sessions keeps old
// ones the transcript forgets and vice versa, so bridge ids are the union.

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type State int

const (
	Dead State = iota
	LiveTmux
	LiveOutside
)

func (s State) String() string {
	return [...]string{"dead", "tmux", "outside"}[s]
}

type PR struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	Repo   string `json:"repo"`
}

// meta is what one pass over a transcript yields. It is cached, and because
// transcripts are append-only a later pass continues from the saved offset.
type meta struct {
	Lines       int
	Cwd         string
	Branch      string
	AITitle     string
	CustomTitle string
	AgentName   string
	FirstPrompt string
	Bridges     []string // in file order
	PRs         []PR
	Stats       Stats
}

// Stats are counted in the same pass as everything else, so they are cached
// and the stats view costs nothing extra.
type Stats struct {
	Prompts    int      `json:"prompts"` // what you typed
	Turns      int      `json:"turns"`   // Claude's replies, one per message
	ToolCalls  int      `json:"toolCalls"`
	InTokens   int64    `json:"inTokens"`
	OutTokens  int64    `json:"outTokens"`
	CacheRead  int64    `json:"cacheRead"`
	CacheWrite int64    `json:"cacheWrite"`
	Models     []string `json:"models"`
	FirstAt    string   `json:"firstAt"`
	LastAt     string   `json:"lastAt"`
	// A reply with several blocks is written as several lines, each repeating
	// the message's full usage; count it once.
	LastMsgID string `json:"lastMsgId,omitempty"`
}

type Session struct {
	UUID        string    `json:"uuid"`
	File        string    `json:"file"`
	Project     string    `json:"project"`
	Cwd         string    `json:"cwd"`
	Branch      string    `json:"branch"`
	Title       string    `json:"title"`
	TitleSource string    `json:"titleSource"`
	FirstPrompt string    `json:"firstPrompt"`
	Mtime       time.Time `json:"mtime"`
	Lines       int       `json:"lines"`
	Bridges     []string  `json:"bridges"` // newest first
	PRs         []PR      `json:"prs"`
	Stats       Stats     `json:"stats"`
	State       State     `json:"-"`
	StateName   string    `json:"state"`
	PID         int       `json:"pid,omitempty"`
	Tmux        string    `json:"tmux,omitempty"`
	procStart   string    // re-checked before signalling PID
}

// Bridge is the current Remote Control id, or "".
func (s *Session) Bridge() string {
	if len(s.Bridges) == 0 {
		return ""
	}
	return s.Bridges[0]
}

// Repo is the cwd relative to /develop, for display.
func (s *Session) Repo() string {
	switch {
	case s.Cwd == "":
		return "?"
	case s.Cwd == "/develop":
		return "/develop"
	case strings.HasPrefix(s.Cwd, "/develop/"):
		return strings.TrimPrefix(s.Cwd, "/develop/")
	}
	return s.Cwd
}

// Scratch reports a session that ran in a /tmp scratchpad: a subagent probe.
func (s *Session) Scratch() bool { return strings.HasPrefix(s.Cwd, "/tmp") }

type Store struct {
	Home     string // ~/.claude
	Sessions []*Session
	byUUID   map[string]*Session
	registry map[string][]regRecord // uuid -> records, newest first
}

type regRecord struct {
	SessionID string `json:"sessionId"`
	Bridge    string `json:"bridgeSessionId"`
	Cwd       string `json:"cwd"`
	PID       int    `json:"pid"`
	ProcStart string `json:"procStart"`
	Tmux      string `json:"tmux"`
	UpdatedAt int64  `json:"updatedAt"`
	file      string
}

func claudeHome() string {
	if h := os.Getenv("CLAUDE_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

func Load() (*Store, error) {
	st := &Store{Home: claudeHome(), byUUID: map[string]*Session{}, registry: map[string][]regRecord{}}
	st.loadRegistry()

	files, _ := filepath.Glob(filepath.Join(st.Home, "projects", "*", "*.jsonl"))
	var paths []string
	for _, f := range files {
		if uuidRe.MatchString(strings.TrimSuffix(filepath.Base(f), ".jsonl")) {
			paths = append(paths, f)
		}
	}
	metas, stats := scanAll(paths)
	for i, p := range paths {
		if stats[i] == nil {
			continue
		}
		s := st.session(p, stats[i], metas[i])
		st.Sessions = append(st.Sessions, s)
		st.byUUID[s.UUID] = s
	}
	sort.Slice(st.Sessions, func(i, j int) bool { return st.Sessions[i].Mtime.After(st.Sessions[j].Mtime) })
	return st, nil
}

func (st *Store) loadRegistry() {
	files, _ := filepath.Glob(filepath.Join(st.Home, "sessions", "*.json"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var r regRecord
		if json.Unmarshal(b, &r) != nil || r.SessionID == "" {
			continue
		}
		r.file = f
		st.registry[r.SessionID] = append(st.registry[r.SessionID], r)
	}
	for _, recs := range st.registry {
		sort.Slice(recs, func(i, j int) bool { return recs[i].UpdatedAt > recs[j].UpdatedAt })
	}
}

func (st *Store) session(path string, fi os.FileInfo, m meta) *Session {
	uuid := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	s := &Session{
		UUID: uuid, File: path, Project: filepath.Base(filepath.Dir(path)),
		Cwd: m.Cwd, Branch: m.Branch, FirstPrompt: m.FirstPrompt,
		Mtime: fi.ModTime(), Lines: m.Lines, PRs: m.PRs, Stats: m.Stats,
	}
	s.Title, s.TitleSource = title(m)

	recs := st.registry[uuid]
	// cwd: the transcript, else the registry, else the project dir name. A
	// session spawned by Remote Control may never write a cwd line at all.
	if s.Cwd == "" {
		for _, r := range recs {
			if r.Cwd != "" {
				s.Cwd = r.Cwd
				break
			}
		}
	}
	if s.Cwd == "" {
		s.Cwd = decodeProject(s.Project)
	}

	for _, r := range recs {
		s.Bridges = appendUnique(s.Bridges, normBridge(r.Bridge))
	}
	// Transcript entries have no timestamp; the last one written is the newest.
	for i := len(m.Bridges) - 1; i >= 0; i-- {
		s.Bridges = appendUnique(s.Bridges, m.Bridges[i])
	}

	for _, r := range recs {
		if alive(r.PID, r.ProcStart) {
			s.PID, s.procStart = r.PID, r.ProcStart
			if r.Tmux != "" {
				s.State, s.Tmux = LiveTmux, r.Tmux
			} else {
				s.State = LiveOutside
			}
			break
		}
	}
	s.StateName = s.State.String()
	return s
}

// title prefers your own rename, then Claude's summary. A session launched
// with a name gets a custom-title equal to its agent-name; that is the launch
// name, usually just the repo, and loses to the ai-title.
func title(m meta) (string, string) {
	userRename := m.CustomTitle != "" && m.CustomTitle != m.AgentName
	switch {
	case userRename:
		return m.CustomTitle, "custom"
	case m.AITitle != "":
		return m.AITitle, "ai"
	case m.CustomTitle != "":
		return m.CustomTitle, "name"
	case m.FirstPrompt != "":
		return oneLine(m.FirstPrompt, 80), "prompt"
	}
	return "(untitled)", "none"
}

// decodeProject reverses Claude's project dir naming (every non-alphanumeric
// character of the path becomes '-') by walking the filesystem, since the
// name alone is ambiguous: -develop-nfsen-ng could be nfsen-ng or nfsen/ng.
func decodeProject(name string) string {
	dir, rest := "/", name
	for rest != "" {
		entries, err := os.ReadDir(dir)
		if err != nil {
			break
		}
		best := ""
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			m := "-" + mangle(e.Name())
			if strings.HasPrefix(rest, m) && (len(rest) == len(m) || rest[len(m)] == '-') && len(m) > len(best) {
				best = e.Name()
			}
		}
		if best == "" {
			break
		}
		dir = filepath.Join(dir, best)
		rest = rest[len("-"+mangle(best)):]
	}
	if dir == "/" {
		return ""
	}
	return dir
}

func mangle(s string) string {
	b := []byte(s)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			b[i] = '-'
		}
	}
	return string(b)
}

// Both prefixes name the same object; keep the one the phone shows.
func normBridge(b string) string {
	if strings.HasPrefix(b, "cse_") {
		return "session_" + strings.TrimPrefix(b, "cse_")
	}
	return b
}

func appendUnique(xs []string, x string) []string {
	if x == "" {
		return xs
	}
	for _, y := range xs {
		if y == x {
			return xs
		}
	}
	return append(xs, x)
}

// alive is true only for the same process: pids restart from 1 after every
// container rebuild, so a pid that exists may be somebody else.
func alive(pid int, procStart string) bool {
	return pid > 0 && procStart != "" && procStartOf(pid) == procStart
}

func procStartOf(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	// comm is field 2 and may contain spaces and parens: split after the last ')'.
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return ""
	}
	fields := strings.Fields(string(b[i+1:]))
	if len(fields) < 20 {
		return ""
	}
	return fields[19] // starttime, field 22 overall
}

// ---- scanning, with an offset cache --------------------------------------

const cacheVersion = 3

type cacheEntry struct {
	Size    int64
	ModTime int64
	Offset  int64 // end of the last complete line parsed
	Meta    meta
}

type cacheFile struct {
	Version int
	Entries map[string]cacheEntry
}

func cachePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "sessel", "index.json")
}

func readCache() map[string]cacheEntry {
	b, err := os.ReadFile(cachePath())
	if err != nil {
		return map[string]cacheEntry{}
	}
	var c cacheFile
	if json.Unmarshal(b, &c) != nil || c.Version != cacheVersion || c.Entries == nil {
		return map[string]cacheEntry{}
	}
	return c.Entries
}

func writeCache(entries map[string]cacheEntry) {
	p := cachePath()
	if p == "" || os.MkdirAll(filepath.Dir(p), 0o755) != nil {
		return
	}
	b, err := json.Marshal(cacheFile{Version: cacheVersion, Entries: entries})
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".index-*")
	if err != nil {
		return
	}
	if _, err := tmp.Write(b); err != nil || tmp.Close() != nil {
		os.Remove(tmp.Name())
		return
	}
	os.Rename(tmp.Name(), p)
}

// scanAll returns meta and stat for each path, parsing in parallel and only
// what the cache does not already cover.
func scanAll(paths []string) ([]meta, []os.FileInfo) {
	cache := readCache()
	metas := make([]meta, len(paths))
	stats := make([]os.FileInfo, len(paths))
	next := make(map[string]cacheEntry, len(paths))
	var mu sync.Mutex

	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < runtime.NumCPU(); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				p := paths[i]
				fi, err := os.Stat(p)
				if err != nil {
					continue
				}
				c, ok := cache[p]
				switch {
				case ok && c.Size == fi.Size() && c.ModTime == fi.ModTime().UnixNano():
					// unchanged
				case ok && fi.Size() > c.Offset && c.Offset > 0:
					c.Meta, c.Offset = scan(p, c.Offset, c.Meta)
				default: // new, or rewritten shorter than we had read
					c.Meta, c.Offset = scan(p, 0, meta{})
				}
				c.Size, c.ModTime = fi.Size(), fi.ModTime().UnixNano()
				metas[i], stats[i] = c.Meta, fi
				mu.Lock()
				next[p] = c
				mu.Unlock()
			}
		}()
	}
	for i := range paths {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	writeCache(next)
	return metas, stats
}

var (
	kCwdKey  = []byte(`"cwd":"`)
	kBranch  = []byte(`"gitBranch":"`)
	kAITitle = []byte(`"type":"ai-title"`)
	kCustom  = []byte(`"type":"custom-title"`)
	kAgent   = []byte(`"type":"agent-name"`)
	kPR      = []byte(`"type":"pr-link"`)
	kBridge  = []byte(`"type":"bridge-session"`)
	kUser    = []byte(`"type":"user"`)
	kAsst    = []byte(`"type":"assistant"`)
)

const chunk = 1 << 20

// scan continues from off, which is always a line start. Lines longer than the
// buffer (tool results can be over a megabyte) are skipped past without being
// copied: the keys it looks for sit near the start of an entry, and the
// entries it decodes are short. A last line without its newline is still
// being written, so it is left for the next pass.
func scan(path string, off int64, m meta) (meta, int64) {
	f, err := os.Open(path)
	if err != nil {
		return m, off
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return m, off
	}
	r := bufio.NewReaderSize(f, chunk)
	for {
		line, err := r.ReadSlice('\n')
		switch {
		case err == nil:
			m.Lines++
			parseLine(line, &m, true)
			off += int64(len(line))
		case errors.Is(err, bufio.ErrBufferFull):
			// Read the head now, before the next ReadSlice reuses the buffer.
			// Re-reading it after an EOF below only finds the same cwd again.
			parseLine(line, &m, false)
			n := int64(len(line))
			for errors.Is(err, bufio.ErrBufferFull) {
				line, err = r.ReadSlice('\n')
				n += int64(len(line))
			}
			if err != nil { // EOF inside a long line: still being written
				return m, off
			}
			m.Lines++
			off += n
		default: // io.EOF, possibly after a partial line
			return m, off
		}
	}
}

// parseLine reads what it needs from one entry. whole is false when line is
// only the start of an oversized entry, which must not be JSON-decoded.
func parseLine(line []byte, m *meta, whole bool) {
	if m.Cwd == "" {
		if v := field(line, kCwdKey); v != "" {
			m.Cwd = v
			m.Branch = field(line, kBranch)
		}
	}
	if !whole {
		return
	}
	switch {
	case bytes.Contains(line, kAITitle):
		var v struct{ Type, AiTitle string }
		if json.Unmarshal(line, &v) == nil && v.Type == "ai-title" && v.AiTitle != "" {
			m.AITitle = v.AiTitle
		}
	case bytes.Contains(line, kCustom):
		var v struct{ Type, CustomTitle string }
		if json.Unmarshal(line, &v) == nil && v.Type == "custom-title" {
			m.CustomTitle = v.CustomTitle
		}
	case bytes.Contains(line, kAgent):
		var v struct{ Type, AgentName string }
		if json.Unmarshal(line, &v) == nil && v.Type == "agent-name" {
			m.AgentName = v.AgentName
		}
	case bytes.Contains(line, kPR):
		var v struct {
			Type, PrURL, PrRepository string
			PrNumber                  int
		}
		if json.Unmarshal(line, &v) == nil && v.Type == "pr-link" {
			m.PRs = appendPR(m.PRs, PR{v.PrNumber, v.PrURL, v.PrRepository})
		}
	case bytes.Contains(line, kBridge):
		var v struct{ Type, BridgeSessionId string }
		if json.Unmarshal(line, &v) == nil && v.Type == "bridge-session" {
			m.Bridges = appendUnique(m.Bridges, normBridge(v.BridgeSessionId))
		}
	case bytes.Contains(line, kUser) || bytes.Contains(line, kAsst):
		// Either marker can also appear inside content, such as a tool result
		// that read a transcript, so decode and go by the top-level type.
		countTurn(line, m)
	}
}

type turnEntry struct {
	Type      string
	IsMeta    bool `json:"isMeta"`
	Timestamp string
	Message   struct {
		ID      string
		Model   string
		Content json.RawMessage
		Usage   struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		}
	}
}

func countTurn(line []byte, m *meta) {
	var v turnEntry
	if json.Unmarshal(line, &v) != nil || v.IsMeta {
		return
	}
	st := &m.Stats
	switch v.Type {
	case "user":
		t := contentText(v.Message.Content)
		if t == "" {
			return // a tool result or an injected block, not something you typed
		}
		st.Prompts++
		if m.FirstPrompt == "" {
			m.FirstPrompt = t
		}
	case "assistant":
		var blocks []struct{ Type string }
		json.Unmarshal(v.Message.Content, &blocks)
		for _, b := range blocks {
			if b.Type == "tool_use" {
				st.ToolCalls++
			}
		}
		if v.Message.ID == "" || v.Message.ID == st.LastMsgID {
			break
		}
		st.LastMsgID = v.Message.ID
		st.Turns++
		u := v.Message.Usage
		st.InTokens += u.InputTokens
		st.OutTokens += u.OutputTokens
		st.CacheRead += u.CacheReadInputTokens
		st.CacheWrite += u.CacheCreationInputTokens
		if v.Message.Model != "" && v.Message.Model != "<synthetic>" {
			st.Models = appendUnique(st.Models, v.Message.Model)
		}
	default:
		return
	}
	if v.Timestamp != "" {
		if st.FirstAt == "" {
			st.FirstAt = v.Timestamp
		}
		st.LastAt = v.Timestamp
	}
}

// field extracts a plain JSON string value by key without decoding the line,
// so it also works on the head of an oversized entry.
func field(line, key []byte) string {
	i := bytes.Index(line, key)
	if i < 0 {
		return ""
	}
	rest := line[i+len(key):]
	j := bytes.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return string(rest[:j])
}

func appendPR(prs []PR, p PR) []PR {
	for _, q := range prs {
		if q.URL == p.URL {
			return prs
		}
	}
	return append(prs, p)
}

// userText is the typed text of a user entry, or "" for tool results and the
// harness's own injected blocks.
func userText(line []byte) string {
	var v struct {
		Type    string
		IsMeta  bool `json:"isMeta"`
		Message struct {
			Content json.RawMessage
		}
	}
	if json.Unmarshal(line, &v) != nil || v.Type != "user" || v.IsMeta {
		return ""
	}
	return contentText(v.Message.Content)
}

// contentText is the typed text of a user message's content, or "".
func contentText(content json.RawMessage) string {
	var text string
	if json.Unmarshal(content, &text) != nil {
		var parts []struct{ Type, Text string }
		if json.Unmarshal(content, &parts) != nil {
			return ""
		}
		for _, p := range parts {
			if p.Type == "text" {
				text += p.Text
			}
		}
	}
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "<") { // <command-name>, <local-command-stdout>, …
		return ""
	}
	return text
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// ---- lookups --------------------------------------------------------------

// Resolve accepts a uuid, session_01…, cse_01…, or a claude.ai/code URL.
func (st *Store) Resolve(id string) (*Session, error) {
	id = strings.TrimSpace(id)
	if i := strings.IndexByte(id, '?'); i >= 0 {
		id = id[:i]
	}
	id = strings.TrimRight(id, "/")
	if i := strings.LastIndexByte(id, '/'); i >= 0 {
		id = id[i+1:]
	}
	if uuidRe.MatchString(id) {
		if s := st.byUUID[id]; s != nil {
			return s, nil
		}
		return nil, fmt.Errorf("no transcript for %s, so --resume has nothing to open", id)
	}
	if strings.HasPrefix(id, "session_") || strings.HasPrefix(id, "cse_") {
		want := normBridge(id)
		for _, s := range st.Sessions {
			for _, b := range s.Bridges {
				if b == want {
					return s, nil
				}
			}
		}
		for uuid, recs := range st.registry {
			for _, r := range recs {
				if normBridge(r.Bridge) == want {
					return nil, fmt.Errorf("%s was bridged as %s, but its transcript is gone", uuid, id)
				}
			}
		}
		return nil, fmt.Errorf("no local session was ever bridged as %s", id)
	}
	return nil, fmt.Errorf("not a session id: %s", id)
}

func (st *Store) Get(uuid string) *Session { return st.byUUID[uuid] }

// registryFiles are the pid-keyed records naming a session, for delete.
func (st *Store) registryFiles(uuid string) []string {
	var out []string
	for _, r := range st.registry[uuid] {
		out = append(out, r.file)
	}
	return out
}

func age(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d"
	}
}
