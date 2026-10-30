// sessel lists, peeks, resumes, moves, renames and deletes Claude Code, Codex
// and Antigravity (agy) sessions on this container.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
)

const usage = `sessel: Claude Code, Codex and Antigravity sessions on this container

  sessel                                  browse them (TUI)
  sessel ls [-a] [-agent claude|codex|agy]
                                          table, newest first (-a: include /tmp scratchpads)
  sessel open [-d] <id>                   what Enter does: resume, attach, or move into tmux
                                          (-d: resume a stopped one in tmux, don't attach)
  sessel resolve <id>                     uuid, cwd, state (dead|tmux|outside), tmux target, agent
  sessel json                             every session, for tools
  sessel peek <id>                        what the session was about and where it stopped
  sessel rename <id> <title>              set a title, as /rename does
  sessel rm [-n] [-f] <id>...             delete for real (-n: only list, -f: no prompt)
  sessel new [-agent claude|codex|agy] [<agent>:]<name>
                                          create /develop/<name>, git init, and serve or start
  sessel serve [-n] [-status]             keep a Claude spawner per recent repo, for the phone
  sessel upgrade [-n]                     restart idle spawners and sessions on the newest claude

<id> is a session uuid, a session_01… or cse_01… id, or a claude.ai/code URL.
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
	agent := fs.String("agent", "", "filter by agent (claude, codex or agy)")
	fs.Parse(args)
	if *agent != "" && !validAgent(*agent) {
		return fmt.Errorf("unknown agent %q (use claude, codex or agy)", *agent)
	}
	st := mustLoad()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, " \tAGE\tAGENT\tREPO\tTITLE\tUUID (--resume)\tREMOTE CONTROL ID")
	n := 0
	for _, s := range st.Sessions {
		if *agent != "" && s.AgentName() != *agent {
			continue
		}
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
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			marker(s.State), age(s.Mtime), s.AgentName(), oneLine(s.Repo(), 28), oneLine(s.Title, 48), s.UUID, rc)
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

// cmdResolve prints uuid, cwd, state, tmux target and agent, tab-separated.
// The fish functions read this; state is what tells "not running" from
// "running outside tmux", which an empty tmux target alone cannot.
func cmdResolve(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: sessel resolve <id>")
	}
	s, err := mustLoad().Resolve(args[0])
	if err != nil {
		return err
	}
	fmt.Printf("%s\t%s\t%s\t%s\t%s\n", s.UUID, s.Cwd, s.State, s.Tmux, s.AgentName())
	return nil
}

func cmdOpen(args []string) error {
	fl := flag.NewFlagSet("open", flag.ExitOnError)
	detached := fl.Bool("d", false, "resume a stopped session in tmux without attaching")
	fl.Parse(args)
	if fl.NArg() != 1 {
		return fmt.Errorf("usage: sessel open [-d] <id>")
	}
	s, err := mustLoad().Resolve(fl.Arg(0))
	if err != nil {
		return err
	}
	if *detached {
		if s.State != Dead {
			return fmt.Errorf("%s is still running", s.UUID)
		}
		name, err := ResumeDetached(s)
		if err == nil {
			fmt.Printf("resumed %s in tmux %s\n", s.UUID, name)
		}
		return err
	}
	if s.State == LiveOutside {
		if s.PID == 0 {
			return Stop(s)
		}
		if !confirm(MoveQuestion(s, 50)) {
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
