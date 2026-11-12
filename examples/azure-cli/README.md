# Azure CLI through Orka

Run `az` in a built-in coding agent without giving it an Azure credential.
Read requests to Azure Resource Manager go through Orka's broker, which adds
the Agent's own Entra identity or the requester's linked account.

- `Dockerfile` and `az`: the toolbox image (Python and the Azure CLI under
  `/opt/azure-cli`, and a wrapper that signs in on the first command).
- `as-agent.yaml`: az acts as the Agent's Entra agent identity.
- `as-person.yaml`: az acts as the person who asked, through their linked
  Azure account.

See the [Azure CLI guide](../../website/docs/guides/azure-cli.md).
