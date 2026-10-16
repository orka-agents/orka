import React from 'react';
import Link from '@docusaurus/Link';
import {features} from '../data/landingPageData';

export default function FeaturesSection() {
  return (
    <section className="landing-section features-section">
      <h2 className="section-title">What you can do</h2>
      <p className="section-subtitle">
        One install covers AI tasks, coding agents, repository automation, and the
        ways people reach them.
      </p>
      <div className="features-grid">
        {features.map((feature) => (
          <Link key={feature.title} to={feature.href} className="feature-card">
            <div className="feature-emoji" aria-hidden="true">
              {feature.emoji}
            </div>
            <h3 className="feature-title">{feature.title}</h3>
            <p className="feature-description">{feature.description}</p>
          </Link>
        ))}
      </div>
    </section>
  );
}
