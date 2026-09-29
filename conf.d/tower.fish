# Shortcuts for the tower develop container.
# Docs: Obsidian → 1 PROJECTS/NAS/Develop Container.md

# --- Helpers ----------------------------------------------------------------

function __dev_ssh --description "Non-interactive ssh to the develop container"
    ssh -n -o BatchMode=yes -o ConnectTimeout=5 tower-dev $argv
end

function __dev_path --description "Normalise a /develop argument to an absolute remote path"
    set -l rel (string replace -r '^/?develop(/|$)' '' -- (string trim -- "$argv[1]"))
    if test -z "$rel"
        echo /develop
    else
        echo /develop/$rel
    end
end

function __dev_resolve --description "Resolve a /develop argument to a directory that exists on tower"
    set -l p (__dev_path "$argv[1]")
    if __dev_ssh "test -d '$p'"
        echo $p
        return 0
    end
    # "owner/repo", the form devclone takes, means the repo itself.
    if string match -q '/develop/*/*' -- $p
        set -l base /develop/(string replace -r '^.*/' '' -- $p)
        if __dev_ssh "test -d '$base'"
            echo $base
            return 0
        end
    end
    echo "dev: $p does not exist on tower-dev" >&2
    return 1
end

function __dev_rc_owner --description "PID of the Remote Control process serving a directory"
    __dev_ssh "d='$argv[1]'; "'for p in $(pgrep -f "claude remote-control" 2>/dev/null); do
        c=$(tr "\0" " " 2>/dev/null </proc/$p/cmdline) || continue
        case "$c" in
            "claude remote-control"*)
                [ "$(readlink /proc/$p/cwd 2>/dev/null)" = "$d" ] && echo "$p" ;;
        esac
    done'
end

function __dev_rc_panes --description "Text of every pane in the Remote Control tmux session"
    __dev_ssh 'for w in $(tmux list-windows -t rc -F "#{window_index}" 2>/dev/null); do
        tmux capture-pane -p -J -t rc:$w 2>/dev/null
    done'
end

# --- Repo list (cached, drives tab completion) ------------------------------

set -g __dev_cache_ttl 600
set -g __dev_cache /tmp/fish-devcode-repos-$USER

function __dev_repos --description "Top-level /develop dirs on tower-dev, cached"
    set -l age 99999
    test -f $__dev_cache; and set age (path mtime -R $__dev_cache)
    if test $age -gt $__dev_cache_ttl
        set -l out (__dev_ssh 'find /develop -mindepth 1 -maxdepth 1 -type d -not -name ".*" -printf "%P\n" | sort')
        # Keep a stale cache rather than blanking it when tower is unreachable.
        test -n "$out"; and printf '%s\n' $out >$__dev_cache
    end
    test -f $__dev_cache; and cat $__dev_cache
end

function __dev_complete_path --description "Complete a /develop repo or sub-directory"
    set -l token (commandline -ct)
    set -l pfx ''
    set -l rel $token
    if string match -qr '^/develop/' -- $token
        set pfx /develop/
        set rel (string replace '/develop/' '' -- $token)
    else if string match -qr '^develop/' -- $token
        set pfx develop/
        set rel (string replace 'develop/' '' -- $token)
    else if string match -qr '^/' -- $token
        echo /develop/
        return
    end
    if string match -q '*/*' -- $rel
        set -l base (string replace -r '/[^/]*$' '' -- $rel)
        for d in (__dev_ssh "find '/develop/$base' -mindepth 1 -maxdepth 1 -type d \
                -not -name '.*' -not -name node_modules -not -name vendor -printf '%P\n' | sort")
            echo $pfx$base/$d
        end
    else
        for r in (__dev_repos)
            echo $pfx$r
        end
    end
end

# --- Commands ---------------------------------------------------------------

function dev --description "SSH into the tower develop container"
    ssh tower-dev $argv
end

function devcode --description "Open a /develop repo from tower in VS Code (Remote-SSH)"
    set -l remote (__dev_resolve "$argv[1]"); or return 1
    code --folder-uri "vscode-remote://ssh-remote+tower-dev$remote"
end

function devclone --description "Clone a GitHub repo into /develop on tower"
    set -l repo $argv[1]
    if test -z "$repo"
        echo "devclone: usage: devclone <owner/repo>" >&2
        return 1
    end
    ssh tower-dev "cd /develop && git clone git@github.com:$repo.git \
        && /usr/local/bin/trust-develop.sh /develop"
    and rm -f $__dev_cache # let devcode completion pick it up
end

function devlink --description "Show the claude.ai/code link (and QR) for the tower environment"
    set -l url (__dev_rc_panes | string match -r 'https://claude\.ai/code\?environment=env_[A-Za-z0-9]+' | tail -1)
    if test -z "$url"
        echo "devlink: no environment link found, is the spawner running? (devhoststart)" >&2
        return 1
    end
    echo $url
    command -q qrencode; and qrencode -t ANSIUTF8 -m 1 -- $url
    if command -q wl-copy
        printf '%s' $url | wl-copy
        echo "(copied to clipboard)"
    end
end

function devrc --description "Attach to the Remote Control tmux session (detach: ctrl-b d)"
    # LANG and -u for the same reason as __dev_tmux: a non-login shell.
    ssh -t tower-dev 'LANG=C.UTF-8 tmux -u attach -t rc'
end

function devstatus --description "Show develop container status and Remote Control state"
    ssh -n tower 'docker ps --filter name=claude-code --format "{{.Names}}  {{.Status}}"
        echo
        docker exec -u dev claude-code sh -c '\''for w in $(tmux list-windows -t rc -F "#{window_index}" 2>/dev/null); do
            tmux capture-pane -p -J -t rc:$w 2>/dev/null
        done'\'' | grep -E "Ready|Capacity|claude\.ai/code|Session completed" | tail -6'
end

function devrestart --description "Restart the develop container (Remote Control reconnects itself)"
    ssh tower 'docker restart claude-code'
end

function devrebuild --description "Rebuild the develop image and recreate the container (container/rebuild.sh)"
    # rebuild.sh is the one place the run flags live. A copy of them here built
    # from the pre-repo Dockerfile in appdata and dropped the docker socket.
    ssh tower 'cd /mnt/user/develop/claude-code-dev-container/container && sh rebuild.sh'
end

function devtrust --description "Pre-accept the workspace trust dialog for every repo in /develop"
    __dev_ssh '/usr/local/bin/trust-develop.sh /develop'
end

# --- Sessions ---------------------------------------------------------------
# Remote Control sessions are server-side objects (session_01...), separate from
# the local transcript UUIDs under ~/.claude/projects. Only one Remote Control
# process may serve a directory at a time, so devattach stops the spawner for
# the duration and starts it again afterwards.
#
# Every session below runs under tmux on the container, named after the repo.
# A session on the bare ssh pty dies when the notebook suspends and the pty
# closes, and its subprocesses are left orphaned on PID 1.

function devls --description "List Claude sessions on tower; either id works for devresume (-a: all)"
    # dev-sessions lives in the image (container/dev-sessions) because the
    # registry, the transcripts and tmux are all on tower: one ssh call.
    __dev_ssh "dev-sessions ls $argv
        echo; echo \"tmux: \$(tmux list-sessions -F '#{session_name}' 2>/dev/null | tr '\n' ' ')\""
end

function __dev_tmux_name --description "tmux session name for a /develop directory"
    # string replace exits 1 when it changes nothing, which a clean name does.
    string replace -r '^.*/' '' -- "$argv[1]" | string replace -ra '[^A-Za-z0-9_-]' -
    return 0
end

function __dev_tmux --description "Run claude in a named tmux session on the container"
    # usage: __dev_tmux <dir> <tmux session name> [claude args...]
    set -l dir $argv[1]
    set -l name $argv[2]
    # -A attaches to an existing session and ignores the command, which is what
    # picking work back up should do.
    #
    # LANG and -u: tmux decides per client whether the terminal is UTF-8 from
    # LC_ALL/LC_CTYPE/LANG. This is a non-login shell, so /etc/profile.d never
    # runs and none of them are set, and tmux then mangles every box-drawing and
    # powerline glyph. -u tells tmux the same thing directly.
    ssh -t tower-dev "LANG=C.UTF-8 tmux -u new-session -A -s '$name' -c '$dir' claude $argv[3..]"
end

function __dev_complete_sessions --description "uuid, then repo and branch as the description, for devresume"
    __dev_ssh 'dev-sessions json' 2>/dev/null | jq -r '.[]
        | select(.cwd | startswith("/tmp") | not)
        | "\(.uuid)\t\(if .live then "● " else "" end)\(.cwd | sub("^/develop/?"; "") | if . == "" then "/develop" else . end) \(.branch)"'
end

function __dev_attach --description "Attach to a running tmux pane on the container (session:@window.%pane)"
    set -l sess (string split -m1 : -- $argv[1])[1]
    set -l win (string split -m1 . -- (string split -m1 : -- $argv[1])[2])[1]
    set -l pane (string split -m1 . -- (string split -m1 : -- $argv[1])[2])[2]
    ssh -t tower-dev "LANG=C.UTF-8 tmux -u select-window -t '$sess:$win' \; select-pane -t '$pane' \; attach -t '$sess'"
end

function devwork --description "Start or reattach a Claude session for a repo (tmux + Remote Control)"
    set -l dir (__dev_resolve "$argv[1]"); or return 1
    __dev_tmux $dir (__dev_tmux_name $dir)
end

function devresume --description "Resume a session by uuid, session_ id or claude.ai URL (picker if none)"
    if not set -q argv[1]
        __dev_tmux /develop picker --resume
        return
    end

    # uuid, cwd and the tmux pane it runs in if it is live, from either id.
    set -l r (__dev_ssh "dev-sessions resolve "(string escape -- $argv[1])); or return 1
    set -l f (string split \t -- $r)
    set -l uuid $f[1]
    set -l dir $f[2]
    set -l live $f[3]

    # Already running somewhere: attach to it rather than start a second
    # process writing the same transcript.
    if test -n "$live"
        echo "$uuid is already running in $live, attaching"
        __dev_attach $live
        return
    end

    # Named after the conversation, not the directory, so -A neither lands in
    # a devwork session for the same repo nor in a different conversation.
    __dev_tmux $dir (__dev_tmux_name $dir)-(string sub -l 8 -- $uuid) --resume $uuid
end

function devcontinue --description "Continue the last Claude session in a tower directory"
    set -l dir (__dev_resolve "$argv[1]"); or return 1
    __dev_tmux $dir (__dev_tmux_name $dir) --continue
end

function devattach --description "Re-attach a Remote Control session by id or claude.ai/code URL"
    argparse f/force -- $argv; or return 1
    if not set -q argv[1]
        echo "devattach: usage: devattach [-f] <session-id|claude.ai-url> [directory]" >&2
        return 1
    end
    set -l sid (string replace -r '^.*/' '' -- $argv[1] | string trim)
    set -l dir /develop
    if set -q argv[2]
        set dir (__dev_resolve "$argv[2]"); or return 1
    end

    # One Remote Control process per directory, so the spawner has to let go.
    set -l owner (__dev_rc_owner $dir)
    set -l restart 0
    if test -n "$owner"
        if not set -q _flag_force
            read -l -P "Remote Control already serves $dir (pid $owner). Stop it, attach, restart after? [y/N] " reply
            or return 1
            string match -qr '^[yY]' -- $reply; or return 1
        end
        __dev_ssh "kill $owner"; and echo "spawner stopped (pid $owner)"
        set restart 1
    end

    echo "attaching $sid in $dir"
    ssh -t tower-dev "cd $dir && claude remote-control --session-id $sid --name attached"

    if test $restart -eq 1
        echo "restarting the spawner"
        devhoststart
    end
end

function devhoststop --description "Stop the Remote Control spawner (frees /develop for devattach)"
    set -l owner (__dev_rc_owner /develop)
    if test -z "$owner"
        echo "not running"
        return 1
    end
    __dev_ssh "kill $owner"; and echo "stopped (pid $owner)"
end

function devhoststart --description "Start the Remote Control spawner in /develop"
    set -l owner (__dev_rc_owner /develop)
    if test -n "$owner"
        echo "already running (pid $owner)"
        return 0
    end
    __dev_ssh 'c="claude remote-control --name tower --spawn same-dir --capacity 8; exec bash"
        if tmux has-session -t rc 2>/dev/null; then
            tmux new-window -t rc -c /develop "$c"
        else
            cd /develop && tmux new-session -d -s rc "$c"
        fi
        echo started'
end
