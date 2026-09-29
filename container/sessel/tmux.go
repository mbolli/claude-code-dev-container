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
	name := tmuxName(s.Cwd) + "-" + s.UUID[:8]
	if err := ensureSession(name, s.Cwd, "claude", "--remote-control", s.Title, "--resume", s.UUID); err != nil {
		return err
	}
	return execTmux("=" + name)
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

// Start opens a fresh session in dir, like devwork.
func Start(dir string) error {
	name := tmuxName(dir)
	if err := ensureSession(name, dir, "claude", "--remote-control", filepath.Base(dir)); err != nil {
		return err
	}
	return execTmux("=" + name)
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
