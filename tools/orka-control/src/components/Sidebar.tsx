import { Bot, Boxes, CircleGauge, Play, Rocket } from "lucide-react";
import type { ClusterReport, KubeContext, PageId } from "../domain";

const navigation: Array<{ id: PageId; label: string; icon: typeof CircleGauge }> = [
  { id: "discover", label: "Crawl", icon: CircleGauge },
  { id: "tasks", label: "Run", icon: Play },
  { id: "agents", label: "Agent pod", icon: Bot },
  { id: "install", label: "Quick deploy", icon: Rocket },
];

interface SidebarProps {
  page: PageId;
  contexts: KubeContext[];
  contextName: string;
  report?: ClusterReport;
  onContextChange: (context: string) => void;
}

export function Sidebar({ page, contexts, contextName, report, onContextChange }: SidebarProps) {
  return (
    <aside className="sidebar">
      <a className="brand" href="#/discover" aria-label="Orka Control discovery">
        <span className="brand-mark" aria-hidden="true"><span /></span>
        <span><strong>ORKA</strong><small>CONTROL</small></span>
      </a>

      <label className="context-select">
        <span className="cluster-icon"><Boxes size={17} /></span>
        <span>
          <small>Kube context</small>
          <select value={contextName} onChange={(event) => onContextChange(event.target.value)} aria-label="Kubernetes context">
            {contexts.map((context) => <option key={context.name} value={context.name}>{context.name}</option>)}
          </select>
        </span>
        <i className={report ? "online" : ""} />
      </label>

      <nav className="primary-nav" aria-label="Primary navigation">
        <span className="nav-label">Modes</span>
        {navigation.map((item) => {
          const Icon = item.icon;
          return (
            <a key={item.id} href={`#/${item.id}`} className={page === item.id ? "active" : ""} aria-current={page === item.id ? "page" : undefined}>
              <Icon size={18} strokeWidth={1.8} /><span>{item.label}</span>
            </a>
          );
        })}
      </nav>

      <div className="sidebar-spacer" />
      <div className="sidebar-status">
        <span className={`pulse state-${report?.orka.state ?? "unknown"}`} />
        <div><strong>{report?.orka.state ?? "waiting"}</strong><small>{report?.kubernetesVersion ?? "Kubernetes"} · {report?.nodeCount ?? "—"} nodes</small></div>
      </div>
      <div className="identity-chip"><span>K8s</span><div><strong>Local kubeconfig</strong><small>{report?.orka.namespace ?? "No namespace"}</small></div></div>
    </aside>
  );
}
