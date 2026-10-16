import React from 'react';
import Link from '@docusaurus/Link';

export default function QuickStartSection() {
  return (
    <section className="landing-section quickstart-section">
      <h2 className="section-title">Quick start</h2>
      <p className="section-subtitle">
        From an empty cluster to a coding agent reviewing a real repository.
      </p>
      <div className="quickstart-grid">
        <div className="quickstart-card">
          <h3>1. Install with Helm</h3>
          <p>
            One Helm command. The chart creates its namespaces, its encryption
            key, and its webhook certificate. Works on kind or minikube. No
            model API key needed yet.
          </p>
          <p>
            <Link to="/docs/installation">Install Orka</Link>
          </p>
        </div>
        <div className="quickstart-card">
          <h3>2. Run your first AI task</h3>
          <p>
            Connect to the API, add your Anthropic, OpenAI, or Azure OpenAI key
            as a Provider, and submit a Task.
          </p>
          <p>
            <Link to="/docs/getting-started#your-first-task">
              Run your first task
            </Link>
          </p>
        </div>
        <div className="quickstart-card">
          <h3>3. Run a coding agent</h3>
          <p>
            Connect your model gateway and let Codex or Claude Code review a
            repository from a warm pool.
          </p>
          <p>
            <Link to="/docs/getting-started#running-a-coding-agent">
              Run a coding agent
            </Link>
          </p>
        </div>
      </div>
      <p className="section-subtitle">
        New here? Read <Link to="/docs">What is Orka?</Link> For development,{' '}
        <Link to="/docs/build-from-source">build from source</Link>.
      </p>
    </section>
  );
}
