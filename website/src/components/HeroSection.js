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
        Run AI agents and coding agents on your cluster.{' '}
        <span className="hero-highlight">Model keys never leave it.</span>
      </p>
      <p className="hero-description">
        Describe work as a Task. Orka runs it in a Pod, keeps a durable record
        of what happened, and hands you the result through an API, a CLI, or
        the built-in dashboard. One Helm command to install.
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
