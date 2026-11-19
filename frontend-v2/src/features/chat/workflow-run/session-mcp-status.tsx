import { useGetWorkflowSessionMcp, useProbeWorkflowSessionMcp } from '@/lib/mcp-api/generated'
import type { SessionMcpStatus } from '@/lib/mcp-api/generated'
import { Button } from '@/components/ui/button'

const convergence = {
  unknown: '等待宿主确认', ended: '会话已结束', stopping: '正在确认停止',
  current: '当前配置已送达', pending: '等待刷新', refreshing: '正在刷新',
  blocked: '刷新受阻，修复配置后可重试',
}

export function SessionMcpStatusPanel({ tid, rid, nodeRunId }: { tid: string; rid: string; nodeRunId: string }) {
  const status = useGetWorkflowSessionMcp(tid, rid, nodeRunId, {
    query: {
      retry: false, staleTime: 0, gcTime: 0,
      refetchInterval: q => q.state.data?.convergence === 'ended' || q.state.data?.members.length === 0 ? false : 2500,
    },
  })
  const probe = useProbeWorkflowSessionMcp({ mutation: { onSettled: () => { void status.refetch() } } })
  const data = status.data
  if (!data && status.isError) return <p role="status" className="border-b px-3 py-2 text-xs">暂时无法读取会话 MCP 状态。</p>
  if (!data?.members.length) return null
  const retry = (pluginId: string) => {
    if (data.generation !== undefined) probe.mutate({ tid, rid, nodeRunId, data: { pluginId, generation: data.generation } })
  }
  return <SessionMcpFacts data={data} disconnected={status.isError || status.isPaused} busy={probe.isPending} failed={probe.isError} retry={retry} />
}

export function SessionMcpFacts({ data, disconnected, busy, failed, retry }: {
  data: SessionMcpStatus; disconnected: boolean; busy: boolean; failed: boolean; retry: (id: string) => void
}) {
  const live = !disconnected && data.convergence !== 'ended' && data.convergence !== 'unknown'
  const current = live && data.convergence === 'current' && data.activeGeneration !== undefined && data.activeGeneration === data.generation
  const delivery = current ? convergence.current : convergence[disconnected || data.convergence === 'current' ? 'unknown' : data.convergence]
  return (
    <section aria-label="会话 MCP" className="max-h-48 shrink-0 overflow-auto border-b px-3 py-2 text-xs">
      <p className="font-medium">会话 MCP</p>
      <p>ACP · {delivery}{live && data.activeGeneration !== undefined && `（${current ? '版本' : '上次送达'} ${data.activeGeneration}）`}</p>
      {data.convergence !== 'ended' && <p className="text-muted-foreground">插件升级或多次刷新可能重启 Agent，未保存的内存状态可能丢失。</p>}
      {data.members.map(m => {
        const health = !live ? '未知' : m.cleanupPending ? '等待清理' : m.queued ? '排队中' : m.inFlight ? '探测中' : m.health === 'healthy' ? '健康' : m.health === 'unhealthy' ? '异常' : '未知'
        return <div key={m.id} className="mt-2 flex items-center justify-between gap-2">
          <div className="min-w-0"><span className="break-all font-medium">{m.id}</span><p>{m.configured ? `已配置 · ${m.version}` : m.code === 'mcp_plugin_missing' ? '插件已移除' : '缺少设置'} · 宿主健康：{health}</p></div>
          {data.canProbe && m.configured && <Button type="button" size="sm" variant="outline" disabled={busy || !live || data.convergence === 'stopping' || m.inFlight} onClick={() => retry(m.id)}>重新探测</Button>}
        </div>
      })}
      {live && data.probeCapacityAvailable === false && <p>宿主探测容量暂不可用</p>}
      {failed && <p role="status">探测请求未确认，请查看最新状态。</p>}
    </section>
  )
}
