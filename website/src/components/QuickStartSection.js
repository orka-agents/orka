import React from 'react';
import CodeBlock from '@theme/CodeBlock';
import Link from '@docusaurus/Link';

export default function QuickStartSection() {
  return (
    <section className="landing-section quickstart-section">
      <h2 className="section-title">Quick start</h2>
      <p className="section-subtitle">
        Install harness v2, then complete API access and provider setup with
        the getting-started guide.
      </p>
      <div className="quickstart-grid">
        <div className="quickstart-card">
          <h3>Install the controller</h3>
          <p>
            Build the current images and prepare the provider proxy, namespace,
            and installation Secrets using the{' '}
            <Link to="/docs/getting-started#install">installation guide</Link>.
            Generate the chart from the same checkout.
          </p>
          <CodeBlock language="bash">{`make docker-build-all
make docker-push-all
make manifests
# Continue with the Helm values in the installation guide.`}</CodeBlock>
        </div>
        <div className="quickstart-card">
          <h3>Configure access and open the dashboard</h3>
          <p>
            Complete the{' '}
            <Link to="/docs/getting-started#give-yourself-an-api-client">
              API client setup
            </Link>{' '}
            and{' '}
            <Link to="/docs/getting-started#your-first-task">Provider setup</Link>{' '}
            in Getting started. Then forward the API port and sign in to the
            dashboard with your client token.
          </p>
          <CodeBlock language="bash">{`kubectl port-forward -n orka-system svc/orka 8080:8080
# open http://localhost:8080`}</CodeBlock>
        </div>
      </div>
      <p className="section-subtitle">
        Coding agents such as Codex and Claude Code use the ACP runtime path. See{' '}
        <Link to="/docs/getting-started">Getting started</Link> and{' '}
        <Link to="/docs/release-status">Release status</Link>.
      </p>
    </section>
  );
}
