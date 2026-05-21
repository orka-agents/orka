# Use the provider route for Workspace Daemon calls

For Substrate-backed Execution Workspaces, Orka workers should call the Workspace Daemon through Substrate's router and actor DNS route instead of calling provider-native worker pod IPs directly. This keeps Orka coupled to the provider's stable routing abstraction rather than transient actor placement details.
