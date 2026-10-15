import type { LinkProps, NavigateOptions } from '@tanstack/react-router'

export interface TourLink {
  label: string
  description: string
  to: LinkProps['to']
}

export interface TourStep {
  title: string
  body: string
  /** The page this step shows. The welcome and closing steps leave the visitor where they are. */
  route?: NavigateOptions
  /** Chat prompts the visitor can send from the step. */
  prompts?: string[]
  /** Pages to explore after the tour. */
  links?: TourLink[]
}

/** The incident Task the tour follows; it waits on a rollback approval when the demo loads. */
export const INCIDENT_TASK = 'checkout-latency-investigation-7k2qp'
export const TOUR_MONITOR = 'checkout-service'

const incident = (tab?: string): NavigateOptions => ({
  to: '/tasks/$taskId',
  params: { taskId: INCIDENT_TASK },
  search: tab ? { tab } : {},
})

/**
 * One story through the dashboard: an agent works an incident, its trace shows
 * what it did, a person approves the risky step, and then the ways work starts
 * and what it costs. The rest is listed at the end rather than toured.
 */
export const TOUR_STEPS: TourStep[] = [
  {
    title: 'Welcome to the Orka demo',
    body: 'Orka runs AI agents and containers as Tasks on Kubernetes, with approvals, traces and usage tracking built in. This demo follows Northwind Labs, a made-up engineering team. It runs on sample data in your browser, so nothing reaches a cluster or a model.',
  },
  {
    title: 'An agent handles an incident',
    body: 'Checkout latency spiked in production. Someone asked for help in the #incidents Slack channel, and Orka started this Task for the incident-responder agent. All work in Orka is a Task with a phase, events, logs and a result.',
    route: incident(),
  },
  {
    title: 'See what it did',
    body: 'The Trace tab lists every model request and tool call in order. The agent read latency and error rates from Datadog, found the merged pull request that shrank the database connection pool, and posted its findings to PagerDuty and Slack.',
    route: incident('trace'),
  },
  {
    title: 'You make the call',
    body: 'Risky steps wait for a person. The agent wants to roll production back, and the Task stays paused until someone decides. Approve or decline it: either way the agent carries on and finishes about a minute later.',
    route: incident('approvals'),
  },
  {
    title: 'Ask for work in chat',
    body: 'Chat is the quickest way to start work. Ask in plain language: the assistant answers from the cluster and starts Tasks for you. Try one of these:',
    route: { to: '/chat' },
    prompts: ["What's running right now?", 'Review PR #1486 in checkout-service'],
  },
  {
    title: 'Monitors find work on their own',
    body: 'A repository monitor watches checkout-service on GitHub. It implements issues labeled for Orka, reviews new pull requests and repairs failing ones, each as a Task. Right now it is fixing issue #2214.',
    route: { to: '/monitors/$monitorId', params: { monitorId: TOUR_MONITOR } },
  },
  {
    title: 'See what each change cost',
    body: 'Usage follows model tokens from the original issue through review to the merged pull request, so you see how many tokens each shipped change took, including attempts that failed along the way.',
    route: { to: '/usage' },
  },
  {
    title: "That's the tour",
    body: 'Keep exploring on your own. You can reopen this tour from Take the tour in the sidebar.',
    links: [
      { label: 'Security', description: 'Findings from scan agents, with evidence and fixes', to: '/security' },
      { label: 'Gateways', description: 'Slack and GitHub conversations that start Tasks', to: '/gateways' },
      { label: 'Runtimes', description: 'The agent runtimes Tasks run on', to: '/runtimes' },
    ],
  },
]

/** Steps that show a page, which the progress indicator counts. */
export const TOUR_STOPS = TOUR_STEPS.filter((step) => step.route)
