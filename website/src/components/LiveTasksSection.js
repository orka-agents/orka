import React, {useEffect, useRef, useState} from 'react';
import clsx from 'clsx';
import {Pause, Play} from 'lucide-react';

// A scripted slice of a real Orka day (Task names come from the dashboard demo). The feed
// and the terminal both derive from the same events, so they always agree.
const TASKS = {
  checkout: {name: 'checkout-latency-investigation-7k2qp', type: 'ai', priority: 5},
  fix: {name: 'fix-cart-rounding-bug-x9m4d', type: 'agent', priority: 0},
  drift: {name: 'terraform-drift-check-u5r1a', type: 'container', priority: 0},
  review: {name: 'review-pr-1482-q8m4d', type: 'agent', priority: 0},
  notes: {name: 'release-v2-14-0-notes-h4t6n', type: 'agent', priority: 0},
};

const SCRIPT = [
  {task: 'checkout', phase: 'Pending'},
  {task: 'checkout', phase: 'Running'},
  {task: 'fix', phase: 'Pending'},
  {task: 'fix', phase: 'Running'},
  {task: 'checkout', approval: 'requested'},
  {task: 'drift', phase: 'Pending'},
  {task: 'drift', phase: 'Running'},
  {task: 'review', phase: 'Pending'},
  {task: 'checkout', approval: 'approved'},
  {task: 'drift', phase: 'Succeeded'},
  {task: 'review', phase: 'Running'},
  {task: 'fix', phase: 'Succeeded'},
  {task: 'checkout', phase: 'Succeeded'},
  {task: 'notes', phase: 'Pending'},
  {task: 'notes', phase: 'Running'},
  {task: 'review', phase: 'Succeeded'},
];

// The static frame (server render, reduced motion): approval pending, three Tasks running.
const REST_STEP = 7;
const HOLD_STEPS = 2;
const STEP_MS = 1700;
const SECONDS_PER_STEP = 9;

function age(steps) {
  const s = steps * SECONDS_PER_STEP + 2;
  return s < 60 ? `${s}s` : `${Math.floor(s / 60)}m${s % 60 ? `${s % 60}s` : ''}`;
}

function derive(step) {
  const last = Math.min(step, SCRIPT.length - 1);
  const state = {};
  const lines = [];
  SCRIPT.slice(0, last + 1).forEach((event, i) => {
    const current = state[event.task] || {key: event.task, created: i, ...TASKS[event.task]};
    if (event.phase) {
      current.phase = event.phase;
      lines.push({i, task: TASKS[event.task], phase: event.phase, age: age(i - current.created)});
    }
    if (event.approval === 'requested') current.approval = 'requested';
    if (event.approval === 'approved') {
      current.approval = 'approved';
      current.approvedAt = i;
    }
    state[event.task] = current;
  });
  // Oldest first: a Task keeps its row from the moment it appears, so nothing shifts.
  const rows = Object.values(state)
    .sort((a, b) => a.created - b.created)
    .map((row) => ({...row, age: age(last - row.created)}));
  return {rows, lines: lines.slice(-9), last};
}

const SLOTS = Object.keys(TASKS).length;

function pad(value, width) {
  return String(value).padEnd(width, ' ');
}

function Phase({phase}) {
  return (
    <span className={`orka-phase orka-phase--${phase}`}>
      <i aria-hidden="true" />
      {phase}
    </span>
  );
}

export default function LiveTasksSection() {
  const [step, setStep] = useState(REST_STEP);
  const [paused, setPaused] = useState(false);
  const [reduced, setReduced] = useState(false);
  const ref = useRef(null);

  useEffect(() => {
    const query = window.matchMedia?.('(prefers-reduced-motion: reduce)');
    if (!query) return undefined;
    const update = () => setReduced(query.matches);
    update();
    query.addEventListener?.('change', update);
    return () => query.removeEventListener?.('change', update);
  }, []);

  // Plays only while on screen, not paused, and motion is welcome; reduced motion shows the
  // static frame.
  useEffect(() => {
    if (reduced) setStep(REST_STEP);
    if (reduced || paused || !ref.current) return undefined;
    let timer = null;
    const start = () => {
      if (timer) return;
      timer = setInterval(() => {
        setStep((s) => (s >= SCRIPT.length - 1 + HOLD_STEPS ? 0 : s + 1));
      }, STEP_MS);
    };
    const stop = () => {
      clearInterval(timer);
      timer = null;
    };
    const observer = new IntersectionObserver(
      ([entry]) => (entry.isIntersecting ? start() : stop()),
      {threshold: 0.2},
    );
    observer.observe(ref.current);
    return () => {
      observer.disconnect();
      stop();
    };
  }, [reduced, paused]);

  const {rows, lines, last} = derive(step);

  return (
    <section
      className="orka-band"
      ref={ref}
      aria-labelledby="orka-live-title"
      data-paused={paused || undefined}>
      <div className="orka-container">
        <header className="orka-band__head">
          <p className="orka-kicker">Live view</p>
          <h2 id="orka-live-title">Every agent runs as a Task you can watch.</h2>
          <p>
            Each Task runs in a Pod and reports its phase. Consequential actions
            wait for a person, and every step is kept in a durable record.
          </p>
          <button
            type="button"
            className="orka-band__pause"
            hidden={reduced}
            aria-label={paused ? 'Play the live view' : 'Pause the live view'}
            onClick={() => setPaused((value) => !value)}>
            {paused ? <Play size={13} aria-hidden="true" /> : <Pause size={13} aria-hidden="true" />}
            {paused ? 'Play' : 'Pause'}
          </button>
        </header>

        <p className="orka-sr-only">
          Example Task timeline: {TASKS.checkout.name} (AI) moves from Pending to Running,
          requests approval to roll back checkout-service in production to v3.18.2,
          receives approval, and succeeds. {TASKS.fix.name} and {TASKS.review.name} (agents),
          and {TASKS.drift.name} (container), each move from Pending to Running to Succeeded.
          {' '}{TASKS.notes.name} (agent) moves from Pending to Running and is still running
          when the example ends. The terminal shows the same phase changes as
          kubectl get tasks -w, with columns for name, type, phase, priority, and age.
        </p>

        <div className="orka-band__grid" aria-hidden="true">
          <div className="orka-window">
            <div className="orka-window__bar">
              <strong>Tasks</strong>
              <span>default</span>
              <span className="orka-window__live">
                <span className="orka-live-dot" />
                {paused ? 'paused' : 'live'}
              </span>
            </div>
            <ul className="orka-feed">
              {rows.map((row) => {
                const waiting = row.approval === 'requested';
                const approvedNow = row.approval === 'approved' && last - row.approvedAt <= 1;
                return (
                  <li key={row.key} className={clsx('orka-feed__row', waiting && 'orka-feed__row--attn')}>
                    <span className="orka-feed__name">
                      {row.name}
                      {waiting && (
                        <span className="orka-feed__note">
                          Roll back checkout-service (prod) to v3.18.2?
                        </span>
                      )}
                      {approvedNow && <span className="orka-feed__note">Approved by maya.chen</span>}
                    </span>
                    <span className="orka-feed__type">{row.type}</span>
                    {waiting ? (
                      <span className="orka-phase orka-phase--attn">
                        <i aria-hidden="true" />
                        Needs approval
                      </span>
                    ) : (
                      <Phase phase={row.phase} />
                    )}
                    <span className="orka-feed__age">{row.age}</span>
                  </li>
                );
              })}
              {Array.from({length: SLOTS - rows.length}, (_, i) => (
                <li key={`empty-${i}`} className="orka-feed__row orka-feed__row--empty" />
              ))}
            </ul>
          </div>

          <div className="orka-window">
            <div className="orka-window__bar">
              <strong>Terminal</strong>
              <span>orka-system</span>
            </div>
            <pre className="orka-terminal">
              <span className="orka-terminal__cmd">$ kubectl get tasks -w{'\n'}</span>
              <span className="orka-terminal__head">
                {pad('NAME', 38)}
                {pad('TYPE', 11)}
                {pad('PHASE', 11)}
                {pad('PRIORITY', 10)}
                AGE{'\n'}
              </span>
              {lines.map((line) => (
                <span
                  key={line.i}
                  className="orka-terminal__line"
                  style={{'--st': `var(--orka-${{Pending: 'pending', Running: 'running', Succeeded: 'ok'}[line.phase]})`}}>
                  {pad(line.task.name, 38)}
                  {pad(line.task.type, 11)}
                  <b>{pad(line.phase, 11)}</b>
                  {pad(line.task.priority, 10)}
                  {line.age}
                </span>
              ))}
              <span className="orka-terminal__caret" />
            </pre>
          </div>
        </div>

        <div className="orka-band__foot">
          <span>Tasks run in Pods on your cluster</span>
          <span>Approvals hold consequential actions for a person</span>
          <span>Every phase change is recorded</span>
        </div>
      </div>
    </section>
  );
}
