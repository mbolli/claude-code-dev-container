#!/bin/sh
# Rebuild and recreate the claude-code container.
#
# This container is NOT defined by an Unraid template - its run configuration
# lives only here. Keep this file in sync with any change made via docker run
# or the Unraid UI, otherwise the config is lost the moment the container is
# removed.
#
# Persistence boundary: only /home/dev, /develop and /tmp survive a rebuild.
# Anything installed into the image filesystem at runtime is discarded - put it
# in the Dockerfile instead. /tmp is on the cache disk to keep scratchpads out
# of docker.img; the entrypoint expires old scratchpad files.
#
# All three mounts use /mnt/cache, not /mnt/user: appdata and develop are
# cache-only shares, and a crash of shfs (20 Nov 2026) took every /mnt/user
# mount down with it. If either share ever moves to the array, switch back.
set -e
cd "$(dirname "$0")"

docker build -t claude-code:local .

docker stop claude-code 2>/dev/null || true
docker rm   claude-code 2>/dev/null || true

# seccomp=unconfined: the default profile blocks user namespaces, which the Codex
# and agy sandboxes need. The mounted docker socket already outweighs it.
docker run -d \
  --name claude-code \
  --restart unless-stopped \
  --memory=36g --memory-swap=36g \
  --security-opt seccomp=unconfined \
  -p 2222:22 \
  -v /mnt/cache/appdata/claude-code/home:/home/dev \
  -v /mnt/cache/develop:/develop \
  -v /mnt/cache/appdata/claude-code/tmp:/tmp \
  -v /var/run/docker.sock:/var/run/docker.sock \
  claude-code:local

docker ps --filter name=claude-code --format '{{.Names}} | {{.Image}} | {{.Status}} | {{.Ports}}'
