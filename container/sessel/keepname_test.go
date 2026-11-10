package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeepName(t *testing.T) {
	uuid := "39253f94-b68e-5d7a-b1f7-7f21ef0d38db"
	file := filepath.Join(t.TempDir(), uuid+".jsonl")
	// no trailing newline: the records must still start on a line of their own
	if err := os.WriteFile(file, []byte(`{"type":"user","sessionId":"`+uuid+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Session{UUID: uuid, File: file}

	if err := KeepName(s, "MOXI"); err != nil {
		t.Fatal(err)
	}
	m, _ := scan(file, 0, meta{}, parseLine)
	if m.AgentName != "MOXI" || m.CustomTitle != "MOXI" {
		t.Fatalf("agent-name %q, custom-title %q, want MOXI for both", m.AgentName, m.CustomTitle)
	}
	before, _ := os.ReadFile(file)
	if n := strings.Count(string(before), "\n"); n != 3 {
		t.Fatalf("got %d lines, want the original line plus two records:\n%s", n, before)
	}

	if err := KeepName(s, "MOXI"); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(file); string(after) != string(before) {
		t.Fatalf("a name already stored was written again:\n%s", after)
	}

	if err := KeepName(s, "ZEIT"); err != nil {
		t.Fatal(err)
	}
	if m, _ := scan(file, 0, meta{}, parseLine); m.AgentName != "ZEIT" {
		t.Fatalf("agent-name %q after a new name, want ZEIT", m.AgentName)
	}

	s.PID, s.procStart = os.Getpid(), procStartOf(os.Getpid())
	if err := KeepName(s, "OTHER"); err == nil {
		t.Fatal("a running session was renamed behind its back")
	}
}
