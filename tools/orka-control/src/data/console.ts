import type { ClusterReport, CrawlInput, InstallInput, InstallPlan, KubeContext, OrkaTask, RunTaskInput } from "../domain";

const fragmentParams = new URLSearchParams(window.location.hash.startsWith("#cap=") ? window.location.hash.slice(1) : "");
const launchCapability = fragmentParams.get("cap");
if (launchCapability) {
  sessionStorage.setItem("orka-console-capability", launchCapability);
  window.history.replaceState(null, "", "#/discover");
}

function capability(): string {
  const value = sessionStorage.getItem("orka-console-capability");
  if (!value) throw new Error("This console session is not authenticated. Restart the local console to open a fresh session.");
  return value;
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(`/api/console${path}`, {
    ...init,
    credentials: "same-origin",
    headers: {
      Accept: "application/json",
      "X-Orka-Capability": capability(),
      ...(init?.body ? { "Content-Type": "application/json", "X-Orka-Console": "1" } : {}),
      ...init?.headers,
    },
  });
  const payload = await response.json().catch(() => null) as { message?: string } | null;
  if (!response.ok) throw new Error(payload?.message ?? `Console gateway returned ${response.status}.`);
  return payload as T;
}

export const consoleApi = {
  async contexts(): Promise<KubeContext[]> {
    const response = await request<{ items: KubeContext[] }>("/contexts");
    return response.items;
  },
  crawl(input: CrawlInput): Promise<ClusterReport> {
    return request("/crawl", { method: "POST", body: JSON.stringify(input) });
  },
  runTask(input: RunTaskInput): Promise<OrkaTask> {
    return request("/tasks", { method: "POST", body: JSON.stringify(input) });
  },
  installPlan(context: string): Promise<InstallPlan> {
    return request("/install/plan", { method: "POST", body: JSON.stringify({ context }) });
  },
  install(input: InstallInput): Promise<ClusterReport> {
    return request("/install", { method: "POST", body: JSON.stringify(input) });
  },
};
