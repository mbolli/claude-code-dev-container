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
#   Crucially, the spawner host ALREADY reclaims the session in its own
#   directory's pointer on startup. Rebinding that one races it and breaks it,
#   so that session id is skipped. Other sessions in the same directory are not.
#
#   Server ids live in two places. bridge-pointer.json holds only the MOST
#   RECENT session per project directory. The registry (~/.claude/sessions/
#   <pid>.json) has every session with its cwd, transcript uuid and entrypoint.
#   Both get overwritten once the new processes start, so entrypoint.sh
#   snapshots them first (REBIND_POINTER_LIST, REBIND_REGISTRY_LIST).
#
#   Registry entries come in two kinds. entrypoint "sdk-cli" is a session the
#   spawner created, reattached with `claude remote-control --session-id`.
#   entrypoint "cli" is an interactive `claude --remote-control` in its own
#   tmux session; it has no bridge environment, so it is resumed from its
#   transcript in a tmux session of the same name instead.
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
REBIND_MAX=${REBIND_MAX:-10}
REBIND_MAX_AGE_H=${REBIND_MAX_AGE_H:-48}
REBIND_POINTERS=${REBIND_POINTERS:-1}
REBIND_DRYRUN=${REBIND_DRYRUN:-0}
REBIND_POINTER_LIST=${REBIND_POINTER_LIST:-}
REBIND_REGISTRY_LIST=${REBIND_REGISTRY_LIST:-}
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
# Newest first: an old transcript may have been started from another directory.
cwd_for_project() {
  local dir=$1 f
  [ -d "$dir" ] || return 1
  while IFS= read -r f; do
    grep -m1 -oE '"cwd":"[^"]*"' "$f" 2>/dev/null | sed -E 's/^"cwd":"(.*)"$/\1/' && return 0
  done < <(ls -t "$dir"/*.jsonl 2>/dev/null)
  return 1
}

# The session the host reclaims: the one in its own directory's pointer.
host_sid() {
  local pdir="$PROJECTS/$(printf '%s' "$REBIND_HOST_DIR" | tr '/.' '--')" sid pd
  if [ -n "$REBIND_POINTER_LIST" ] && [ -s "$REBIND_POINTER_LIST" ]; then
    while IFS=$'\t' read -r sid pd; do
      [ "$pd" = "$pdir" ] && { echo "$sid"; return 0; }
    done < "$REBIND_POINTER_LIST"
  elif [ -f "$pdir/bridge-pointer.json" ]; then
    grep -o '"sessionId":"[^"]*"' "$pdir/bridge-pointer.json" | sed -E 's/^"sessionId":"(.*)"$/\1/'
  fi
}
HOST_SID=$(host_sid)

launched=0
declare -A seen=()

# An interactive `claude --remote-control` session: no bridge environment to
# reattach to, so resume the transcript in a tmux session of the same name.
resume_cli() {
  local sid=$1 uuid=$2 dir=$3 tsess=$4 name=$5

  [ -n "${seen[$sid]:-}" ] && return 0
  seen[$sid]=1
  if is_skipped "$sid"; then log "skip $sid (registry) - listed in no-rebind"; return 0; fi
  if [ -z "$dir" ] || [ ! -d "$dir" ]; then
    log "skip $sid (registry) - directory '${dir:-unknown}' does not exist"; return 0
  fi
  if [ ! -f "$PROJECTS/$(printf '%s' "$dir" | tr '/.' '--')/$uuid.jsonl" ]; then
    log "skip $sid (registry) - no transcript $uuid to resume"; return 0
  fi
  tsess=${tsess:-$name}

  if [ "$REBIND_DRYRUN" = "1" ]; then
    log "DRYRUN would resume $uuid in $dir as tmux '$tsess' ($sid)"
  elif tmux has-session -t "=$tsess" 2>/dev/null; then
    log "resuming $uuid in $dir as a window in tmux '$tsess' ($sid)"
    tmux new-window -d -t "=$tsess" -c "$dir" \
      "LANG=C.UTF-8 claude --remote-control '$name' --resume $uuid; exec bash"
  else
    log "resuming $uuid in $dir as tmux '$tsess' ($sid)"
    tmux new-session -d -s "$tsess" -c "$dir" \
      "LANG=C.UTF-8 claude --remote-control '$name' --resume $uuid; exec bash"
  fi
  launched=$((launched + 1))
}

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

  # The host reclaims its pointer's session; racing it returns a 400.
  if [ "$origin" != "pinned" ] && [ "$sid" = "$HOST_SID" ]; then
    log "skip $sid ($origin) - already reclaimed by the main host"
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

# 2. The registry snapshot: every bridged session active within
#    REBIND_MAX_AGE_H, newest first, once per server id.
if [ -n "$REBIND_REGISTRY_LIST" ] && [ -s "$REBIND_REGISTRY_LIST" ]; then
  cutoff=$(( ($(date +%s) - REBIND_MAX_AGE_H * 3600) * 1000 ))
  while IFS=$'\t' read -r updated sid uuid dir entry tsess name; do
    case "$sid" in session_*) ;; *) continue ;; esac
    [ "${updated:-0}" -ge "$cutoff" ] || break
    if [ "$launched" -ge "$REBIND_MAX" ]; then log "reached REBIND_MAX=$REBIND_MAX"; break; fi
    if [ "$entry" = "cli" ]; then
      resume_cli "$sid" "$uuid" "$dir" "$tsess" "$name"
    else
      rebind_one "$sid" "$dir" "registry"
    fi
  done < <(sort -t$'\t' -k1,1nr "$REBIND_REGISTRY_LIST")
fi

# 3. Then the pre-start pointer snapshot, falling back to live pointers when
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
