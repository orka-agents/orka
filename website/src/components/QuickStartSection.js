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
          <h3>1. Install Orka</h3>
          <p>
            Follow the Helm guide to set up a new installation and check it
            with a test task.
          </p>
          <p>
            <Link to="/docs/installation">Open the installation guide</Link>
          </p>
        </div>
        <div className="quickstart-card">
          <h3>2. Run your first AI task</h3>
          <p>
            Connect to Orka's API, add a model provider, and submit a task.
          </p>
          <p>
            <Link to="/docs/getting-started#give-yourself-an-api-client">
              Continue with Getting started
            </Link>
          </p>
        </div>
      </div>
      <p className="section-subtitle">
        For development,{' '}
        <Link to="/docs/getting-started#option-b-current-main-from-source">
          build from source
        </Link>.
      </p>
    </section>
  );
}
