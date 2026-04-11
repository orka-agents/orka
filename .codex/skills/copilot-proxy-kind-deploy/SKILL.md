---
name: copilot-proxy-kind-deploy
description: Build and deploy the local copilot-proxy checkout into a kind Kubernetes cluster using the repo's Dockerfile and `k8s/copilot-proxy.yaml` manifest. Use when the user wants to rebuild copilot-proxy, load it into the current kind cluster, or roll out the proxy service for local cluster testing.
---

# Copilot Proxy Kind Deploy

Use the bundled script for the normal local cluster flow instead of rebuilding the Docker and kubectl steps manually.

## Workflow

1. Confirm the `copilot-proxy` repository path. Default to `/Users/sozercan/projects/copilot-proxy` when it exists.
2. Prefer the active `kubectl` context. If it starts with `kind-`, derive the cluster name from it. Otherwise require `--cluster`.
3. Run the script:
   - Default: `scripts/deploy_copilot_proxy_kind.sh`
   - Explicit repo or namespace: `scripts/deploy_copilot_proxy_kind.sh --repo /path/to/copilot-proxy --namespace default`
   - Safe preview: `scripts/deploy_copilot_proxy_kind.sh --dry-run`
4. Let the script:
   - build a local `copilot-proxy:kind` image
   - load it into the target kind cluster
   - patch the manifest to use the local image with `imagePullPolicy: IfNotPresent`
   - create the target namespace when needed
   - apply the manifest and wait for `deployment/copilot-proxy`
   - print either the GitHub device-login URL and user code from pod logs or a message that the pod is already authenticated
5. If the script prints a login URL and code, relay them to the user exactly and stop to wait for their confirmation that login is complete.
6. If the script reports that the pod is already authenticated, tell the user no manual login step is needed.
7. After the user confirms login, verify success with `kubectl --context <context> -n <namespace> logs deployment/copilot-proxy --tail=50` and summarize the resulting `pods`, `services`, and `deployments` for `copilot-proxy`.

## Guardrails

- Use this for local kind-based deployment. Do not assume it is appropriate for remote clusters unless the image distribution path is explicitly changed.
- The stock manifest mounts the token cache as `emptyDir`, so Copilot authentication inside the pod is ephemeral unless the user changes the volume strategy.
- When the script prints a device code, do not keep going as if login is done. Pause and wait for the user's confirmation.
- If rollout fails, inspect `kubectl -n <namespace> describe deployment/copilot-proxy`, `kubectl -n <namespace> get pods -l app=copilot-proxy`, and `kubectl -n <namespace> logs deployment/copilot-proxy`.
