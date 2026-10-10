# Toolbox E2E fixtures

Small toolbox images for the live agent-runtime lane
(`scripts/agent-runtime-kind-e2e.sh`). The lane builds them with the local
Docker daemon, publishes them through the run's kind registry, enables
toolboxes on the deployed controller, and runs `run_toolbox_check` in
`scripts/agent-runtime-e2e.sh`.

| Fixture | Declared as | Expected outcome |
| --- | --- | --- |
| `yq` | `mountPath: /opt/yq-jq`, `pathEntries: [bin]` | A Codex Task runs `yq --version` and reports `NoNewPrivs: 1` and an empty `CapEff` |
| `wrong-arch` | `mountPath: /opt/yq-jq`, `pathEntries: [bin]` | `ToolboxUnavailable: TOOLBOX_ARCH_MISMATCH` (the ELF machine is the opposite of the build architecture) |
| `fifo` | `mountPath: /opt/fifo-tool`, `pathEntries: [bin]` | `ToolboxUnavailable: TOOLBOX_UNSUPPORTED_FILE_TYPE` in `copy` mode; skipped in `imageVolume` mode, where the FIFO is mounted as-is and harmless |
| `missing` | `mountPath: /opt/missing-tool` | `ToolboxUnavailable: TOOLBOX_SOURCE_OPEN` in `copy` mode, `TOOLBOX_MOUNT_FAILED` in `imageVolume` mode (the folder does not exist in the image) |
