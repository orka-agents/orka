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
3. If explicit provider routing is required, write or locate a JSON/YAML providers file that uses secret env references, then pass it with existing Kubernetes secrets:
   ```bash
   scripts/deploy_vekil_reverse_proxy.sh \
     --context <kubectl-context> \
     --providers-config /path/to/providers.yaml \
     --env-secret AZURE_OPENAI_API_KEY=azure-openai:key
   ```
4. For non-interactive Copilot auth in Kubernetes, either wire an existing Secret or let the script create/update one from `COPILOT_GITHUB_TOKEN`; if that env var is unset, the script falls back to `gh auth token`. Prefer existing Secrets for production clusters that use a secret manager.
   ```bash
   # Existing Secret
   scripts/deploy_vekil_reverse_proxy.sh \
     --env-secret COPILOT_GITHUB_TOKEN=copilot-github-token:token

   # Script-created Secret; the token is not printed or embedded in the rendered workload.
   # Uses COPILOT_GITHUB_TOKEN if set, otherwise `gh auth token`.
   gh auth status
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

- Zero-config mode uses Vekil's built-in GitHub Copilot upstream. In Kubernetes, device-code login can work from pod logs; use `--skip-wait`, watch `kubectl -n <namespace> logs deploy/<name>`, complete the login, then verify `/readyz`. `COPILOT_GITHUB_TOKEN` via `--env-secret` or `--create-copilot-token-secret` is better for non-interactive deployments; `--create-copilot-token-secret` uses local `COPILOT_GITHUB_TOKEN` when available and otherwise reads `gh auth token`. If the script-created Secret changes, the script restarts the Deployment so the Secret-backed env var is reloaded.
- Explicit provider configs should use `api_key_env`, not inline `api_key`, because the bundled script stores the config as a ConfigMap and refuses inline API keys by default.
- OpenAI Codex providers need `auth.json` from `codex login`. If needed, mount an existing secret with `--codex-auth-secret <secret>[:auth.json]` and verify whether the deployment needs a writable token source for refresh behavior.
- Vekil token cache defaults to an `emptyDir`; use `--token-pvc <claim>` if preserving Vekil-managed cached auth across pod restarts matters.

## Common Commands

Render the manifest without applying:

```bash
scripts/deploy_vekil_reverse_proxy.sh --print
```

Create/update a Copilot token Secret from GitHub CLI auth and deploy:

```bash
gh auth status
scripts/deploy_vekil_reverse_proxy.sh \
  --create-copilot-token-secret copilot-github-token:token
```

Or force a specific token from the local environment:

```bash
export COPILOT_GITHUB_TOKEN=...
scripts/deploy_vekil_reverse_proxy.sh \
  --create-copilot-token-secret copilot-github-token:token
```

Deploy to a custom namespace and expose inside the cluster:

```bash
scripts/deploy_vekil_reverse_proxy.sh --namespace ai-proxy --name vekil
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
- If an existing Secret used with `--env-secret` changes outside this script, restart the Deployment so env vars reload: `kubectl -n <namespace> rollout restart deploy/<name>`.
- If `/healthz` works but `/readyz` fails, focus on provider auth, provider config model ownership, upstream reachability, or missing secret env vars.
- If clients cannot connect, verify the base URL path: Anthropic/Gemini use `http://host:1337`; OpenAI/Codex use `http://host:1337/v1`.
- Do not paste secrets into prompts, commit provider files with inline credentials, or expose a public service without confirming the user's intended security boundary.
