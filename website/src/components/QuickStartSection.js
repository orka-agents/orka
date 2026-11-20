import React from 'react';
import Link from '@docusaurus/Link';
import {ArrowRight} from 'lucide-react';

const STEPS = [
  {
    title: 'Install with Helm',
    body: 'One Helm command. The chart creates its namespaces, its encryption key, and its webhook certificate. Works on kind or minikube. No model API key needed yet.',
    link: {to: '/docs/installation', label: 'Install Orka'},
  },
  {
    title: 'Run your first AI task',
    body: 'Connect to the API, add your Anthropic, OpenAI, or Azure OpenAI key as a Provider, and submit a Task.',
    link: {to: '/docs/getting-started#your-first-task', label: 'Run your first task'},
  },
  {
    title: 'Run a coding agent',
    body: 'Connect your model gateway and let Codex or Claude Code review a repository from a warm pool.',
    link: {to: '/docs/getting-started#running-a-coding-agent', label: 'Run a coding agent'},
  },
];

export default function QuickStartSection() {
  return (
    <section className="orka-section">
      <div className="orka-container">
        <header className="orka-section__head">
          <p className="orka-kicker">Quick start</p>
          <h2>From an empty cluster to a coding agent reviewing a real repository.</h2>
        </header>
        <ol className="orka-steps">
          {STEPS.map((step, i) => (
            <li key={step.title} className="orka-step">
              <span className="orka-step__n" aria-hidden="true">
                {i + 1}
              </span>
              <h3>{step.title}</h3>
              <p>{step.body}</p>
              <Link className="orka-text-link" to={step.link.to}>
                {step.link.label} <ArrowRight size={16} aria-hidden="true" />
              </Link>
            </li>
          ))}
        </ol>
        <p className="orka-section__note">
          New here? Read <Link to="/docs">What is Orka?</Link> For development,{' '}
          <Link to="/docs/build-from-source">build from source</Link>.
        </p>
      </div>
    </section>
  );
}
