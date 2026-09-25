import React from 'react';
import Link from '@docusaurus/Link';

export default function QuickStartSection() {
  return (
    <section className="landing-section quickstart-section">
      <h2 className="section-title">Quick start</h2>
      <p className="section-subtitle">
        Install Orka on Kubernetes and run your first task.
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
            <Link to="/docs/installation">Open the installation guide</Link>
          </p>
        </div>
        <div className="quickstart-card">
          <h3>2. Run your first AI task</h3>
          <p>
            Connect to the API, add your Anthropic, OpenAI, or Azure OpenAI key
            as a Provider, and submit a Task.
          </p>
          <p>
            <Link to="/docs/getting-started#connect-to-the-api">
              Continue with Getting started
            </Link>
          </p>
        </div>
      </div>
      <p className="section-subtitle">
        For development,{' '}
        <Link to="/docs/build-from-source">
          build from source
        </Link>.
      </p>
    </section>
  );
}
