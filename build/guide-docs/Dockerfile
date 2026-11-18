# syntax=docker/dockerfile:1.7.1@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e

# Docs snapshot for the in-cluster Orka guide, mounted read-only into the guide
# Agent as a toolbox at /opt/orka-docs. Data only: Orka never runs anything from
# a toolbox image, so it starts from scratch. Build it with
# `make docker-build-guide-docs GUIDE_DOCS_REF=<release tag>`, which sends a git
# archive of the snapshot's paths as the build context and stamps VERSION from
# that ref. A plain directory context would copy untracked files and keep
# private file modes that the toolbox copier can't read.
FROM scratch

ARG ORKA_VERSION=dev
ARG ORKA_COMMIT=unknown
ARG ORKA_COMMIT_DATE=unknown

LABEL org.opencontainers.image.title="orka-guide-docs" \
      org.opencontainers.image.description="Docs snapshot read by the in-cluster Orka guide" \
      org.opencontainers.image.version="${ORKA_VERSION}" \
      org.opencontainers.image.revision="${ORKA_COMMIT}" \
      ai.orka.toolbox='{"version":1,"mountPath":"/opt/orka-docs"}'

# VERSION: release tag (or dev), full commit, commit date.
COPY <<EOF /opt/orka-docs/VERSION
${ORKA_VERSION}
${ORKA_COMMIT}
${ORKA_COMMIT_DATE}
EOF

# The guide's instructions: the orka-guide skill, including references/in-cluster.md.
COPY .agents/skills/orka-guide/ /opt/orka-docs/guide/

# The sources the guide answers from, at the paths the skill's source map uses.
COPY README.md SECURITY.md /opt/orka-docs/src/
COPY website/docs/ /opt/orka-docs/src/website/docs/
COPY docs/adr/ /opt/orka-docs/src/docs/adr/
COPY docs/development/ /opt/orka-docs/src/docs/development/
COPY api/ /opt/orka-docs/src/api/
COPY config/crd/bases/workspace.orka.ai_*.yaml /opt/orka-docs/src/config/crd/bases/
COPY manifest_staging/charts/orka/values.yaml /opt/orka-docs/src/manifest_staging/charts/orka/values.yaml
COPY charts/orka/values.yaml /opt/orka-docs/src/charts/orka/values.yaml
COPY .agents/skills/ /opt/orka-docs/src/.agents/skills/
