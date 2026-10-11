#!/bin/bash
# Block until Claude can actually make an API call, then exit 0.
#
# `claude auth status` only checks that credentials exist, so it reports
# loggedIn=true with an expired, unrefreshable token. A tiny headless request
# refreshes the token when it can and fails when it cannot, which is the same
# check remote-control trips over. Run /login anywhere in the container and
# this returns within one interval.

INTERVAL=${LOGIN_WAIT_INTERVAL:-60}
MARKER=/tmp/claude-login-needed

n=0
until (cd /tmp && timeout 60 claude -p --model haiku --no-session-persistence 'reply with ok' >/dev/null 2>&1); do
  n=$((n + 1))
  [ "$n" -eq 1 ] && echo "[login] $(date -Is) not logged in, run /login; checking every ${INTERVAL}s"
  date -Is > "$MARKER"
  sleep "$INTERVAL"
done
rm -f "$MARKER"
echo "[login] $(date -Is) logged in"
