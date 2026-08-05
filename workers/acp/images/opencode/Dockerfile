# syntax=docker/dockerfile:1.7.1@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e

# The tag documents the human-readable version; the manifest-list digest and
# checksum-verified upstream artifacts make every supported platform immutable.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26.2-bookworm@sha256:47ce5636e9936b2c5cbf708925578ef386b4f8872aec74a67bd13a627d242b19 AS upstream-artifacts
ARG TARGETARCH
COPY workers/acp/images/artifact-tool.go /tmp/artifact-tool.go
COPY workers/acp/images/opencode/AGENTS.md /tmp/opencode-AGENTS.md
COPY workers/acp/images/opencode/NOTICE.md /tmp/orka-NOTICE.md
RUN go build -buildvcs=false -trimpath -ldflags='-s -w' -o /usr/local/bin/artifact-tool /tmp/artifact-tool.go
RUN set -eu; \
    mkdir -p /artifacts /src; \
    artifact-tool download \
      "https://codeload.github.com/anomalyco/opencode/tar.gz/4da7bb44c84e013fa53e9c5d02ac753d1435c81a" \
      "bbc3363c6886c63865a60b75d456fff3fbe6b87d8a86f44b2439900695b35de3" \
      /artifacts/opencode-source.tgz 100663296; \
    case "$TARGETARCH" in \
      amd64) \
        native_url="https://registry.npmjs.org/opencode-linux-x64-baseline/-/opencode-linux-x64-baseline-1.18.9.tgz"; \
        native_sha="59bf22db1f20c6c9836c926800a8e6416228330c2b518ade3d431d315cd76366"; \
        ripgrep_url="https://snapshot.debian.org/file/096560a159a8be70155f16209d91777019011677"; \
        ripgrep_fallback_url="https://deb.debian.org/debian/pool/main/r/rust-ripgrep/ripgrep_15.2.0-1_amd64.deb"; \
        ripgrep_deb_sha="d9d86586969e925b8627936ec4387d10cc65baf7d42195258743a49e711b6157" ;; \
      arm64) \
        native_url="https://registry.npmjs.org/opencode-linux-arm64/-/opencode-linux-arm64-1.18.9.tgz"; \
        native_sha="4846a7e470797950f209126442a291cc06574fd44ddbb96c89d730017f60aae7"; \
        ripgrep_url="https://snapshot.debian.org/file/482bbe93dc82997d7d84c901990e8d9f4327457c"; \
        ripgrep_fallback_url="https://deb.debian.org/debian/pool/main/r/rust-ripgrep/ripgrep_15.2.0-1_arm64.deb"; \
        ripgrep_deb_sha="8f18379cd499dd890d27190a359fe194d0bce92d7efad8e47b0f636e64bf4b46" ;; \
      *) echo "unsupported TARGETARCH: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    artifact-tool download \
      "$native_url" "$native_sha" /artifacts/opencode-native.tgz 100663296; \
    if ! artifact-tool download \
      "$ripgrep_url" "$ripgrep_deb_sha" /artifacts/ripgrep.deb 16777216; then \
      artifact-tool download \
        "$ripgrep_fallback_url" "$ripgrep_deb_sha" /artifacts/ripgrep.deb 16777216; \
    fi; \
    tar -xzf /artifacts/opencode-source.tgz -C /src --strip-components=1; \
    grep -Eq '"name":[[:space:]]*"opencode"' /src/packages/opencode/package.json; \
    grep -Eq '"version":[[:space:]]*"1\.18\.9"' /src/packages/opencode/package.json; \
    printf '%s  %s\n' '625f0f619133f89bbbb2abe37369613dfa1885eba1e50d02170deb62bb42cb6b' /src/LICENSE | sha256sum -c -; \
    printf '%s  %s\n' 'b8e691e210a9502c9689ba20705806dfb1b5b10b5ca1062f73f9a85b1043a812' /tmp/opencode-AGENTS.md | sha256sum -c -; \
    printf '%s  %s\n' '93167c1d7505d3e38efce8bf019c2af4e10ef56529465b87c34e4b3401508a98' /tmp/orka-NOTICE.md | sha256sum -c -; \
    cp /src/LICENSE /artifacts/opencode-LICENSE; \
    cp /src/README.md /artifacts/opencode-README.md; \
    cp /tmp/opencode-AGENTS.md /artifacts/opencode-AGENTS.md; \
    cp /tmp/orka-NOTICE.md /artifacts/orka-NOTICE.md; \
    artifact-tool download \
      "https://raw.githubusercontent.com/orka-agents/orka/d03acb995b6014a6e855181c50b922b65ea8e7ff/LICENSE" \
      "7df20dcdf9197e9945c14858d41c60f11b52b93e5b69e2b63416b874d598d322" \
      /artifacts/orka-LICENSE 65536

FROM --platform=$TARGETPLATFORM docker.io/library/debian:trixie-slim@sha256:020c0d20b9880058cbe785a9db107156c3c75c2ac944a6aa7ab59f2add76a7bd AS opencode-layout
ARG TARGETARCH
COPY --from=upstream-artifacts /artifacts/opencode-native.tgz /tmp/opencode-native.tgz
COPY --from=upstream-artifacts /artifacts/ripgrep.deb /tmp/ripgrep.deb
RUN set -eu; \
    mkdir -p /tmp/native /tmp/ripgrep /opt/opencode/bin /opt/ripgrep/bin /opt/ripgrep/licenses; \
    tar -xzf /tmp/opencode-native.tgz -C /tmp/native; \
    case "$TARGETARCH" in \
      amd64) \
        package_name=opencode-linux-x64-baseline; \
        binary_sha=7c4d91c84d2bfdeabb59257e3490c5e5acb08f2aacb3e42f3ddc296a1c3f1aca; \
        debian_arch=amd64; \
        ripgrep_sha=af751db9b06d308fd5078aa973880cc8c75678289386bf1a6ee5faa614aa6f2d ;; \
      arm64) \
        package_name=opencode-linux-arm64; \
        binary_sha=ae8a9b7c591d3c763463f16bfb5f45230c4c741f039e8967218662836449ecdf; \
        debian_arch=arm64; \
        ripgrep_sha=d3febe5d02e2a15da0909200caeb5af0be65e58736331c2ab88b7086d62e942b ;; \
      *) echo "unsupported TARGETARCH: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    grep -Eq '"name":[[:space:]]*"'"$package_name"'"' /tmp/native/package/package.json; \
    grep -Eq '"version":[[:space:]]*"1\.18\.9"' /tmp/native/package/package.json; \
    test -x /tmp/native/package/bin/opencode; \
    printf '%s  %s\n' "$binary_sha" /tmp/native/package/bin/opencode | sha256sum -c -; \
    install -m 0755 /tmp/native/package/bin/opencode /opt/opencode/bin/opencode; \
    test "$(/opt/opencode/bin/opencode --version)" = "1.18.9"; \
    test "$(dpkg-deb -f /tmp/ripgrep.deb Package)" = ripgrep; \
    test "$(dpkg-deb -f /tmp/ripgrep.deb Version)" = 15.2.0-1; \
    test "$(dpkg-deb -f /tmp/ripgrep.deb Architecture)" = "$debian_arch"; \
    dpkg-deb -x /tmp/ripgrep.deb /tmp/ripgrep; \
    test -x /tmp/ripgrep/usr/bin/rg; \
    printf '%s  %s\n' "$ripgrep_sha" /tmp/ripgrep/usr/bin/rg | sha256sum -c -; \
    install -m 0755 /tmp/ripgrep/usr/bin/rg /opt/ripgrep/bin/rg; \
    cp /tmp/ripgrep/usr/share/doc/ripgrep/copyright /opt/ripgrep/licenses/COPYRIGHT; \
    ldd /opt/ripgrep/bin/rg | grep -Fq 'libc.so.6'; \
    /opt/ripgrep/bin/rg --version | grep -Eq '^ripgrep 15\.2\.0'

FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26.2-bookworm@sha256:47ce5636e9936b2c5cbf708925578ef386b4f8872aec74a67bd13a627d242b19 AS supervisor-builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked go mod download
COPY . .
RUN set -eu; \
    grep -Eq '^[[:space:]]*OpenCodeVersion[[:space:]]*= "1\.18\.9"$' internal/acp/pins.go; \
    grep -Eq '^[[:space:]]*OpenCodeSourceCommit[[:space:]]*= "4da7bb44c84e013fa53e9c5d02ac753d1435c81a"$' internal/acp/pins.go; \
    grep -Eq '^[[:space:]]*OpenCodeSourceTarSHA256[[:space:]]*= "bbc3363c6886c63865a60b75d456fff3fbe6b87d8a86f44b2439900695b35de3"$' internal/acp/pins.go; \
    grep -Eq '^[[:space:]]*OpenCodeLinuxX64BaselineTarSHA256[[:space:]]*= "59bf22db1f20c6c9836c926800a8e6416228330c2b518ade3d431d315cd76366"$' internal/acp/pins.go; \
    grep -Eq '^[[:space:]]*OpenCodeLinuxARM64TarSHA256[[:space:]]*= "4846a7e470797950f209126442a291cc06574fd44ddbb96c89d730017f60aae7"$' internal/acp/pins.go; \
    grep -Eq '^[[:space:]]*OpenCodeLinuxX64BinarySHA256[[:space:]]*= "7c4d91c84d2bfdeabb59257e3490c5e5acb08f2aacb3e42f3ddc296a1c3f1aca"$' internal/acp/pins.go; \
    grep -Eq '^[[:space:]]*OpenCodeLinuxARM64BinarySHA256[[:space:]]*= "ae8a9b7c591d3c763463f16bfb5f45230c4c741f039e8967218662836449ecdf"$' internal/acp/pins.go; \
    grep -Eq '^[[:space:]]*OpenCodeRipgrepVersion[[:space:]]*= "15\.2\.0"$' internal/acp/pins.go; \
    grep -Eq '^[[:space:]]*OpenCodeRipgrepDebianVersion[[:space:]]*= "15\.2\.0-1"$' internal/acp/pins.go; \
    grep -Eq '^[[:space:]]*OpenCodeRipgrepLinuxX64DebSHA256[[:space:]]*= "d9d86586969e925b8627936ec4387d10cc65baf7d42195258743a49e711b6157"$' internal/acp/pins.go; \
    grep -Eq '^[[:space:]]*OpenCodeRipgrepLinuxARM64DebSHA256[[:space:]]*= "8f18379cd499dd890d27190a359fe194d0bce92d7efad8e47b0f636e64bf4b46"$' internal/acp/pins.go; \
    grep -Eq '^[[:space:]]*OpenCodeRipgrepLinuxX64BinarySHA256[[:space:]]*= "af751db9b06d308fd5078aa973880cc8c75678289386bf1a6ee5faa614aa6f2d"$' internal/acp/pins.go; \
    grep -Eq '^[[:space:]]*OpenCodeRipgrepLinuxARM64BinarySHA256[[:space:]]*= "d3febe5d02e2a15da0909200caeb5af0be65e58736331c2ab88b7086d62e942b"$' internal/acp/pins.go; \
    grep -Eq '^[[:space:]]*OpenCodeRootInstructionSHA256[[:space:]]*= "b8e691e210a9502c9689ba20705806dfb1b5b10b5ca1062f73f9a85b1043a812"$' internal/acp/pins.go; \
    grep -Eq '^[[:space:]]*OpenCodeImageNoticeSHA256[[:space:]]*= "93167c1d7505d3e38efce8bf019c2af4e10ef56529465b87c34e4b3401508a98"$' internal/acp/pins.go; \
    test "$TARGETOS" = linux; \
    case "$TARGETARCH" in amd64|arm64) ;; *) exit 1 ;; esac; \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
      go build -buildvcs=false -trimpath -ldflags='-s -w' \
      -o /out/orka-acp-runtime ./cmd/orka-acp-runtime; \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
      go build -buildvcs=false -trimpath -ldflags='-s -w' \
      -o /out/orka-acp-exec-helper ./cmd/orka-acp-exec-helper

FROM --platform=$TARGETPLATFORM docker.io/library/debian:trixie-slim@sha256:020c0d20b9880058cbe785a9db107156c3c75c2ac944a6aa7ab59f2add76a7bd
LABEL org.opencontainers.image.title="Orka ACP OpenCode runtime" \
      org.opencontainers.image.description="Orka harness v2 ACP supervisor with native OpenCode ACP 1.18.9" \
      org.opencontainers.image.source="https://github.com/orka-agents/orka" \
      org.opencontainers.image.licenses="MIT AND (MIT OR Unlicense)" \
      io.orka.acp.provider="opencode" \
      io.orka.acp.adapter.version="1.18.9" \
      io.orka.acp.adapter.revision="4da7bb44c84e013fa53e9c5d02ac753d1435c81a" \
      io.orka.acp.adapter.source.digest="sha256:bbc3363c6886c63865a60b75d456fff3fbe6b87d8a86f44b2439900695b35de3" \
      io.orka.acp.adapter.digest.amd64="sha256:59bf22db1f20c6c9836c926800a8e6416228330c2b518ade3d431d315cd76366" \
      io.orka.acp.adapter.digest.arm64="sha256:4846a7e470797950f209126442a291cc06574fd44ddbb96c89d730017f60aae7" \
      io.orka.acp.adapter.binary.digest.amd64="sha256:7c4d91c84d2bfdeabb59257e3490c5e5acb08f2aacb3e42f3ddc296a1c3f1aca" \
      io.orka.acp.adapter.binary.digest.arm64="sha256:ae8a9b7c591d3c763463f16bfb5f45230c4c741f039e8967218662836449ecdf" \
      io.orka.acp.ripgrep.version="15.2.0" \
      io.orka.acp.ripgrep.debian.version="15.2.0-1" \
      io.orka.acp.ripgrep.package.digest.amd64="sha256:d9d86586969e925b8627936ec4387d10cc65baf7d42195258743a49e711b6157" \
      io.orka.acp.ripgrep.package.digest.arm64="sha256:8f18379cd499dd890d27190a359fe194d0bce92d7efad8e47b0f636e64bf4b46" \
      io.orka.acp.ripgrep.binary.digest.amd64="sha256:af751db9b06d308fd5078aa973880cc8c75678289386bf1a6ee5faa614aa6f2d" \
      io.orka.acp.ripgrep.binary.digest.arm64="sha256:d3febe5d02e2a15da0909200caeb5af0be65e58736331c2ab88b7086d62e942b" \
      io.orka.acp.root-instruction.digest="sha256:b8e691e210a9502c9689ba20705806dfb1b5b10b5ca1062f73f9a85b1043a812" \
      io.orka.notice.digest="sha256:93167c1d7505d3e38efce8bf019c2af4e10ef56529465b87c34e4b3401508a98"
ENV HOME=/root \
    ORKA_ACP_PROVIDER=opencode \
    PATH=/usr/local/bin:/usr/bin:/bin
RUN set -eu; \
    mkdir -p /sessions /opt/opencode /usr/share/licenses/orka /usr/share/licenses/opencode /usr/share/licenses/ripgrep; \
    chmod 0711 /sessions
COPY --from=supervisor-builder /out/orka-acp-runtime /usr/local/bin/orka-acp-runtime
COPY --from=supervisor-builder /out/orka-acp-exec-helper /usr/local/bin/orka-acp-exec-helper
COPY --from=opencode-layout /opt/opencode/bin/opencode /opt/opencode/bin/opencode
COPY --from=opencode-layout /opt/ripgrep/bin/rg /usr/local/bin/rg
COPY --from=opencode-layout /opt/ripgrep/licenses/COPYRIGHT /usr/share/licenses/ripgrep/COPYRIGHT
COPY --from=upstream-artifacts /artifacts/opencode-AGENTS.md /opt/opencode/AGENTS.md
COPY --from=upstream-artifacts /artifacts/opencode-README.md /opt/opencode/README.md
COPY --from=upstream-artifacts /artifacts/opencode-LICENSE /usr/share/licenses/opencode/LICENSE
COPY --from=upstream-artifacts /artifacts/orka-LICENSE /usr/share/licenses/orka/LICENSE
COPY --from=upstream-artifacts /artifacts/orka-NOTICE.md /usr/share/licenses/orka/NOTICE.md
WORKDIR /
# The supervisor must start as root to assign each child a unique UID/GID and
# prove process cleanup. Kubernetes drops all capabilities and adds back only
# the explicit identity/process-management set.
# trivy:ignore:DS-0002
USER 0:0
EXPOSE 8080
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/orka-acp-runtime"]
