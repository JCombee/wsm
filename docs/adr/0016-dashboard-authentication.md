# 0016: Dashboard authentication

## Status

Accepted.

## Context

M4 adds the web dashboard. Plan 3.11 asks for passkey login, a `__Host-` session cookie, a CSRF token and an Origin check on the WebSocket, and invariant 10 fixes the RP ID and cookie rules. Spike 04 measured how Chromium and go-webauthn behave; see `spikes/04-passkeys/FINDINGS.md`.

## Decision

1. **Library:** `github.com/go-webauthn/webauthn` (v0.18.2) in the main module. WebAuthn verification is too security-critical to write ourselves, and this is the maintained Go implementation the spike proved.
2. **Relying party:** the RP ID is exactly the dashboard host, with exactly one origin, `https://<dashboard host>`. No per-ceremony RP ID overrides, and `RPAllowCrossOrigin` stays false.
3. **Never serve `/.well-known/webauthn`.** A related-origins document there would let a preview sign assertions.
4. **One owner, discoverable login.** The requirements have a single user. Registration requires a resident key, and login uses `BeginDiscoverableLogin`, so the owner never types a user name.
5. **Enrolment:** `wsm auth enroll` on the server (over the admin socket) creates a one-time registration link, valid for 15 minutes. It is also the recovery path until TOTP exists.
6. **Ceremony state** lives on the server, is single-use, expires after 5 minutes and is bound to the browser with `__Host-wsm_ceremony` (Secure, HttpOnly, SameSite=Strict).
7. **Session:** `__Host-wsm_session`, 32 random bytes, Secure, HttpOnly, SameSite=Lax, Path=/, no Domain, valid 7 days. The database stores only its SHA-256. Only `__Host-` cookies carry trust.
8. **Requests that change state:** every non-GET request needs `Origin` = the dashboard origin, `Sec-Fetch-Site: same-origin` (or absent, for non-browser clients that send the CSRF header), a valid session, and the session's CSRF token in `X-WSM-CSRF`.
9. **WebSocket:** the handshake needs a valid session and `Origin` = the dashboard origin, because handshakes carry no `Sec-Fetch-Site`.
10. **Transport:** the dashboard listens on loopback; Caddy terminates TLS in front of it (plan §7).

## Consequences

- A preview origin can neither use the passkey nor act on the API with the owner's session.
- Losing every passkey means running `wsm auth enroll` on the server.
- Isolation tests check the HTTP-level rules against a real install (auth required, origin and CSRF checks, the WebSocket origin). The browser-level passkey behaviour is covered by spike 04; repeating it with chromedp in the isolation tests is left for later.
