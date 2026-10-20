# Orka documentation website

This directory contains the public Orka documentation site (Docusaurus). Do not mix its toolchain with the `ui/` dashboard: the website uses Node.js with Yarn, while the dashboard uses Bun.

## Prerequisites

- Node.js (the website CI uses Node.js 24)
- Corepack

If `corepack --version` is unavailable, install it with `npm install --global corepack` first. [Node.js 25 and later no longer bundle Corepack](https://github.com/nodejs/corepack#how-to-install).

Enable Yarn through Corepack so the repository-pinned version is used:

```bash
corepack enable
```

The project pins Yarn 1.22.22 in `website/package.json` (`packageManager` field).

## Install and preview

Run from the repository root:

```bash
cd website
yarn --version   # should print 1.22.22
yarn install --frozen-lockfile
yarn start --host 127.0.0.1
```

The development server prints a local address, normally `http://127.0.0.1:3000/orka/` (the site is built under the `/orka/` base path). Open it, browse to a documentation page, and follow its links. Stop the server with Ctrl+C.

## Edit pages

Documentation sources live in `website/docs/`. Their URLs start with `/orka/docs/`, combining `baseUrl` and `routeBasePath` from `website/docusaurus.config.js`. A page's front-matter `slug` sets its path within that prefix; without a `slug`, Docusaurus derives the path from the source file path. For example, `website/docs/reference/cli.md` declares `slug: /cli-reference`, so its preview URL is `http://127.0.0.1:3000/orka/docs/cli-reference`.

The page navigation is defined in `website/sidebars.js`.

- To change an existing page, edit its Markdown file under `website/docs/` and inspect it in the running preview.
- To add a new page, create the Markdown file under `website/docs/` and add it to the relevant sidebar entry in `website/sidebars.js`.

## Production build and preview

From `website/`, after stopping the development server:

```bash
yarn build
yarn serve --host 127.0.0.1
```

`yarn build` validates the site (links, references, and build itself) and writes the static output; `yarn serve` previews that output locally, again under the `/orka/` base path. Stop the server with Ctrl+C.

These commands only preview locally; they do not publish anything. Publishing happens through the repository's website workflow on the `main` branch.
