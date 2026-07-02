// Content for the landing page. Editing here keeps the page logic clean and
// makes it easy to change copy without touching components.

export const features = [
  {
    emoji: '🤖',
    title: 'AI Agents',
    description:
      'Anthropic, OpenAI, or Azure OpenAI agents with tools, skills, and session persistence.',
  },
  {
    emoji: '🔀',
    title: 'Multi-Agent Coordination',
    description:
      'Coordinators decompose tasks and delegate to specialists with depth and concurrency controls.',
  },
  {
    emoji: '💬',
    title: 'Interactive Chat',
    description:
      'An agentic orchestrator with SSE streaming that creates and manages agents and tasks for you.',
  },
  {
    emoji: '🧠',
    title: 'Durable Memory',
    description:
      'Namespace-scoped recall, transcript search, and reviewable memory proposals for coordinated agents.',
  },
  {
    emoji: '🛡️',
    title: 'Repository Security Scanning',
    description:
      'Scheduled scans with threat models, validated findings, patch generation, and remediation PRs.',
  },
  {
    emoji: '🔌',
    title: 'OpenAI & Anthropic APIs',
    description:
      'Drop-in compatible endpoints for Continue, Cursor, Claude Code, and any OpenAI-native client.',
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
    href: '/docs/configuration',
    description: 'Enterprise OpenAI deployments running on Azure.',
  },
  {
    name: 'Agent Runtimes',
    href: '/docs/agent-runtimes',
    description:
      'Delegate to Codex CLI, Claude Code CLI, or GitHub Copilot CLI for autonomous coding.',
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
