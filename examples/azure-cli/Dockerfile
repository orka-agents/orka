# Azure CLI toolbox for Orka's built-in coding agents.
#
# The image holds a self-contained Python and the stock Azure CLI under
# /opt/azure-cli. It carries no credentials: in a runtime whose Task allows an
# Azure forwarding Tool, the supervisor points az at a loopback endpoint that
# forwards Azure Resource Manager requests through Orka's broker, and signs az
# in, so any az works there.
#
#   docker buildx build --platform linux/amd64,linux/arm64 \
#     -t registry.example.com/tools/azure-cli:2.92.0 --push examples/azure-cli
ARG UV_VERSION=0.13.7
FROM ghcr.io/astral-sh/uv:${UV_VERSION} AS uv

FROM debian:trixie-slim AS build
ARG AZURE_CLI_VERSION=2.92.0
ARG PYTHON_VERSION=3.12
COPY --from=uv /uv /usr/local/bin/uv
ENV UV_PYTHON_INSTALL_DIR=/opt/azure-cli/python \
    UV_PYTHON_PREFERENCE=only-managed \
    UV_NO_CACHE=1 \
    UV_LINK_MODE=copy
RUN set -eu; \
    apt-get update; apt-get install -y --no-install-recommends ca-certificates; rm -rf /var/lib/apt/lists/*; \
    uv python install "$PYTHON_VERSION"; \
    uv venv --python "$PYTHON_VERSION" /opt/azure-cli/venv; \
    uv pip install --python /opt/azure-cli/venv/bin/python "azure-cli==$AZURE_CLI_VERSION"; \
    find /opt/azure-cli -name '__pycache__' -type d -prune -exec rm -rf {} +; \
    # Hash-checked bytecode stays valid when the toolbox copy changes file
    # times, so az neither recompiles on every session nor prints the SDK's
    # compile-time warnings.
    /opt/azure-cli/venv/bin/python -Im compileall -q -j 0 --invalidation-mode unchecked-hash /opt/azure-cli/venv/lib >/dev/null 2>&1; \
    # Only az goes on PATH, not the venv's python or pip. The entry point
    # names the venv's python, so the link runs it from anywhere.
    mkdir -p /opt/azure-cli/bin; \
    ln -s ../venv/bin/az /opt/azure-cli/bin/az; \
    /opt/azure-cli/bin/az version --output json >/dev/null

FROM scratch
COPY --from=build /opt/azure-cli /opt/azure-cli
LABEL ai.orka.toolbox='{"version":1,"mountPath":"/opt/azure-cli","pathEntries":["bin"]}'
