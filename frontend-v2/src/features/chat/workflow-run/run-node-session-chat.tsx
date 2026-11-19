import { useMemo, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'
import type { GraphWorkflowNodeStatus, WorkflowNodeConversationItem } from '@/lib/workflows/runtime'
import { FirstTurnWait } from '@/components/agent-thread/preparation'
import { Thread } from '@/components/agent-thread/thread'
import { useRunTenant } from './run-tenant'
import { nodeConversationTurns } from './conversation-turns'
import { useNodeSession } from './node-session'
import { SessionMcpStatusPanel } from './session-mcp-status'
import { isNodeWorking } from './run-status-style'
import { RunNodeSessionActions, type InteractiveNodeTarget } from './run-node-session-actions'

// A node session has nothing to ask permission for, so answering is never offered.
const noAnswer = () => {}

/**
 * An Agent node's conversation, drawn by the chat page's own thread: the same prompts, answers,
 * thinking and tool rows as a chat session. As desktop shows the node's real session here, the
 * attempt's session is followed over v2-server's node events stream; until it can be read, or when
 * it cannot, the task and answer the run recorded stand in. While a working node waits for its first
 * turn the session's preparation stages show, as on the chat page. An interactive node adds the
 * starter's composer, stop and complete controls below the thread.
 */
export function RunNodeSessionChat({
  conversation,
  status,
  errorMessage,
  sessionActions,
  session,
  interactive,
}: {
  conversation: WorkflowNodeConversationItem[]
  status: GraphWorkflowNodeStatus
  errorMessage?: string
  sessionActions?: ReactNode
  /** The attempt whose session to follow; absent before it was dispatched. */
  session?: { runId: string; nodeRunId: string }
  /** An interactive node's identity and session summary; absent for automatic nodes. */
  interactive?: InteractiveNodeTarget
}) {
  const { t } = useTranslation()
  const tenantId = useRunTenant()
  const { conversation: followed, refused } = useNodeSession(tenantId, session?.runId, session?.nodeRunId)
  const recorded = useMemo(() => nodeConversationTurns(conversation, status, errorMessage), [conversation, status, errorMessage])
  const thread = followed !== null && followed.turns.length > 0 ? followed : recorded
  return (
    <div className="flex h-full min-h-0 flex-col">
      {tenantId && session && <SessionMcpStatusPanel tid={tenantId} rid={session.runId} nodeRunId={session.nodeRunId} />}
      <div className="flex min-h-0 flex-1 flex-col">
        {thread.turns.length > 0 ? (
          <Thread conversation={thread} onAnswer={noAnswer} />
        ) : isNodeWorking(status) && !refused ? (
          <div role="status" className="min-h-0 flex-1 overflow-y-auto">
            <FirstTurnWait preparation={followed?.preparation} />
          </div>
        ) : (
          <p className="flex flex-1 items-center justify-center px-4 text-center text-sm text-muted-foreground">{t(refused ? 'workflowRun.v2.sessionRefused' : 'workflowRun.v2.sessionWaiting')}</p>
        )}
      </div>
      {interactive && (interactive.status === 'running' || interactive.status === 'awaiting_input') && (
        <div className="shrink-0">
          <RunNodeSessionActions key={session?.nodeRunId} target={interactive} />
        </div>
      )}
      {sessionActions && <div className="flex shrink-0 items-center pt-1">{sessionActions}</div>}
    </div>
  )
}
