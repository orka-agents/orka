import { AlertTriangle, CheckCircle2, LoaderCircle, LockKeyhole, Rocket, ShieldAlert } from "lucide-react";
import { useState } from "react";
import type { ClusterReport, InstallPlan } from "../domain";

export function Install({
  report, plan, planning, installing, onPlan, onInstall,
}: {
  report: ClusterReport;
  plan?: InstallPlan;
  planning: boolean;
  installing: boolean;
  onPlan: () => Promise<void>;
  onInstall: (plan: InstallPlan, confirmation: string, access: boolean, persistence: boolean) => Promise<void>;
}) {
  const [confirmation, setConfirmation] = useState("");
  const [access, setAccess] = useState(false);
  const [persistence, setPersistence] = useState(false);
  const existing = report.orka.state !== "absent";
  const canInstall = Boolean(plan?.allowed && confirmation === plan.context && access && persistence && !installing);
  return (
    <div className="page-content">
      <section className="deploy-intro">
        <span><Rocket size={25} /></span>
        <div><small>Pinned release</small><h2>Deploy Orka {plan?.chartVersion ?? "release"}</h2><p>Plan and install the official digest-pinned Helm chart into a clean cluster.</p></div>
        <code>{plan ? `sha256:${plan.chartDigest.slice(0, 8)}…${plan.chartDigest.slice(-4)}` : "Verified before planning"}</code>
      </section>
      {existing ? <section className={`install-stop state-${report.orka.state}`}><ShieldAlert size={24} /><div><h2>{report.orka.state === "ready" ? "Orka is already installed" : "Automatic deployment is blocked"}</h2><p>{report.orka.message} This console never upgrades, repairs, or overwrites an existing installation.</p></div></section> : <div className="install-grid">
        <section className="panel">
          <header className="panel-heading"><div><span className="section-label">Step 1</span><h2>Preflight plan</h2></div><LockKeyhole size={18} /></header>
          <div className="install-body">
            <div className="install-target"><span>Target context</span><strong>{report.context.name}</strong><code>{report.context.server}</code></div>
            <ul className="impact-list"><li><CheckCircle2 size={15} />Create <code>orka-system</code> and <code>orka-runtimes</code></li><li><CheckCircle2 size={15} />Install release CRDs and admission policy</li><li><CheckCircle2 size={15} />Create cluster-wide RBAC and webhooks</li><li><CheckCircle2 size={15} />Provision persistent controller and publisher volumes</li></ul>
            <button className="button button-secondary full-button" type="button" onClick={() => void onPlan()} disabled={planning}>{planning && <LoaderCircle className="spin" size={16} />}{planning ? "Running preflight…" : plan ? "Refresh install plan" : "Create install plan"}</button>
          </div>
        </section>
        <section className="panel">
          <header className="panel-heading"><div><span className="section-label">Step 2</span><h2>Review and confirm</h2></div>{plan?.allowed && <span className="plan-valid"><CheckCircle2 size={13} />Plan valid</span>}</header>
          <div className="install-body">
            {!plan ? <div className="empty-compact"><LockKeyhole size={22} /><span>Create a preflight plan before confirmation.</span></div> : <>
              {plan.blockers.map((blocker) => <div className="plan-blocker" key={blocker}><AlertTriangle size={14} />{blocker}</div>)}
              {plan.warnings.map((warning) => <div className="plan-warning" key={warning}><AlertTriangle size={14} />{warning}</div>)}
              <label className="confirm-check"><input type="checkbox" checked={access} onChange={(event) => setAccess(event.target.checked)} /><span>I understand this grants Orka cluster-wide control-plane access.</span></label>
              <label className="confirm-check"><input type="checkbox" checked={persistence} onChange={(event) => setPersistence(event.target.checked)} /><span>I understand persistent cluster data and Secrets will be created.</span></label>
              <label className="field confirm-field"><span>Type <code>{plan.context}</code> to confirm</span><input value={confirmation} onChange={(event) => setConfirmation(event.target.value)} autoComplete="off" /></label>
              <button className="button button-primary full-button" type="button" disabled={!canInstall} onClick={() => void onInstall(plan, confirmation, access, persistence)}>{installing ? <LoaderCircle className="spin" size={17} /> : <Rocket size={17} />}{installing ? "Installing Orka (up to 10m)…" : "Install Orka"}</button>
            </>}
          </div>
        </section>
      </div>}
    </div>
  );
}
