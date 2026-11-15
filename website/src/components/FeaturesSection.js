import React from 'react';
import Link from '@docusaurus/Link';
import {
  ArrowRight,
  Bot,
  GitMerge,
  GitPullRequest,
  MessagesSquare,
  Network,
  Plug,
  ShieldCheck,
  SquareTerminal,
  UserCheck,
} from 'lucide-react';
import {features} from '../data/landingPageData';

const ICONS = {
  Bot,
  GitMerge,
  GitPullRequest,
  MessagesSquare,
  Network,
  Plug,
  ShieldCheck,
  SquareTerminal,
  UserCheck,
};

export default function FeaturesSection() {
  return (
    <section className="orka-section">
      <div className="orka-container">
        <header className="orka-section__head">
          <p className="orka-kicker">What you can do</p>
          <h2>
            One install covers AI tasks, coding agents, repository automation,
            and the ways people reach them.
          </h2>
        </header>
        <div className="orka-features">
          {features.map((feature) => {
            const Icon = ICONS[feature.icon];
            return (
              <Link key={feature.title} to={feature.href} className="orka-feature">
                <span className="orka-feature__icon" aria-hidden="true">
                  {Icon && <Icon size={18} strokeWidth={1.75} />}
                </span>
                <h3>{feature.title}</h3>
                <p>{feature.description}</p>
                <span className="orka-feature__more">
                  Read more <ArrowRight size={14} aria-hidden="true" />
                </span>
              </Link>
            );
          })}
        </div>
      </div>
    </section>
  );
}
