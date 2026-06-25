---
name: vekil-reverse-proxy-deploy
description: Deploy or update Vekil from github.com/sozercan/vekil as a Kubernetes or local reverse proxy for Anthropic, Gemini, OpenAI Chat Completions, and OpenAI Responses-compatible clients. Use when the user asks to install, redeploy, configure provider routing for, expose, port-forward, validate, or troubleshoot Vekil reverse proxy deployments.
---

# Vekil Reverse Proxy Deploy

Deploy Vekil as the single reverse-proxy endpoint for Claude/Anthropic, Gemini, OpenAI-compatible, and Codex clients. Prefer the bundled Kubernetes script for repeatable cluster deployments; use Docker only for quick local runs.

## Standard Kubernetes Workflow

1. Confirm the target before changing anything:
   - `kubectl config current-context`
   - `kubectl cluster-info`
   - For remote/shared clusters, state the context and ask before exposing `LoadBalancer` or `NodePort` services.
2. Deploy the default zero-config Copilot-backed proxy:
   - `scripts/deploy_vekil_reverse_proxy.sh --context <kubectl-context>`
   - Default namespace: `vekil-system`; default service: `ClusterIP`; default image: `ghcr.io/sozercan/vekil:latest`; default port: `1337`.
   - The script renders a restrictive `NetworkPolicy` by default. In-cluster clients must either run in the Vekil namespace with pod label `vekil.sozercan.io/access=true` or run in a namespace labeled `vekil.sozercan.io/access=true`, and the cluster CNI must enforce NetworkPolicy.
3. If explicit provider routing is required, write or locate a JSON/YAML providers file that uses secret env references, then pass it with existing Kubernetes secrets:
   ```bash
   scripts/deploy_vekil_reverse_proxy.sh \
     --context <kubectl-context> \
     --providers-config /path/to/providers.yaml \
     --env-secret AZURE_OPENAI_API_KEY=azure-openai:key
   ```
4. For non-interactive Copilot auth in Kubernetes, either wire an existing Secret or let the script create/update one from an explicitly exported `COPILOT_GITHUB_TOKEN`. Prefer existing Secrets for production clusters that use a secret manager.
   ```bash
   # Existing Secret
   scripts/deploy_vekil_reverse_proxy.sh \
     --env-secret COPILOT_GITHUB_TOKEN=copilot-github-token:token

   # Script-created Secret; the token is not printed or embedded in the rendered workload.
   export COPILOT_GITHUB_TOKEN=...
   scripts/deploy_vekil_reverse_proxy.sh \
     --create-copilot-token-secret copilot-github-token:token
   ```
5. Wait for rollout, then verify through a port-forward:
   ```bash
   kubectl -n vekil-system port-forward svc/vekil 1337:1337
   curl http://127.0.0.1:1337/healthz
   curl http://127.0.0.1:1337/readyz
   curl http://127.0.0.1:1337/v1/models
   ```

## Provider and Auth Notes

- Vekil's provider/Copilot credentials are upstream credentials for the proxy, not client authentication. A `ClusterIP` Service is still reachable by other pods in a default Kubernetes network unless NetworkPolicy or equivalent controls block it. Keep the default NetworkPolicy enabled for shared clusters, label only trusted same-namespace client pods or trusted client namespaces with `vekil.sozercan.io/access=true`, and do not use `--no-network-policy` unless an equivalent authentication or network boundary is already in place. Re-running the script with `--no-network-policy` deletes the managed `<name>-ingress` NetworkPolicy when applying to a cluster.
- `NodePort` and `LoadBalancer` Services are still subject to the default NetworkPolicy. External callers and kubelet health probes can be blocked on CNIs that enforce host-sourced traffic, so add explicit ingress exceptions for the intended source ranges/probes or choose `--no-network-policy` only when another boundary protects the proxy.
- Zero-config mode uses Vekil's built-in GitHub Copilot upstream. In Kubernetes, device-code login can work from pod logs; use `--skip-wait`, watch `kubectl -n <namespace> logs deploy/<name>`, complete the login, then verify `/readyz`. `COPILOT_GITHUB_TOKEN` via `--env-secret` or `--create-copilot-token-secret` is better for non-interactive deployments; `--create-copilot-token-secret` requires local `COPILOT_GITHUB_TOKEN` and does not fall back to GitHub CLI OAuth tokens. If the script-created Secret changes, the script restarts the Deployment so the Secret-backed env var is reloaded.
- Explicit provider configs should use `api_key_env`, not inline `api_key`, because the bundled script stores the config as a ConfigMap and refuses inline API keys by default. When the script creates or updates the providers ConfigMap, it restarts the Deployment so Vekil reloads provider routing read at startup.
- OpenAI Codex providers need `auth.json` from `codex login`. If needed, mount an existing secret with `--codex-auth-secret <secret>[:auth.json]` and verify whether the deployment needs a writable token source for refresh behavior.
- Vekil token cache defaults to an `emptyDir`; use `--token-pvc <claim>` if preserving Vekil-managed cached auth across pod restarts matters.

## Common Commands

Render the manifest without applying:

```bash
scripts/deploy_vekil_reverse_proxy.sh --print
```

Create/update a Copilot token Secret from an explicit local environment token and deploy:

```bash
export COPILOT_GITHUB_TOKEN=...
scripts/deploy_vekil_reverse_proxy.sh \
  --create-copilot-token-secret copilot-github-token:token
```

Deploy to a custom namespace and expose inside the cluster:

```bash
scripts/deploy_vekil_reverse_proxy.sh --namespace ai-proxy --name vekil
kubectl -n ai-proxy label pod <trusted-client-pod> vekil.sozercan.io/access=true
```

Allow all pods in a trusted client namespace to reach Vekil:

```bash
kubectl label namespace <trusted-client-namespace> vekil.sozercan.io/access=true
```

Expose for a local kind/minikube workflow with an explicit context:

```bash
scripts/deploy_vekil_reverse_proxy.sh --context kind-orka --namespace vekil-system
kubectl --context kind-orka -n vekil-system port-forward svc/vekil 1337:1337
```

Use the local Docker quick-start instead of Kubernetes when the user only needs a workstation proxy:

```bash
docker run -p 1337:1337 \
  -v ~/.config/vekil:/home/nonroot/.config/vekil \
  ghcr.io/sozercan/vekil:latest
```

## Client Smoke Tests

Run these only after `/readyz` succeeds and the requested model appears in `/v1/models`.

```bash
env ANTHROPIC_BASE_URL=http://127.0.0.1:1337 \
  ANTHROPIC_API_KEY=dummy \
  claude --model claude-sonnet-4 --print --output-format text "Reply with exactly PROXY_OK"
```

```bash
env OPENAI_API_KEY=dummy \
  OPENAI_BASE_URL=http://127.0.0.1:1337/v1 \
  codex exec --skip-git-repo-check -m gpt-5.5 "Reply with exactly PROXY_OK"
```

```bash
env GEMINI_API_KEY=dummy \
  GOOGLE_GEMINI_BASE_URL=http://127.0.0.1:1337 \
  GOOGLE_GENAI_API_VERSION=v1beta \
  GEMINI_CLI_NO_RELAUNCH=true \
  gemini -m gemini-2.5-pro -p "Reply with exactly PROXY_OK" -o json
```

## Troubleshooting

- If rollout fails: `kubectl -n <namespace> describe deploy/<name>` and `kubectl -n <namespace> logs deploy/<name>`.
- If readiness/liveness probes fail on a strict NetworkPolicy CNI, add an ingress exception for kubelet or node-originated health checks, or temporarily disable the default policy only behind an equivalent network boundary.
- If an existing Secret used with `--env-secret` changes outside this script, restart the Deployment so env vars reload: `kubectl -n <namespace> rollout restart deploy/<name>`.
- If `/healthz` works but `/readyz` fails, focus on provider auth, provider config model ownership, upstream reachability, or missing secret env vars.
- If clients cannot connect, verify the base URL path: Anthropic/Gemini use `http://host:1337`; OpenAI/Codex use `http://host:1337/v1`.
- Do not paste secrets into prompts, commit provider files with inline credentials, or expose a public service without confirming the user's intended security boundary.
