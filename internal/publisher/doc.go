// Package publisher implements the clean-room Git publication boundary.
//
// It never publishes from an agent-controlled checkout. Prepare imports a
// trusted workspace-delta artifact into a fresh bare repository at an exact
// source baseline, creates one deterministic Orka-owned commit, and persists a
// content-addressed Git bundle. Publish performs an exact-old-OID, fast-forward
// compare-and-swap. Verify uses a separate read-only observation to classify
// the remote outcome.
package publisher
