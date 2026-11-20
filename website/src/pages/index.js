import React from 'react';
import Layout from '@theme/Layout';
import HeroSection from '../components/HeroSection';
import LiveTasksSection from '../components/LiveTasksSection';
import QuickStartSection from '../components/QuickStartSection';
import FeaturesSection from '../components/FeaturesSection';
import ProvidersSection from '../components/ProvidersSection';
import CtaSection from '../components/CtaSection';

export default function Home() {
  return (
    <Layout
      title="Kubernetes-native AI task orchestration"
      description="Orka turns AI agents and tool-using workflows into durable, observable Kubernetes tasks."
    >
      <main className="orka-landing">
        <HeroSection />
        <LiveTasksSection />
        <QuickStartSection />
        <FeaturesSection />
        <ProvidersSection />
        <CtaSection />
      </main>
    </Layout>
  );
}
