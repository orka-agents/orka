import { describe, expect, it } from 'vitest'
import { render, screen } from '@/test/test-utils'
import { PostureLineView } from './posture-line'
import { makePosture } from '@/test/fixtures/oversight'

describe('PostureLineView latency', () => {
  it('adds "decided within ~p95" only when latency is reported', () => {
    const { unmount } = render(<PostureLineView posture={makePosture({ basis: { ...makePosture().basis, decisionLatency: { count: 5, p50Seconds: 2, p95Seconds: 41, maxSeconds: 60 } } })} incidents={[]} />)
    expect(screen.getByTestId('posture-latency')).toHaveTextContent('decided within ~41s')
    unmount()
    render(<PostureLineView posture={makePosture()} incidents={[]} />)
    expect(screen.queryByTestId('posture-latency')).not.toBeInTheDocument()
  })
})
