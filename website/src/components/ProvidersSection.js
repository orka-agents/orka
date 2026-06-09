import React from 'react';
import Link from '@docusaurus/Link';
import {providers} from '../data/landingPageData';

export default function ProvidersSection() {
  return (
    <section className="landing-section providers-section">
      <h2 className="section-title">Providers &amp; Runtimes</h2>
      <p className="section-subtitle">
        Bring your own models and coding agents — Orka keeps the credentials in
        the cluster and gives them a common API.
      </p>
      <div className="providers-grid">
        {providers.map((provider) => (
          <Link
            key={provider.name}
            to={provider.href}
            className="provider-card"
          >
            <h3 className="provider-name">{provider.name}</h3>
            <p className="provider-description">{provider.description}</p>
          </Link>
        ))}
      </div>
    </section>
  );
}
