// @ts-check

/** @type {import('@docusaurus/plugin-content-docs').SidebarsConfig} */
const sidebars = {
  tutorialSidebar: [
    'getting-started',
    {
      type: 'category',
      label: 'Core Concepts',
      collapsed: false,
      items: [
        'concepts/architecture',
        'concepts/configuration',
        'concepts/agent-runtimes',
        'concepts/agent-sandbox',
        'concepts/substrate',
        'concepts/memory',
        'concepts/transaction-tokens',
        'concepts/outbound-access',
        'concepts/security',
      ],
    },
    {
      type: 'category',
      label: 'Guides',
      collapsed: false,
      items: [
        'guides/chat',
        'guides/bring-your-own-agent-runtime',
        'guides/multi-agent-coordination',
        'guides/autonomous-tasks',
        'guides/scheduled-tasks',
        'guides/transaction-token-migration',
        'guides/repository-security-scanning',
        'guides/repository-monitors',
        'guides/github-label-triggers',
        'guides/issue-to-pr-automation',
        'guides/ui',
        'guides/observability',
      ],
    },
    {
      type: 'category',
      label: 'Operations',
      collapsed: false,
      items: [
        'operations/harness-modes',
        'operations/agent-runtime-security',
        'operations/gateways',
        'operations/runbook',
      ],
    },
    {
      type: 'category',
      label: 'API Reference',
      collapsed: false,
      items: [
        'reference/api-reference',
        'reference/cli',
        'reference/cli-commands',
        'reference/execution-events',
        'reference/gateway-api',
        'reference/openai-compat',
        'reference/anthropic-compat',
      ],
    },
    {
      type: 'category',
      label: 'Development',
      collapsed: false,
      items: [
        'development/development',
        'development/testing',
        'development/security-scanning-design',
        'development/agent-runtime-adapter-contract',
      ],
    },
  ],
};

module.exports = sidebars;
