# Require explicit trust for the Substrate control API

Orka should connect to the Substrate control API with explicit TLS trust configuration instead of normalizing insecure TLS verification. Local kind setups may opt into insecure mode for development, but production configuration should provide a CA or equivalent trust material for lifecycle calls that create, resume, suspend, and delete Substrate Actors.

The Substrate API trust setting belongs to provider installation/configuration, not to individual Tasks. Tasks select a Workspace Provider and Template; they do not choose whether provider control-plane TLS is trusted.
