package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// An Artifact is one path a session left behind.
type Artifact struct {
	Path  string
	Files int
	Bytes int64
}

// Footprint is everything a real delete removes for one session. It leaves
// history.jsonl (shared by every session, by decision) and the project's
// memory dir alone, and it cannot reach the Remote Control entries on
// claude.ai.
func (st *Store) Footprint(s *Session) []Artifact {
	tmpRoot := fmt.Sprintf("/tmp/claude-%d", os.Getuid())
	candidates := []string{
		s.File,
		filepath.Join(filepath.Dir(s.File), s.UUID),
		filepath.Join(st.Home, "file-history", s.UUID),
		filepath.Join(st.Home, "uploads", s.UUID),
		filepath.Join(st.Home, "session-env", s.UUID),
		filepath.Join(tmpRoot, s.Project, s.UUID),
	}
	candidates = append(candidates, st.registryFiles(s.UUID)...)
	// Each process also leaves sessions/<pid>.<hash>.key. Claude removes it on a
	// clean exit but not when killed, so they pile up across rebuilds. Keys are
	// named by pid and pids are reused, so only take them while no process has
	// that pid: then they can only belong to a dead one.
	for _, r := range st.registry[s.UUID] {
		if r.PID <= 0 {
			continue
		}
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", r.PID)); err == nil {
			continue
		}
		m, _ := filepath.Glob(filepath.Join(st.Home, "sessions", fmt.Sprintf("%d.*.key", r.PID)))
		candidates = append(candidates, m...)
	}
	if p := filepath.Join(filepath.Dir(s.File), "bridge-pointer.json"); pointsAt(p, s) {
		candidates = append(candidates, p)
	}
	for _, b := range s.Bridges {
		id := strings.TrimPrefix(b, "session_")
		if !bridgeIDRe.MatchString(id) {
			continue // never glob anything not shaped like an id
		}
		m, _ := filepath.Glob(filepath.Join(st.Home, "bridge-spawn", "cse_"+id+"-*"))
		candidates = append(candidates, m...)
	}

	var out []Artifact
	for _, p := range candidates {
		if !within(p, st.Home) && !within(p, tmpRoot) {
			continue
		}
		if a, ok := measure(p); ok {
			out = append(out, a)
		}
	}
	return out
}

var bridgeIDRe = regexp.MustCompile(`^[A-Za-z0-9]+$`)

// within is true if p, cleaned, is strictly inside root.
func within(p, root string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(p))
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..")
}

func pointsAt(pointer string, s *Session) bool {
	b, err := os.ReadFile(pointer)
	if err != nil {
		return false
	}
	var v struct{ SessionID string }
	if json.Unmarshal(b, &v) != nil {
		return false
	}
	if v.SessionID == s.UUID {
		return true
	}
	for _, br := range s.Bridges {
		if normBridge(v.SessionID) == br {
			return true
		}
	}
	return false
}

// measure counts without following symlinks.
func measure(p string) (Artifact, bool) {
	fi, err := os.Lstat(p)
	if err != nil {
		return Artifact{}, false
	}
	a := Artifact{Path: p}
	if !fi.IsDir() {
		a.Files, a.Bytes = 1, fi.Size()
		return a, true
	}
	filepath.WalkDir(p, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		a.Files++
		if info, err := d.Info(); err == nil {
			a.Bytes += info.Size()
		}
		return nil
	})
	return a, true
}

func totals(arts []Artifact) (files int, bytes int64) {
	for _, a := range arts {
		files += a.Files
		bytes += a.Bytes
	}
	return
}

// Stop ends a running session: SIGTERM, then up to 10 s for it to exit. The
// pid is checked against its start time first, because pids are reused.
func Stop(s *Session) error {
	if s.State == Dead {
		return nil
	}
	if !alive(s.PID, s.procStart) {
		return nil // exited on its own since we looked
	}
	if err := syscall.Kill(s.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("stop %s (pid %d): %w", s.UUID[:8], s.PID, err)
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		if !alive(s.PID, s.procStart) {
			s.State = Dead
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("%s (pid %d) did not exit within 10 s, nothing deleted", s.UUID[:8], s.PID)
}

// Delete removes a session's footprint. It refuses a running session: stop it
// first, or the process keeps writing to the transcript it holds open.
func (st *Store) Delete(s *Session) error {
	if alive(s.PID, s.procStart) {
		return fmt.Errorf("%s is still running, stop it first", s.UUID[:8])
	}
	var errs []error
	for _, a := range st.Footprint(s) {
		if err := os.RemoveAll(a.Path); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Rename appends the entry Claude Code's own /rename writes. A running
// session holds its title in memory and would write it back over this.
func Rename(s *Session, title string) error {
	title = strings.TrimSpace(title)
	if title == "" {
		return fmt.Errorf("empty title")
	}
	if alive(s.PID, s.procStart) {
		return fmt.Errorf("%s is running and would overwrite this, use /rename inside it", s.UUID[:8])
	}
	entry, err := json.Marshal(struct {
		Type        string `json:"type"`
		CustomTitle string `json:"customTitle"`
		SessionID   string `json:"sessionId"`
	}{"custom-title", title, s.UUID})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.File, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if !endsWithNewline(s.File) {
		entry = append([]byte("\n"), entry...)
	}
	_, err = f.Write(append(entry, '\n'))
	return err
}

func endsWithNewline(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return true
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return true
	}
	b := make([]byte, 1)
	if _, err := f.ReadAt(b, fi.Size()-1); err != nil {
		return true
	}
	return b[0] == '\n'
}

func cmdRename(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: sessel rename <id> <title>")
	}
	s, err := mustLoad().Resolve(args[0])
	if err != nil {
		return err
	}
	return Rename(s, strings.Join(args[1:], " "))
}

func cmdRm(args []string) error {
	fl := flag.NewFlagSet("rm", flag.ExitOnError)
	dry := fl.Bool("n", false, "only list what would be removed")
	force := fl.Bool("f", false, "no prompt; stops running sessions")
	fl.Parse(args)
	if fl.NArg() == 0 {
		return fmt.Errorf("usage: sessel rm [-n] [-f] <id>...")
	}
	st := mustLoad()
	var sessions []*Session
	for _, id := range fl.Args() {
		s, err := st.Resolve(id)
		if err != nil {
			return err
		}
		sessions = append(sessions, s)
	}

	var files int
	var bytes int64
	var running []*Session
	for _, s := range sessions {
		arts := st.Footprint(s)
		f, b := totals(arts)
		files, bytes = files+f, bytes+b
		fmt.Printf("%s  %s  (%s)\n", s.UUID, s.Title, stateWords(s))
		for _, a := range arts {
			fmt.Printf("    %-8s %4d files  %s\n", humanBytes(a.Bytes), a.Files, a.Path)
		}
		if s.State != Dead {
			running = append(running, s)
		}
	}
	fmt.Printf("\n%d session(s), %d files, %s. Their Remote Control entries on claude.ai are not touched.\n",
		len(sessions), files, humanBytes(bytes))
	if *dry {
		return nil
	}
	if !*force {
		q := "Delete for good?"
		if len(running) > 0 {
			q = fmt.Sprintf("%d of them are running and will be stopped first. Delete for good?", len(running))
		}
		if !confirm(q) {
			return fmt.Errorf("nothing deleted")
		}
	}
	for _, s := range running {
		if err := Stop(s); err != nil {
			return err
		}
	}
	for _, s := range sessions {
		if err := st.Delete(s); err != nil {
			return err
		}
	}
	fmt.Println("deleted.")
	return nil
}

func confirm(q string) bool {
	fmt.Printf("%s [y/N] ", q)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.EqualFold(strings.TrimSpace(line), "y")
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fM", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0fK", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%dB", n)
}
