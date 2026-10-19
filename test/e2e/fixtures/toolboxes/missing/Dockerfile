# syntax=docker/dockerfile:1.7.1@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e
# A toolbox image that does not contain the declared mountPath
# (/opt/missing-tool); the copier must fail with TOOLBOX_SOURCE_OPEN.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.27.2-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61 AS build
RUN mkdir -p /out/opt/other-tool/bin && printf '#!/bin/sh\necho other\n' >/out/opt/other-tool/bin/other && chmod 0755 /out/opt/other-tool/bin/other

FROM scratch
COPY --from=build /out/ /
