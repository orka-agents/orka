// Package workspaceagent defines the stable workspace.orka.ai/v1 data-plane
// protocol and its transport-safe HTTP client. The package is public so
// out-of-tree provider adapters can pin a tagged Orka module without importing
// internal implementation packages. Secured agents require a full reset using
// the current binding generation before the first attachment after every agent
// process start or restart.
package workspaceagent
