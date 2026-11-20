import React from 'react';
import Link from '@docusaurus/Link';
import {ArrowUpRight} from 'lucide-react';
import {providers} from '../data/landingPageData';

export default function ProvidersSection() {
  return (
    <section className="orka-section">
      <div className="orka-container orka-split">
        <header className="orka-section__head">
          <p className="orka-kicker">Providers &amp; runtimes</p>
          <h2>Bring your own models and coding agents.</h2>
          <p>Orka keeps the credentials in the cluster and gives them a common API.</p>
        </header>
        <ul className="orka-providers">
          {providers.map((provider) => (
            <li key={provider.name}>
              <Link to={provider.href} className="orka-provider">
                <span className="orka-provider__name">{provider.name}</span>
                <span className="orka-provider__desc">{provider.description}</span>
                <ArrowUpRight size={18} aria-hidden="true" />
              </Link>
            </li>
          ))}
        </ul>
      </div>
    </section>
  );
}
