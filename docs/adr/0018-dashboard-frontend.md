# 0018: Dashboard frontend

## Status

Accepted. CLAUDE.md asked to confirm the framework with the owner first; the owner asked to finish phase 1 in one go and to adjust afterwards, so this follows the proposal from the plan.

## Context

M4 needs a web dashboard with a terminal, a file editor and the project, service and audit pages (plan 3.7, 3.8). The plan proposed Vue 3 with TypeScript, Monaco and xterm.js.

## Decision

1. **Stack:** Vue 3, TypeScript and Vite in `web/`, with no router or state library: the dashboard has a handful of pages, and a small history router is enough.
2. **Editor:** `monaco-editor` with only the core editor and the basic-language syntax highlighting, plus the editor worker. No TypeScript, JSON, CSS or HTML language workers: they quadruple the bundle and are not needed to edit files.
3. **Terminal:** `@xterm/xterm` with the fit add-on, over the existing WebSocket protocol (`internal/terminal`).
4. **Embedding:** `make web` builds into `web/dist`, which `go:embed` puts into the `wsm` binary (package `web`). The build output is not committed; without it the dashboard answers with a hint to run `make web`. `make build` therefore works without Node.js.
5. **Safety:** sandbox content is only ever rendered as text (Vue interpolation, Monaco, xterm), never with `v-html` (CLAUDE.md invariant 11). The content security policy allows only the dashboard's own scripts.
6. **Versions:** exact versions in `package.json`, with `package-lock.json` committed. TypeScript stays on 5.x, because `vue-tsc` does not work with TypeScript 7 yet. `dompurify`, which Monaco pulls in, is overridden to a version without known advisories.

## Consequences

- The `wsm` binary grows by about 4 MB.
- Building a release needs Node.js and npm once (`make web`); the server does not.
- Changing the framework later means replacing `web/` only; the API stays.
