#!/bin/bash
# Re-attach Claude Code Remote Control sessions on container start.
#
# HOW REMOTE CONTROL SESSIONS ACTUALLY WORK (learned the hard way)
#   Sessions are server-side objects with ids like session_01AXM6aqqcQprxa5fFDZPQxh.
#   They are NOT the local transcript UUIDs under ~/.claude/projects
#   (b337adcd-...). Passing a transcript UUID to --session-id fails with
#   "Could not reach the server to look up session".
#
#   Reconnecting by server id works, and is verified to restore the exact same
#   session and URL - provided nothing else is currently serving it. If another
#   instance holds it you get either "already being served by another instance"
#   or a 400 "Could not re-queue session".
#
#   Crucially, the spawner host ALREADY reclaims its own directory's session on
#   startup. Rebinding that same session here races it and breaks it, so the
#   host's directory is deliberately skipped.
#
#   The only local trace of a server session id is bridge-pointer.json, one per
#   project directory, holding just the MOST RECENT session there. The host
#   overwrites it within a second of starting, so entrypoint.sh snapshots the
#   pointers first and passes the list via REBIND_POINTER_LIST.
#
# CONTROLLING IT
#   ~/.claude/rebind-sessions   sessions to restore on every start. One per
#                               line. A bare id or a full claude.ai/code URL,
#                               plus an optional directory:
#                                   session_01XXXX
#                                   https://claude.ai/code/session_01YYYY
#                                   session_01ZZZZ  /develop/php-via
#   ~/.claude/no-rebind         ids to never restore. There is no local
#                               "archived" flag - archiving happens on
#                               claude.ai and is invisible from in here - so
#                               this file is the manual equivalent.

set -uo pipefail

HOME_DIR=${HOME_DIR:-/home/dev}
TMUX_SESSION=${REBIND_TMUX_SESSION:-rc}
PROJECTS="$HOME_DIR/.claude/projects"
SKIP_FILE="$HOME_DIR/.claude/no-rebind"
PIN_FILE="$HOME_DIR/.claude/rebind-sessions"

REBIND_ENABLE=${REBIND_ENABLE:-1}
REBIND_MAX=${REBIND_MAX:-5}
REBIND_POINTERS=${REBIND_POINTERS:-1}
REBIND_DRYRUN=${REBIND_DRYRUN:-0}
REBIND_POINTER_LIST=${REBIND_POINTER_LIST:-}
REBIND_FALLBACK_DIR=${REBIND_FALLBACK_DIR:-/develop}
REBIND_HOST_DIR=${REBIND_HOST_DIR:-/develop}
REBIND_RETRIES=${REBIND_RETRIES:-3}

log() { echo "[rebind] $*"; }

[ "$REBIND_ENABLE" = "1" ] || { log "disabled via REBIND_ENABLE=$REBIND_ENABLE"; exit 0; }

for _ in $(seq 1 30); do
  tmux has-session -t "$TMUX_SESSION" 2>/dev/null && break
  sleep 1
done
tmux has-session -t "$TMUX_SESSION" 2>/dev/null || {
  log "tmux session '$TMUX_SESSION' never appeared, skipping"; exit 0; }

is_skipped() {
  [ -f "$SKIP_FILE" ] || return 1
  sed -e 's/#.*//' -e 's/[[:space:]]//g' "$SKIP_FILE" | grep -qx "$1"
}

# cwd is absent from bridge-pointer.json, and decoding the project directory
# name is ambiguous for paths containing dashes, so read it from a transcript.
cwd_for_project() {
  local dir=$1 f
  [ -d "$dir" ] || return 1
  for f in "$dir"/*.jsonl; do
    [ -f "$f" ] || continue
    grep -m1 -oE '"cwd":"[^"]*"' "$f" 2>/dev/null | sed -E 's/^"cwd":"(.*)"$/\1/' && return 0
  done
  return 1
}

launched=0
declare -A seen=()

rebind_one() {
  local sid=$1 dir=$2 origin=$3 label

  [ -n "${seen[$sid]:-}" ] && return 0
  seen[$sid]=1

  case "$sid" in
    session_*) ;;
    *) log "skip $sid ($origin) - not a server session id"; return 0 ;;
  esac

  if is_skipped "$sid"; then
    log "skip $sid ($origin) - listed in no-rebind"
    return 0
  fi

  if [ -z "$dir" ] || [ ! -d "$dir" ]; then
    log "skip $sid ($origin) - directory '${dir:-unknown}' does not exist"
    return 0
  fi

  # The spawner reclaims its own directory's session; racing it returns a 400.
  if [ "$origin" != "pinned" ] && [ "$dir" = "$REBIND_HOST_DIR" ]; then
    log "skip $sid ($origin) - $dir is already reclaimed by the main host"
    return 0
  fi

  label="$(basename "$dir")-${sid#session_01}"
  label="${label:0:24}"

  if [ "$REBIND_DRYRUN" = "1" ]; then
    log "DRYRUN would rebind $sid in $dir as $label ($origin)"
  else
    log "rebinding $sid in $dir as $label ($origin)"
    # Reconnect is occasionally transient ("Could not re-queue session. Try
    # again."), so retry a few times. If it stays dead, claude exits and the
    # window drops to a shell: visible, harmless, blocks nothing.
    tmux new-window -t "$TMUX_SESSION" -n "$label" -c "$dir" \
      "for n in \$(seq 1 $REBIND_RETRIES); do claude remote-control --session-id $sid --name $label && break; echo \"[rebind] attempt \$n failed, retrying in 5s\"; sleep 5; done; exec bash"
  fi
  launched=$((launched + 1))
}

# 1. Explicitly pinned ids first - the ones you chose to keep.
if [ -f "$PIN_FILE" ]; then
  while IFS= read -r raw; do
    line=${raw%%#*}
    ref=$(printf '%s' "$line" | awk '{print $1}')
    sid=${ref##*/}
    [ -n "$sid" ] || continue
    [ "$launched" -ge "$REBIND_MAX" ] && break
    dir=$(printf '%s' "$line" | awk '{print $2}')
    if [ -z "$dir" ]; then
      for p in "$PROJECTS"/*/bridge-pointer.json; do
        [ -f "$p" ] || continue
        if grep -q "\"$sid\"" "$p" 2>/dev/null; then
          dir=$(cwd_for_project "$(dirname "$p")")
          break
        fi
      done
    fi
    [ -n "$dir" ] || dir=$REBIND_FALLBACK_DIR
    rebind_one "$sid" "$dir" "pinned"
  done < "$PIN_FILE"
fi

# 2. Then the pre-start pointer snapshot, falling back to live pointers when
#    this is run by hand rather than from entrypoint.sh.
if [ "$REBIND_POINTERS" = "1" ]; then
  if [ -n "$REBIND_POINTER_LIST" ] && [ -s "$REBIND_POINTER_LIST" ]; then
    while IFS=$'\t' read -r sid pdir; do
      [ -n "${sid:-}" ] || continue
      if [ "$launched" -ge "$REBIND_MAX" ]; then log "reached REBIND_MAX=$REBIND_MAX"; break; fi
      rebind_one "$sid" "$(cwd_for_project "$pdir")" "pointer-snapshot"
    done < "$REBIND_POINTER_LIST"
  elif [ -d "$PROJECTS" ]; then
    for p in "$PROJECTS"/*/bridge-pointer.json; do
      [ -f "$p" ] || continue
      if [ "$launched" -ge "$REBIND_MAX" ]; then log "reached REBIND_MAX=$REBIND_MAX"; break; fi
      sid=$(grep -o '"sessionId":"[^"]*"' "$p" | sed -E 's/^"sessionId":"(.*)"$/\1/')
      [ -n "$sid" ] || continue
      rebind_one "$sid" "$(cwd_for_project "$(dirname "$p")")" "pointer-live"
    done
  fi
fi

log "launched $launched rebind window(s)"
