// A small history router: the dashboard has only a handful of pages.
import { reactive } from 'vue'

export const route = reactive({ path: location.pathname, query: new URLSearchParams(location.search) })

export function navigate(to: string) {
  history.pushState(null, '', to)
  update()
}

function update() {
  route.path = location.pathname
  route.query = new URLSearchParams(location.search)
}

window.addEventListener('popstate', update)

// safeNext keeps redirects after login on this origin.
export function safeNext(next: string | null): string {
  if (!next || !next.startsWith('/') || next.startsWith('//') || next.includes('\\')) return '/'
  return next
}
