import React from 'react';
import Link from '@docusaurus/Link';
import useBaseUrl from '@docusaurus/useBaseUrl';

export default function HeroSection() {
  return (
    <section className="hero-section">
      <img
        className="hero-logo"
        alt="Orka logo"
        src={useBaseUrl('/img/orka-logo.png')}
      />
      <p className="hero-tagline">
        Kubernetes-native AI agent orchestration —{' '}
        <span className="hero-highlight">no orchestration graphs required.</span>
      </p>
      <p className="hero-description">
        Orka turns your cluster into an AI task execution platform. A
        coordinator agent decomposes complex work, spawns specialist agents to
        run in parallel, and synthesizes their results — each as an isolated,
        observable Kubernetes Job.
      </p>
      <div className="hero-buttons">
        <Link
          to="/docs/getting-started"
          className="button button--primary button--lg"
        >
          Get Started
        </Link>
        <Link
          to="https://github.com/orka-agents/orka"
          className="button button--secondary button--lg"
        >
          View on GitHub
        </Link>
      </div>
    </section>
  );
}
