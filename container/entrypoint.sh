#!/bin/bash
set -euo pipefail

HOME_DIR=/home/dev
WORKDIR=/develop

# The bind mount arrives owned by whatever the host says; make sure dev can use it.
chown 99:100 "$HOME_DIR" 2>/dev/null || true
mkdir -p "$HOME_DIR/.ssh" "$HOME_DIR/.sshd" "$HOME_DIR/go" "$HOME_DIR/.local/bin"
chown -R 99:100 "$HOME_DIR/.ssh" "$HOME_DIR/.sshd" "$HOME_DIR/go" 2>/dev/null || true
chown 99:100 "$HOME_DIR/.local" "$HOME_DIR/.local/bin" 2>/dev/null || true
chmod 700 "$HOME_DIR/.ssh"

# The host docker socket arrives owned by the host's docker group (281 here).
# Mirror that gid inside the container and put dev in it, otherwise every
# docker call from an ssh session fails with permission denied on the socket.
# Read at runtime rather than hardcoded: Unraid renumbers the group whenever
# the docker.img is recreated.
if [ -S /var/run/docker.sock ]; then
  sock_gid=$(stat -c %g /var/run/docker.sock)
  getent group "$sock_gid" >/dev/null || groupadd -g "$sock_gid" docker-host
  usermod -aG "$(getent group "$sock_gid" | cut -d: -f1)" dev
fi

# Seed a shell profile only if the persisted home doesn't have one yet.
if [ ! -f "$HOME_DIR/.bashrc" ]; then
  cat > "$HOME_DIR/.bashrc" <<'RC'
export PATH=$HOME/.local/bin:/usr/local/go/bin:$HOME/go/bin:/usr/local/bin:$PATH
export GOPATH=$HOME/go
export EDITOR=vi
alias ll='ls -alF'
RC
  chown 99:100 "$HOME_DIR/.bashrc"
fi

# Claude Code, native build, into the persisted home. The image ships none, so
# this runs once on a fresh home and is a no-op afterwards; from then on Claude
# updates itself in place, which the old root-owned npm install could not do.
if [ ! -x "$HOME_DIR/.local/bin/claude" ]; then
  su dev -c 'curl -fsSL https://claude.ai/install.sh | bash -s latest' \
    || echo "entrypoint: claude install failed, continuing" >&2
fi

# Pre-accept the workspace trust dialog for everything under $WORKDIR. Claude
# does not inherit trust across a repository root, so the ~45 clones in there
# each need their own entry; see the script for the details.
su dev -c "/usr/local/bin/trust-develop.sh $WORKDIR" || true

# Persist sshd host keys in the volume, otherwise every image rebuild trips
# VS Code's "REMOTE HOST IDENTIFICATION HAS CHANGED" warning.
if [ ! -f "$HOME_DIR/.sshd/ssh_host_ed25519_key" ]; then
  ssh-keygen -t ed25519 -N '' -f "$HOME_DIR/.sshd/ssh_host_ed25519_key" >/dev/null
fi
chmod 600 "$HOME_DIR/.sshd/ssh_host_ed25519_key"

mkdir -p /etc/ssh/sshd_config.d
cat > /etc/ssh/sshd_config.d/dev.conf <<EOF
Port 22
HostKey $HOME_DIR/.sshd/ssh_host_ed25519_key
PermitRootLogin no
PasswordAuthentication no
KbdInteractiveAuthentication no
PubkeyAuthentication yes
AuthorizedKeysFile $HOME_DIR/.ssh/authorized_keys
AllowUsers dev
AcceptEnv LANG LC_*
EOF

/usr/sbin/sshd -D &
SSHD_PID=$!


# Snapshot each directory's Remote Control pointer BEFORE the host starts. The
# host pre-creates a session in its cwd and rewrites bridge-pointer.json within
# a second, which would otherwise destroy the previous run's session id.
POINTER_LIST=/tmp/rebind-pointers.tsv
: > "$POINTER_LIST"
for ptr in "$HOME_DIR"/.claude/projects/*/bridge-pointer.json; do
  [ -f "$ptr" ] || continue
  sid=$(grep -o '"sessionId":"[^"]*"' "$ptr" | sed -E 's/^"sessionId":"(.*)"$/\1/')
  [ -n "$sid" ] && printf '%s\t%s\n' "$sid" "$(dirname "$ptr")" >> "$POINTER_LIST"
done
chown 99:100 "$POINTER_LIST" 2>/dev/null || true

# Remote Control under tmux so it survives docker exec sessions detaching.
# If Claude isn't logged in yet the command exits and the window drops to a
# shell, which is exactly where you'd run /login.
#
# same-dir, not worktree: $WORKDIR holds many repos and is not itself a git
# repository, which worktree mode requires. Press 'w' in the pane to toggle at
# runtime when the cwd is a single repo.
su dev -c "cd $WORKDIR && tmux new-session -d -s rc \
  'claude remote-control --name tower --spawn same-dir --capacity 8; exec bash'" || true

# A spawner per recently used repo, so the phone can start sessions in any of
# them rather than only /develop. Reconciled hourly; see sessel serve.
su dev -c "sessel serve" >/tmp/serve.log 2>&1 || true
su dev -c "while sleep 3600; do sessel serve; done" >>/tmp/serve.log 2>&1 &

# Re-attach sessions from the previous run, each in its own tmux window.
# Backgrounded: it waits for the tmux session above and then adds windows, and
# nothing else should block on it. See rebind-sessions.sh for the reasoning.
su dev -c "cd $WORKDIR && REBIND_POINTER_LIST=$POINTER_LIST \
  REBIND_ENABLE=${REBIND_ENABLE:-1} REBIND_MAX=${REBIND_MAX:-5} \
  REBIND_POINTERS=${REBIND_POINTERS:-1} \
  /usr/local/bin/rebind-sessions.sh" >/tmp/rebind.log 2>&1 &

wait $SSHD_PID
