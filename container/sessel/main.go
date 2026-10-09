// sessel lists, peeks, resumes, moves, renames and deletes the Claude Code
// sessions on this container, addressed by either of their two ids.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
)

const usage = `sessel: Claude Code sessions on this container

  sessel                        browse them (TUI)
  sessel ls [-a]                table, newest first (-a: include /tmp scratchpads)
  sessel open <id>              what Enter does: resume, attach, or move into tmux
  sessel resolve <id>           uuid, cwd, state (dead|tmux|outside), tmux target
  sessel json                   every session, for tools
  sessel peek <id>              what the session was about and where it stopped
  sessel rename <id> <title>    set a title, as /rename does
  sessel rm [-n] [-f] <id>...   delete for real (-n: only list, -f: no prompt)
  sessel new <name>             create /develop/<name>, git init, serve it to the phone
  sessel serve [-n] [-status]   keep a spawner per recent repo, for the phone
  sessel upgrade [-n]           restart idle spawners and sessions on the newest claude

<id> is a transcript uuid, a session_01… or cse_01… id, or a claude.ai/code URL.
`

func main() {
	if len(os.Args) < 2 {
		os.Exit(runTUI())
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "ls":
		err = cmdLs(args)
	case "open":
		err = cmdOpen(args)
	case "resolve":
		err = cmdResolve(args)
	case "json":
		err = cmdJSON()
	case "peek":
		err = cmdPeek(args)
	case "rename":
		err = cmdRename(args)
	case "rm":
		err = cmdRm(args)
	case "new":
		err = cmdNew(args)
	case "serve":
		err = cmdServe(args)
	case "upgrade":
		err = cmdUpgrade(args)
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "sessel:", err)
		os.Exit(1)
	}
}

func mustLoad() *Store {
	st, err := Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "sessel:", err)
		os.Exit(1)
	}
	return st
}

func cmdLs(args []string) error {
	fs := flag.NewFlagSet("ls", flag.ExitOnError)
	all := fs.Bool("a", false, "include /tmp scratchpad sessions")
	fs.Parse(args)
	st := mustLoad()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, " \tAGE\tREPO\tTITLE\tUUID (--resume)\tREMOTE CONTROL ID")
	n := 0
	for _, s := range st.Sessions {
		if s.Scratch() && !*all {
			continue
		}
		if !*all && n >= 15 {
			break
		}
		n++
		rc := s.Bridge()
		if rc == "" {
			rc = "-"
		} else if len(s.Bridges) > 1 {
			rc = fmt.Sprintf("%s (+%d older)", rc, len(s.Bridges)-1)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			marker(s.State), age(s.Mtime), oneLine(s.Repo(), 28), oneLine(s.Title, 48), s.UUID, rc)
	}
	return w.Flush()
}

func marker(s State) string {
	switch s {
	case LiveTmux:
		return "●"
	case LiveOutside:
		return "◐"
	}
	return " "
}

// cmdResolve prints uuid, cwd, state and tmux target, tab-separated. The fish
// functions read this; state is what tells "not running" from "running
// outside tmux", which an empty tmux target alone cannot.
func cmdResolve(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: sessel resolve <id>")
	}
	s, err := mustLoad().Resolve(args[0])
	if err != nil {
		return err
	}
	fmt.Printf("%s\t%s\t%s\t%s\n", s.UUID, s.Cwd, s.State, s.Tmux)
	return nil
}

func cmdOpen(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: sessel open <id>")
	}
	s, err := mustLoad().Resolve(args[0])
	if err != nil {
		return err
	}
	if s.State == LiveOutside {
		q := fmt.Sprintf("%q runs outside tmux (pid %d). Stop it and continue it in tmux?", oneLine(s.Title, 50), s.PID)
		if !confirm(q) {
			return fmt.Errorf("left as it is")
		}
	}
	return Open(s)
}

func cmdJSON() error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(mustLoad().Sessions)
}
