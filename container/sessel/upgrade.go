package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The updater only repoints ~/.local/bin/claude; running processes keep their
// binary, and a spawner starts its sessions with its own. upgrade restarts
// whatever runs an older version, but only once it has been idle for a while:
// a restarted spawner takes back its newest session, the others come back the
// next time someone writes to them.

const quietFor = 10 * time.Minute

type regEntry struct {
	PID             int    `json:"pid"`
	ProcStart       string `json:"procStart"`
	SessionID       string `json:"sessionId"`
	Name            string `json:"name"`
	Cwd             string `json:"cwd"`
	Entrypoint      string `json:"entrypoint"`
	Status          string `json:"status"`
	StatusUpdatedAt int64  `json:"statusUpdatedAt"`
}

// liveRegistry maps the pid of every running claude to its registry entry.
func liveRegistry() map[int]regEntry {
	out := map[int]regEntry{}
	files, _ := filepath.Glob(filepath.Join(claudeHome(), "sessions", "*.json"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var r regEntry
		if json.Unmarshal(b, &r) == nil && alive(r.PID, r.ProcStart) {
			out[r.PID] = r
		}
	}
	return out
}

func quiet(r regEntry, ok bool) bool {
	return ok && r.Status == "idle" && time.Since(time.UnixMilli(r.StatusUpdatedAt)) > quietFor
}

// claudeVersion is the version of the binary a process runs, e.g. "2.1.289".
func claudeVersion(pid int) string {
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return ""
	}
	exe = strings.TrimSuffix(exe, " (deleted)")
	if filepath.Base(filepath.Dir(exe)) != "versions" {
		return ""
	}
	return filepath.Base(exe)
}

func cmdline(pid int) []string {
	b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	return strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
}

func childrenOf(pid int) []int {
	var out []int
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		if c, err := strconv.Atoi(e.Name()); err == nil && ppidOf(c) == pid {
			out = append(out, c)
		}
	}
	return out
}

// claudeIn finds the claude process of a pane: the pane process itself, or the
// child of a `bash -c 'claude …; exec bash'` wrapper.
func claudeIn(panePID int) int {
	if claudeVersion(panePID) != "" {
		return panePID
	}
	for _, c := range childrenOf(panePID) {
		if claudeVersion(c) != "" {
			return c
		}
	}
	return 0
}

type pane struct {
	ID, Session, Window, Path, Start string
	PID                              int
}

func panes() []pane {
	b, err := tmuxCmd("list-panes", "-a", "-F", "#{pane_id}\t#{session_name}\t#{window_name}\t#{pane_current_path}\t#{pane_pid}\t#{pane_start_command}").Output()
	if err != nil {
		return nil
	}
	var out []pane
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		f := strings.SplitN(line, "\t", 6)
		if len(f) < 6 {
			continue
		}
		pid, _ := strconv.Atoi(f[4])
		out = append(out, pane{f[0], f[1], f[2], f[3], f[5], pid})
	}
	return out
}

func attached(session string) bool {
	b, _ := tmuxCmd("list-clients", "-t", "="+session).Output()
	return strings.TrimSpace(string(b)) != ""
}

func latestClaude() string {
	home, _ := os.UserHomeDir()
	p, err := filepath.EvalSymlinks(filepath.Join(home, ".local", "bin", "claude"))
	if err != nil {
		return ""
	}
	return filepath.Base(p)
}

func has(args []string, flag string) bool {
	for _, a := range args {
		if a == flag || strings.HasPrefix(a, flag+"=") {
			return true
		}
	}
	return false
}

func cmdUpgrade(args []string) error {
	fl := flag.NewFlagSet("upgrade", flag.ExitOnError)
	dry := fl.Bool("n", false, "only show what would be restarted")
	noFetch := fl.Bool("no-update", false, "skip `claude update`")
	fl.Parse(args)

	if !*noFetch && !*dry {
		if out, err := exec.Command("claude", "update").CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "sessel: claude update: %s\n", strings.TrimSpace(string(out)))
		}
	}
	latest := latestClaude()
	if latest == "" {
		return fmt.Errorf("cannot resolve ~/.local/bin/claude")
	}
	reg := liveRegistry()
	restarted := 0

	for _, p := range panes() {
		pid := claudeIn(p.PID)
		ver := claudeVersion(pid)
		if pid == 0 || ver == latest {
			continue
		}
		args := cmdline(pid)
		label := fmt.Sprintf("%s:%s (%s, %s)", p.Session, p.Window, ver, strings.Join(args[1:min(len(args), 3)], " "))
		var argv []string // empty: respawn with the pane's own start command
		switch {
		case has(args, "remote-control") && has(args, "--spawn"):
			for _, c := range childrenOf(pid) {
				if r, ok := reg[c]; !quiet(r, ok) {
					fmt.Printf("wait   %s: session %s is %s\n", label, r.Name, orUnknown(r.Status))
					goto next
				}
			}
			if p.Session == serveTmux {
				argv = spawnerArgv(p.Path)
			}
		case has(args, "--remote-control"):
			r, ok := reg[pid]
			if !quiet(r, ok) || attached(p.Session) {
				fmt.Printf("wait   %s: %s, attached=%v\n", label, orUnknown(r.Status), attached(p.Session))
				continue
			}
			// resume this conversation, never start a fresh one in its place
			argv = []string{"claude", "--remote-control", r.Name, "--resume", r.SessionID}
		default:
			continue // workers follow their spawner; single-session hosts end on their own
		}
		fmt.Printf("restart %s -> %s\n", label, latest)
		if !*dry {
			// one remote-control per folder: the old one must be gone before the new one starts.
			// Keep the pane while it is empty, or a pane running claude directly closes with it.
			tmuxCmd("set-option", "-p", "-t", p.ID, "remain-on-exit", "on").Run()
			stop(pid)
			rargs := []string{"respawn-pane", "-k", "-t", p.ID, "-c", p.Path}
			if len(argv) > 0 {
				rargs = append(rargs, "-e", "LANG=C.UTF-8")
				rargs = append(rargs, argv...)
			}
			out, err := tmuxCmd(rargs...).CombinedOutput()
			tmuxCmd("set-option", "-p", "-u", "-t", p.ID, "remain-on-exit").Run()
			if err != nil {
				fmt.Fprintf(os.Stderr, "sessel: %s: %s\n", label, strings.TrimSpace(string(out)))
				continue
			}
			restarted++
		}
	next:
	}
	if restarted == 0 && !*dry {
		fmt.Printf("nothing restarted, latest is %s\n", latest)
	}
	return nil
}

func stop(pid int) {
	if proc, err := os.FindProcess(pid); err == nil {
		proc.Signal(syscall.SIGTERM)
	}
	for i := 0; i < 30 && claudeVersion(pid) != ""; i++ {
		time.Sleep(500 * time.Millisecond)
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
