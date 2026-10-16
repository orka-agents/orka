import { describe, expect, it } from 'vitest'
import { handleDemoRequest } from '../server'
import { INCIDENT_TASK, RELEASE_TASK, TOUR_AGENT, TOUR_FINDING, TOUR_GATEWAY, TOUR_MONITOR } from './steps'

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

  it('reach a coordinator with child Tasks, so its Children tab shows', async () => {
    const task = await (await get(`/tasks/${RELEASE_TASK}?namespace=default`)).json()
    expect(task.status.childTasks).toHaveLength(2)
  })
})
