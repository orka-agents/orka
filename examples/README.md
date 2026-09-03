# Examples

These manifests are working starting points, not abstract templates: CI
strict-decodes every Orka document in this directory against the typed API
(unknown fields fail the build) and runs each Agent through the same admission
contract a live cluster enforces.

Two assumptions to adjust before applying them:

- **Apply into the controller's watched namespace.** The static harness-v2
  controller reconciles exactly one namespace (`--watch-namespace`, which is
  `orka-system` for `make deploy` and the Helm chart). The manifests carry no
  `metadata.namespace`, so pass it explicitly, for example
  `kubectl apply -n orka-system -k examples/repository-monitor-pr-review-repair`;
  a resource created anywhere else is never reconciled.
- **Models and providers are placeholders.** Agent `spec.model.name`, Provider
  names, and credential Secret names reflect one working setup. Swap in the
  models and Secrets that exist in your cluster; note that built-in runtime
  Agents *must* set `spec.model.name` — the ACP session has no default model,
  and admission rejects an Agent without one.

Each subdirectory is self-contained; see the comments in its manifests for the
flow it demonstrates.
