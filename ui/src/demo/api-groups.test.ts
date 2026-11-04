import { readdirSync, readFileSync } from 'node:fs'
import path from 'node:path'
import { describe, expect, it } from 'vitest'

// Every apiVersion in the demo's sample data and tour YAML must name a group
// the CRDs define, so the YAML a visitor copies refers to real resources.
const repoRoot = path.resolve(__dirname, '../../..')
const apiGroups = new Set(
  readdirSync(path.join(repoRoot, 'api'), { recursive: true, encoding: 'utf8' })
    .filter((file) => file.endsWith('groupversion_info.go'))
    .flatMap((file) => [...readFileSync(path.join(repoRoot, 'api', file), 'utf8').matchAll(/Group:\s*"([^"]+)",\s*Version:\s*"([^"]+)"/g)])
    .map(([, group, version]) => `${group}/${version}`),
)

describe('demo API versions', () => {
  it('use only API groups the CRDs define', () => {
    expect(apiGroups).toContain('core.orka.ai/v1alpha1')
    const sources = readdirSync(__dirname, { recursive: true, encoding: 'utf8' })
      .filter((file) => /\.tsx?$/.test(file) && !/\.test\.tsx?$/.test(file))
    const used = sources.flatMap((file) =>
      [...readFileSync(path.join(__dirname, file), 'utf8').matchAll(/apiVersion:\s*'([^']+)'/g)].map(([, apiVersion]) => ({ file, apiVersion })))
    expect(used.length).toBeGreaterThan(0)
    for (const { file, apiVersion } of used) expect(apiGroups, `${file}: ${apiVersion}`).toContain(apiVersion)
  })
})
