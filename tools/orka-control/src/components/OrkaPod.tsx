import { AlertCircle, CheckCircle2, Clock3, Coffee, Fish, MousePointer2, RotateCcw, Sparkles, Waves } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import type { OrkaAgent, OrkaTask, TaskPhase } from "../domain";

type OrkaMood = "working" | "thinking" | "waiting" | "resting" | "succeeded" | "failed";
interface PodOrka { id: string; name: string; mood: OrkaMood; task: string; update: string; runtime: string }

const moodCopy: Record<OrkaMood, { label: string; message: string; icon: typeof Waves }> = {
  working: { label: "Working", message: "Deep in the current task", icon: Waves },
  thinking: { label: "Thinking", message: "Exploring the problem space", icon: Sparkles },
  waiting: { label: "Waiting", message: "Queued for cluster capacity", icon: Clock3 },
  resting: { label: "Resting", message: "Available for a new task", icon: Coffee },
  succeeded: { label: "Succeeded", message: "Task completed successfully", icon: CheckCircle2 },
  failed: { label: "Needs help", message: "The latest task needs attention", icon: AlertCircle },
};

function taskMood(phase: TaskPhase): OrkaMood {
  if (phase === "Running" || phase === "Finalizing") return "working";
  if (phase === "Pending" || phase === "Scheduled") return "waiting";
  if (phase === "Succeeded") return "succeeded";
  if (phase === "Failed" || phase === "Cancelled") return "failed";
  return "thinking";
}

function makePod(agents: OrkaAgent[], tasks: OrkaTask[]): PodOrka[] {
  const liveTasks = tasks.filter((task) => ["Pending", "Running", "Finalizing", "Scheduled"].includes(task.phase));
  const pod = agents.map((agent) => {
    const task = liveTasks.find((candidate) => candidate.agentName === agent.name);
    return {
      id: agent.uid,
      name: agent.name,
      mood: task ? taskMood(task.phase) : agent.ready ? "resting" as const : "failed" as const,
      task: task?.name ?? "No active task",
      update: task ? `${task.type} task is ${task.phase.toLowerCase()}` : agent.ready ? `${agent.runtime} runtime is ready` : "Agent is not ready",
      runtime: task?.startedAt ? `Since ${new Date(task.startedAt).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}` : "Idle",
    };
  });
  for (const task of liveTasks.filter((candidate) => !candidate.agentName || !agents.some((agent) => agent.name === candidate.agentName))) {
    pod.push({ id: task.uid, name: task.name.slice(0, 14), mood: taskMood(task.phase), task: task.name, update: `${task.type} task is ${task.phase.toLowerCase()}`, runtime: task.startedAt ? "In progress" : "Queued" });
  }
  return pod;
}

function PixelOrka({ mood }: { mood: OrkaMood }) {
  return (
    <span className={`pixel-orka orka-${mood}`} aria-hidden="true">
      <span className="orka-fin" />
      <span className="orka-body"><span className="orka-eye" /><span className="orka-patch" /><span className="orka-belly" /></span>
      <span className="orka-tail orka-tail-top" /><span className="orka-tail orka-tail-bottom" />
      <span className="orka-bubbles"><i /><i /><i /></span>
    </span>
  );
}

export function OrkaPod({ agents, tasks }: { agents: OrkaAgent[]; tasks: OrkaTask[] }) {
  const pod = makePod(agents, tasks);
  const [selectedId, setSelectedId] = useState<string>();
  const [casting, setCasting] = useState(false);
  const [reelingId, setReelingId] = useState<string>();
  const timer = useRef<number | undefined>(undefined);
  const selected = pod.find((orka) => orka.id === selectedId);
  const activeCount = pod.filter((orka) => ["working", "thinking", "waiting"].includes(orka.mood)).length;

  useEffect(() => () => window.clearTimeout(timer.current), []);
  const startCasting = () => {
    window.clearTimeout(timer.current);
    setReelingId(undefined);
    setSelectedId(undefined);
    setCasting(true);
  };
  const inspect = (orka: PodOrka) => {
    if (!casting || reelingId) return;
    setReelingId(orka.id);
    const duration = window.matchMedia("(prefers-reduced-motion: reduce)").matches ? 0 : 650;
    timer.current = window.setTimeout(() => {
      setSelectedId(orka.id);
      setReelingId(undefined);
      setCasting(false);
    }, duration);
  };

  return (
    <section className="panel pod-panel">
      <header className="panel-heading">
        <div><span className="section-label">Agent habitat</span><h2>The Orka pod</h2></div>
        <div className="pod-actions"><span><i />{activeCount} active</span><button type="button" className={casting ? "casting" : ""} onClick={startCasting} disabled={Boolean(reelingId) || pod.length === 0}>{selected ? <RotateCcw size={14} /> : <Fish size={14} />}{pod.length === 0 ? "Pod is empty" : casting ? "Rod ready" : selected ? "Cast again" : "Cast status rod"}</button></div>
      </header>
      <div className="pod-body">
        <div className={`orka-tank ${casting ? "is-casting" : ""}`}>
          <div className="fishing-rig" aria-hidden="true"><i /><b /><span /><em /></div>
          {casting && !reelingId && <div className="cast-instruction"><MousePointer2 size={13} />Pick an Orka to inspect</div>}
          <div className={`orka-grid ${pod.length === 0 ? "empty" : ""}`}>
            {pod.length === 0 && <div className="empty-pod"><PixelOrka mood="resting" /><strong>No agents are swimming yet</strong><span>Create an Agent and run work to populate the pod.</span></div>}
            {pod.map((orka) => {
              const isSelected = selected?.id === orka.id;
              const isReeling = reelingId === orka.id;
              return (
                <button key={orka.id} type="button" className={`orka-tile ${isSelected ? "selected" : ""} ${isReeling ? "hooked" : ""}`} aria-pressed={isSelected} disabled={!casting || Boolean(reelingId)} onClick={() => inspect(orka)}>
                  <span className={`state-dot state-${orka.mood}`} /><PixelOrka mood={orka.mood} /><small>{isReeling ? "Reeling…" : orka.name}</small>
                </button>
              );
            })}
          </div>
        </div>
        {selected ? <StatusCard orka={selected} /> : <div className={`pod-empty-card ${casting ? "armed" : ""}`}><Fish size={28} /><small>{casting ? "Rod armed" : "Status inspector"}</small><strong>{casting ? "Choose an Orka in the pod" : "Cast the rod to check a status"}</strong><p>The rod reads telemetry without interrupting the Task.</p></div>}
      </div>
    </section>
  );
}

function StatusCard({ orka }: { orka: PodOrka }) {
  const mood = moodCopy[orka.mood];
  const Icon = mood.icon;
  return (
    <div className={`pod-status status-${orka.mood}`} aria-live="polite">
      <div className="caught"><Fish size={12} />Status caught</div>
      <div className="pod-status-title"><span><PixelOrka mood={orka.mood} /></span><div><small>Orka agent</small><strong>{orka.name}</strong></div><b><Icon size={13} />{mood.label}</b></div>
      <div className="pod-speech"><strong>{mood.message}.</strong><p>{orka.update}</p></div>
      <dl><div><dt>Current task</dt><dd>{orka.task}</dd></div><div><dt>Runtime</dt><dd>{orka.runtime}</dd></div></dl>
      <p className="pod-tip">Status only: no agents were disturbed.</p>
    </div>
  );
}
