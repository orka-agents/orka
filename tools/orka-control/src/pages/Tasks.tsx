import { Bot, Box, Clock3, LoaderCircle, Play, TerminalSquare } from "lucide-react";
import { useRef, useState, type FormEvent } from "react";
import type { ClusterReport, RunTaskInput } from "../domain";
import { relativeTime } from "../lib/format";
import { StatusPill } from "../components/Status";

export function Tasks({ report, running, onRun, onNavigate }: { report: ClusterReport; running: boolean; onRun: (input: RunTaskInput) => Promise<boolean>; onNavigate: (page: string) => void }) {
  const [mode, setMode] = useState<"smoke" | "agent">("smoke");
  const [agentName, setAgentName] = useState(report.agents[0]?.name ?? "");
  const [prompt, setPrompt] = useState("");
  const requestKey = useRef(crypto.randomUUID());
  const submit = async (event: FormEvent) => {
    event.preventDefault();
    const succeeded = await onRun({ idempotencyKey: requestKey.current, context: report.context.name, namespace: report.orka.namespace, mode, agentName: mode === "agent" ? agentName : undefined, prompt: mode === "agent" ? prompt : undefined });
    if (succeeded) {
      requestKey.current = crypto.randomUUID();
      if (mode === "agent") setPrompt("");
    }
  };
  if (report.orka.state !== "ready") return <div className="blocked-page"><span><Box size={28} /></span><h2>Orka is not ready</h2><p>Crawl found no ready controller on <code>{report.context.name}</code>.</p><button className="button button-primary" type="button" onClick={() => onNavigate("install")}>Review quick deploy</button></div>;
  return (
    <div className="page-content run-layout">
      <form className="panel run-composer" onSubmit={submit}>
        <header className="panel-heading"><div><span className="section-label">Create Task</span><h2>Give Orka some work</h2></div><Play size={18} /></header>
        <div className="run-body">
          <div className="mode-cards">
            <button type="button" className={mode === "smoke" ? "active" : ""} onClick={() => setMode("smoke")}><TerminalSquare size={19} /><span><strong>Smoke task</strong><small>Model-free install check</small></span></button>
            <button type="button" className={mode === "agent" ? "active" : ""} onClick={() => setMode("agent")} disabled={report.agents.length === 0}><Bot size={19} /><span><strong>Agent task</strong><small>{report.agents.length ? "Use an existing Agent" : "No Agents configured"}</small></span></button>
          </div>
          {mode === "smoke" ? <div className="fixed-task"><small>Fixed command</small><code>printf ORKA_CONSOLE_OK</code><p>Runs with a three-minute timeout and no retries. Arbitrary image or shell input is not accepted.</p></div> : <>
            <label className="field"><span>Agent</span><select value={agentName} onChange={(event) => setAgentName(event.target.value)}>{report.agents.map((agent) => <option key={agent.uid} value={agent.name}>{agent.name} · {agent.runtime}</option>)}</select></label>
            <label className="field"><span>Prompt</span><textarea value={prompt} onChange={(event) => setPrompt(event.target.value)} maxLength={8000} required placeholder="Describe the work for this agent…" /><small>{prompt.length}/8000 characters</small></label>
          </>}
          <button className="button button-primary submit-task" type="submit" disabled={running || (mode === "agent" && (!agentName || !prompt.trim()))}>{running ? <LoaderCircle className="spin" size={17} /> : <Play size={17} />}{running ? "Submitting…" : mode === "smoke" ? "Run smoke task" : "Run agent task"}</button>
        </div>
      </form>
      <section className="panel">
        <header className="panel-heading"><div><span className="section-label">Kubernetes records</span><h2>Recent tasks</h2></div><span className="mini-count">{report.tasks.length}</span></header>
        <div className="task-list">
          {report.tasks.length === 0 ? <div className="empty-compact"><Clock3 size={22} /><span>No tasks yet. Run the smoke check first.</span></div> : report.tasks.slice(0, 12).map((task) => (
            <article key={task.uid}><span className={`task-icon type-${task.type}`}>{task.type === "agent" ? <Bot size={16} /> : <TerminalSquare size={16} />}</span><div><strong>{task.name}</strong><small>{task.agentName ?? task.type} · {relativeTime(task.createdAt)}</small></div><StatusPill value={task.phase} /></article>
          ))}
        </div>
      </section>
    </div>
  );
}
