#!/bin/sh
# Rebuild and recreate the claude-code container.
#
# This container is NOT defined by an Unraid template - its run configuration
# lives only here. Keep this file in sync with any change made via docker run
# or the Unraid UI, otherwise the config is lost the moment the container is
# removed.
#
# Persistence boundary: only /home/dev and /develop survive a rebuild. Anything
# installed into the image filesystem at runtime is discarded - put it in the
# Dockerfile instead.
set -e
cd "$(dirname "$0")"

docker build -t claude-code:local .

docker stop claude-code 2>/dev/null || true
docker rm   claude-code 2>/dev/null || true

docker run -d \
  --name claude-code \
  --restart unless-stopped \
  -p 2222:22 \
  -v /mnt/user/appdata/claude-code/home:/home/dev \
  -v /mnt/user/develop:/develop \
  -v /var/run/docker.sock:/var/run/docker.sock \
  claude-code:local

docker ps --filter name=claude-code --format '{{.Names}} | {{.Image}} | {{.Status}} | {{.Ports}}'
