import type { NavigateOptions } from '@tanstack/react-router'
import { findingID } from '../data/security'
import { REPOS } from '../world'
import { agentManifest, bindingManifest, monitorManifest, taskManifest, type Manifest } from './manifests'

export type TourPart = 'basics' | 'more'

export interface TourStep {
  title: string
  body: string
  /** Short statements listed under the body, for points a visitor must not miss. */
  points?: string[]
  /** The page this step shows. The welcome, checkpoint and closing steps leave the visitor where they are. */
  route?: NavigateOptions
  /** Which part of the tour this stop belongs to; progress counts stops within it. */
  part?: TourPart
  /** Chat prompts the visitor can send from the step. */
  prompts?: string[]
  /** The end of the basics, where the visitor can finish or keep going. */
  checkpoint?: boolean
  /** The Kubernetes resource behind the page, which the visitor can show as YAML. */
  manifest?: Manifest
  /** Where the page's data comes from, for pages not backed by a resource. */
  note?: string
  /** The same thing from the orka CLI. Lines starting with # are comments. */
  cli?: string[]
}

/** The incident Task the tour follows; it waits on a rollback approval when the demo loads. */
export const INCIDENT_TASK = 'checkout-latency-investigation-7k2qp'
export const INCIDENT_APPROVAL = 'apr-rollback-checkout'
/** The agent the incident Task runs. */
export const TOUR_AGENT = 'incident-responder'
export const TOUR_MONITOR = 'checkout-service'
/** A coordinator Task that delegated two child Tasks to other agents. */
export const RELEASE_TASK = 'release-v2-14-0-plan-r2w8z'
/** The critical finding whose patch is pull request #1486, the one the chat stop offers to review. */
export const TOUR_FINDING = findingID('checkout-refund-idor')
/** The gateway that created the incident Task from a Slack message. */
export const TOUR_GATEWAY = 'slack-northwind'
/** The binding that sends the #incidents channel to the incident agent. */
export const TOUR_BINDING = 'slack-incidents'

const RECORD_NOTE = "comes from Orka's own record, not from a Kubernetes resource."

const incident = (tab?: string): NavigateOptions => ({
  to: '/tasks/$taskId',
  params: { taskId: INCIDENT_TASK },
  search: tab ? { tab } : {},
})

/**
 * One story through the dashboard: an agent works an incident, its trace shows
 * what it did, a person approves the risky step, the agent definition explains
 * why it could ask, and then the ways work starts and what it costs.
 * Coordination, security, gateways and runtimes follow for visitors who keep
 * going; the rest of the sidebar is left to explore.
 */
export const TOUR_STEPS: TourStep[] = [
  {
    title: 'Welcome to the Orka demo',
    body: 'Orka runs AI agents and containers as Tasks on Kubernetes.\nThis demo follows Northwind Labs, a made-up company, as its platform team works a production checkout incident, reviews pull requests and ships a release.',
    points: [
      'This is the optional web dashboard. It is off unless you turn it on, and the rest of Orka works without it.',
      'Everything here is mock data in your browser. Nothing reaches a cluster or a model.',
      'Everything you see or do here can be done with the orka CLI or kubectl. Agents, Tasks, monitors and gateways are Kubernetes resources you declare in YAML. In a real install, starting a Task or approving a step in the dashboard calls the same API the CLI does.',
    ],
  },
  {
    title: 'An agent handles an incident',
    body: 'Checkout latency spiked in production. Someone asked for help in the #incidents Slack channel, and Orka started this Task for the incident-responder agent. All work in Orka is a Task: a Kubernetes resource with a phase, events, logs and a result.',
    route: incident(),
    part: 'basics',
    manifest: taskManifest(INCIDENT_TASK),
    cli: [`orka task get ${INCIDENT_TASK}`],
  },
  {
    title: 'See what it did',
    body: 'The Activity tab traces every model request and tool call in order. The agent read latency and error rates from Datadog, found the merged pull request that shrank the database connection pool, and posted its findings to PagerDuty and Slack.',
    route: incident('activity'),
    part: 'basics',
    note: `The trace ${RECORD_NOTE}`,
    cli: [`orka task trace ${INCIDENT_TASK}`],
  },
  {
    title: 'You make the call',
    body: 'Risky steps wait for a person. The agent wants to roll production back, and the Task stays paused until someone decides. Approve or decline it: either way the agent carries on and finishes about a minute later.',
    route: incident('approvals'),
    part: 'basics',
    note: "Approvals come from Orka's own record, not from the Task resource.",
    cli: [
      '# See what is waiting, then approve it',
      `orka task approvals ${INCIDENT_TASK}`,
      `orka task approve ${INCIDENT_TASK} ${INCIDENT_APPROVAL}`,
    ],
  },
  {
    title: 'Every Task runs an agent',
    body: 'An agent is a reusable definition: a model, instructions and the tools it may use. incident-responder can query Datadog, update PagerDuty, search GitHub and post to Slack. It runs in autonomous mode, which lets it pause its own Task for approval, as it did for the rollback. Other agents, such as code-reviewer, run a coding agent like Claude Code instead.',
    route: { to: '/agents/$agentId', params: { agentId: TOUR_AGENT } },
    part: 'basics',
    manifest: agentManifest(TOUR_AGENT),
    cli: [`orka agent get ${TOUR_AGENT}`],
  },
  {
    title: 'Ask for work in chat',
    body: "This is the dashboard's chat. Ask in plain language: the assistant answers from the cluster and starts Tasks for you.\nYou don't need the dashboard to chat. Orka also serves OpenAI- and Anthropic-compatible APIs, so Claude Code, Codex, OpenCode and more can talk to the same assistant from the tools you already use.\nTry one of these:",
    route: { to: '/chat' },
    part: 'basics',
    prompts: ["What's running right now?", 'Review PR #1486 in checkout-service'],
    note: "Chat sessions live in Orka's own record; the Tasks they start are Kubernetes resources.",
    cli: [
      "# orka run is the CLI's chat",
      'orka run "What\'s running right now?"',
      '# Or point Claude Code at Orka: set ANTHROPIC_BASE_URL to https://<orka>/anthropic',
    ],
  },
  {
    title: 'Monitors find work on their own',
    body: 'A repository monitor watches checkout-service on GitHub. It implements issues labeled for Orka, reviews new pull requests and repairs failing ones, each as a Task. Right now it is fixing issue #2214.',
    route: { to: '/monitors/$monitorId', params: { monitorId: TOUR_MONITOR } },
    part: 'basics',
    manifest: monitorManifest(TOUR_MONITOR),
    cli: [`orka monitor issue status ${TOUR_MONITOR} 2214`],
  },
  {
    title: 'See what each change cost',
    body: 'Usage follows model tokens from the original issue through review to the merged pull request, so you see how many tokens each shipped change took, including attempts that failed along the way.',
    route: { to: '/usage' },
    part: 'basics',
    note: `Usage ${RECORD_NOTE}`,
    cli: [`orka usage summary --repository ${REPOS.checkout.slug}`],
  },
  {
    title: "That's the basics",
    body: 'Finish here, or keep going for four more stops: agents that hand work to other agents, security findings, the gateways that bring in Slack and GitHub, and the runtimes agents run on.',
    checkpoint: true,
  },
  {
    title: 'Agents hand work to other agents',
    body: 'release-captain is preparing a storefront release. It gave two jobs to other agents, each as a child Task: issue-triager checks for release blockers and docs-writer drafts the release notes. The Child tasks graph at the bottom of this page shows both; release-captain waits for them before it finishes. Its agent definition limits which agents it may delegate to and how many children run at once.',
    route: { to: '/tasks/$taskId', params: { taskId: RELEASE_TASK }, search: {} },
    part: 'more',
    cli: [`orka task children ${RELEASE_TASK}`],
  },
  {
    title: 'Agents find and fix vulnerabilities',
    body: "Security scans run agents over each repository and report findings with evidence. This one lets a merchant refund another merchant's payment. An agent reproduced it to confirm it, then wrote a patch and opened it as pull request #1486, the one chat offered to review.",
    route: { to: '/security/findings/$findingId', params: { findingId: TOUR_FINDING } },
    part: 'more',
    note: "Findings live in Orka's own record; the scan that found them is a RepositoryScan resource.",
    cli: [`orka security finding get ${TOUR_FINDING}`],
  },
  {
    title: 'Gateways bring in Slack and GitHub',
    body: 'A gateway connects a chat or code platform to agents. On this Slack gateway, a binding sends the #incidents channel to incident-responder, which is how the incident Task at the start of the tour began. The event timeline shows each message: accepted, queued behind a running turn, or rejected because the sender is not allowed.',
    route: { to: '/gateways/$gatewayId', params: { gatewayId: TOUR_GATEWAY } },
    part: 'more',
    manifest: bindingManifest(TOUR_BINDING),
    cli: [`orka gateway events list --gateway ${TOUR_GATEWAY}`],
  },
  {
    title: 'Where agents run',
    body: 'Coding agents such as Claude Code, Codex, Copilot and OpenCode run in runtime pools that Orka manages. Each pool is pinned to an exact image, drains its sessions before an upgrade replaces it, and scales to zero when idle. Agents hosted elsewhere register under External runtimes.',
    route: { to: '/runtimes' },
    part: 'more',
    cli: ['orka runtime-pool list'],
  },
  {
    title: "That's the full tour",
    body: 'The sidebar has the rest: tools, connectors, chat sessions, and a board of running work. Remember that the dashboard is optional: everything in this tour works from the orka CLI and kubectl. You can reopen this tour from Take the tour in the sidebar.',
  },
]

/** The stops in one part of the tour, which its progress indicator counts. */
export const stopsIn = (part: TourPart) => TOUR_STEPS.filter((step) => step.part === part)
