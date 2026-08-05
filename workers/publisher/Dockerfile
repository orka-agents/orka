# syntax=docker/dockerfile:1.7.1@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e

ARG GIT_VERSION=2.55.0
ARG GIT_SOURCE_SHA256=457fdb04dc8728e007d4688695e6912e6f680727920f2a40bf11eacc17505357

FROM --platform=$TARGETPLATFORM docker.io/library/debian:bookworm-slim@sha256:7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818 AS git-builder
ARG GIT_VERSION
ARG GIT_SOURCE_SHA256
RUN apt-get update && apt-get install -y --no-install-recommends \
      build-essential=12.9 ca-certificates=20230311+deb12u1 \
      libcurl4-openssl-dev=7.88.1-10+deb12u15 libexpat1-dev=2.5.0-1+deb12u2 \
      libssl-dev=3.0.20-1~deb12u2 perl=5.36.0-7+deb12u3 pkg-config=1.8.1-1 \
      xz-utils=5.4.1-1+deb12u1 zlib1g-dev=1:1.2.13.dfsg-1 \
    && rm -rf /var/lib/apt/lists/*
ADD --checksum=sha256:457fdb04dc8728e007d4688695e6912e6f680727920f2a40bf11eacc17505357 \
    https://www.kernel.org/pub/software/scm/git/git-2.55.0.tar.xz /tmp/git.tar.xz
RUN set -eux; \
    test "$GIT_VERSION" = "2.55.0"; \
    test "$GIT_SOURCE_SHA256" = "457fdb04dc8728e007d4688695e6912e6f680727920f2a40bf11eacc17505357"; \
    mkdir /src; \
    tar -xJf /tmp/git.tar.xz --strip-components=1 -C /src; \
    make -C /src -j"$(nproc)" prefix=/usr/local \
      NO_GETTEXT=YesPlease NO_TCLTK=YesPlease NO_PERL=YesPlease NO_PYTHON=YesPlease NO_RUST=YesPlease \
      NO_INSTALL_HARDLINKS=YesPlease all; \
    make -C /src prefix=/usr/local \
      NO_GETTEXT=YesPlease NO_TCLTK=YesPlease NO_PERL=YesPlease NO_PYTHON=YesPlease NO_RUST=YesPlease \
      NO_INSTALL_HARDLINKS=YesPlease install; \
    test "$(/usr/local/bin/git version)" = "git version 2.55.0"; \
    rm -rf /usr/local/share/man /usr/local/share/doc /usr/local/share/git-gui /usr/local/share/gitk

FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26.2-bookworm@sha256:47ce5636e9936b2c5cbf708925578ef386b4f8872aec74a67bd13a627d242b19 AS service-builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked go mod download
COPY . .
RUN set -eu; \
    test "$TARGETOS" = linux; \
    case "$TARGETARCH" in amd64|arm64) ;; *) echo "unsupported TARGETARCH: $TARGETARCH" >&2; exit 1 ;; esac; \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
      go build -buildvcs=false -trimpath -ldflags='-s -w' \
      -o /out/orka-workspace-publisher ./cmd/orka-workspace-publisher

FROM docker.io/library/debian:bookworm-slim@sha256:7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818
LABEL org.opencontainers.image.title="Orka workspace publisher" \
      org.opencontainers.image.description="Clean-room source workspace and exact Git publication service" \
      org.opencontainers.image.source="https://github.com/orka-agents/orka" \
      org.opencontainers.image.licenses="MIT AND GPL-2.0-only" \
      io.orka.network.identity="workspace-publisher" \
      io.orka.scm-egress="authenticated-proxy-only" \
      io.orka.provider-access="false" \
      io.orka.mcp-access="false" \
      io.orka.git.version="2.55.0" \
      io.orka.git.revision="e9019fcafe0040228b8631c30f97ae1adb61bcdc" \
      io.orka.git.source.digest="sha256:457fdb04dc8728e007d4688695e6912e6f680727920f2a40bf11eacc17505357"
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates=20230311+deb12u1 \
      libcurl4=7.88.1-10+deb12u15 \
      libexpat1=2.5.0-1+deb12u2 \
      zlib1g=1:1.2.13.dfsg-1 \
    && rm -rf /var/lib/apt/lists/* \
    && install -d -m 0700 -o 65532 -g 65532 \
      /data/publications /data/service /tmp/orka-workspace-publisher/home \
      /var/run/secrets/orka-publisher/credentials /usr/share/licenses/git
COPY --from=git-builder /usr/local/ /usr/local/
COPY --from=git-builder /src/COPYING /usr/share/licenses/git/COPYING
COPY --from=service-builder /out/orka-workspace-publisher /usr/local/bin/orka-workspace-publisher
ENV HOME=/tmp/orka-workspace-publisher/home \
    GIT_CONFIG_GLOBAL=/dev/null \
    GIT_CONFIG_NOSYSTEM=1 \
    ORKA_PUBLISHER_ARTIFACT_ROOT=/data/publications \
    ORKA_PUBLISHER_JOURNAL_ROOT=/data/service \
    ORKA_PUBLISHER_TEMP_ROOT=/tmp/orka-workspace-publisher \
    ORKA_PUBLISHER_GIT_BINARY=/usr/local/bin/git \
    ORKA_PUBLISHER_REQUIRED_GIT_VERSION=2.55.0 \
    PATH=/usr/local/bin:/usr/bin:/bin
WORKDIR /tmp/orka-workspace-publisher
USER 65532:65532
EXPOSE 8080
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/orka-workspace-publisher"]
