import { reactive, computed } from 'vue'
import { api } from '@/api/client'

/**
 * Global app state. Deliberately a plain reactive singleton instead of Pinia:
 * the app has one store and adding a dependency for it would not pay off.
 */
export const store = reactive({
  // providers & roles are needed by almost every page (model pickers, badges),
  // so they load once at boot and are refreshed on demand.
  providers: [] as any[],
  roles: [] as any[],
  conversations: [] as any[],
  skills: [] as any[],
  methodologies: [] as any[],
  modules: [] as any[],
  settings: null as any,
  theme: (localStorage.getItem('agenthub-theme') || 'dark') as 'dark' | 'light',
  backendVersion: '',
  booted: false,
})

export const themeClass = computed(() => (store.theme === 'dark' ? 'theme-dark' : 'theme-light'))

export function toggleTheme() {
  store.theme = store.theme === 'dark' ? 'light' : 'dark'
  localStorage.setItem('agenthub-theme', store.theme)
  applyTheme()
}

export function applyTheme() {
  document.documentElement.classList.toggle('dark', store.theme === 'dark')
  document.documentElement.dataset.theme = store.theme
}

/** Loads everything the shell needs. Failures are non-fatal: the pages retry. */
export async function boot() {
  try {
    const h = await api.health()
    store.backendVersion = h?.version || ''
  } catch (e: any) {
    store.backendVersion = 'offline'
  }
  await Promise.allSettled([
    api.providers().then((d) => (store.providers = d || [])),
    api.roles().then((d) => (store.roles = d || [])),
    api.skills().then((d) => (store.skills = d || [])),
    api.methodologies().then((d) => (store.methodologies = d || [])),
    api.modules().then((d) => (store.modules = d || [])),
    api.settings().then((d) => (store.settings = d)),
    // Conversation list lives in the sidebar shell, so it has to be loaded here
    // rather than by the chat page: any route may be the first one opened.
    api.conversations().then((d) => (store.conversations = d || [])),
  ])
  store.booted = true
}

export async function refreshRoles() {
  store.roles = (await api.roles()) || []
}
export async function refreshProviders() {
  store.providers = (await api.providers()) || []
}
export async function refreshSkills() {
  store.skills = (await api.skills()) || []
}
export async function refreshMethodologies() {
  store.methodologies = (await api.methodologies()) || []
}
export async function refreshModules() {
  store.modules = (await api.modules()) || []
}

export async function refreshConversations() {
  store.conversations = (await api.conversations()) || []
}

export async function deleteConversation(id: string) {
  await api.conversationDelete(id)
  await refreshConversations()
}

export function roleById(id: string) {
  return store.roles.find((r) => r.id === id) || null
}

export function roleBadgeColor(id: string) {
  // Stable per-role hue so the badge colour never changes between sessions.
  let h = 0
  for (let i = 0; i < id.length; i++) h = (h * 31 + id.charCodeAt(i)) % 360
  return `hsl(${h} 70% 55%)`
}

export type ModelOption = { value: string; label: string; disabled: boolean }

/** The user's per-model decision on a provider's discovered catalogue.
 *
 * The backend stores this sparsely and beside the catalogue rather than inside
 * it, so an absent entry is the normal case and means "keep, not pinned".
 */
export function modelPrefOf(p: any, modelId: string): { disabled: boolean; favorite: boolean } {
  const pref = (p?.modelPrefs || {})[modelId]
  return { disabled: pref?.disabled === true, favorite: pref?.favorite === true }
}

/** The models a picker may offer from one provider, pinned ones first.
 *
 * The full catalogue is not a picker. An aggregator advertises several hundred
 * models under a single provider, so narrowing a picker to one vendor is only
 * half the job — the other half is that the user has already marked the handful
 * they use, and that marking is what the list reads.
 */
export function usableModelsOf(p: any): any[] {
  const models = ((p?.models || []) as any[]).filter((m) => !modelPrefOf(p, m.id).disabled)
  return models
    .map((m, i) => ({ m, i, pinned: modelPrefOf(p, m.id).favorite }))
    .sort((a, b) => Number(b.pinned) - Number(a.pinned) || a.i - b.i)
    .map((x) => x.m)
}

/** Models offered by one provider.
 *
 * A model id only means something inside its provider: the same name exists on
 * several vendors and resolves to different weights, prices and limits. So a
 * picker that also names a provider is scoped to it, instead of pooling every
 * vendor's models into one flat list — which is not a picker, it is a
 * catalogue, and at a hundred entries nobody can find anything in it.
 *
 * `keepId` is offered even when the provider no longer advertises it, so an
 * existing binding still renders what it actually points at instead of reading
 * as unset.
 */
export function modelsOfProvider(providerId: string, keepId = ''): ModelOption[] {
  const opt = (value: string, label: string, disabled = false): ModelOption => ({ value, label, disabled })
  if (!providerId) return []
  const p = store.providers.find((x) => x.id === providerId)
  if (!p) return keepId ? [opt(keepId, `${keepId}（服务商已删除）`)] : []
  const off = p.enabled === false
  const out: ModelOption[] = usableModelsOf(p).map((m: any) => opt(m.id, m.name || m.id, off))
  if (keepId && !out.some((o) => o.value === keepId)) {
    out.push(opt(keepId, `${keepId}（该服务商未提供）`))
  }
  return out
}

/** Models grouped by provider, for the in-conversation override.
 *
 * Grouped rather than pooled: two hundred models across five vendors is a
 * catalogue you have to read, not a picker you can use. The value keeps the
 * `provider/model` shape the orchestrator parses, so one turn can be pointed at
 * another vendor without editing the role. Switched-off providers are left out
 * — the backend refuses them, so offering one only surfaces as a send-time error
 * that reads as a connectivity problem.
 */
export const modelGroups = computed(() => {
  const groups: { label: string; options: ModelOption[] }[] = []
  for (const p of store.providers) {
    if (p.enabled === false) continue
    const models = usableModelsOf(p)
    if (!models.length) continue
    groups.push({
      label: p.name,
      options: models.map((m: any) => ({
        value: `${p.id}/${m.id}`,
        label: `${m.name || m.id}${modelPrefOf(p, m.id).favorite ? ' ★' : ''}`,
        disabled: false,
      })),
    })
  }
  return groups
})
