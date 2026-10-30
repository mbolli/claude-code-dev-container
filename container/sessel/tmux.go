package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// The handoff replaces sessel with tmux, so the ssh session that ran sessel
// becomes the tmux client. Session names match the fish devresume:
// <repo>-<uuid8> for a resume, <repo> for a fresh session.

var tmuxUnsafe = regexp.MustCompile(`[^A-Za-z0-9_-]`)

func tmuxName(cwd string) string {
	return tmuxUnsafe.ReplaceAllString(filepath.Base(cwd), "-")
}

// LANG and -u: tmux decides per client whether the terminal is UTF-8 from the
// locale, and without one draws box characters as ACS, which renders as
// underscores.
func tmuxCmd(args ...string) *exec.Cmd {
	c := exec.Command("tmux", append([]string{"-u"}, args...)...)
	c.Env = append(os.Environ(), "LANG=C.UTF-8")
	return c
}

func hasTmuxSession(name string) bool {
	return tmuxCmd("has-session", "-t", "="+name).Run() == nil
}

// ensureSession starts a detached tmux session running argv, unless one of
// that name exists.
func ensureSession(name, dir string, argv ...string) error {
	if hasTmuxSession(name) {
		return nil
	}
	args := append([]string{"new-session", "-d", "-s", name, "-c", dir, "-e", "LANG=C.UTF-8"}, argv...)
	if out, err := tmuxCmd(args...).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// execTmux ends this process and becomes a tmux client on target. Inside tmux
// it switches the client instead, since attaching would nest.
func execTmux(target string, pre ...string) error {
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		return err
	}
	verb := []string{"attach-session", "-t", target}
	if os.Getenv("TMUX") != "" {
		c, err := callerClient()
		if err != nil {
			return err
		}
		verb = []string{"switch-client", "-c", c, "-t", target}
	}
	args := []string{"tmux", "-u"}
	args = append(args, pre...)
	args = append(args, verb...)
	env := append(os.Environ(), "LANG=C.UTF-8")
	return syscall.Exec(tmux, args, env)
}

// callerClient is the client showing the pane sessel runs in. switch-client
// without -c acts on the most recently active client, which from a pane nobody
// is looking at is someone else's terminal.
func callerClient() (string, error) {
	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		return "", fmt.Errorf("inside tmux but TMUX_PANE is unset, not switching anything")
	}
	sess, err := tmuxCmd("display-message", "-p", "-t", pane, "#{session_name}").Output()
	if err != nil {
		return "", fmt.Errorf("find this pane's session: %w", err)
	}
	out, err := tmuxCmd("list-clients", "-t", "="+strings.TrimSpace(string(sess)),
		"-F", "#{client_activity} #{client_tty}").Output()
	if err != nil {
		return "", err
	}
	best, bestAt := "", ""
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		at, tty, ok := strings.Cut(line, " ")
		if ok && at > bestAt { // same width epoch seconds, so string order works
			best, bestAt = tty, at
		}
	}
	if best == "" {
		return "", fmt.Errorf("no terminal is showing this tmux session, so there is nothing to switch")
	}
	return best, nil
}

// Resume opens a session that is not running, in its own tmux session, with
// Remote Control on and the title as the name the phone shows.
func Resume(s *Session) error {
	name, err := ResumeDetached(s)
	if err != nil {
		return err
	}
	return execTmux("=" + name)
}

// ResumeDetached starts Resume's tmux session without attaching to it.
// Non-Claude sessions use '@' before the agent name because tmuxName only
// emits [A-Za-z0-9_-], so <repo>@codex cannot collide with a repo named
// <repo>-codex.
func ResumeDetached(s *Session) (string, error) {
	switch s.AgentName() {
	case AgentCodex:
		name := tmuxName(s.Cwd) + "@codex-" + s.UUID[:8]
		return name, ensureSession(name, s.Cwd, "codex", "resume", s.UUID)
	case AgentAgy:
		name := tmuxName(s.Cwd) + "@agy-" + s.UUID[:8]
		return name, ensureSession(name, s.Cwd, "agy", "--remote-control", "--conversation", s.UUID)
	default:
		name := tmuxName(s.Cwd) + "-" + s.UUID[:8]
		return name, ensureSession(name, s.Cwd, "claude", "--remote-control", s.Title, "--resume", s.UUID)
	}
}

// Attach goes to the pane a live session runs in (session:@window.%pane).
func Attach(target string) error {
	sess, rest, _ := strings.Cut(target, ":")
	win, pane, _ := strings.Cut(rest, ".")
	var pre []string
	if win != "" {
		pre = append(pre, "select-window", "-t", sess+":"+win, ";")
	}
	if pane != "" {
		pre = append(pre, "select-pane", "-t", pane, ";")
	}
	return execTmux(sess, pre...)
}

// StartDetached starts a fresh session in dir without attaching to it.
func StartDetached(dir, agent string) (string, error) {
	switch agent {
	case AgentCodex:
		name := tmuxName(dir) + "@codex"
		return name, ensureSession(name, dir, "codex")
	case AgentAgy:
		name := tmuxName(dir) + "@agy"
		return name, ensureSession(name, dir, "agy", "--remote-control")
	default:
		name := tmuxName(dir)
		return name, ensureSession(name, dir, "claude", "--remote-control", filepath.Base(dir))
	}
}

// Start opens a fresh session in dir, like devwork.
func Start(dir, agent string) error {
	name, err := StartDetached(dir, agent)
	if err != nil {
		return err
	}
	return execTmux("=" + name)
}

// MoveQuestion asks before Open stops a session that has no pane of its own.
func MoveQuestion(s *Session, titleW int) string {
	t := oneLine(s.Title, titleW)
	if s.Spawner == "" {
		return fmt.Sprintf("%q runs outside tmux (pid %d). Stop it and continue it in tmux?", t, s.PID)
	}
	q := fmt.Sprintf("%q runs headless under the %s spawner, no screen to attach to. "+
		"Stop it and continue it in tmux? The phone keeps it under a new link", t, spawnerName(s.Spawner))
	if s.Busy {
		q += ", and it is mid-reply: stopping now cuts that reply off"
	}
	return q + "."
}

// spawnerName turns a spawner's pane (serve:@19.%19) into its window name.
func spawnerName(pane string) string {
	out, err := tmuxCmd("display-message", "-p", "-t", pane, "#{window_name}").Output()
	if name := strings.TrimSpace(string(out)); err == nil && name != "" {
		return name
	}
	return pane
}

// Open does what Enter does: resume, attach, or move into tmux.
func Open(s *Session) error {
	switch s.State {
	case LiveTmux:
		return Attach(s.Tmux)
	case LiveOutside:
		// A process outside tmux cannot be pulled in. Stop it and continue the
		// same conversation in tmux; the transcript holds all of it.
		if err := Stop(s); err != nil {
			return err
		}
		s.State = Dead
	}
	return Resume(s)
}
