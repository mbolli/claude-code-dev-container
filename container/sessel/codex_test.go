package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexLifecycle(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("CODEX_HOME", tmp)
	t.Setenv("CLAUDE_HOME", filepath.Join(tmp, "empty-claude"))
	t.Setenv("AGY_HOME", filepath.Join(tmp, "empty-agy"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, "cache"))

	origBin := codexBin
	t.Cleanup(func() { codexBin = origBin })
	codexBin = "codex-binary-not-installed-in-test"

	db := filepath.Join(tmp, "state_5.sqlite")
	schema := `
	CREATE TABLE threads (
		id TEXT PRIMARY KEY,
		rollout_path TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		source TEXT NOT NULL,
		model_provider TEXT NOT NULL,
		cwd TEXT NOT NULL,
		title TEXT NOT NULL,
		sandbox_policy TEXT NOT NULL,
		approval_mode TEXT NOT NULL,
		tokens_used INTEGER NOT NULL DEFAULT 0,
		has_user_event INTEGER NOT NULL DEFAULT 0,
		archived INTEGER NOT NULL DEFAULT 0,
		git_branch TEXT,
		first_user_message TEXT NOT NULL DEFAULT '',
		model TEXT,
		name TEXT,
		attached_thread_parent_id TEXT
	);
	CREATE TABLE thread_dynamic_tools (
		thread_id TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
		position INTEGER NOT NULL,
		name TEXT NOT NULL,
		PRIMARY KEY(thread_id, position)
	);`
	if err := sqliteExec(db, schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}

	histDB := filepath.Join(tmp, "thread_history_1.sqlite")
	histSchema := `
	CREATE TABLE thread_turns (thread_id TEXT NOT NULL, turn_id TEXT NOT NULL);
	CREATE TABLE thread_items (thread_id TEXT NOT NULL, turn_id TEXT NOT NULL, item_id TEXT NOT NULL);
	CREATE TABLE thread_realtime_items (thread_id TEXT NOT NULL, item_id TEXT NOT NULL);
	CREATE TABLE thread_history_projection_state (thread_id TEXT PRIMARY KEY);`
	if err := sqliteExec(histDB, histSchema); err != nil {
		t.Fatalf("create history schema: %v", err)
	}

	uuid := "01a18b7d-ae9e-7882-953c-660ecbf89677"
	subUUID := "01a18b7d-ae9e-7882-953c-999999999999"
	rolloutDir := filepath.Join(tmp, "sessions", "2026", "10", "30")
	if err := os.MkdirAll(rolloutDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rollout := filepath.Join(rolloutDir, "rollout-"+uuid+".jsonl")
	rolloutLines := `{"timestamp":"2026-10-30T06:27:12.779Z","ordinal":0,"type":"session_meta","payload":{"id":"01a18b7d-ae9e-7882-953c-660ecbf89677","cwd":"/develop/trips"}}
{"timestamp":"2026-10-30T06:27:13.586Z","ordinal":1,"type":"response_item","payload":{"type":"message","id":"msg_env","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions\nfoo"},{"type":"input_text","text":"<environment_context>\nbar\n</environment_context>"}]}}
{"timestamp":"2026-10-30T06:27:13.595Z","ordinal":2,"type":"response_item","payload":{"type":"message","id":"msg_u1","role":"user","content":[{"type":"input_text","text":"Fix the router bug"}]}}
{"timestamp":"2026-10-30T06:27:14.100Z","ordinal":3,"type":"response_item","payload":{"type":"function_call","id":"fc_1","name":"exec_command","arguments":"{\"cmd\":\"rg router\"}"}}
{"timestamp":"2026-10-30T06:27:14.883Z","ordinal":4,"type":"response_item","payload":{"type":"message","id":"msg_a1","role":"assistant","content":[{"type":"output_text","text":"Found and fixed it."}]}}
{"timestamp":"2026-10-30T06:27:14.890Z","ordinal":5,"type":"token_usage_record","payload":{"thread_token_usage":{"input_tokens":1200,"cached_input_tokens":800,"cache_write_input_tokens":0,"output_tokens":45}}}
`
	if err := os.WriteFile(rollout, []byte(rolloutLines), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, dir := range []string{"tui-thread-reference-capabilities", "thread-writer-locks", "shell_snapshots"} {
		if err := os.MkdirAll(filepath.Join(tmp, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	capFile := filepath.Join(tmp, "tui-thread-reference-capabilities", uuid)
	lockFile := filepath.Join(tmp, "thread-writer-locks", uuid+".lock")
	snapFile := filepath.Join(tmp, "shell_snapshots", uuid+".sh")
	for _, f := range []string{capFile, lockFile, snapFile} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	insert := `
	INSERT INTO threads (id, rollout_path, created_at, updated_at, source, model_provider, cwd, title, sandbox_policy, approval_mode, git_branch, first_user_message, model, attached_thread_parent_id)
	VALUES
		('` + uuid + `', '` + rollout + `', 1793341632, 1793341635, 'cli', 'openai', '/develop/trips', 'Fix router bug', 'workspace-write', 'on-request', 'main', 'Fix the router bug', 'gpt-6-luna', NULL),
		('` + subUUID + `', '` + rollout + `', 1793341632, 1793341635, 'cli', 'openai', '/develop/trips', 'Subagent thread', 'workspace-write', 'on-request', 'main', 'Sub', 'gpt-6-luna', '` + uuid + `');
	INSERT INTO thread_dynamic_tools (thread_id, position, name) VALUES ('` + uuid + `', 0, 'custom_tool');
	`
	if err := sqliteExec(db, insert); err != nil {
		t.Fatalf("insert threads: %v", err)
	}
	if err := sqliteExec(histDB, "INSERT INTO thread_turns VALUES ('"+uuid+"', 'turn_1');"); err != nil {
		t.Fatalf("insert history: %v", err)
	}

	st, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(st.Sessions) != 1 {
		t.Fatalf("expected 1 top-level session, got %d", len(st.Sessions))
	}
	s, err := st.Resolve(uuid)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if s.AgentName() != AgentCodex {
		t.Errorf("agent = %q, want %q", s.AgentName(), AgentCodex)
	}
	if s.Repo() != "trips" {
		t.Errorf("repo = %q, want trips", s.Repo())
	}
	if s.Branch != "main" {
		t.Errorf("branch = %q, want main", s.Branch)
	}
	if s.Title != "Fix router bug" || s.TitleSource != "ai" {
		t.Errorf("title = %q (%s), want Fix router bug (ai)", s.Title, s.TitleSource)
	}
	if s.FirstPrompt != "Fix the router bug" {
		t.Errorf("firstPrompt = %q", s.FirstPrompt)
	}
	if s.Stats.Prompts != 1 || s.Stats.Turns != 1 || s.Stats.ToolCalls != 1 {
		t.Errorf("stats counts = %+v", s.Stats)
	}
	if s.Stats.InTokens != 1200 || s.Stats.OutTokens != 45 || s.Stats.CacheRead != 800 {
		t.Errorf("stats tokens = %+v", s.Stats)
	}

	turns, err := lastTurnsFor(s, 10)
	if err != nil {
		t.Fatalf("lastTurnsFor: %v", err)
	}
	if len(turns) != 3 || turns[0].Who != "you" || turns[1].Who != "tool" || turns[2].Who != "codex" {
		t.Fatalf("unexpected turns: %+v", turns)
	}
	if turns[1].Text != "exec_command(rg router)" {
		t.Errorf("tool turn = %q, want exec_command(rg router)", turns[1].Text)
	}

	if err := Rename(s, "Custom Codex Title"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	st2, err := Load()
	if err != nil {
		t.Fatalf("Load after rename: %v", err)
	}
	s2 := st2.Get(uuid)
	if s2 == nil || s2.Title != "Custom Codex Title" || s2.TitleSource != "custom" {
		t.Fatalf("after rename got %+v", s2)
	}

	arts := st2.Footprint(s2)
	files, _ := totals(arts)
	if files != 4 || len(arts) != 6 {
		t.Errorf("expected 4 files across 6 footprint entries (4 files + 2 db rows), got %d files in %d entries: %+v", files, len(arts), arts)
	}
	if err := st2.Delete(s2); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	st3, err := Load()
	if err != nil {
		t.Fatalf("Load after delete: %v", err)
	}
	if len(st3.Sessions) != 0 {
		t.Errorf("expected 0 sessions after delete, got %d", len(st3.Sessions))
	}

	// Verify ON DELETE CASCADE removed thread_dynamic_tools and thread_history_1 was cleaned up.
	type countRow struct {
		N int `json:"n"`
	}
	if rows, _ := sqliteQueryJSON[countRow](db, "SELECT COUNT(*) AS n FROM thread_dynamic_tools WHERE thread_id = '"+uuid+"';"); len(rows) != 1 || rows[0].N != 0 {
		t.Errorf("expected 0 thread_dynamic_tools rows after cascade delete, got %+v", rows)
	}
	if rows, _ := sqliteQueryJSON[countRow](histDB, "SELECT COUNT(*) AS n FROM thread_turns WHERE thread_id = '"+uuid+"';"); len(rows) != 1 || rows[0].N != 0 {
		t.Errorf("expected 0 thread_turns rows after delete, got %+v", rows)
	}
}

func TestCodexDeleteDelegatesToCLI(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("CODEX_HOME", tmp)
	origBin := codexBin
	t.Cleanup(func() { codexBin = origBin })

	uuid := "01a18b7d-ae9e-7882-953c-660ecbf89677"
	marker := filepath.Join(tmp, "called-args")
	fake := filepath.Join(tmp, "fake-codex")
	script := "#!/bin/sh\nprintf '%s' \"$*\" > " + marker + "\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	codexBin = fake

	s := &Session{UUID: uuid, Agent: AgentCodex}
	if err := deleteCodex(s); err != nil {
		t.Fatalf("deleteCodex with fake CLI: %v", err)
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if string(got) != "delete --force "+uuid {
		t.Errorf("CLI args = %q, want %q", string(got), "delete --force "+uuid)
	}

	// Verify CLI failure is returned rather than ignored.
	failScript := "#!/bin/sh\necho 'thread is locked' >&2\nexit 1\n"
	if err := os.WriteFile(fake, []byte(failScript), 0o755); err != nil {
		t.Fatal(err)
	}
	err = deleteCodex(s)
	if err == nil || !strings.Contains(err.Error(), "thread is locked") {
		t.Errorf("expected CLI error to propagate, got %v", err)
	}
}

func TestCodexLiveMatchingAndLocks(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("CODEX_HOME", tmp)

	u1 := "01a18b7d-ae9e-7882-953c-111111111111"
	u2 := "01a18b7d-ae9e-7882-953c-222222222222"
	u3 := "01a18b7d-ae9e-7882-953c-333333333333"

	s1 := &Session{UUID: u1, Agent: AgentCodex, Cwd: "/develop/trips", File: filepath.Join(tmp, "r1.jsonl")}
	s2 := &Session{UUID: u2, Agent: AgentCodex, Cwd: "/develop/trips", File: filepath.Join(tmp, "r2.jsonl")}
	s3 := &Session{UUID: u3, Agent: AgentCodex, Cwd: "/develop/trips", File: filepath.Join(tmp, "r3.jsonl")}
	sessions := []*Session{s1, s2, s3}

	lock1 := filepath.Join(tmp, "thread-writer-locks", u1+".lock")
	lock2 := filepath.Join(tmp, "thread-writer-locks", u2+".lock")

	procs := []agentProc{
		// Daemon holds writer locks for u1 and u2.
		{
			pid:       100,
			comm:      "codex",
			procStart: "1000",
			args:      []string{"codex", "app-server", "--listen", "unix://"},
			fds:       []string{lock1, lock2},
		},
		// Client 1 resumed u1 (confirmed by lock1).
		{
			pid:       201,
			comm:      "codex",
			procStart: "2001",
			args:      []string{"codex", "resume", u1},
			tmux:      "trips@codex-01a18b7d:@1.%1",
		},
		// Client 2 was started with u3 on cmdline, but u3 is NOT locked and not open,
		// so u3 must not be attached to Client 2, and u2 (locked in same repo) must NOT
		// be guessed onto Client 2 by cwd.
		{
			pid:       202,
			comm:      "codex",
			procStart: "2002",
			args:      []string{"codex", "resume", u3},
			tmux:      "trips@codex-01a18b7d:@2.%2",
		},
	}

	attachCodexLive(sessions, procs)

	if s1.State != LiveTmux || s1.PID != 201 || !s1.Locked {
		t.Errorf("s1: got state=%v pid=%d locked=%v, want LiveTmux pid=201 locked=true", s1.State, s1.PID, s1.Locked)
	}
	if s2.State != LiveOutside || s2.PID != 0 || !s2.Locked || !s2.Busy {
		t.Errorf("s2 (locked by daemon, no client): got state=%v pid=%d locked=%v busy=%v, want LiveOutside pid=0 locked=true busy=true",
			s2.State, s2.PID, s2.Locked, s2.Busy)
	}
	if s3.State != Dead || s3.PID != 0 || s3.Locked {
		t.Errorf("s3 (stale cmdline uuid, unlocked): got state=%v pid=%d locked=%v, want Dead pid=0 locked=false",
			s3.State, s3.PID, s3.Locked)
	}

	// Unowned locked thread must be refused by Stop, Rename, and Delete.
	if err := Stop(s2); err == nil {
		t.Errorf("Stop on unowned locked thread s2 succeeded, want error")
	}
	if err := Rename(s2, "new title"); err == nil {
		t.Errorf("Rename on unowned locked thread s2 succeeded, want error")
	}
	st := &Store{Home: tmp}
	if err := st.Delete(s2); err == nil {
		t.Errorf("Delete on unowned locked thread s2 succeeded, want error")
	}
}

func TestCodexFootprintRejectsInvalidUUID(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("CODEX_HOME", tmp)
	capDir := filepath.Join(tmp, "tui-thread-reference-capabilities")
	if err := os.MkdirAll(capDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(capDir, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", ".", "..", "../tui-thread-reference-capabilities", "not-a-uuid"} {
		if arts := codexFootprint(&Session{UUID: bad, Agent: AgentCodex}); len(arts) != 0 {
			t.Errorf("codexFootprint(%q) = %+v, want empty", bad, arts)
		}
	}
}

func TestIsCodexSubcommand(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{"resume", "01a18b7d-ae9e-7882-953c-660ecbf89677"}, false},
		{[]string{"fork", "--last"}, false},
		{[]string{"-m", "gpt-6-luna", "delete the old file"}, false},
		{[]string{"-C", "/develop/trips", "review the router"}, false},
		{[]string{"app-server", "--listen", "unix://"}, true},
		{[]string{"-C", "/tmp/codex-probe", "exec", "Reply pong"}, true},
		{[]string{"delete", "--force", "01a18b7d-ae9e-7882-953c-660ecbf89677"}, true},
		{[]string{"agents"}, true},
		{[]string{"review"}, true},
		{[]string{"queue"}, true},
		{[]string{"archive", "foo"}, true},
	}
	for _, tc := range cases {
		if got := isCodexSubcommand(tc.args); got != tc.want {
			t.Errorf("isCodexSubcommand(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

func TestTitleCompactionAndToolCollapse(t *testing.T) {
	// Compaction preamble and inter-session message are skipped by contentText.
	if got := contentText([]byte(`"This session is being continued from a previous conversation that ran out of context."`)); got != "" {
		t.Errorf("compaction preamble got %q, want empty", got)
	}
	if got := contentText([]byte(`"Another Claude session sent a message:\n<agent-message>"`)); got != "" {
		t.Errorf("inter-session message got %q, want empty", got)
	}
	if got := contentText([]byte(`"Release php-via 0.14 now"`)); got != "Release php-via 0.14 now" {
		t.Errorf("normal prompt got %q", got)
	}

	// Folder launch name ("tickit" in /develop/tickit) loses to FirstPrompt,
	// whereas a custom name ("LITH" in /develop/starbase) wins over FirstPrompt.
	gotTitle, gotSrc := title(meta{CustomTitle: "tickit", AgentName: "tickit", FirstPrompt: "read docs/requirements.md"}, "/develop/tickit")
	if gotTitle != "read docs/requirements.md" || gotSrc != "prompt" {
		t.Errorf("folder launch name title = %q (%s), want first prompt", gotTitle, gotSrc)
	}
	gotTitle, gotSrc = title(meta{CustomTitle: "LITH", AgentName: "LITH", FirstPrompt: "There are now quite a few golang projects"}, "/develop/starbase")
	if gotTitle != "LITH" || gotSrc != "name" {
		t.Errorf("custom resumed name title = %q (%s), want LITH (name)", gotTitle, gotSrc)
	}
	gotTitle, gotSrc = title(meta{}, "/develop/empty")
	if gotTitle != "(empty session)" || gotSrc != "none" {
		t.Errorf("empty transcript title = %q (%s), want (empty session)", gotTitle, gotSrc)
	}

	// Consecutive tool runs > 4 collapse to a summary line + last 3 calls.
	turns := []Turn{
		{Who: "you", Text: "check the files"},
		{Who: "tool", Text: "Read(a)"},
		{Who: "tool", Text: "Read(b)"},
		{Who: "tool", Text: "Read(c)"},
		{Who: "tool", Text: "Read(d)"},
		{Who: "tool", Text: "Read(e)"},
		{Who: "tool", Text: "Read(f)"},
		{Who: "claude", Text: "done"},
	}
	collapsed := collapseTools(turns, 3)
	if len(collapsed) != 6 {
		t.Fatalf("len(collapsed) = %d, want 6: %+v", len(collapsed), collapsed)
	}
	if collapsed[1].Text != "… 3 earlier tool calls" || collapsed[4].Text != "Read(f)" {
		t.Errorf("collapsed = %+v", collapsed)
	}
}
