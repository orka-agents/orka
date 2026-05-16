import Heading from '@theme/Heading';
import Layout from '@theme/Layout';
import Link from '@docusaurus/Link';
import logo from '@site/static/img/orka-logo.png';

const features = [
  {
    title: 'Run AI work as Kubernetes tasks',
    description:
      'Create container, AI, and delegated agent tasks with Kubernetes-native lifecycle, logs, results, and artifacts.',
  },
  {
    title: 'Coordinate multi-agent workflows',
    description:
      'Give coordinator agents explicit tools for delegation, waiting, and autonomous goal execution across child tasks.',
  },
  {
    title: 'Use familiar chat APIs',
    description:
      'Expose OpenAI-compatible and Anthropic-compatible endpoints while keeping provider credentials in Kubernetes Secrets.',
  },
];

function Feature({title, description}) {
  return (
    <article className="orka-feature-card">
      <Heading as="h3">{title}</Heading>
      <p>{description}</p>
    </article>
  );
}

export default function Home() {
  return (
    <Layout
      title="Kubernetes-native AI task orchestration"
      description="Orka turns AI agents and tool-using workflows into Kubernetes-native tasks.">
      <header className="hero hero--orka">
        <div className="container hero__content">
          <img className="hero__logo" src={logo} alt="Orka logo" />
          <Heading as="h1" className="hero__title">
            Kubernetes-native AI task orchestration
          </Heading>
          <p className="hero__subtitle">
            Orka turns AI agents and tool-using workflows into durable,
            observable Kubernetes tasks.
          </p>
          <div className="hero__buttons">
            <Link className="button button--primary button--lg" to="/docs/getting-started">
              Get Started
            </Link>
            <Link
              className="button button--secondary button--lg"
              to="/docs/api-reference">
              API Reference
            </Link>
          </div>
        </div>
      </header>
      <main>
        <section className="orka-features">
          <div className="container">
            <div className="orka-feature-grid">
              {features.map((feature) => (
                <Feature key={feature.title} {...feature} />
              ))}
            </div>
          </div>
        </section>
        <section className="orka-callout">
          <div className="container orka-callout__inner">
            <Heading as="h2">Designed for platform teams</Heading>
            <p>
              Use Orka to standardize provider access, agent runtime isolation,
              task artifacts, memory review, and repository security workflows on
              top of Kubernetes primitives.
            </p>
            <Link className="button button--outline button--lg" to="/docs/architecture">
              Explore the architecture
            </Link>
          </div>
        </section>
      </main>
    </Layout>
  );
}
