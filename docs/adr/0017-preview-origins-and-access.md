# 0017: Preview origins and access

## Status

Accepted.

## Context

Each sandbox gets an HTTPS preview of its app, reachable only after login (requirements; plan 3.2, 3.11). Preview content is controlled by the sandbox, so it must never be able to act as the dashboard. Spike 04 measured the cookie and WebAuthn behaviour of both layouts.

## Decision

1. **Layout:** previews live at `<sandbox>--<project>.<preview domain>`. A separate registrable domain is the default and the advice. The same registrable domain as the dashboard is allowed, relying on ADR 0016's `__Host-` cookies, exact RP ID and Origin checks.
2. **Host pattern:** exactly one label before the preview domain, lowercase, with exactly one `--` that splits a sandbox slug and a project slug.
3. **Access token:** without a valid preview cookie the proxy redirects to `https://<dashboard>/preview-auth?host=…&next=…`. That endpoint needs a dashboard session and returns a token of 32 random bytes: single-use, valid 60 seconds, bound to the host. Only its hash is stored. Tokens are consumed even when the check fails.
4. **Preview cookie:** `/__wsm/auth` on the preview host sets `__Host-wsm_preview` (random, Secure, HttpOnly, SameSite=Lax, valid 12 hours, bound to that host on the server side). Lax, because Strict misses the first request after the click from the dashboard.
5. **Reserved path:** `/__wsm/` on preview hosts belongs to wsm and is never forwarded.
6. **`next`:** must be a path starting with exactly one `/`, without `//` or `\`; anything else becomes `/`.
7. **Proxy:** removes `__Host-wsm_preview` from the request's `Cookie` header and any `Set-Cookie` for that name from the app's response. It forwards to the sandbox's app port published on `127.0.0.1` (plan 3.3).
8. **Cookie tossing between previews** is a known limitation: a preview can set a cookie for the whole preview domain, which other previews' apps receive. All previews belong to the one owner, so this is accepted for phase 1.

## Consequences

- A sandbox's app never sees wsm's cookies, and a preview cannot reach the dashboard API.
- Previews need a wildcard DNS record and certificate for the preview domain (plan §7).
- Revisit cookie tossing when there are multiple users.
