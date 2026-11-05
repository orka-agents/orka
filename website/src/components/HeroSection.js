import React, {useState} from 'react';
import Link from '@docusaurus/Link';
import useBaseUrl from '@docusaurus/useBaseUrl';
import ThemedImage from '@theme/ThemedImage';
import {ArrowRight, Pause, Play} from 'lucide-react';
import InstallCommand from './InstallCommand';

// Pods orbit the mascot on the sonar rings. Angles are fixed; the rings rotate.
const ORBITS = [
  {inset: '0%', dur: '120s', dir: 'normal', pods: [{a: 200}, {a: 320, variant: 'hollow'}]},
  {inset: '11%', dur: '80s', dir: 'reverse', pods: [{a: 30, variant: 'run'}, {a: 150}, {a: 255, variant: 'hollow'}]},
  {inset: '22%', dur: '55s', dir: 'normal', pods: [{a: 95}, {a: 290, variant: 'run'}]},
];

function podPosition(angle) {
  const rad = (angle * Math.PI) / 180;
  return {
    '--x': `${(50 + 50 * Math.cos(rad)).toFixed(2)}%`,
    '--y': `${(50 + 50 * Math.sin(rad)).toFixed(2)}%`,
  };
}

export default function HeroSection() {
  const [paused, setPaused] = useState(false);

  return (
    <section className="orka-hero" data-paused={paused || undefined}>
      <div className="orka-container orka-hero__grid">
        <div className="orka-hero__copy">
          <p className="orka-eyebrow orka-reveal" style={{'--d': '0ms'}}>
            <span className="orka-live-dot" aria-hidden="true" />
            Open source · MIT · Kubernetes-native
          </p>
          <h1 className="orka-hero__title orka-reveal" style={{'--d': '80ms'}}>
            Run AI agents and coding agents on your cluster.{' '}
            <span className="orka-hero__kicker">Model keys never leave it.</span>
          </h1>
          <p className="orka-hero__lede orka-reveal" style={{'--d': '160ms'}}>
            Describe work as a Task. Orka runs it in a Pod, keeps a durable
            record of what happened, and hands you the result through an API, a
            CLI, or the built-in dashboard. One Helm command to install.
          </p>
          <div className="orka-hero__actions orka-reveal" style={{'--d': '240ms'}}>
            <Link className="button button--primary button--lg" to="/docs/getting-started">
              Get started
            </Link>
            <Link className="button button--secondary button--lg" to="pathname:///demo/">
              Try the dashboard
            </Link>
            <Link className="orka-text-link" to="/docs/demos">
              Watch demos <ArrowRight size={16} aria-hidden="true" />
            </Link>
          </div>
          <InstallCommand className="orka-reveal" style={{'--d': '320ms'}} />
          <button
            type="button"
            className="orka-hero__pause"
            onClick={() => setPaused((value) => !value)}>
            {paused ? <Play size={13} aria-hidden="true" /> : <Pause size={13} aria-hidden="true" />}
            {paused ? 'Play animations' : 'Pause animations'}
          </button>
        </div>

        <div className="orka-hero__visual">
          <div className="orka-sonar" aria-hidden="true">
            <span className="orka-sonar__ring orka-sonar__ring--dashed" style={{'--s': '100%'}} />
            <span className="orka-sonar__ring" style={{'--s': '78%'}} />
            <span className="orka-sonar__ring" style={{'--s': '56%'}} />
            <span className="orka-sonar__ping" />
            <span className="orka-sonar__ping orka-sonar__ping--late" />
            {ORBITS.map((orbit) => (
              <div
                key={orbit.inset}
                className="orka-orbit"
                style={{'--inset': orbit.inset, '--dur': orbit.dur, '--dir': orbit.dir}}>
                {orbit.pods.map((pod) => (
                  <span
                    key={pod.a}
                    className={`orka-pod${pod.variant ? ` orka-pod--${pod.variant}` : ''}`}
                    style={podPosition(pod.a)}
                  />
                ))}
              </div>
            ))}
          </div>
          <ThemedImage
            className="orka-hero__mascot"
            alt="Orka mascot: an orca in a captain's cap with a headset and a terminal pendant"
            width={667}
            height={721}
            sources={{
              light: useBaseUrl('/img/orka-mascot.webp'),
              dark: useBaseUrl('/img/orka-mascot-dark.webp'),
            }}
          />
        </div>
      </div>
    </section>
  );
}
