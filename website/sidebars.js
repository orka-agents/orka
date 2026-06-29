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
        'concepts/kontxt',
        'concepts/security',
      ],
    },
    {
      type: 'category',
      label: 'Guides',
      collapsed: false,
      items: [
        'guides/chat',
        'guides/multi-agent-coordination',
        'guides/autonomous-tasks',
        'guides/kontxt-quickstart',
        'guides/cli-harness-wrapper',
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
      label: 'API Reference',
      collapsed: false,
      items: [
        'reference/api-reference',
        'reference/cli',
        'reference/cli-commands',
        'reference/execution-events',
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
      ],
    },
  ],
};

module.exports = sidebars;
