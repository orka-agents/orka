# Contributing to Orka

This project welcomes contributions and suggestions. Most contributions require you to agree to a
Contributor License Agreement (CLA) declaring that you have the right to, and actually do, grant us
the rights to use your contribution. For details, visit [Contributor License Agreements](https://cla.opensource.microsoft.com).

When you submit a pull request, a CLA bot will automatically determine whether you need to provide
a CLA and decorate the PR appropriately (for example, status check or comment). Simply follow the
instructions provided by the bot. You will only need to do this once across all repos using our CLA.

## Code of Conduct

Help us keep this project open and inclusive. Please read and follow our [Code of Conduct](CODE_OF_CONDUCT.md).

## Security issues

Please do not report security vulnerabilities through public GitHub issues. See [SECURITY.md](SECURITY.md)
for security reporting guidance.

## Issues and feature requests

Use GitHub Issues to report bugs and request features. Before opening a new issue, please search
existing issues to avoid duplicates. For feature work, please open or comment on an issue before
starting a large implementation so maintainers can confirm the direction.

## Commit sign-off

Please sign commits with a Developer Certificate of Origin style `Signed-off-by` line:

```bash
git commit -s -m "type(scope): describe the change"
```

## Pull requests

Before submitting a pull request:

1. Fork the repository and create a branch for your change.
2. Keep the change focused and avoid unrelated edits.
3. Add or update tests and documentation when they are relevant to the change.
4. Run the appropriate build, lint, and test commands.
5. Open a pull request with a clear description of the change and verification performed.

## Build and test

Common commands:

```bash
make manifests          # Regenerate CRDs after editing API markers or *_types.go
make generate           # Regenerate generated Go code
make build              # Build the controller and embedded UI assets
make test               # Run Go tests
make lint-fix           # Run lint and automatic fixes
```

UI development:

```bash
cd ui
bun install
bun run dev
```

Additional development guidance is available in [website/docs/development/development.md](website/docs/development/development.md).
