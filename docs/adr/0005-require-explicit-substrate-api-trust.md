# ADR 0005: Require explicit trust for the Substrate control API

## Status

Accepted as a requirement for any future Substrate ACP provider; not active in the current Kubernetes RuntimePool path.

A future integration must connect to Substrate with explicit TLS trust material. Local kind evaluation may opt into insecure verification, but production configuration must provide a reviewed CA or equivalent trust anchor for Actor lifecycle calls.

Trust configuration belongs to the operator-managed provider/runtime profile, never an individual Task. Tasks do not choose whether control-plane TLS is trusted.
