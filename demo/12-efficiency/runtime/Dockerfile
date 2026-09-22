# syntax=docker/dockerfile:1
FROM docker.io/sozercan/orka-acp-codex-runtime@sha256:34dcdd75807740232ca224897831c17becd25fc5f395a239455c557793199191

# Match Vekil's policy-model launcher settings while retaining Orka's v2
# supervisor, ACP adapter, and original Codex executable.
COPY policy-catalog.mjs /opt/orka-semantic-router/policy-catalog.mjs
RUN /opt/codex/bin/codex debug models --bundled \
    | node /opt/orka-semantic-router/policy-catalog.mjs \
    > /opt/orka-semantic-router/models.json
RUN mv /opt/codex/bin/codex /opt/codex/bin/codex-native
COPY --chmod=755 codex-policy.sh /opt/codex/bin/codex
