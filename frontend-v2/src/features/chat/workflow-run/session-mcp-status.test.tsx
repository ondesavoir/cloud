import { fireEvent, render, screen } from '@testing-library/react'
import { expect, it, vi } from 'vitest'
import type { SessionMcpStatus } from '@/lib/mcp-api/generated'
import { SessionMcpFacts } from './session-mcp-status'

const data: SessionMcpStatus = {
  generation: 2, activeGeneration: 1, convergence: 'pending', canProbe: true,
  members: [{ id: 'official/fixture.search', configured: true, version: '1.0.0', health: 'healthy' }],
}

it('distinguishes the committed target, last delivery and independently observed health', () => {
  const retry = vi.fn()
  const props = { data, retry, busy: false, failed: false, disconnected: false }
  const { rerender } = render(<SessionMcpFacts {...props} />)
  expect(screen.getByText(/等待刷新.*上次送达 1/)).toBeVisible()
  expect(screen.getByText(/已配置.*宿主健康：健康/)).toBeVisible()
  fireEvent.click(screen.getByRole('button', { name: '重新探测' }))
  expect(retry).toHaveBeenCalledWith('official/fixture.search')
  rerender(<SessionMcpFacts {...props} disconnected />)
  expect(screen.getByText(/宿主健康：未知/)).toBeVisible()
  expect(screen.queryByText(/上次送达|宿主健康：健康/)).toBeNull()
  expect(screen.getByRole('button')).toBeDisabled()
  rerender(<SessionMcpFacts {...props} data={{ ...data, convergence: 'current', canProbe: false }} />)
  expect(screen.queryByText(/当前配置已送达/)).toBeNull()
  expect(screen.queryByRole('button')).toBeNull()
})
