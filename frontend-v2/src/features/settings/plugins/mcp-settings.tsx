import { useState } from 'react'
import { isAxiosError } from 'axios'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { ActionError } from '../components/page-parts'
import { getListMcpQueryKey, updateMcpSettings, type McpInstallation } from '@/lib/mcp-api/generated'

/** Edits changed settings only, keeping stored secrets out of the form. */
export function McpSettings({ installation, tenantId, spaceId, canManage }: {
  installation: McpInstallation
  tenantId: string
  spaceId: string
  canManage: boolean
}) {
  const [draft, setDraft] = useState<Record<string, string>>({})
  const [cleared, setCleared] = useState<string[]>([])
  const [saved, setSaved] = useState(false)
  const [editRevision, setEditRevision] = useState<number | null>(null)
  const client = useQueryClient()
  const save = useMutation({
    mutationFn: async () => {
      const values: Record<string, string | number | boolean> = Object.create(null)
      for (const field of installation.settings) {
        if (!Object.hasOwn(draft, field.id) || cleared.includes(field.id)) continue
        const raw = draft[field.id]
        if (field.secret && raw === '') continue
        if (field.type === 'number') {
          if (raw.trim() === '' || !Number.isFinite(Number(raw))) throw new Error('数字设置需要有限数值。')
          values[field.id] = Number(raw)
        } else if (field.type === 'boolean') {
          if (raw !== 'true' && raw !== 'false') throw new Error('请选择是或否，或清除已有设置。')
          values[field.id] = raw === 'true'
        } else {
          values[field.id] = raw
        }
      }
      return updateMcpSettings(tenantId, spaceId, {
        identifier: installation.id, revision: editRevision ?? installation.revision, values, clear: cleared,
      })
    },
    onSuccess: async () => {
      setDraft({})
      setCleared([])
      setEditRevision(null)
      await Promise.all([
        client.invalidateQueries({ queryKey: getListMcpQueryKey(tenantId) }),
        client.invalidateQueries({ queryKey: ['workflow-catalog', 'plugins'] }),
      ])
      setSaved(true)
    },
  })
  const conflict = isAxiosError(save.error) && save.error.response?.status === 409
  const reload = useMutation({
    mutationFn: () => client.refetchQueries({ queryKey: getListMcpQueryKey(tenantId) }, { throwOnError: true }),
    onSuccess: () => {
      setDraft({})
      setCleared([])
      setEditRevision(null)
      setSaved(false)
      save.reset()
    },
  })
  if (installation.settingsUnavailable) return <p role="status">服务暂时无法解密此插件的设置，请管理员检查部署密钥。</p>
  const pending = save.isPending || reload.isPending
  const change = (id: string, value: string) => {
    setEditRevision((current) => current ?? installation.revision)
    setDraft((current) => ({ ...current, [id]: value }))
    setCleared((current) => current.filter((item) => item !== id))
    setSaved(false)
  }
  return (
    <form className="space-y-3" onSubmit={(event) => { event.preventDefault(); save.mutate() }}>
      <h3 className="text-sm font-medium">MCP 配置</h3>
      <p className="text-xs text-muted-foreground">团队成员的运行共用此配置。秘密值保存后只显示是否已配置。</p>
      {!installation.complete && <p className="text-xs text-destructive">待配置 {installation.settingsMissing.join('、')}</p>}
      {installation.settings.map((field) => {
        const id = `mcp-setting-${field.id}`
        const stored = Object.hasOwn(installation.values, field.id) ? installation.values[field.id] : field.default
        const edited = Object.hasOwn(draft, field.id) ? draft[field.id] : undefined
        const value = cleared.includes(field.id) ? '' : edited ?? (field.secret || stored === undefined ? '' : String(stored))
        const configured = field.secret && installation.configuredSecrets.includes(field.id)
        return (
          <div key={field.id} className="space-y-1">
            <label htmlFor={id} className="text-sm">{field.label || field.id}{field.required ? '（必填）' : ''}</label>
            {field.type === 'boolean' ? (
              <select id={id} value={value} disabled={!canManage || pending} onChange={(event) => change(field.id, event.target.value)} className="block rounded border p-2 text-sm">
                <option value="">未设置</option><option value="true">是</option><option value="false">否</option>
              </select>
            ) : (
              <Input id={id} type={field.secret ? 'password' : field.type === 'number' ? 'number' : 'text'}
                step={field.type === 'number' ? 'any' : undefined}
                autoComplete="off" value={value} disabled={!canManage || pending}
                placeholder={configured ? '已配置，留空保留' : undefined}
                onChange={(event) => change(field.id, event.target.value)} />
            )}
            {field.description && <p className="text-xs text-muted-foreground">{field.description}</p>}
            {configured && <p className="text-xs text-muted-foreground">{cleared.includes(field.id) ? '保存后清除' : '秘密值已配置'}</p>}
            {canManage && (configured || Object.hasOwn(installation.values, field.id)) && (
              <Button type="button" variant="ghost" size="sm" disabled={pending}
                onClick={() => {
                  setEditRevision((current) => current ?? installation.revision)
                  setCleared((current) => [...new Set([...current, field.id])])
                  setSaved(false)
                }}>
                清除 {field.label || field.id}
              </Button>
            )}
          </div>
        )
      })}
      {conflict ? (
        <div role="alert" className="space-y-2 rounded-lg border border-destructive/30 p-3 text-sm">
          <p>配置已被其他操作修改。重新加载会放弃当前未保存的修改，请核对最新配置后再编辑。</p>
          <Button type="button" variant="outline" disabled={pending} onClick={() => reload.mutate()}>
            {reload.isPending ? '正在重新加载…' : '放弃修改并重新加载'}
          </Button>
        </div>
      ) : <ActionError error={save.error} />}
      <ActionError error={reload.error} />
      {saved && <p role="status" className="text-sm">MCP 配置已保存</p>}
      {canManage && <Button type="submit" disabled={pending || conflict || (Object.keys(draft).length === 0 && cleared.length === 0)}>
        {save.isPending ? '保存中…' : '保存 MCP 配置'}
      </Button>}
    </form>
  )
}
