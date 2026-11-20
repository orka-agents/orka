import { readFileSync } from 'node:fs'
import path from 'node:path'
import { describe, expect, it } from 'vitest'
import { handleDemoRequest } from '../server'
import { INCIDENT_APPROVAL, INCIDENT_TASK, RELEASE_TASK, TOUR_AGENT, TOUR_FINDING, TOUR_GATEWAY, TOUR_MONITOR, TOUR_STEPS } from './steps'

const get = (path: string) => handleDemoRequest(new Request(`http://localhost/api/v1${path}`))

// Generated from `orka --help` by `make docs-cli`: one section per command.
const cliReference = new Map(
  readFileSync(path.resolve(__dirname, '../../../../website/docs/reference/cli-commands.md'), 'utf8')
    .split(/^## /m)
    .flatMap((section) => {
      const name = section.match(/^`(orka[^`]*)`/)?.[1]
      return name ? [[name, section] as const] : []
    }),
)

describe('demo tour steps', () => {
  it('point at sample data that exists', async () => {
    expect((await get(`/tasks/${INCIDENT_TASK}?namespace=default`)).status).toBe(200)
    expect((await get(`/agents/${TOUR_AGENT}?namespace=default`)).status).toBe(200)
    expect((await get(`/monitors/repositories/${TOUR_MONITOR}?namespace=default`)).status).toBe(200)
    expect((await get(`/security/findings/${TOUR_FINDING}?namespace=default`)).status).toBe(200)
    expect((await get(`/gateways/${TOUR_GATEWAY}?namespace=default`)).status).toBe(200)
  })

  it('reach the incident while its rollback approval is waiting', async () => {
    const body = await (await get(`/tasks/${INCIDENT_TASK}/approvals?namespace=default`)).json()
    expect(body.approvals.map((approval: { id: string; status: string }) => [approval.id, approval.status]))
      .toEqual([[INCIDENT_APPROVAL, 'pending']])
  })

  it('show YAML that agrees with the resource each page shows', async () => {
    const paths: Record<string, string> = {
      Task: '/tasks',
      Agent: '/agents',
      RepositoryMonitor: '/monitors/repositories',
      GatewayBinding: '/gatewaybindings',
    }
    const manifests = TOUR_STEPS.flatMap((step) => step.manifest ?? [])
    expect(manifests.map((manifest) => manifest.kind)).toEqual(['Task', 'Agent', 'RepositoryMonitor', 'GatewayBinding'])
    for (const { kind, metadata, spec } of manifests) {
      const served = await (await get(`${paths[kind]}/${metadata.name}?namespace=default`)).json()
      expect(served, `${kind} ${metadata.name}`).toMatchObject({ metadata, spec })
    }
  })

  it('show CLI commands the orka CLI has, with the arguments and flags it takes', () => {
    const commands = TOUR_STEPS.flatMap((step) => step.cli ?? []).filter((line) => !line.startsWith('#'))
    expect(commands.length).toBeGreaterThan(0)
    for (const command of commands) {
      const words = command.match(/"[^"]*"|\S+/g) ?? []
      let length = words.length
      while (length > 1 && !cliReference.has(words.slice(0, length).join(' '))) length--
      const section = cliReference.get(words.slice(0, length).join(' '))
      expect(section, command).toBeDefined()

      const rest = words.slice(length)
      const flags = rest.filter((word) => word.startsWith('--'))
      for (const flag of flags) expect(section, `${command}: ${flag}`).toContain(`${flag} `)
      const args = rest.filter((word, index) => !word.startsWith('--') && !rest[index - 1]?.startsWith('--'))
      const usage = section!.match(/Usage:\n\s+(orka[^\n]*)/)?.[1] ?? ''
      const required = usage.match(/<[^>]+>/g)?.length ?? 0
      const optional = usage.match(/\[(?!flags\])[^\]]+\]/g)?.length ?? 0
      expect(args.length, command).toBeGreaterThanOrEqual(required)
      expect(args.length, command).toBeLessThanOrEqual(required + optional)
    }
  })

  it('reach a coordinator with child Tasks, so its Overview draws them', async () => {
    const task = await (await get(`/tasks/${RELEASE_TASK}?namespace=default`)).json()
    expect(task.status.childTasks).toHaveLength(2)
  })
})
