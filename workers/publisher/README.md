# Orka clean-room Workspace/Publisher image

This image is intentionally separate from provider runtime images. It has Git and
SCM egress, but no provider or MCP clients, credentials, configuration, or
endpoints. Deploy it with its own ServiceAccount and network identity.

Required mounted files and configuration:

- controller bearer token file
- publisher operation-capability HMAC secret file
- controller artifact-authorization broker URL; the Publisher never receives the artifact signing key
- controller credential-broker URL; Task Secret values are delivered only for the exact active operation
- a bounded writable volume at `/data` for prepared bundles and operation journals
- a bounded writable `emptyDir` at `/tmp/orka-workspace-publisher`
- `ORKA_PUBLISHER_ARTIFACT_API_URL`
- `ORKA_PUBLISHER_ARTIFACT_AUTHORIZATION_BROKER_URL`
- `ORKA_PUBLISHER_CREDENTIAL_BROKER_URL`
- an exact `ORKA_PUBLISHER_ALLOWED_SCM_HOSTS` allowlist in production

The Git credential API accepts references only. A Git credential file contains a
single `Authorization:` extra header and is copied into an operation-private Git
configuration. A PR backend receives only an operation-private copy of a
`forge-token` file. Neither credential value is accepted in JSON, returned, or
logged.

Recommended network policy:

- allow controller-to-publisher traffic
- allow publisher-to-controller artifact API traffic
- allow DNS and reviewed SCM/forge destinations
- deny provider proxies, MCP servers, Kubernetes API, metadata services, and all
  other egress

Build and inspect:

```bash
docker buildx build --builder remote-vm \
  --platform linux/amd64,linux/arm64 \
  --file workers/publisher/Dockerfile \
  --provenance=mode=max --sbom=true \
  --tag docker.io/sozercan/orka-workspace-publisher:<tag> \
  --push .

docker buildx build --check --file workers/publisher/Dockerfile .
trivy config workers/publisher/Dockerfile
```

Git is built from the official `git-2.55.0.tar.xz` archive with a frozen SHA-256
and source commit. The Dockerfile pins the multi-architecture Debian and Go base
manifest digests.
