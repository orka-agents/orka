import React from 'react';
import CodeBlock from '@theme/CodeBlock';

export default function QuickStartSection() {
  return (
    <section className="landing-section quickstart-section">
      <h2 className="section-title">Quick Start</h2>
      <p className="section-subtitle">
        One Helm install, one LLM secret, and you're chatting with an
        orchestrator that handles the rest.
      </p>
      <div className="quickstart-grid">
        <div className="quickstart-card">
          <h3>Install the controller</h3>
          <p>
            Deploy Orka into your cluster with Helm — CRDs, controller, and the
            built-in dashboard included.
          </p>
          <CodeBlock language="bash">{`helm install orka charts/orka \\
  --namespace orka-system \\
  --create-namespace`}</CodeBlock>
        </div>
        <div className="quickstart-card">
          <h3>Add a provider &amp; chat</h3>
          <p>
            Store an LLM key as a Kubernetes Secret, register a Provider, then
            open the dashboard or any OpenAI-compatible client.
          </p>
          <CodeBlock language="bash">{`kubectl create secret generic anthropic-secret \\
  --from-literal=api-key=your-api-key

kubectl port-forward -n orka-system svc/orka-api 8080:8080
# open http://localhost:8080`}</CodeBlock>
        </div>
      </div>
    </section>
  );
}
