import React from 'react';
import Link from '@docusaurus/Link';
import {ctaLinks} from '../data/landingPageData';

export default function CtaSection() {
  return (
    <section className="orka-section">
      <div className="orka-container">
        <div className="orka-cta">
          <div>
            <h2>Join the community</h2>
            <p>Issues, ideas, and pull requests are all welcome.</p>
          </div>
          <div className="orka-cta__actions">
            {ctaLinks.map((link, i) => (
              <Link
                key={link.label}
                to={link.href}
                className={`button button--lg ${i === 0 ? 'button--primary' : 'button--secondary'}`}>
                {link.label}
              </Link>
            ))}
          </div>
        </div>
      </div>
    </section>
  );
}
