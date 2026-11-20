# syntax=docker/dockerfile:1.7.1@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e
# Static yq toolbox: the documented multi-architecture recipe from
# website/docs/guides/toolboxes.md with per-architecture checksums.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.27.2-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61 AS fetch
ARG TARGETARCH
RUN set -eu; case "$TARGETARCH" in \
      amd64) sha=8e34fc298390875de416e6a4afcb8cabeceb25d9aa8506c1a2f9353cf702ea5f ;; \
      arm64) sha=189088da0c6429ec5178dfaab1a114805f6cab0b61b165ab236efedf1d57a71b ;; \
      *) echo "unsupported TARGETARCH: $TARGETARCH" >&2; exit 1 ;; esac; \
    mkdir -p /out/opt/yq-jq/bin; \
    curl -fsSLo /out/opt/yq-jq/bin/yq "https://github.com/mikefarah/yq/releases/download/v4.54.1/yq_linux_${TARGETARCH}"; \
    echo "$sha  /out/opt/yq-jq/bin/yq" | sha256sum -c -; \
    chmod 0755 /out/opt/yq-jq/bin/yq

FROM scratch
COPY --from=fetch /out/ /
LABEL ai.orka.toolbox='{"version":1,"mountPath":"/opt/yq-jq","pathEntries":["bin"]}'
