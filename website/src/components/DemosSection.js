import React from 'react';
import Link from '@docusaurus/Link';
import Heading from '@theme/Heading';
import DemoVideo from './DemoVideo';
import {demos} from '../data/demos';

export default function DemosSection() {
  return (
    <section
      className="landing-section demos-section"
      aria-labelledby="demos"
    >
      <Heading as="h2" id="demos" className="section-title">
        Demos
      </Heading>
      <p className="section-subtitle">
        Watch Orka workflows on Kubernetes, then follow the feature guides to
        try them on your cluster.
      </p>
      <div className="demos-grid">
        {demos.map((demo) => (
          <article key={demo.id} className="demo-card">
            <h3 className="demo-feature">{demo.feature}</h3>
            <DemoVideo videoId={demo.id} />
            <Link to={`/docs/demos#${demo.featureId}`}>
              Setup and details
            </Link>
          </article>
        ))}
      </div>
      <div className="demos-actions">
        <Link to="/docs/demos" className="button button--primary button--lg">
          Browse demo guides
        </Link>
      </div>
    </section>
  );
}
