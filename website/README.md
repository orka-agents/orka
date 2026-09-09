# Contributing to the website

The `website/` directory contains Orka's public documentation site. It is a Docusaurus
site with its own Node.js and Yarn toolchain. The `ui/` directory is the Orka dashboard
and uses Bun; use the workflow below for documentation changes.

## Prerequisites

- Node.js 24, matching the version used by the website CI workflow
- Corepack, available with the Node.js installation
- Git

The repository pins Yarn 1.22.22 through `website/package.json`. From the repository
root, enable Corepack and install the website dependencies:

```bash
corepack enable
cd website
yarn --version              # 1.22.22
yarn install --frozen-lockfile
```

## Edit the documentation

Existing pages live under `website/docs/`. Edit the Markdown file for an existing page,
or add a new Markdown file there and register it in `website/sidebars.js` so readers can
find it in the navigation. The Docusaurus configuration uses `/orka/` as the site base
path, so local URLs include that prefix.

## Development preview

Start the development server from `website/`:

```bash
yarn start --host 127.0.0.1
```

Open the printed address, normally `http://127.0.0.1:3000/orka/`. Visit the page you
changed, follow its links, and check that the surrounding navigation works. Stop the
server with `Ctrl+C` when finished.

## Production preview

Build the site and serve the generated files locally:

```bash
yarn build
yarn serve --host 127.0.0.1
```

Open the `/orka/` base path again and check the changed page and its links. Stop the
preview with `Ctrl+C`; the generated `website/build/` directory is ignored and should
not be committed.
