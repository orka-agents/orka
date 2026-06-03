# Use a minimal Substrate control API client

Orka should generate or vendor only the small Substrate control API surface needed for actor lifecycle operations instead of importing the full `github.com/agent-substrate/substrate` Go module. Orka only needs a narrow gRPC client, and avoiding the full module reduces Kubernetes dependency churn and cloud-provider transitive dependencies.
