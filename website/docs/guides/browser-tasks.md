---
slug: /browser-tasks
description: "Running Playwright and Chromium as container Tasks under Orka's hardened Pod defaults, and when a browser Task is the right tool."
---

# Browser tasks

A browser task is a [container Task](container-tasks.md) that drives Chromium: a
screenshot, a scrape, an end-to-end check, or a look at what a live page says right now.
Orka ships an image for this, `browser-worker`. It puts Playwright's own Chromium behind
Orka's worker entrypoint, so a script only has to write files and print its answer.

## A screenshot in one Task

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: example-screenshot
  namespace: orka-system
spec:
  type: container
  # Pin the image by digest; see "Which image" below.
  image: ghcr.io/orka-agents/orka/browser-worker@sha256:<digest>
  args:
    - sh
    - -c
    - playwright screenshot --browser=chromium https://example.com /tmp/artifacts/example.png
  resources:
    requests: {cpu: 500m, memory: 1Gi}
    limits: {cpu: "2", memory: 2Gi}
  timeout: 5m
```

```bash
orka task download -n orka-system example-screenshot example.png
```

Put the script in `args` and leave `command` out. The image's entrypoint waits for the
Pod's network to settle (below), then runs Orka's `/worker`, which runs `args` as the
command, keeps its output as the result, and uploads every file in `/tmp/artifacts`. A
`command` replaces that entrypoint, and then nothing is uploaded.

An agent creates the same Task with `create_container_task`:

```json
{
  "image": "ghcr.io/orka-agents/orka/browser-worker@sha256:<digest>",
  "args": ["sh", "-c", "playwright screenshot --browser=chromium https://example.com /tmp/artifacts/example.png"],
  "resources": {"requests": {"cpu": "500m", "memory": "1Gi"}, "limits": {"cpu": "2", "memory": "2Gi"}},
  "timeout": "5m"
}
```

### Which image

Releases publish `ghcr.io/orka-agents/orka/browser-worker:<version>`, signed and with an
SBOM, and `main` publishes `:0.0.0-dev`. Resolve the tag to a digest once and use that:

```bash
docker buildx imagetools inspect ghcr.io/orka-agents/orka/browser-worker:<version>
```

The Helm chart passes this image to the controller as `--browser-worker-image`, so the
chat prompt tells agents which image to use and when a browser Task fits. Set
`workers.browser.image.repository` to `""` to leave that guidance out.

| In the image | Details |
| --- | --- |
| Playwright 1.64.1 | `playwright` CLI on `PATH`; scripts can `require("playwright")` without installing anything. `playwright install` returns at once for Chromium, which is already installed; other browsers cannot be added |
| Chromium | Playwright's own build and headless shell for that version, under `/ms-playwright` |
| Node.js 24 on Debian 13 | Noto fonts, including color emoji, for legible screenshots; `git`, for workspaces. There is no Python |
| `chromium-headless` | Playwright's headless shell with the flags below, for `--screenshot`, `--dump-dom`, or `--print-to-pdf`. Unlike full Chromium, it calls no Google services of its own |
| User | UID/GID 1000, `HOME=/tmp` |
| Entrypoint | A wrapper that waits for the Pod's network to settle, then runs `/worker`, the same binary as the default worker image |

## What a Task Pod does to Chromium

Every Task Pod runs with a [hardened security context](../concepts/security.md#execution-workloads).
Each restriction affects a browser:

| Restriction | Effect on Chromium | What the image and scripts do |
| --- | --- | --- |
| `RuntimeDefault` seccomp, all capabilities dropped | Chromium's sandbox needs new user namespaces, which this blocks | Run with `--no-sandbox`. Playwright already does, because `chromiumSandbox` defaults to off. Use gVisor or Kata (below) for sensitive work. |
| `/dev/shm` stays at the 64 MiB default; Tasks have no volume field | Large pages crash the renderer | Run with `--disable-dev-shm-usage`. Playwright already does. |
| UID and GID 1000, whatever the image says | Files must be readable by 1000 | The image runs as 1000 and its browsers are world-readable. |
| Read-only root filesystem; only `/tmp` is writable | Profiles, caches, and downloads need a writable home | `HOME=/tmp`, `XDG_CACHE_HOME=/tmp/.cache`, `XDG_CONFIG_HOME=/tmp/.config`, and `--user-data-dir=/tmp/chrome-profile` for raw Chromium |
| `/tmp` is an `emptyDir` with no size limit | Traces and videos land on the node's disk | Keep recordings short; each artifact is limited to 10 MB |
| Default limits of 1 CPU and 2 GiB | Page loads stall at 1 CPU | Set `resources`, below |
| The Pod's IPv6 link-local address finishes duplicate-address detection about a second after the Pod starts | Chromium treats it as a network change and fails requests in flight with `net::ERR_NETWORK_CHANGED`, so the first navigation to a slow site fails | The entrypoint and `chromium-headless` wait for it, at most 5 seconds. With another image, wait until `/proc/net/if_inet6` lists no address with flag `0x40`, or retry the first navigation once. |

With a workspace, Orka sets `HOME=/home/worker` and runs the command in `/workspace`.

## Resources

Start from requests of `500m` CPU and `1Gi` memory and limits of `2` CPUs and `2Gi`. Raise
the memory limit to `4Gi` for traces, video, or long sessions with many tabs. The namespace's
`ResourceQuota` and `LimitRange` still apply.

A Pod that goes over its memory limit is killed whole, worker included: the Task fails with
an out-of-memory message, and nothing in `/tmp/artifacts` is uploaded. Full-page screenshots
of long pages are CPU-heavy. At 2 CPUs, several taken in parallel can exceed Playwright's
30-second timeout, so take them one at a time or raise the CPU limit.

## Scripts

Anything a script prints becomes the result: stdout, then stderr. Print a JSON object with
`"version": 1` as the only output to return a
[structured result](container-tasks.md#recommended-result-shape) that a coordinator can
branch on. Send diagnostics to a file in `/tmp/artifacts` instead of stderr.

```yaml
spec:
  type: container
  image: ghcr.io/orka-agents/orka/browser-worker@sha256:<digest>
  env:
    - {name: PAGE_URL, value: "https://example.com"}
  args:
    - node
    - -e
    - |
      const { chromium } = require("playwright");
      (async () => {
        const browser = await chromium.launch();
        try {
          const page = await browser.newPage();
          await page.goto(process.env.PAGE_URL, { timeout: 30000 });
          await page.screenshot({ path: "/tmp/artifacts/page.png", fullPage: true });
          const title = await page.title();
          console.log(JSON.stringify({
            version: 1, outcome: "completed", summary: `Title: ${title}`,
            data: { title },
            artifacts: [{ filename: "page.png", contentType: "image/png" }],
          }));
        } finally {
          // Always close the browser: an open browser keeps Node running until
          // the soft deadline, which would replace this result with unknown_outcome.
          await browser.close();
        }
      })().catch((err) => {
        // A launch, network, or script error is not the site refusing: fail the
        // Task with the error instead of reporting an outcome such as blocked.
        console.error(String(err));
        process.exitCode = 1;
      });
  resources:
    requests: {cpu: 500m, memory: 1Gi}
    limits: {cpu: "2", memory: 2Gi}
  timeout: 5m
```

Without Node, `chromium-headless` covers simple jobs:

```bash
chromium-headless --screenshot=/tmp/artifacts/page.png --window-size=1280,800 https://example.com
```

Chromium prints D-Bus connection errors on stderr in a Pod. They are harmless, but the
worker keeps them in the result after the command's stdout, and a structured result has to
be the JSON alone. Redirect them when the script prints one, for example with
`chromium-headless ... 2>/tmp/artifacts/chromium.log`. Playwright keeps the browser's
stderr out of the script's own output, so the Node example above is unaffected.

Kubernetes rewrites `args` and `command` before the shell sees them: `$$` becomes `$`, and
`$(NAME)` becomes the value of `NAME` when the Task's `env` defines it. Playwright's
`page.$$eval` therefore arrives as `page.$eval` and fails. Use
`page.locator(...).evaluateAll(...)` instead, or write `$$$$` wherever the script needs `$$`.

## Deadlines and stop conditions

A Task without a `timeout` has no deadline: a script that hangs runs until someone deletes
the Task. When a Task has a `timeout`, the worker stops the script shortly before it. It uploads what
is already in `/tmp/artifacts` and posts the output so far with the outcome
`unknown_outcome`, and the Task ends `Failed`. Write screenshots as the script goes, not
only at the end, so a run that hangs still shows where it was.

That early stop is the worker's. An image without Orka's `/worker`, as under
[Bring your own image](#bring-your-own-image), gets none: the deadline kills it with nothing
more uploaded, so it has to upload as it goes and stop itself in time using
`ORKA_TIMEOUT_SECONDS`.

Do not rely on the timeout alone. Write the conditions for stopping into the script or the
prompt that produces it, in three forms:

| Form | Example |
| --- | --- |
| An attempt cap that holds across Tasks | Pass `ATTEMPTS_SO_FAR=2` from the coordinator. "Two booking attempts were already refused; do not try a third, report back." |
| A stage boundary | "Fill the cart and stop before checkout; this run prepares, it does not buy." Report `decision_required`. |
| A halt triggered by state | "If the form submitted but no confirmation appeared, do not submit it again. Report `unknown_outcome`." |

A run that may already have acted must not be retried blindly; see
[Retry only runs that can safely repeat](container-tasks.md#recommended-result-shape).

## Watch a browser Task live

With live view on, a running browser Task's page in the dashboard shows what its browser
shows, refreshed about twice a second while the page changes. It is view only: nothing done
in the dashboard reaches the browser.

Turn it on with the Helm value `workers.browser.liveView: true`, which renders
`--browser-live-view`. Only Tasks on the configured browser image stream. Their Chromium then
also listens for DevTools on `127.0.0.1` inside the Pod, and the worker screencasts the
pages that Playwright or `chromium-headless` opens, up to four at a time.

- **Who can see it.** Anyone allowed to read the Task's artifacts. A frame shows whatever
  the page shows, including account data, and Orka redacts nothing, so turn live view on
  only where that is acceptable.
- **What is kept.** Only the latest frame, in controller memory, until the Task stops
  running. Nothing is written to disk. To keep a record, have the script save a Playwright
  video or trace in `/tmp/artifacts`.
- **What it shows.** The page that last repainted. A page that does not change sends no
  frames, and with several pages open the view follows whichever changed last.
- **What does not stream.** Images other than the configured browser image, including
  [your own](#bring-your-own-image), a Task whose `command` replaces the entrypoint, and a
  persistent context whose `userDataDir` is outside `/tmp`: the worker finds browsers by
  the profiles Chromium keeps directly under `/tmp`. Nor do `chromium-headless`'s
  one-shot `--screenshot`, `--dump-dom`, and `--print-to-pdf` runs: Chromium refuses those
  commands with a DevTools port, so they run without one.

## Stronger isolation

`--no-sandbox` removes Chromium's own sandbox, so the container boundary is all that
separates page code from the node. For untrusted sites or credentialed sessions, run the Task
under a sandboxed RuntimeClass such as gVisor or Kata:

```yaml
spec:
  execution:
    runtimeClassName: gvisor
```

Agents can request one through `create_container_task` only when the operator lists it in
`workers.containerTasks.runtimeClasses` (`--container-task-runtime-classes`).

## When a browser Task is the right tool

A browser costs more and sees more than a text fetch. Choose in this order:

1. A connector or skill that covers the site wins over both paths.
2. Use `web_fetch` or `web_search` from an AI Task when fetched text can establish the fact.
3. Use a browser Task when the person asked to visit or act on a site, when the fact depends
   on live state (availability, an exact price for given dates, the state of an account), or
   when the page needs interaction: a login, a form, or a JavaScript-only flow.

A failed `web_fetch` proves nothing about what a browser would see, so it is not a reason to
switch, or a reason not to. Search snippets never establish availability, price, or booking
state.

## Security and network

- **Page content is untrusted input.** A page can carry instructions aimed at whatever model
  reads its text. Treat text a browser Task returns as data, never as new instructions or
  new URLs to visit.
- **Credentials.** Use short-lived, least-privilege credentials, and keep them in Secrets.
  `env[].valueFrom.secretKeyRef` and `secretRef` reach a container Task only when an operator
  enables direct Secret mounts; literal `env` values are visible to anyone who can read the
  Task. Never print cookies, tokens, or session state.
- **No redaction.** Orka does not scrub screenshots, traces, or HAR files. A masked password
  field is masked only because the page draws it that way, and traces and HAR files record
  requests, responses, and typed input. Anyone who can read the Task can download them.
- **Downloads are untrusted and unscanned.** Files a page downloads stay in the Pod unless
  the script uploads them as artifacts; nothing checks them for malware.
- **No default egress policy.** Worker Pods have no Orka NetworkPolicy, so a browser Task can
  reach whatever the node can, including other in-cluster services and cloud metadata
  endpoints. Restrict egress with your own NetworkPolicies for the namespace.
- **Proxies, certificates, and DNS.** `HTTPS_PROXY`, `HTTP_PROXY`, and `NO_PROXY` in the
  Task's `env` apply to Chromium's page traffic, to tooling such as `npx`, and to the worker's
  own result and artifact uploads; Node's built-in `fetch` honors them only with
  `NODE_USE_ENV_PROXY=1`. Whenever you set them, list the cluster's service domains in
  `NO_PROXY`, for example `.svc,.cluster.local`: otherwise the uploads to
  `ORKA_CONTROLLER_URL` go to the proxy with the Task's callback token, and fail if the proxy
  cannot reach the cluster. To proxy only the browser, leave the env unset and use
  `proxy: { server: "http://proxy:3128" }` in `chromium.launch`, `--proxy-server` with
  `playwright screenshot`, or `--proxy-server` with `chromium-headless`.
  A private CA belongs in your own image built on this one; do not turn certificate checks
  off. Name resolution is the cluster's DNS.
- **CAPTCHAs, MFA, bot detection, and rate limits.** Stop and report `captcha`,
  `login_required`, or `blocked` rather than trying to get around them.

## Bring your own image

Any image works if it tolerates the Pod rules above: it runs as UID 1000 with a read-only
root filesystem and `HOME=/tmp`, its browser is readable by that user, and raw Chromium gets
`--headless --no-sandbox --disable-dev-shm-usage --disable-gpu --user-data-dir=/tmp/chrome-profile`.

Without Orka's `/worker` entrypoint, the image has to deliver its own result and artifacts
through the [custom-image contract](container-tasks.md#custom-image-result-and-artifact-contract).
Otherwise only its log is kept. The official Playwright image is the common case. It ships
the browsers but not the `playwright` package, so the script fetches it with `npx` into
`/tmp`:

```yaml
spec:
  type: container
  image: mcr.microsoft.com/playwright:v1.64.1-noble
  command: ["sh", "-c"]
  args:
    - |
      set -eu
      export HOME=/tmp npm_config_cache=/tmp/npm-cache npm_config_update_notifier=false
      # Wait for the Pod's IPv6 link-local address to settle; see "What a Task Pod does to Chromium".
      # Without IPv6 the file is absent and there is nothing to wait for.
      i=0; while [ "$i" -lt 100 ] && [ -r /proc/net/if_inet6 ]; do
        tentative=0
        while read -r _ _ _ _ flags _; do [ "$(( 0x$flags & 0x40 ))" -ne 0 ] && tentative=1; done < /proc/net/if_inet6
        [ "$tentative" -eq 0 ] && break; i=$((i + 1)); sleep 0.05
      done
      npx -y playwright@1.64.1 screenshot --browser=chromium https://example.com /tmp/example.png
      node -e '
        const fs = require("fs");
        const env = process.env;
        const url = `${env.ORKA_CONTROLLER_URL}/internal/v1/artifacts/${env.ORKA_TASK_NAMESPACE}/${env.ORKA_TASK_NAME}/example.png`;
        // A request can arrive before the controller has recorded the Pod, which answers
        // 503 until it has: retry transient answers, reading the token each time.
        (async () => {
          for (let attempt = 1; attempt <= 15; attempt++) {
            const token = fs.readFileSync(env.ORKA_SA_TOKEN_PATH, "utf8").trim();
            const response = await fetch(url, { method: "POST", body: fs.readFileSync("/tmp/example.png"),
              headers: { Authorization: `Bearer ${token}`, "Content-Type": "image/png" } }).catch(() => null);
            if (response && response.ok) return;
            if (response && ![502, 503, 504].includes(response.status)) {
              console.error(`upload failed: HTTP ${response.status}`); process.exit(1);
            }
            await new Promise((resolve) => setTimeout(resolve, 1000));
          }
          console.error("upload gave up"); process.exit(1);
        })();
      '
      echo uploaded example.png
  resources:
    requests: {cpu: 500m, memory: 1Gi}
    limits: {cpu: "2", memory: 2Gi}
  timeout: 10m
```

This image is about 1 GB compressed and installs Playwright on every run, which needs
access to the npm registry. The Orka image avoids both, and its entrypoint does the
network wait for you.
