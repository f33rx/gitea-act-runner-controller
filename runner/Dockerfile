# Default runner image for garc. act_runner runs every job in host mode inside this
# container, so JavaScript actions need node here. Also carries git, python3, curl, jq,
# sudo, unzip, xz and zstd; compilers and build-essential are left to setup-* actions
# or a custom image.
ARG ACT_RUNNER_VERSION=0.6.1
ARG NODE_MAJOR=22

FROM docker.io/gitea/act_runner:${ACT_RUNNER_VERSION} AS act
FROM docker.io/library/node:${NODE_MAJOR}-bookworm-slim AS node

FROM docker.io/library/ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

# libstdc++6 is what the copied node binary links against.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      bash ca-certificates curl git gnupg jq libstdc++6 python3 sudo tini unzip \
      xz-utils zstd \
 && rm -rf /var/lib/apt/lists/*

COPY --from=node /usr/local/bin/node /usr/local/bin/node
COPY --from=node /usr/local/lib/node_modules /usr/local/lib/node_modules
RUN ln -s ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm \
 && ln -s ../lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx \
 && ln -s ../lib/node_modules/corepack/dist/corepack.js /usr/local/bin/corepack

COPY --from=act /usr/local/bin/act_runner /usr/local/bin/run.sh /usr/local/bin/

# Steps run on a PTY: a pager waits forever for a keypress, and a pager or spinner that
# keeps redrawing grows the job log, which defeats stall detection.
ENV PAGER=cat \
    GIT_PAGER=cat \
    TERM=dumb \
    LANG=C.UTF-8

ENTRYPOINT ["/usr/bin/tini", "--", "run.sh"]
