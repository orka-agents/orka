---
slug: /release-status
description: "Where to get Orka releases and what each release includes."
---

# Release status

Get published versions from [GitHub Releases](https://github.com/orka-agents/orka/releases).
For a new installation, follow [Install Orka](../operations/installation.md).

## Release files

The release workflow publishes these files. Replace `<version>` with a version
number such as `0.3.0`:

| File | What it contains |
| --- | --- |
| `orka-<version>.tgz` | The Helm chart used to install Orka |
| `orka_v<version>_linux_amd64.tar.gz` | The CLI for Linux on x86-64 |
| `orka_v<version>_linux_arm64.tar.gz` | The CLI for Linux on ARM64 |
| `orka_v<version>_darwin_amd64.tar.gz` | The CLI for macOS on Intel |
| `orka_v<version>_darwin_arm64.tar.gz` | The CLI for macOS on Apple silicon |
| `orka_v<version>_windows_amd64.zip` | The CLI for Windows on x86-64 |
| `orka_v<version>_checksums.txt` | SHA-256 checksums for the five CLI archives |
| `orka_v<version>_checksums.txt.bundle` | The keyless Cosign signature and verification material for the checksum file |
| `candidate.json` | The version, source commit, image digests, and SHA-256 hashes for the chart and CLI assets |
| `qualification.json` | The release test run and checksums for its reports |
| `acceptance.json` | Results for installation, restart, agent, publication, and cleanup tests |

The chart uses release version tags by default. `candidate.json` records the
exact image digests and SHA-256 hashes for the chart, CLI archives, checksum file,
and signature bundle. Use these to inspect a release or pin images. See
[CLI installation](./cli.md#installation) to choose a download and verify it.
The chart is also available from Orka's Helm repository
at `https://orka-agents.github.io/orka/charts`.

## Check your installation {#which-one-am-i-running}

For a Helm release named `orka` in `orka-system`, check the chart version,
controller image, and CRDs. Use the same cluster connection name as your installation:

```bash
export ORKA_CONTEXT='<your-kubeconfig-context>'
helm list --kube-context "${ORKA_CONTEXT}" --namespace orka-system
kubectl --context "${ORKA_CONTEXT}" -n orka-system get deploy orka-controller \
  -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'
kubectl --context "${ORKA_CONTEXT}" get crd -o name | grep -c '\.orka\.ai$'
```

Compare the CRDs with those in your release's chart. Development CRDs are
packaged separately. A CRD count alone does not identify the running version
because Kubernetes keeps CRDs after an uninstall. Check the chart version and
image too.

## Build from source {#installing-main}

Follow [Build from source](../development/build-from-source.md)
for development. This builds your own images; ordinary pushes to `main` do not
publish release images.

Use `manifest_staging/charts/orka`, which `make manifests` generates from current
source. The root `charts/orka` and `deploy` directories are release snapshots
and may lag behind the source code.

## How a release is published {#release-publication}

1. A maintainer starts **Prepare Release** from `main` and enters the version.
2. The workflow prepares a release branch, builds the images, chart, and CLI
   archives, and waits for approval to run the release tests.
3. After the tests pass, a maintainer approves publication. The workflow tags
   the tested commit and publishes the images, chart, and GitHub Release files.

See [Release automation](../development/release-qualification.md) for the
maintainer instructions and test details.

## Support

Orka is pre-1.0 and is not yet supported for production use.
See [Upgrading](../operations/upgrading.md) before updating an installation
or planning a backup restore.
