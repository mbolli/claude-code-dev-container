FROM debian:sid-slim

ARG GO_VERSION=1.26.5
ARG TASK_VERSION=3.45.4
# Debian has not done the PHP 8.5 transition yet (sid is still 8.4), so PHP
# comes from Sury's trixie suite, which resolves cleanly on sid. Set to 8.4 for
# a more conservative version, or add a second set of php${VER}-* packages to
# run versions side by side.
ARG PHP_VERSION=8.5

ENV DEBIAN_FRONTEND=noninteractive

# Base toolchain. sid ships nodejs 24.19 and python 3.13 natively, so no
# NodeSource repo is needed. mariadb-client is 11.8.8, the same version as the
# mariadb container it talks to; mdbook is 0.5.4.
# ImageMagick reads AVIF but cannot write it without an encoder: Debian ships
# libheif's AV1 encoders as separate plugin packages. GD needs nothing extra.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl wget git gh tmux openssh-server openssh-client less jq \
      procps sudo xz-utils unzip zip build-essential pkg-config \
      ripgrep fd-find bat shellcheck sqlite3 mariadb-client mdbook rsync imagemagick ffmpeg \
      docker-cli docker-compose docker-buildx \
      brotli ncurses-term \
      libheif-plugin-aomenc libheif-plugin-svtenc \
      nodejs npm \
      python3 python3-pip python3-venv pipx \
      chromium fonts-liberation fonts-noto-color-emoji \
    && ln -sf /usr/bin/fdfind /usr/local/bin/fd \
    && ln -sf /usr/bin/batcat /usr/local/bin/bat \
    && rm -rf /var/lib/apt/lists/*

# PHP from Sury
RUN curl -fsSL https://packages.sury.org/php/apt.gpg -o /usr/share/keyrings/sury-php.gpg \
    && echo "deb [signed-by=/usr/share/keyrings/sury-php.gpg] https://packages.sury.org/php/ trixie main" \
       > /etc/apt/sources.list.d/sury-php.list \
    && apt-get update && apt-get install -y --no-install-recommends \
      php${PHP_VERSION}-cli php${PHP_VERSION}-mbstring php${PHP_VERSION}-xml \
      php${PHP_VERSION}-curl php${PHP_VERSION}-zip php${PHP_VERSION}-intl \
      php${PHP_VERSION}-sqlite3 php${PHP_VERSION}-mysql php${PHP_VERSION}-gd \
      php${PHP_VERSION}-bcmath php${PHP_VERSION}-gmp php${PHP_VERSION}-imagick \
      php${PHP_VERSION}-redis php${PHP_VERSION}-xdebug php${PHP_VERSION}-sockets \
    && phpdismod -v ${PHP_VERSION} xdebug \
    && rm -rf /var/lib/apt/lists/*

# openswoole. Sury ships no PHP 8.5 build (openswoole lags new PHP releases),
# but 26.2.0 compiles fine from PECL against it. The six answers correspond to:
# coroutine sockets, openssl, http2, mysqlnd, coroutine curl, postgres.
RUN apt-get update && apt-get install -y --no-install-recommends \
      php${PHP_VERSION}-dev php-pear \
      libbrotli-dev libssl-dev libpq-dev libcurl4-openssl-dev libc-ares-dev \
    && printf 'yes\nyes\nyes\nyes\nyes\nyes\n' | pecl install openswoole \
    && printf '; priority=30\nextension=openswoole.so\n' \
       > /etc/php/${PHP_VERSION}/mods-available/openswoole.ini \
    && phpenmod -v ${PHP_VERSION} openswoole \
    && rm -rf /var/lib/apt/lists/*

# brotli. Build deps are already present from the openswoole layer above.
# This uses the extension's bundled libbrotli rather than the system one:
# it costs ~3 MB in the .so and misses distro security updates, but avoids a
# manual phpize build for what is a dev container. Swap to a phpize build with
# --with-libbrotli if the shared-library linkage matters. pecl does not prompt
# here, package.xml declares no configureoptions, and it resolves extension_dir
# itself so nothing needs hardcoding.
RUN pecl install brotli \
    && printf 'extension=brotli.so\n' \
       > /etc/php/${PHP_VERSION}/mods-available/brotli.ini \
    && phpenmod -v ${PHP_VERSION} brotli

# Composer
RUN curl -fsSL https://getcomposer.org/installer -o /tmp/composer-setup.php \
    && php /tmp/composer-setup.php --install-dir=/usr/local/bin --filename=composer \
    && rm /tmp/composer-setup.php

# Go
RUN curl -fsSL https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz -o /tmp/go.tgz \
    && tar -C /usr/local -xzf /tmp/go.tgz \
    && rm /tmp/go.tgz

# Task
RUN curl -fsSL https://taskfile.dev/install.sh | sh -s -- -d -b /usr/local/bin v${TASK_VERSION}

# uv, so individual projects can pin their own Python without rebuilding
RUN curl -LsSf https://astral.sh/uv/install.sh | UV_INSTALL_DIR=/usr/local/bin sh

# Node tooling. Chromium comes from apt, so puppeteer/lighthouse reuse it
# instead of each downloading their own copy.
#
# Claude Code is deliberately not installed here. An npm global install lands
# in root-owned /usr/local, which the dev user cannot write, so every
# auto-update attempt dies with no_permissions. The entrypoint installs the
# native build into the persisted home instead: dev owns it, it survives an
# image rebuild, and it can update itself.
RUN npm i -g pnpm yarn lighthouse

ENV CHROME_PATH=/usr/bin/chromium
ENV PUPPETEER_SKIP_DOWNLOAD=1
ENV PUPPETEER_EXECUTABLE_PATH=/usr/bin/chromium
ENV PATH=/usr/local/go/bin:/home/dev/go/bin:/home/dev/.local/bin:/usr/local/bin:$PATH
ENV GOPATH=/home/dev/go

# Which environment the statusline reports. claude-powerline reads it through
# its env segment; the laptop sets work or private from claude.fish.
ENV CLAUDE_PROFILE=tower

# tmux decides per client whether the terminal is UTF-8 from LC_ALL, LC_CTYPE or
# LANG. debian-slim sets none and ships no locales package, so without this tmux
# mangles every multibyte glyph. C.UTF-8 needs no locales package.
ENV LANG=C.UTF-8

# Debian /etc/profile hard-sets PATH, discarding the ENV above, so login shells
# (ssh into this container, bash -l) lose ~/.local/bin and the Go toolchain.
# A profile.d snippet re-adds them after /etc/profile has run.
RUN printf 'PATH=/usr/local/go/bin:$HOME/go/bin:$HOME/.local/bin:$PATH\nexport CLAUDE_PROFILE=tower\nexport LANG=C.UTF-8\n' \
      > /etc/profile.d/00-dev-path.sh

# UNRAID share convention: nobody:users == 99:100
RUN useradd -u 99 -g 100 -m -d /home/dev -s /bin/bash dev \
    && mkdir -p /run/sshd

# sshd in a container cannot write /proc/self/loginuid, so pam_loginuid fails.
# PAM still runs the rest of the stack (the MOTD prints), then the session is
# marked failed and sshd closes it WITHOUT exec-ing the command: ssh appears to
# connect and then silently does nothing, exit 254. Downgrading to optional is
# the standard container fix and avoids having to run the container privileged.
RUN sed -i 's/^session\s\+required\s\+pam_loginuid.so/session    optional     pam_loginuid.so/' /etc/pam.d/sshd

COPY entrypoint.sh /entrypoint.sh
COPY rebind-sessions.sh /usr/local/bin/rebind-sessions.sh
COPY trust-develop.sh /usr/local/bin/trust-develop.sh
COPY dev-sessions /usr/local/bin/dev-sessions
RUN chmod +x /entrypoint.sh /usr/local/bin/rebind-sessions.sh \
    /usr/local/bin/trust-develop.sh /usr/local/bin/dev-sessions

ENTRYPOINT ["/entrypoint.sh"]
