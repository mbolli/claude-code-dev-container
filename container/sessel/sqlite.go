package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// sqliteQueryJSON runs a read-only query via the sqlite3 CLI so sessel builds
// with CGO_ENABLED=0 and no external driver. A missing database file is normal
// when an agent has never been used and returns (nil, nil).
func sqliteQueryJSON[T any](dbPath, query string) ([]T, error) {
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		fmt.Fprintf(os.Stderr, "sessel: sqlite3 %s: %v\n", filepath.Base(dbPath), err)
		return nil, err
	}
	var stderr bytes.Buffer
	cmd := exec.Command("sqlite3", "-readonly", "-json", dbPath, query)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		fmt.Fprintf(os.Stderr, "sessel: sqlite3 %s: %s\n", filepath.Base(dbPath), msg)
		return nil, fmt.Errorf("sqlite3 %s: %s", dbPath, msg)
	}
	out = bytes.TrimSpace(out)
	if len(out) == 0 {
		return nil, nil
	}
	var rows []T
	if err := json.Unmarshal(out, &rows); err != nil {
		fmt.Fprintf(os.Stderr, "sessel: sqlite3 %s: decode json: %v\n", filepath.Base(dbPath), err)
		return nil, err
	}
	return rows, nil
}

// sqliteExec runs a write statement with a busy timeout so concurrent agent
// readers do not cause immediate SQLITE_BUSY failures.
func sqliteExec(dbPath, sql string) error {
	stmt := "PRAGMA busy_timeout = 2000; " + sql
	out, err := exec.Command("sqlite3", dbPath, stmt).CombinedOutput()
	if err != nil {
		return fmt.Errorf("sqlite3 %s: %s", dbPath, strings.TrimSpace(string(out)))
	}
	return nil
}

// sqlQuote escapes a string literal for SQLite SQL.
func sqlQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
