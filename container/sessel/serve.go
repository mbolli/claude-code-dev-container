package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// One `claude remote-control` server serves one directory, and the phone can
// only start sessions where one runs. serve keeps one per recently used repo
// in the tmux session "serve", a window per repo. Nothing outside that tmux
// session is touched: the /develop spawner in "rc" belongs to the entrypoint.

const (
	developRoot = "/develop"
	serveTmux   = "serve"
	serveDays   = 14
)

var repoNameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)

// wantedRepos is every top-level repo with a session in the last serveDays,
// plus git repos whose directory changed in that time. The second covers a
// project from `sessel new` that has no transcript yet; requiring .git keeps
// out folders that are merely touched, like a directory of skill clones.
func wantedRepos(st *Store) []string {
	cutoff := time.Now().Add(-serveDays * 24 * time.Hour)
	set := map[string]bool{}
	for _, s := range st.Sessions {
		if s.Mtime.Before(cutoff) || !strings.HasPrefix(s.Cwd, developRoot+"/") {
			continue
		}
		top := strings.SplitN(strings.TrimPrefix(s.Cwd, developRoot+"/"), "/", 2)[0]
		set[filepath.Join(developRoot, top)] = true
	}
	entries, _ := os.ReadDir(developRoot)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(developRoot, e.Name())
		if fi, err := e.Info(); err == nil && fi.ModTime().After(cutoff) {
			if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
				set[dir] = true
			}
		}
	}
	var out []string
	for p := range set {
		name := filepath.Base(p)
		if strings.HasPrefix(name, ".") || !repoNameRe.MatchString(name) {
			continue // hidden scratch dirs and anything unsafe as a window name
		}
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

type spawner struct {
	Window string
	PID    int
}

func servedWindows() map[string]spawner {
	out := map[string]spawner{}
	b, err := tmuxCmd("list-windows", "-t", "="+serveTmux, "-F", "#{window_name}\t#{pane_pid}").Output()
	if err != nil {
		return out
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		name, pid, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		n, _ := strconv.Atoi(pid)
		out[name] = spawner{name, n}
	}
	return out
}

// busy reports whether a spawner has spawned sessions running: stopping it
// would end whatever someone is doing from the phone.
func busy(pid int) bool {
	if pid <= 0 {
		return false
	}
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		child, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if ppidOf(child) == pid {
			return true
		}
	}
	return false
}

func ppidOf(pid int) int {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(f[1]) // ppid, field 4 overall
	return n
}

// trusted reads ~/.claude.json the way trust-develop.sh writes it.
func trusted(dir string) bool {
	home, _ := os.UserHomeDir()
	b, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		return false
	}
	var v struct {
		Projects map[string]struct {
			HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if json.Unmarshal(b, &v) != nil {
		return false
	}
	return v.Projects[dir].HasTrustDialogAccepted
}

// trust runs trust-develop.sh only when needed. A live Claude may write its
// in-memory copy of ~/.claude.json back over it, which the hourly serve then
// repairs; calling it only on a gap keeps those writes rare.
func trust(dir string) error {
	if trusted(dir) {
		return nil
	}
	out, err := exec.Command("trust-develop.sh", developRoot).CombinedOutput()
	if err != nil {
		return fmt.Errorf("trust %s: %s", dir, strings.TrimSpace(string(out)))
	}
	return nil
}

func startSpawner(dir string) error {
	if err := trust(dir); err != nil {
		return err
	}
	name := filepath.Base(dir)
	argv := []string{"claude", "remote-control", "--name", name,
		"--spawn", "same-dir", "--no-create-session-in-dir", "--capacity", "4"}
	var args []string
	if hasTmuxSession(serveTmux) {
		args = append([]string{"new-window", "-d", "-t", "=" + serveTmux + ":", "-n", name, "-c", dir}, argv...)
	} else {
		args = append([]string{"new-session", "-d", "-s", serveTmux, "-n", name, "-c", dir, "-e", "LANG=C.UTF-8"}, argv...)
	}
	if out, err := tmuxCmd(args...).CombinedOutput(); err != nil {
		return fmt.Errorf("start %s: %s", name, strings.TrimSpace(string(out)))
	}
	return nil
}

type servePlan struct {
	Start, Stop, Keep, Busy []string
}

func planServe(st *Store) (servePlan, map[string]spawner) {
	running := servedWindows()
	var p servePlan
	want := map[string]bool{}
	for _, dir := range wantedRepos(st) {
		name := filepath.Base(dir)
		want[name] = true
		if _, ok := running[name]; ok {
			p.Keep = append(p.Keep, dir)
		} else {
			p.Start = append(p.Start, dir)
		}
	}
	for name, sp := range running {
		if want[name] {
			continue
		}
		if busy(sp.PID) {
			p.Busy = append(p.Busy, name)
		} else {
			p.Stop = append(p.Stop, name)
		}
	}
	sort.Strings(p.Stop)
	sort.Strings(p.Busy)
	return p, running
}

func cmdServe(args []string) error {
	fl := flag.NewFlagSet("serve", flag.ExitOnError)
	dry := fl.Bool("n", false, "only show what would change")
	status := fl.Bool("status", false, "list the spawners")
	fl.Parse(args)
	st := mustLoad()
	p, _ := planServe(st)

	if *status {
		for _, d := range p.Keep {
			fmt.Printf("serving  %s\n", filepath.Base(d))
		}
		for _, d := range p.Start {
			fmt.Printf("missing  %s\n", filepath.Base(d))
		}
		for _, n := range p.Busy {
			fmt.Printf("busy     %s (no longer recent, kept: phone session running)\n", n)
		}
		for _, n := range p.Stop {
			fmt.Printf("stale    %s\n", n)
		}
		return nil
	}
	for _, d := range p.Start {
		fmt.Printf("start  %s\n", filepath.Base(d))
		if !*dry {
			if err := startSpawner(d); err != nil {
				fmt.Fprintln(os.Stderr, "sessel:", err)
			}
		}
	}
	for _, n := range p.Stop {
		fmt.Printf("stop   %s\n", n)
		if !*dry {
			tmuxCmd("kill-window", "-t", "="+serveTmux+":="+n).Run()
		}
	}
	for _, n := range p.Busy {
		fmt.Printf("keep   %s (phone session running)\n", n)
	}
	if len(p.Start)+len(p.Stop) == 0 {
		fmt.Printf("nothing to do, %d served\n", len(p.Keep))
	}
	return nil
}

// NewProject creates /develop/<name>, runs git init, trusts it and serves it
// to the phone. An existing directory is only served.
func NewProject(name string) (string, error) {
	if !repoNameRe.MatchString(name) || strings.HasPrefix(name, ".") {
		return "", fmt.Errorf("%q: use letters, digits, '.', '_' and '-', not starting with '.'", name)
	}
	dir := filepath.Join(developRoot, name)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if err := os.Mkdir(dir, 0o755); err != nil {
			return "", err
		}
		if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
			return "", fmt.Errorf("git init: %s", strings.TrimSpace(string(out)))
		}
	}
	if _, ok := servedWindows()[name]; !ok {
		if err := startSpawner(dir); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func cmdNew(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: sessel new <name>")
	}
	dir, err := NewProject(args[0])
	if err != nil {
		return err
	}
	fmt.Printf("%s is ready and on your phone as %q\n", dir, filepath.Base(dir))
	return nil
}
