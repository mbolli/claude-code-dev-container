#!/bin/bash
# Pre-accept the workspace trust dialog for everything under /develop.
#
# Claude stores trust per project in ~/.claude.json and does inherit it from
# parent directories, but the walk upwards stops at the enclosing repository
# root. /develop holds ~45 clones, so trusting /develop alone reaches none of
# them: each repo prompts on first use, and an unattended Remote Control
# rebind into one fails outright with "Workspace not trusted".
#
# The entrypoint runs this before Claude starts, which is the one moment
# nothing else has the config open. Re-run it by hand after cloning a new repo
# (devclone does), ideally with no session running: a live Claude may later
# write its own in-memory copy of the config over these edits.
set -euo pipefail

ROOT=${1:-/develop}
CONFIG=${CLAUDE_CONFIG_FILE:-$HOME/.claude.json}

python3 - "$ROOT" "$CONFIG" <<'PY'
import json, os, sys, tempfile

root, config = sys.argv[1], sys.argv[2]

if not os.path.isdir(root):
    sys.exit(f"trust-develop: {root} is not a directory, nothing to do")

paths = [os.path.realpath(root)]
for entry in os.scandir(root):
    if entry.is_dir() and not entry.name.startswith("."):
        paths.append(os.path.realpath(entry.path))
paths = sorted(set(paths))

try:
    with open(config) as fh:
        data = json.load(fh)
except FileNotFoundError:
    data = {}
except json.JSONDecodeError:
    # Better to prompt for trust than to truncate a config we cannot parse.
    sys.exit(f"trust-develop: {config} is not valid JSON, leaving it alone")

# Same shape Claude writes for a fresh project, minus the trust flag itself.
DEFAULTS = {
    "allowedTools": [],
    "mcpContextUris": [],
    "mcpServers": {},
    "enabledMcpjsonServers": [],
    "disabledMcpjsonServers": [],
    "projectOnboardingSeenCount": 0,
    "hasClaudeMdExternalIncludesApproved": False,
    "hasClaudeMdExternalIncludesWarningShown": False,
    "hasUnseenTeamArtifacts": False,
}

projects = data.setdefault("projects", {})
changed = []
for path in paths:
    entry = projects.get(path)
    if entry is None:
        entry = projects[path] = dict(DEFAULTS)
    if entry.get("hasTrustDialogAccepted") is not True:
        entry["hasTrustDialogAccepted"] = True
        changed.append(path)

if changed:
    directory = os.path.dirname(config) or "."
    fd, tmp = tempfile.mkstemp(dir=directory, prefix=".claude.json.")
    try:
        with os.fdopen(fd, "w") as fh:
            json.dump(data, fh, indent=2)
            fh.write("\n")
        os.chmod(tmp, 0o600)
        os.replace(tmp, config)
    except BaseException:
        os.unlink(tmp)
        raise

print(f"trust-develop: {len(changed)} newly trusted of {len(paths)} paths under {root}")
PY
