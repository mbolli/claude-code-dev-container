package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAgyLifecycle(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("AGY_HOME", tmp)
	t.Setenv("CLAUDE_HOME", filepath.Join(tmp, "empty-claude"))
	t.Setenv("CODEX_HOME", filepath.Join(tmp, "empty-codex"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, "cache"))

	db := filepath.Join(tmp, "conversation_summaries.db")
	schema := `
	CREATE TABLE conversation_summaries (
		conversation_id TEXT PRIMARY KEY,
		title TEXT,
		preview TEXT,
		step_count INTEGER,
		created_time DATETIME,
		last_modified_time DATETIME,
		workspace_uris TEXT,
		status TEXT,
		parent_conversation_id TEXT,
		nesting_depth INTEGER,
		not_fully_idle INTEGER DEFAULT 0,
		killed INTEGER DEFAULT 0,
		last_user_input_time DATETIME
	);`
	if err := sqliteExec(db, schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}

	uuid := "22222222-e533-4078-87f0-8f245dbbc0c6"
	subUUID := "1a69359d-e533-4078-87f0-000000000000"

	logDir := filepath.Join(tmp, "brain", uuid, ".system_generated", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(logDir, "transcript.jsonl")
	lines := `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-10-30T05:59:12Z","content":"<USER_REQUEST>\nMap the environment and check sessel\n</USER_REQUEST>\n<USER_SETTINGS_CHANGE>\nThe user changed setting ` + "`Model Selection`" + ` from None to Gemini 4 Argon (Medium). No need to comment.\n</USER_SETTINGS_CHANGE>"}
{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-10-30T05:59:20Z","input_tokens":5000,"cache_read_tokens":2000,"output_tokens":120,"tool_calls":[{"name":"run_command","args":{"CommandLine":"sessel ls"}}]}
{"step_index":2,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-10-30T05:59:30Z","input_tokens":6000,"cache_read_tokens":4500,"output_tokens":300,"content":"Environment mapped."}
`
	if err := os.WriteFile(transcript, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, dir := range []string{"conversations", "annotations", "presence"} {
		if err := os.MkdirAll(filepath.Join(tmp, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{
		filepath.Join(tmp, "conversations", uuid+".db"),
		filepath.Join(tmp, "annotations", uuid+".pbtxt"),
		filepath.Join(tmp, "presence", uuid+".lock"),
	} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	insert := `
	INSERT INTO conversation_summaries (conversation_id, title, preview, step_count, last_modified_time, workspace_uris, status, parent_conversation_id, nesting_depth)
	VALUES
		('` + uuid + `', 'Environment Mapping', 'Map the environment', 3, '2026-10-30 06:14:55.062484513+00:00', '["file:///develop/sessel"]', 'CASCADE_RUN_STATUS_IDLE', '', 0),
		('` + subUUID + `', 'Subagent Research', 'Sub', 1, '2026-10-30 06:14:55.062484513+00:00', '["file:///develop/sessel"]', 'CASCADE_RUN_STATUS_IDLE', '` + uuid + `', 1);
	`
	if err := sqliteExec(db, insert); err != nil {
		t.Fatalf("insert summaries: %v", err)
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
	if s.AgentName() != AgentAgy {
		t.Errorf("agent = %q, want %q", s.AgentName(), AgentAgy)
	}
	if s.Repo() != "sessel" {
		t.Errorf("repo = %q, want sessel", s.Repo())
	}
	if s.Title != "Environment Mapping" {
		t.Errorf("title = %q, want Environment Mapping", s.Title)
	}
	if s.FirstPrompt != "Map the environment and check sessel" {
		t.Errorf("firstPrompt = %q", s.FirstPrompt)
	}
	if s.Stats.Prompts != 1 || s.Stats.Turns != 2 || s.Stats.ToolCalls != 1 {
		t.Errorf("stats counts = %+v", s.Stats)
	}
	if s.Stats.InTokens != 11000 || s.Stats.CacheRead != 6500 || s.Stats.OutTokens != 420 {
		t.Errorf("stats tokens = %+v", s.Stats)
	}
	if len(s.Stats.Models) != 1 || s.Stats.Models[0] != "Gemini 4 Argon (Medium)" {
		t.Errorf("models = %+v", s.Stats.Models)
	}

	turns, err := lastTurnsFor(s, 10)
	if err != nil {
		t.Fatalf("lastTurnsFor: %v", err)
	}
	if len(turns) != 3 || turns[0].Who != "you" || turns[1].Who != "tool" || turns[2].Who != "agy" {
		t.Fatalf("unexpected turns: %+v", turns)
	}
	if turns[1].Text != "run_command(sessel ls)" {
		t.Errorf("tool turn = %q, want run_command(sessel ls)", turns[1].Text)
	}

	// Test Rename
	if err := Rename(s, "Renamed Agy Session"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	st2, err := Load()
	if err != nil {
		t.Fatalf("Load after rename: %v", err)
	}
	s2 := st2.Get(uuid)
	if s2 == nil || s2.Title != "Renamed Agy Session" {
		t.Fatalf("after rename got %+v", s2)
	}

	// Test Footprint & Delete
	arts := st2.Footprint(s2)
	files, _ := totals(arts)
	if files != 4 || len(arts) != 5 {
		t.Errorf("expected 4 files across 5 footprint entries (4 files + 1 db row), got %d files in %d entries: %+v", files, len(arts), arts)
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
}

func TestAgyFootprintRejectsInvalidUUID(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("AGY_HOME", tmp)
	brainDir := filepath.Join(tmp, "brain")
	if err := os.MkdirAll(brainDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brainDir, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", ".", "..", "../brain", "not-a-uuid"} {
		if arts := agyFootprint(&Session{UUID: bad, Agent: AgentAgy}); len(arts) != 0 {
			t.Errorf("agyFootprint(%q) = %+v, want empty", bad, arts)
		}
	}
}

func TestAgyLivePrefersPresenceLockAndAgyComm(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("AGY_HOME", tmp)

	uuid := "22222222-e533-4078-87f0-8f245dbbc0c6"
	s := &Session{UUID: uuid, Agent: AgentAgy}
	convDB := filepath.Join(tmp, "conversations", uuid+".db")
	presLock := filepath.Join(tmp, "presence", uuid+".lock")

	procs := []agentProc{
		// Non-agy process with lowest PID holding presence lock must be ignored.
		{
			pid:       50,
			comm:      "sqlite3",
			procStart: "500",
			fds:       []string{presLock},
		},
		// Lower-PID agy process holding only conversations/<uuid>.db.
		{
			pid:       100,
			comm:      "agy",
			procStart: "1000",
			fds:       []string{convDB},
		},
		// Higher-PID agy process holding presence/<uuid>.lock must win.
		{
			pid:       200,
			comm:      "agy",
			procStart: "2000",
			fds:       []string{convDB, presLock},
			tmux:      "repo@agy-22222222:@1.%1",
		},
	}

	attachAgyLive([]*Session{s}, map[string]bool{uuid: true}, procs)
	if s.PID != 200 || s.State != LiveTmux || !s.Busy {
		t.Errorf("got pid=%d state=%v busy=%v, want pid=200 LiveTmux busy=true", s.PID, s.State, s.Busy)
	}
}

