# syntax=docker/dockerfile:1.7.1@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e
# A toolbox whose only tool is an ELF header for the OTHER CPU architecture,
# so the copier's architecture check must fail with TOOLBOX_ARCH_MISMATCH.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.27.2-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61 AS build
ARG TARGETARCH
RUN set -eu; case "$TARGETARCH" in \
      amd64) machine='\267' ;; \
      arm64) machine='\076' ;; \
      *) echo "unsupported TARGETARCH: $TARGETARCH" >&2; exit 1 ;; esac; \
    mkdir -p /out/opt/yq-jq/bin; \
    printf '\177ELF\002\001\001\000\000\000\000\000\000\000\000\000\002\000'"${machine}"'\000\001\000\000\000' >/out/opt/yq-jq/bin/yq; \
    head -c 44 /dev/zero >>/out/opt/yq-jq/bin/yq; \
    chmod 0755 /out/opt/yq-jq/bin/yq

FROM scratch
COPY --from=build /out/ /
