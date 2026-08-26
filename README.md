# claude-code-dev-container

A self-hosted development container that runs [Claude Code](https://claude.ai/code)
alongside a full PHP / Go / Node / Python toolchain, reachable over SSH from
VS Code Remote-SSH and from the Claude mobile app.

It runs on an Unraid box, but nothing here is Unraid-specific beyond the
`nobody:users` (99:100) uid convention and the share paths.

The point of it: one always-on box holds every repo and every Claude session.
You open a repo in VS Code from your laptop, and pick the same work up on your
phone without the laptop staying awake.

## Layout

```
conf.d/      client side: the dev* commands          (fisher installs these)
completions/ client side: tab completion for them    (fisher installs these)
container/   server side: image, entrypoint, helper scripts
install.fish fisher-less installer, symlinks into ~/.config/fish
```

`conf.d/` and `completions/` sit at the repo root because that is the only
place [fisher](https://github.com/jorgebucaran/fisher) looks. Fisher copies
`functions/`, `completions/`, `conf.d/` and `themes/` from the root and ignores
everything else, so `container/` and this README are simply skipped.

## The container

`container/rebuild.sh` is the source of truth for how the container runs. It is
**not** defined by an Unraid template, so a change made through the Unraid UI or
an ad-hoc `docker run` is lost the moment the container is removed. Edit the
script instead.

```sh
./container/rebuild.sh
```

| Path | Purpose |
| --- | --- |
| `Dockerfile` | Debian sid base plus the toolchain |
| `entrypoint.sh` | sshd, Claude install, Remote Control host, session rebind |
| `trust-develop.sh` | Pre-accepts the workspace trust dialog for every repo |
| `rebind-sessions.sh` | Restores the previous run's Claude sessions on start |
| `rebuild.sh` | Build and recreate the container |

### Persistence boundary

Only two volumes survive a rebuild:

| Volume | Holds |
| --- | --- |
| `/home/dev` | Claude's own install, credentials, session history, shell profile |
| `/develop` | Your repos |

Anything installed into the image filesystem at runtime is discarded. Put it in
the `Dockerfile`.

`/home/dev` is bind-mounted at runtime and never built into the image, which is
what `.dockerignore` is for: without it the ~5 GB home is sent to the daemon as
build context on every build.

### What is inside

Debian sid, so nodejs 24 and python 3.13 come from apt with no NodeSource repo.
PHP comes from Sury's trixie suite because Debian has not done the 8.5
transition yet. openswoole is built from PECL against it, since Sury ships no
8.5 build. Plus Go, Task, uv, Composer, pnpm/yarn, Chromium (shared by
puppeteer and lighthouse rather than each downloading its own), ripgrep, fd,
bat, mdbook, mariadb-client, ffmpeg and imagemagick.

Version pins live in `ARG` lines at the top of the `Dockerfile`.

### Two container gotchas worth knowing

Both are already handled, and both cost real debugging time to find:

**`pam_loginuid` makes SSH silently do nothing.** sshd in a container cannot
write `/proc/self/loginuid`. PAM runs the rest of the stack, the MOTD even
prints, then the session is marked failed and sshd closes it *without* exec-ing
your command. SSH appears to connect, then exits 254. The `Dockerfile`
downgrades that module to `optional`, the standard container fix, which avoids
running privileged.

**Claude Code is deliberately not installed in the image.** An npm global
install lands in root-owned `/usr/local`, which the `dev` user cannot write, so
every auto-update dies with `no_permissions`. The entrypoint installs the native
build into the persisted home instead: `dev` owns it, it survives an image
rebuild, and it can update itself.

Host SSH keys are persisted in `/home/dev/.sshd` too, otherwise every rebuild
trips VS Code's "REMOTE HOST IDENTIFICATION HAS CHANGED" warning.

## Host setup

Add to `~/.ssh/config` on your workstation:

```ssh-config
Host tower
    HostName your-host.example
    User root
    IdentityFile ~/.ssh/id_tower
    IdentitiesOnly yes

Host tower-dev
    HostName your-host.example
    Port 2222
    User dev
    IdentityFile ~/.ssh/id_tower
    IdentitiesOnly yes
```

`tower` reaches the Docker host, `tower-dev` reaches the container. Put your
public key in `/home/dev/.ssh/authorized_keys` inside the volume. The fish
commands use both aliases, so keep the names or edit `fish/conf.d/tower.fish`.

## The fish commands

With [fisher](https://github.com/jorgebucaran/fisher):

```sh
fisher install mbolli/claude-code-dev-container
fisher remove  mbolli/claude-code-dev-container
```

Without fisher:

```sh
./install.fish            # symlink into ~/.config/fish
./install.fish --uninstall
```

`install.fish` symlinks, so the file you edit is the file you commit. Existing
regular files are moved to `<name>.pre-install` rather than overwritten.

> Fisher refuses to install over files it does not own, with
> `Cannot install: please remove or move conflicting files first`. If you
> already have `~/.config/fish/conf.d/tower.fish` or any `completions/dev*.fish`
> from a manual install, remove them first (or `./install.fish --uninstall` if
> they are symlinks from this repo).

Every command that takes a repo accepts `swisscyberguard`,
`/develop/swisscyberguard` or `develop/swisscyberguard`, and tab-completes
against the live repo list (cached 10 minutes, with live descent into
subdirectories).

| Command | Does |
| --- | --- |
| `dev` | SSH into the container |
| `devcode <repo>` | Open a repo in VS Code over Remote-SSH |
| `devclone <owner/repo>` | Clone from GitHub into `/develop` and trust it |
| `devlink` | Print the claude.ai/code environment URL, with QR and clipboard |
| `devls` | List sessions and Remote Control environments |
| `devstatus` | Container status and Remote Control state |
| `devrc` | Attach to the Remote Control tmux session |
| `devresume [uuid] [dir]` | Resume a Claude session |
| `devcontinue [dir]` | Continue the last session in a directory |
| `devattach <id\|url> [dir]` | Re-attach a Remote Control session by id or URL |
| `devhoststart` / `devhoststop` | Start or stop the Remote Control spawner |
| `devtrust` | Pre-accept the trust dialog for every repo |
| `devrestart` / `devrebuild` | Restart or rebuild the container |

## Remote Control and the mobile app

`entrypoint.sh` starts a Remote Control host in `/develop` under tmux, in
`same-dir` mode with capacity 8. `same-dir` rather than `worktree` because
`/develop` holds many repos and is not itself a git repository, which worktree
mode requires. Press `w` in the pane to toggle when the cwd is a single repo.

`devlink` gives you the URL and QR code to open on your phone.

Two things that are easy to get wrong:

**One Remote Control process serves one directory.** Sessions spawned from your
phone start in the host's cwd, `/develop`, not in whichever repo you have open
in VS Code. `devattach` detects when the spawner owns a directory and offers to
stop it, attach, and restart it afterwards.

**A session you start yourself is not automatically reachable.** Running
`claude` in a VS Code terminal creates a plain process the host knows nothing
about. Run `/remote-control` inside that session to publish it.

### Session rebind on restart

Claude sessions are server-side objects, separate from the local transcript
UUIDs under `~/.claude/projects`. `rebind-sessions.sh` snapshots each
directory's `bridge-pointer.json` *before* the host starts, because the host
pre-creates a session in its cwd and rewrites that pointer within a second,
which would otherwise destroy the previous run's session id. Each recovered
session comes back in its own tmux window.

Tunables, as environment variables read by the entrypoint:

| Variable | Default | Meaning |
| --- | --- | --- |
| `REBIND_ENABLE` | `1` | Rebind at all |
| `REBIND_MAX` | `5` | Most sessions to restore |
| `REBIND_POINTERS` | `1` | Use the pointer snapshot |

`cat /tmp/rebind.log` in the container shows the last run.
