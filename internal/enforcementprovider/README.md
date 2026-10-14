# Enforcement providers

This package authenticates provider descriptors, preserves exact policy bytes, verifies signed activation/readback proof, and keeps a workload held until proof consumption and its release obligation are committed atomically. Providers implement prepare, activate, independent readback, proof, release, status, reconciliation, and cleanup.
