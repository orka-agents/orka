// Content for the landing page. Editing here keeps the page logic clean and
// makes it easy to change copy without touching components.

export const features = [
  {
    emoji: '🧑‍💻',
    title: 'Coding agents that open PRs',
    description:
      'Codex, Claude Code, Copilot CLI, and OpenCode work on a clean checkout. A separate publisher pushes the verified change and opens the pull request.',
    href: '/docs/agent-runtimes',
  },
  {
    emoji: '🤖',
    title: 'AI tasks with tools',
    description:
      'Anthropic, OpenAI, or Azure OpenAI models with web search, code execution, custom HTTP and MCP tools, skills, and sessions.',
    href: '/docs/guides/agents-skills-and-tools',
  },
  {
    emoji: '🔀',
    title: 'Multi-agent coordination',
    description:
      'Coordinators delegate to specialists within allowlist, depth, and concurrency limits, or work toward a goal autonomously.',
    href: '/docs/multi-agent-coordination',
  },
  {
    emoji: '🔁',
    title: 'Issue to pull request',
    description:
      'Review every pull request, or label an issue and let agents plan, implement, repair, and mark it ready to merge.',
    href: '/docs/repository-monitors',
  },
  {
    emoji: '🛡️',
    title: 'Repository security scanning',
    description:
      'Threat models, validated findings, and remediation pull requests, with a person choosing what gets fixed.',
    href: '/docs/repository-security-scanning',
  },
  {
    emoji: '💬',
    title: 'Chat, dashboard, and CLI',
    description:
      'Describe work in plain language to an orchestrator, watch it live in the dashboard, or script it with the orka CLI.',
    href: '/docs/chat',
  },
  {
    emoji: '🔌',
    title: 'OpenAI & Anthropic APIs',
    description:
      'Point Claude Code, Cursor, or Continue at Orka. People get a cluster token; nobody needs a provider key.',
    href: '/docs/openai-compat',
  },
  {
    emoji: '📨',
    title: 'Teams, Telegram, and A2A',
    description:
      'Gateways bring agents into the conversations people and other agents already have, with durable, deduplicated delivery.',
    href: '/docs/gateways',
  },
  {
    emoji: '✅',
    title: 'Approvals and governance',
    description:
      'Hold consequential actions for a person, let people act through their own linked accounts, and audit every Task.',
    href: '/docs/guides/human-approval-v2',
  },
];

export const providers = [
  {
    name: 'Anthropic',
    href: '/docs/anthropic-compat',
    description:
      'Claude models through the Provider CRD and an Anthropic-compatible Messages API.',
  },
  {
    name: 'OpenAI',
    href: '/docs/openai-compat',
    description:
      'GPT models behind an OpenAI-compatible chat completions endpoint.',
  },
  {
    name: 'Azure OpenAI',
    href: '/docs/resources#provider',
    description: 'Enterprise OpenAI deployments running on Azure.',
  },
  {
    name: 'ACP RuntimePools',
    href: '/docs/agent-runtimes',
    description:
      'Run Codex, OpenCode, Claude, and Copilot in digest-pinned, scale-to-zero RuntimePools with brokered tools and clean-room delivery.',
  },
];

export const ctaLinks = [
  {
    label: 'GitHub',
    href: 'https://github.com/orka-agents/orka',
  },
  {
    label: 'Issues',
    href: 'https://github.com/orka-agents/orka/issues',
  },
  {
    label: 'Releases',
    href: 'https://github.com/orka-agents/orka/releases',
  },
];
