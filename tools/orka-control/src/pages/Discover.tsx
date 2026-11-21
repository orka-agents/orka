import { AlertTriangle, Bot, Box, CheckCircle2, Database, Network, Play, Rocket, Server, ShieldCheck } from "lucide-react";
import type { ClusterReport } from "../domain";
import { StatusPill } from "../components/Status";

export function Discover({ report, onNavigate }: { report: ClusterReport; onNavigate: (page: string) => void }) {
  const readyComponents = report.components.filter((component) => component.ready).length;
  const target = report.orka.state === "ready" ? "tasks" : "install";
  return (
    <div className="page-content">
      <section className={`discovery-hero discovery-${report.orka.state}`}>
        <div>
          <span className="hero-eyebrow"><Network size={14} />Kubernetes context identified</span>
          <h2>{report.orka.state === "ready" ? "Orka is ready for action." : report.orka.state === "absent" ? "This cluster is ready for Orka." : "Orka needs operator attention."}</h2>
          <p>{report.orka.message}</p>
          <div className="hero-context"><code>{report.context.name}</code><span>{report.context.server}</span></div>
          <button className="button hero-button" type="button" onClick={() => onNavigate(target)}>{target === "tasks" ? <Play size={16} /> : <Rocket size={16} />}{target === "tasks" ? "Run work" : "Review quick deploy"}</button>
        </div>
        <div className="radar" aria-hidden="true"><span /><span /><span /><i /></div>
      </section>

      <section className="metric-strip">
        <Metric icon={<Server size={18} />} tone="green" label="Kubernetes" value={`${report.nodeCount}`} detail={report.kubernetesVersion} suffix="nodes" />
        <Metric icon={<Box size={18} />} tone="blue" label="Tasks" value={`${report.counts.tasks}`} detail={report.counts.tasks ? "Observed by crawl" : "No task history"} />
        <Metric icon={<Bot size={18} />} tone="orange" label="Agents" value={`${report.counts.agents}`} detail={`${report.counts.providers} providers`} />
        <Metric icon={<Database size={18} />} tone="coral" label="Components" value={`${readyComponents}`} detail={report.defaultStorageClass ? `${report.defaultStorageClass} storage` : "No default storage"} suffix={`/${report.components.length || "—"}`} />
      </section>

      <div className="crawl-grid">
        <section className="panel">
          <header className="panel-heading"><div><span className="section-label">Discovery report</span><h2>Installation anatomy</h2></div><StatusPill value={report.orka.state} /></header>
          <div className="check-list">
            <Check good={report.orka.crdCount > 0} icon={<Box size={17} />} title="Orka resource definitions" detail={`${report.orka.crdCount} of ${report.orka.expectedCrdCount} release CRDs detected`} />
            <Check good={report.orka.controllerReady} icon={<Server size={17} />} title="Controller" detail={report.orka.controllerReady ? "Deployment is available" : "No ready controller deployment"} />
            <Check good={Boolean(report.defaultStorageClass)} icon={<Database size={17} />} title="Persistent storage" detail={report.defaultStorageClass ? `Default class: ${report.defaultStorageClass}` : "Required before installation"} />
            <Check good icon={<ShieldCheck size={17} />} title="Gateway boundary" detail="Kubeconfig remains in the loopback companion server" />
          </div>
        </section>
        <section className="panel">
          <header className="panel-heading"><div><span className="section-label">Readiness</span><h2>Components</h2></div><span className="mini-count">{report.components.length}</span></header>
          <div className="component-list">
            {report.components.length === 0 ? <div className="empty-compact"><Server size={21} /><span>No Orka workloads discovered</span></div> : report.components.map((component) => (
              <article key={`${component.kind}-${component.name}`}><i className={component.ready ? "ready" : "failed"} /><div><strong>{component.name}</strong><small>{component.kind} · {component.detail}</small></div></article>
            ))}
          </div>
        </section>
      </div>
      {report.warnings.length > 0 && <section className="warnings">{report.warnings.map((warning) => <div key={warning}><AlertTriangle size={16} />{warning}</div>)}</section>}
    </div>
  );
}

function Metric({ icon, tone, label, value, detail, suffix }: { icon: React.ReactNode; tone: string; label: string; value: string; detail: string; suffix?: string }) {
  return <article><span className={`metric-icon ${tone}`}>{icon}</span><div><small>{label}</small><strong>{value}<em>{suffix}</em></strong><p>{detail}</p></div></article>;
}

function Check({ good, icon, title, detail }: { good: boolean; icon: React.ReactNode; title: string; detail: string }) {
  return <article><span className={good ? "check-good" : "check-muted"}>{good ? <CheckCircle2 size={17} /> : icon}</span><div><strong>{title}</strong><p>{detail}</p></div></article>;
}
