/**
 * Thin typed wrapper over the AgentHub REST API.
 *
 * Every call funnels through `request` so error handling is uniform: the
 * backend returns plain-text errors for most failures and JSON error payloads
 * (e.g. missing role vars) for a few, so callers get both a message and the
 * parsed payload when available.
 */

const BASE = '/api/v1'

export class ApiError extends Error {
  status: number
  payload: any
  constructor(status: number, message: string, payload?: any) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.payload = payload
  }
}

async function request<T = any>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(BASE + path, {
    ...init,
    headers: {
      'Content-Type': 'application/json',
      ...(init?.headers || {}),
    },
  })
  const text = await res.text()
  let body: any = undefined
  if (text) {
    try {
      body = JSON.parse(text)
    } catch {
      body = text
    }
  }
  if (!res.ok) {
    const msg =
      (body && typeof body === 'object' && (body.error || body.message)) ||
      (typeof body === 'string' && body) ||
      `HTTP ${res.status}`
    throw new ApiError(res.status, msg, body)
  }
  return body as T
}

const get = <T = any>(p: string) => request<T>(p)
const post = <T = any>(p: string, body?: any) =>
  request<T>(p, { method: 'POST', body: JSON.stringify(body ?? {}) })
const put = <T = any>(p: string, body?: any) =>
  request<T>(p, { method: 'PUT', body: JSON.stringify(body ?? {}) })
const del = <T = any>(p: string) => request<T>(p, { method: 'DELETE' })

export const api = {
  health: () => get('/health'),

  // providers
  providers: () => get('/providers'),
  // The preset vendor list comes from the backend, which already carries each
  // type's default base URL. A second copy here is how the Anthropic template
  // drifted to a bare host and produced /messages instead of /v1/messages.
  providerTypes: () => get('/providers/types'),
  providerCreate: (p: any) => post('/providers', p),
  providerUpdate: (id: string, p: any) => put(`/providers/${id}`, p),
  providerDelete: (id: string) => del(`/providers/${id}`),
  providerKeysAdd: (id: string, secret: string, label?: string) =>
    post(`/providers/${id}/keys`, { secret, label }),
  providerKeysRemove: (id: string, keyId: string) => del(`/providers/${id}/keys/${keyId}`),
  providerTest: (id: string) => post(`/providers/${id}/test`),
  providerRefreshModels: (id: string) => post(`/providers/${id}/models`),
  // Per-model curation. Sending only the half that changed: the backend treats
  // an omitted field as "leave it alone", so starring a model cannot
  // accidentally re-enable one that was switched off.
  providerSetModelPref: (id: string, modelId: string, patch: { disabled?: boolean; favorite?: boolean }) =>
    post(`/providers/${id}/model-prefs`, { modelId, ...patch }),
  providerSetAllModelPrefs: (id: string, patch: { disabled?: boolean; favorite?: boolean }) =>
    post(`/providers/${id}/model-prefs/all`, patch),
  providerToggle: (id: string, enabled: boolean) => post(`/providers/${id}/toggle`, { enabled }),

  // roles
  roles: () => get('/roles'),
  roleCreate: (r: any) => post('/roles', r),
  roleUpdate: (id: string, r: any) => put(`/roles/${id}`, r),
  roleDelete: (id: string) => del(`/roles/${id}`),
  roleDuplicate: (id: string, name?: string) => post(`/roles/${id}/duplicate`, { name }),
  roleExport: (id: string) => get(`/roles/${id}/export`),
  roleExportAll: () => get('/roles/export'),
  roleImport: (doc: any) => post('/roles/import', doc),
  roleRender: (id: string, vars: Record<string, string>) => post(`/roles/${id}/render`, { vars }),

  // conversations
  conversations: () => get('/conversations'),
  conversationCreate: (roleId: string, vars?: Record<string, string>, mode?: string) =>
    post('/conversations', { roleId, vars, mode }),
  conversation: (id: string) => get(`/conversations/${id}`),
  conversationDelete: (id: string) => del(`/conversations/${id}`),
  conversationMessages: (id: string) => get(`/conversations/${id}/messages`),
  conversationPatch: (id: string, patch: any) => put(`/conversations/${id}`, patch),
  conversationExport: (id: string) => get(`/conversations/${id}/export`),

  // skills
  skills: () => get('/skills'),
  skillDirs: () => get('/skills/dirs'),
  skillScan: () => post('/skills/scan'),
  skillToggle: (id: string) => post(`/skills/${id}/toggle`),
  skillCreate: (s: any) => post('/skills', s),
  skillUpdate: (id: string, s: any) => put(`/skills/${id}`, s),
  skillDelete: (id: string) => del(`/skills/${id}`),

  // methodologies
  methodologies: () => get('/methodologies'),
  methodologyCreate: (m: any) => post('/methodologies', m),
  methodologyUpdate: (id: string, m: any) => put(`/methodologies/${id}`, m),
  methodologyToggle: (id: string) => post(`/methodologies/${id}/toggle`),
  methodologyDelete: (id: string) => del(`/methodologies/${id}`),

  // knowledge
  collections: () => get('/knowledge/collections'),
  collectionCreate: (c: any) => post('/knowledge/collections', c),
  collectionDelete: (id: string) => del(`/knowledge/collections/${id}`),
  documents: (collId: string) => get(`/knowledge/collections/${collId}/documents`),
  documentUpload: (collId: string, file: File) => {
    const fd = new FormData()
    fd.append('file', file)
    return request(`/knowledge/collections/${collId}/documents`, {
      method: 'POST',
      body: fd,
      headers: {}, // let the browser set the multipart boundary
    })
  },
  documentDelete: (collId: string, docId: string) =>
    del(`/knowledge/collections/${collId}/documents/${docId}`),
  knowledgeSearch: (q: string, collectionIds?: string[]) =>
    post('/knowledge/search', { query: q, collectionIds }),

  // memory
  memories: (params?: Record<string, string>) => {
    const q = params ? '?' + new URLSearchParams(params).toString() : ''
    return get('/memories' + q)
  },
  memoryDelete: (id: string) => del(`/memories/${id}`),
  memoryInvalidate: (id: string) => post(`/memories/${id}/invalidate`),
  persona: () => get('/memories/persona'),
  personaSave: (markdown: string) => put('/memories/persona', { markdown }),
  scenes: () => get('/memories/scenes'),
  drilldown: (level: string, id: string) => get(`/memories/drilldown?level=${level}&id=${id}`),
  consolidate: () => post('/memories/consolidate'),
  conflicts: () => get('/memories/conflicts'),
  // The conflict endpoint takes a ruling plus the id of the memory to retire.
  // It used to be called with {winner: 'a'|'b'}, which the backend rejected
  // with 400 while the page reported nothing at all.
  conflictResolve: (id: string, resolution: string, supersedeId?: string) =>
    post(`/memories/conflicts/${id}/resolve`, { resolution, resolvedBy: 'user', supersedeId }),

  // tools / modules / jobs
  tools: () => get('/tools'),
  modules: () => get('/modules'),
  moduleToggle: (id: string, enabled: boolean) => post(`/modules/${id}/toggle`, { enabled }),
  jobs: () => get('/jobs'),
  job: (id: string) => get(`/jobs/${id}`),

  // script tools (DESIGN §8 /api/v1/scripts)
  scripts: () => get('/scripts'),
  scriptRuntimes: () => get('/scripts/runtimes'),
  scriptCreate: (s: any) => post('/scripts', { script: s }),
  scriptUpdate: (id: string, s: any) => put(`/scripts/${id}`, { script: s }),
  scriptDelete: (id: string) => del(`/scripts/${id}`),
  scriptAdmit: (id: string) => post(`/scripts/${id}/admit`),
  scriptToggle: (id: string) => post(`/scripts/${id}/toggle`),
  scriptRun: (id: string, input: string) => post(`/scripts/${id}/run`, { input }),

  // MCP servers (DESIGN §8 /api/v1/mcp/servers)
  mcpServers: () => get('/mcp/servers'),
  mcpCreate: (s: any) => post('/mcp/servers', s),

  // software control (DESIGN §8 /api/v1/control/*, §9.1)
  controlStatus: () => get('/control/status'),
  controlApps: () => get('/control/apps'),
  controlInspect: (appId: string, opts?: { maxDepth?: number; maxNodes?: number; includeOffscreen?: boolean }) =>
    post(`/control/apps/${appId}/inspect`, opts ?? {}),
  controlRun: (req: any) => post('/control/run', req),
  // The built-in deny list, shown read-only: it is the one boundary nothing can
  // register around, so there is deliberately no endpoint to edit it.
  controlDenyList: () => get('/control/denylist'),

  // environment probe (DESIGN §8 /api/v1/env/probe): what this machine can do
  // the requested job with.
  envProbe: (q: string) => get(`/env/probe?q=${encodeURIComponent(q)}`),

  // briefs (DESIGN §8 /api/v1/briefs): the notes taken while a task is being
  // prepared, stored as plain Markdown on disk.
  briefList: () => get('/briefs'),
  briefCreate: (title: string) => post('/briefs', { title }),
  briefGet: (id: string) => get(`/briefs/${id}`),
  briefAppend: (id: string, text: string) => post(`/briefs/${id}`, { text }),
  mcpUpdate: (id: string, s: any) => put(`/mcp/servers/${id}`, s),
  mcpDelete: (id: string) => del(`/mcp/servers/${id}`),
  mcpStart: (id: string) => post(`/mcp/servers/${id}/start`),
  mcpStop: (id: string) => post(`/mcp/servers/${id}/stop`),
  mcpTest: (s: any) => post('/mcp/servers/test', s),
  mcpCall: (id: string, tool: string, args: any) => post(`/mcp/servers/${id}/call`, { tool, args }),

  // settings
  settings: () => get('/settings'),
  settingsSave: (s: any) => put('/settings', s),

  // debug
  systemPrompt: (convId: string, query: string) =>
    post('/debug/system-prompt', { convId, query }),
}

/**
 * Streams a chat turn. The backend emits SSE events shaped
 * `{type: "delta"|"route"|"tool"|"done"|"error", data: {...}}`.
 * Returns an abort handle so the caller can stop a turn mid-stream.
 */
export function streamChat(
  convId: string,
  body: { message: string; model?: string; mode?: string },
  onEvent: (type: string, data: any) => void,
): { abort: () => void } {
  const ctrl = new AbortController()
  ;(async () => {
    try {
      const res = await fetch(`${BASE}/conversations/${convId}/messages`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
        signal: ctrl.signal,
      })
      if (!res.ok || !res.body) {
        onEvent('error', { message: `HTTP ${res.status}` })
        return
      }
      const reader = res.body.getReader()
      const decoder = new TextDecoder()
      let buf = ''
      for (;;) {
        const { value, done } = await reader.read()
        if (done) break
        buf += decoder.decode(value, { stream: true })
        // SSE frames are separated by a blank line.
        let idx: number
        while ((idx = buf.indexOf('\n\n')) >= 0) {
          const frame = buf.slice(0, idx)
          buf = buf.slice(idx + 2)
          for (const line of frame.split('\n')) {
            if (!line.startsWith('data:')) continue
            const raw = line.slice(5).trim()
            if (!raw) continue
            try {
              const parsed = JSON.parse(raw)
              // Frames sent as `event: error` carry no type field — surface
              // them as errors instead of silently treating them as content.
              const type = parsed.type || (parsed.message ? 'error' : 'delta')
              onEvent(type, parsed.data ?? parsed)
            } catch {
              /* a malformed frame must not kill the whole turn */
            }
          }
        }
      }
    } catch (e: any) {
      if (e?.name !== 'AbortError') onEvent('error', { message: String(e?.message || e) })
    }
  })()
  return { abort: () => ctrl.abort() }
}
