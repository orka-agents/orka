// @ts-check

/** @type {import('@docusaurus/plugin-content-docs').SidebarsConfig} */
const sidebars = {
  tutorialSidebar: [
    'getting-started',
    {
      type: 'category',
      label: 'Core Concepts',
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
      items: [
        'guides/chat',
        'guides/multi-agent-coordination',
        'guides/autonomous-tasks',
        'guides/kontxt-quickstart',
        'guides/repository-security-scanning',
        'guides/github-label-triggers',
        'guides/ui',
      ],
    },
    {
      type: 'category',
      label: 'API Reference',
      items: [
        'reference/api-reference',
        'reference/openai-compat',
        'reference/anthropic-compat',
      ],
    },
    {
      type: 'category',
      label: 'Development',
      items: ['development/development', 'development/testing'],
    },
  ],
};

module.exports = sidebars;
