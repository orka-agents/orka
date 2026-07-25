# Agent Sandbox Deploy — Troubleshooting

- **Task rejected before Job creation:** confirm the controller actually has
  `--agent-sandbox-enabled=true` (the flag patch is skipped if Orka was not
  deployed first); check `templateRef.name` / default template, `reusePolicy`,
  `cleanupPolicy`, and `sessionRef.name` for `reusePolicy: session`.
- **ImagePullBackOff on sandbox/router pods:** the `localhost:5001` registry is
  missing or not wired as a containerd mirror — see the registry precondition.
- **Inner agent CLI connection refused:** exec into a retained sandbox and verify
  DNS/TCP reachability to the model/proxy base URL from inside the sandbox pod.
- **Controller rollout fails after the flag patch:** preserve the controller's
  `/data` and `/tmp` volume mounts, probes, resources, and security context.
- Full troubleshooting matrix: `website/docs/concepts/agent-sandbox.md`.
