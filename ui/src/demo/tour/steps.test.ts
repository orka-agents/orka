import { describe, expect, it } from 'vitest'
import { handleDemoRequest } from '../server'
import { INCIDENT_TASK, RELEASE_TASK, TOUR_AGENT, TOUR_FINDING, TOUR_GATEWAY, TOUR_MONITOR, TOUR_STEPS } from './steps'

const get = (path: string) => handleDemoRequest(new Request(`http://localhost/api/v1${path}`))

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
    expect(body.approvals.map((approval: { status: string }) => approval.status)).toEqual(['pending'])
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

  it('reach a coordinator with child Tasks, so its Children tab shows', async () => {
    const task = await (await get(`/tasks/${RELEASE_TASK}?namespace=default`)).json()
    expect(task.status.childTasks).toHaveLength(2)
  })
})
