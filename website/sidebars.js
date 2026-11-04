// @ts-check

/** @type {import('@docusaurus/plugin-content-docs').SidebarsConfig} */
const sidebars = {
  tutorialSidebar: [
    'overview',
    {
      type: 'category',
      label: 'Get started',
      collapsed: false,
      items: ['operations/installation', 'getting-started', 'demos'],
    },
    {
      type: 'category',
      label: 'Guides',
      collapsed: false,
      items: [
        {
          type: 'category',
          label: 'Build agents',
          collapsed: false,
          items: [
            'guides/agents-skills-and-tools',
            'guides/toolboxes',
            'concepts/remote-mcp',
            'guides/multi-agent-coordination',
            'guides/autonomous-tasks',
            'concepts/memory',
            'concepts/soul',
          ],
        },
        {
          type: 'category',
          label: 'Run work',
          collapsed: false,
          items: [
            'guides/container-tasks',
            'guides/scheduled-tasks',
            'guides/codex-session-migration',
          ],
        },
        {
          type: 'category',
          label: 'Automate repositories',
          collapsed: false,
          items: [
            'guides/repository-monitors',
            'guides/issue-to-pr-automation',
            'guides/repository-security-scanning',
            'guides/usage-and-pr-outcomes',
          ],
        },
        {
          type: 'category',
          label: 'Connect people and tools',
          collapsed: false,
          items: [
            'guides/chat',
            'guides/ui',
            'reference/openai-compat',
            'reference/anthropic-compat',
            'operations/shared-compatibility-api',
            'guides/gateways',
          ],
        },
        {
          type: 'category',
          label: 'Govern access',
          collapsed: false,
          items: [
            'guides/human-approval-v2',
            'guides/connectors',
            'concepts/outbound-access',
            'concepts/transaction-tokens',
          ],
        },
        'guides/observability',
        'guides/bring-your-own-agent-runtime',
      ],
    },
    {
      type: 'category',
      label: 'Concepts',
      collapsed: false,
      items: [
        'concepts/architecture',
        'concepts/agent-runtimes',
        'concepts/security',
        {
          type: 'category',
          label: 'Execution workspaces',
          link: {type: 'doc', id: 'concepts/execution-workspaces'},
          items: ['concepts/agent-sandbox', 'concepts/substrate'],
        },
      ],
    },
    {
      type: 'category',
      label: 'Operations',
      collapsed: true,
      items: [
        'operations/provider-proxy',
        'operations/gateways',
        'operations/agent-hosts',
        'operations/agent-runtime-security',
        'operations/upgrading',
        'operations/troubleshooting',
        'operations/runbook',
        'operations/harness-modes',
      ],
    },
    {
      type: 'category',
      label: 'Reference',
      collapsed: true,
      items: [
        'reference/glossary',
        'reference/resources',
        'reference/configuration',
        'reference/api-reference',
        'reference/api-authorization',
        'reference/built-in-tools',
        'reference/cli',
        'reference/cli-commands',
        'reference/execution-events',
        'reference/gateway-api',
        'reference/release-status',
      ],
    },
    {
      type: 'category',
      label: 'Contributing',
      collapsed: true,
      items: [
        'development/development',
        'development/build-from-source',
        'development/internals',
        'development/testing',
        'development/release-qualification',
        'development/security-scanning-design',
        'development/agent-runtime-adapter-contract',
      ],
    },
  ],
};

module.exports = sidebars;
