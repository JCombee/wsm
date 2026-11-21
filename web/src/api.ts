// The dashboard's API client. Every state-changing request carries the
// session's CSRF token; the server also checks Origin (ADR 0016).

let csrf = ''

export class ApiError extends Error {
  constructor(public status: number, message: string) {
    super(message)
  }
}

export interface Status {
  logged_in: boolean
  enrolled: boolean
  csrf_token?: string
}

export async function status(): Promise<Status> {
  const r = await fetch('/auth/status', { credentials: 'same-origin' })
  const st = (await r.json()) as Status
  csrf = st.csrf_token ?? ''
  return st
}

async function call<T>(method: string, path: string, body?: unknown, prefix = '/api'): Promise<T> {
  const headers: Record<string, string> = {}
  if (method !== 'GET') headers['X-WSM-CSRF'] = csrf
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  const r = await fetch(prefix + path, {
    method,
    headers,
    credentials: 'same-origin',
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  if (r.status === 204) return undefined as T
  const text = await r.text()
  let data: any = undefined
  try {
    data = text ? JSON.parse(text) : undefined
  } catch {
    data = { error: text }
  }
  if (!r.ok) throw new ApiError(r.status, data?.error ?? r.statusText)
  return data as T
}

export const get = <T>(path: string) => call<T>('GET', path)
export const post = <T>(path: string, body?: unknown) => call<T>('POST', path, body)
export const put = <T>(path: string, body?: unknown) => call<T>('PUT', path, body)
export const del = <T>(path: string) => call<T>('DELETE', path)

export async function logout() {
  await call('POST', '/auth/logout', undefined, '')
}

export interface Project {
  id: string
  slug: string
  name: string
  linux_user: string
  linux_uid: number
  image_source: string
  image: string
  cpu_millis: number
  mem_bytes: number
  pids_limit: number
  repo_url?: string
  base_branch?: string
  app_port?: number
  created_at: string
}

export interface Sandbox {
  id: string
  project: string
  slug: string
  status: string
  pod: string
  branch?: string
  preview_url?: string
  created_at: string
}

export interface Entry {
  name: string
  type: 'file' | 'dir' | 'symlink' | 'other'
  size: number
  mode: number
  mtime: number
}

export interface Enabled {
  block: string
  tag: string
  host: string
  port: number
  user?: string
  database?: string
}

export interface Block {
  block: string
  profile: string
  tags: string[]
}

export interface Service {
  block: string
  tag: string
  address: string
  port: number
  state: string
  version?: string
}

export interface AuditEvent {
  seq: number
  ts: string
  actor: string
  sandbox_id?: string
  tool: string
  args: string
  result: string
  detail?: string
  hash: string
}

export const enc = encodeURIComponent

export function sandboxPath(project: string, sandbox: string) {
  return `/v1/projects/${enc(project)}/sandboxes/${enc(sandbox)}`
}

// Base64 helpers for file contents, which JSON carries as base64.
export function b64ToBytes(b64: string): Uint8Array {
  const bin = atob(b64)
  const out = new Uint8Array(bin.length)
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i)
  return out
}

export function bytesToB64(bytes: Uint8Array): string {
  let bin = ''
  for (let i = 0; i < bytes.length; i += 0x8000) {
    bin += String.fromCharCode(...bytes.subarray(i, i + 0x8000))
  }
  return btoa(bin)
}
