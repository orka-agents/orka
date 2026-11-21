import { AlertTriangle, LoaderCircle, X } from "lucide-react";
import { useCallback, useEffect, useRef, useState } from "react";
import { Header } from "./components/Header";
import { Sidebar } from "./components/Sidebar";
import { consoleApi } from "./data/console";
import type { ClusterReport, InstallPlan, KubeContext, PageId, RunTaskInput } from "./domain";
import { ContextGuard } from "./lib/context-guard";
import { Agents } from "./pages/Agents";
import { Discover } from "./pages/Discover";
import { Install } from "./pages/Install";
import { Tasks } from "./pages/Tasks";

const validPages = new Set<PageId>(["discover", "tasks", "agents", "install"]);
function pageFromHash(): PageId {
  const candidate = window.location.hash.replace(/^#\/?/, "") as PageId;
  return validPages.has(candidate) ? candidate : "discover";
}

export default function App() {
  const [page, setPage] = useState<PageId>(pageFromHash);
  const [contexts, setContexts] = useState<KubeContext[]>([]);
  const [contextName, setContextName] = useState("");
  const [report, setReport] = useState<ClusterReport>();
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [running, setRunning] = useState(false);
  const [planning, setPlanning] = useState(false);
  const [installing, setInstalling] = useState(false);
  const [plan, setPlan] = useState<InstallPlan>();
  const [error, setError] = useState("");
  const [toast, setToast] = useState<string>();
  const toastTimer = useRef<number | undefined>(undefined);
  const contextGuard = useRef(new ContextGuard()).current;

  const showToast = useCallback((message: string) => {
    window.clearTimeout(toastTimer.current);
    setToast(message);
    toastTimer.current = window.setTimeout(() => setToast(undefined), 3200);
  }, []);
  const crawl = useCallback(async (targetContext: string, background = false, notify = true) => {
    if (!targetContext) return;
    const token = contextGuard.beginCrawl(targetContext);
    if (background) setRefreshing(true); else setLoading(true);
    setError("");
    try {
      const next = await consoleApi.crawl({ context: targetContext });
      if (!contextGuard.accepts(token)) return;
      setReport(next);
      if (background && notify) showToast("Cluster crawl complete");
    } catch (crawlError) {
      if (!contextGuard.accepts(token)) return;
      setError(crawlError instanceof Error ? crawlError.message : "Could not crawl the selected cluster.");
    } finally {
      if (contextGuard.accepts(token)) {
        setLoading(false);
        setRefreshing(false);
      }
    }
  }, [contextGuard, showToast]);

  useEffect(() => {
    let active = true;
    void consoleApi.contexts().then((items) => {
      if (!active) return;
      setContexts(items);
      const initial = items.find((context) => context.current)?.name ?? items[0]?.name ?? "";
      contextGuard.select(initial);
      setContextName(initial);
      if (initial) return crawl(initial);
      setError("No Kubernetes contexts were found in the local kubeconfig.");
      setLoading(false);
    }).catch((contextError) => {
      if (!active) return;
      setError(contextError instanceof Error ? contextError.message : "The local gateway is unavailable.");
      setLoading(false);
    });
    return () => { active = false; };
  }, [crawl]);
  useEffect(() => {
    const handler = () => setPage(pageFromHash());
    window.addEventListener("hashchange", handler);
    if (!window.location.hash) window.history.replaceState(null, "", "#/discover");
    return () => window.removeEventListener("hashchange", handler);
  }, []);
  useEffect(() => () => window.clearTimeout(toastTimer.current), []);
  useEffect(() => {
    if (!report?.tasks.some((task) => ["Pending", "Running", "Finalizing"].includes(task.phase)) || !contextName) return;
    const timer = window.setInterval(() => void crawl(contextName, true, false), 5_000);
    return () => window.clearInterval(timer);
  }, [contextName, crawl, report?.tasks]);

  const navigate = (nextPage: string) => { window.location.hash = `/${nextPage}`; };
  const changeContext = (nextContext: string) => {
    contextGuard.select(nextContext);
    setContextName(nextContext);
    setReport(undefined);
    setPlan(undefined);
    navigate("discover");
    void crawl(nextContext);
  };
  const runTask = async (input: RunTaskInput): Promise<boolean> => {
    const operationContext = input.context;
    setRunning(true);
    try {
      const task = await consoleApi.runTask(input);
      showToast(`${task.name} submitted${contextGuard.isCurrent(operationContext) ? "" : ` on ${operationContext}`}`);
      if (contextGuard.isCurrent(operationContext)) await crawl(operationContext, true);
      return true;
    } catch (taskError) {
      showToast(taskError instanceof Error ? taskError.message : "Task submission failed");
      return false;
    } finally { setRunning(false); }
  };
  const createPlan = async () => {
    const operationContext = contextGuard.current();
    setPlanning(true);
    try {
      const nextPlan = await consoleApi.installPlan(operationContext);
      if (!contextGuard.isCurrent(operationContext)) return;
      setPlan(nextPlan);
      showToast(nextPlan.allowed ? "Install plan is ready" : "Preflight found blockers");
    } catch (planError) { showToast(planError instanceof Error ? planError.message : "Could not create an install plan"); }
    finally { setPlanning(false); }
  };
  const install = async (selectedPlan: InstallPlan, confirmation: string, access: boolean, persistence: boolean) => {
    const operationContext = selectedPlan.context;
    setInstalling(true);
    try {
      const nextReport = await consoleApi.install({ planId: selectedPlan.id, context: operationContext, confirmation, acknowledgeClusterAccess: access, acknowledgePersistence: persistence });
      if (!contextGuard.isCurrent(operationContext)) {
        showToast(`Orka installation completed on ${operationContext}`);
        return;
      }
      setReport(nextReport);
      setPlan(undefined);
      showToast("Orka installation completed");
      navigate("discover");
    } catch (installError) { showToast(installError instanceof Error ? installError.message : "Orka installation failed"); }
    finally { setInstalling(false); }
  };

  const content = report ? page === "tasks"
    ? <Tasks report={report} running={running} onRun={runTask} onNavigate={navigate} />
    : page === "agents" ? <Agents report={report} />
      : page === "install" ? <Install report={report} plan={plan} planning={planning} installing={installing} onPlan={createPlan} onInstall={install} />
        : <Discover report={report} onNavigate={navigate} /> : null;

  return (
    <div className="app-shell">
      <Sidebar page={page} contexts={contexts} contextName={contextName} report={report} onContextChange={changeContext} />
      <main className="main-canvas">
        <Header page={page} report={report} refreshing={refreshing} onRefresh={() => void crawl(contextName, true)} onPrimary={() => navigate(report?.orka.state === "ready" ? "tasks" : "install")} />
        {loading && <div className="loading-screen"><span><LoaderCircle className="spin" size={26} /></span><strong>Crawling Kubernetes</strong><small>Looking for Orka resources and controller health…</small></div>}
        {!loading && error && <div className="error-screen"><span><AlertTriangle size={25} /></span><h2>Cluster gateway unavailable</h2><p>{error}</p><button className="button button-primary" type="button" onClick={() => void crawl(contextName)}>Try again</button></div>}
        {!loading && !error && content}
      </main>
      {toast && <div className="toast" role="status"><span>✓</span>{toast}<button type="button" onClick={() => setToast(undefined)} aria-label="Dismiss"><X size={15} /></button></div>}
    </div>
  );
}
