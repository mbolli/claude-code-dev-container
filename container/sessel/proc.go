package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// tmuxPanes maps pane root PIDs and pane IDs (%N) to session:@window.%pane.
type tmuxPanes struct {
	byPID    map[int]string
	byPaneID map[string]string
}

// loadTmuxPanes indexes the active tmux panes from panes() so process lookup
// does not shell out to tmux a second time.
func loadTmuxPanes() tmuxPanes {
	tp := tmuxPanes{
		byPID:    map[int]string{},
		byPaneID: map[string]string{},
	}
	for _, p := range panes() {
		target := p.Session + ":" + p.WindowID + "." + p.ID
		if p.PID > 0 {
			tp.byPID[p.PID] = target
		}
		tp.byPaneID[p.ID] = target
	}
	return tp
}

// targetForPID resolves the tmux pane a process runs in, either from TMUX_PANE
// in its environment or by walking parent PIDs up to the pane's root process.
func (tp tmuxPanes) targetForPID(pid int) string {
	if pid <= 0 {
		return ""
	}
	if pane := procEnv(pid, "TMUX_PANE"); pane != "" {
		if t := tp.byPaneID[pane]; t != "" {
			return t
		}
	}
	cur := pid
	for i := 0; i < 16 && cur > 1; i++ {
		if t := tp.byPID[cur]; t != "" {
			return t
		}
		cur = ppidOf(cur)
	}
	return ""
}

// agentProc holds the /proc snapshot for a running codex or agy process.
type agentProc struct {
	pid       int
	comm      string
	procStart string
	args      []string
	fds       []string
	tmux      string
}

// scanAgentProcs walks /proc once and reads file descriptors only for codex
// and agy processes.
func scanAgentProcs(tp tmuxPanes) []agentProc {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []agentProc
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		commBytes, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
		if err != nil {
			continue
		}
		comm := strings.TrimSpace(string(commBytes))
		if comm != "codex" && comm != "agy" {
			continue
		}
		pstart := procStartOf(pid)
		if pstart == "" {
			continue
		}
		fdEntries, _ := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
		fds := make([]string, 0, len(fdEntries))
		for _, fd := range fdEntries {
			if target, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, fd.Name())); err == nil {
				fds = append(fds, strings.TrimSuffix(target, " (deleted)"))
			}
		}
		out = append(out, agentProc{
			pid:       pid,
			comm:      comm,
			procStart: pstart,
			args:      cmdline(pid),
			fds:       fds,
			tmux:      tp.targetForPID(pid),
		})
	}
	return out
}

// procEnv reads a single environment variable from /proc/<pid>/environ.
func procEnv(pid int, key string) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return ""
	}
	prefix := []byte(key + "=")
	for _, entry := range bytes.Split(b, []byte{0}) {
		if bytes.HasPrefix(entry, prefix) {
			return string(entry[len(prefix):])
		}
	}
	return ""
}

// gitBranchOf reads the checked-out branch name from dir/.git/HEAD when the
// transcript or database did not record one.
func gitBranchOf(dir string) string {
	if dir == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(dir, ".git", "HEAD"))
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(b))
	if after, ok := strings.CutPrefix(line, "ref: refs/heads/"); ok {
		return after
	}
	return ""
}
