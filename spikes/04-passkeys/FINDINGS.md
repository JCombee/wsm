# Findings: spike 04 passkeys

- **Run:** 17 November 2026, VM `tov-38348714` (template `debian-13-public`, address 198.51.100.101), Debian 13, kernel 6.12, Chromium 156.0.8078.79 (Debian package, headless, as a normal user with its sandbox on), go-webauthn v0.18.2, chromedp v0.23.1, cdproto v0.160.1. Binary: 15.7 MB, static.
- **How:** this code ran as the preflight of the spike-04 brief. On 21 November it became the spike as is (renamed from `s04pre` to `pkspike`), because the owner asked to finish phase 1 in one go. The raw output is in `spikes/runs/20261117T003700Z-preflight-tov-38348714/`. The criteria IDs are the preflight's, not the brief's 23 IDs.
- **Outcome:** go for M4, with a **separate registrable preview domain** as the default. Previews under the dashboard's own registrable domain stay possible, but a sandbox can then plant cookies that every other preview and the dashboard see (see `PREVIEW-FLOW` below).

## Criteria

| ID | Result | Evidence |
|---|---|---|
| `BROWSER-START` | PASS | Chromium starts headless as a normal user, sandbox on, no cert-ignore flags |
| `TLS-NAV` | PASS | `https://dash.wsm.test` is a secure context through a test CA trusted in the user's NSS database |
| `WA-ENABLE` | PASS | virtual CTAP2 authenticator (internal, resident key, UV) |
| `PK-REGISTER` | PASS | `FinishRegistration` ok; one resident credential with rpId `dash.wsm.test` |
| `PK-LOGIN` | PASS | `FinishLogin` ok, `__Host-wsm_session` set |
| `PK-PREVIEW-RPID` (×2) | PASS | `get()` with rpId `dash.wsm.test` from `a--b.preview.wsm.test` and `a--b.wsm-preview.test`: `SecurityError` |
| `PK-PREVIEW-CREATE` | PASS | `create()` with that rpId from a preview: `SecurityError` |
| `PK-SERVER-ORIGIN` | PASS | an RP with the broader ID `wsm.test` lets the browser sign on a preview, and `FinishLogin` rejects it on origin |
| `COOKIE-DASH-SEES` | PASS | after the preview's attempts the dashboard receives exactly one, valid `__Host-wsm_session` |
| `PREVIEW-FLOW` (separate domain) | PASS | dashboard → one-time token → `__Host-wsm_preview` → `/app?x=1`; the app gets no wsm cookie, `document.cookie` is empty |
| `PREVIEW-FLOW` (same domain) | **FAIL, expected** | the flow works, but the app receives `__Secure-wsm_session` and `wsm_session`, which an earlier preview set with `Domain=wsm.test` |
| `PREVIEW-REPLAY`, `-WRONGHOST`, `-EXPIRED` | PASS | reuse, another host and an expired token each give 403 |
| `PREVIEW-CLICK` (×2) | PASS | a click on a dashboard link ends on `/app?x=1` in both layouts |

## What we learned

- A strict RP (RP ID exactly the dashboard host, one origin) holds: no preview origin can use or create the passkey.
- When a preview tries, Chromium fetches `https://dash.wsm.test/.well-known/webauthn` (Related Origin Requests). A 404 keeps the refusal. A misconfigured document there lets the browser sign on the preview, and only the server's origin check then stops it (`PK-ROR-MISCONFIG`, INFO). So the dashboard must never serve that path.
- `__Host-` cookies cannot be set or overwritten from a preview: `Domain`, no `Secure` or `Path=/x` are all rejected, and a host-only `__Host-wsm_session` lands on the preview host, not the dashboard. `__Secure-` and plain names with `Domain=wsm.test` do get through. Only `__Host-` names may carry trust.
- A no-cors POST from a same-site preview carries the Lax session cookie (`Sec-Fetch-Site: same-site`). Requiring `Origin` = dashboard and `Sec-Fetch-Site: same-origin` stops it (403 for both layouts, 200 from the dashboard).
- A WebSocket handshake has **no** `Sec-Fetch-Site`, but it has `Origin`. The same-site preview's handshake carries the session cookie, so the WebSocket must check `Origin`.
- The preview cookie must be `SameSite=Lax`: with Strict, the first request after a cross-site click from the dashboard does not carry it, which would loop.

## Surprises and open questions

- **Cert-ignore flags do not stop WebAuthn** in Chromium 156 (`run-ignore`, `run-spki`): they would hide a TLS mistake, so tests must use a trusted test CA.
- **chromedp v0.23 has a new, generic API**; most examples online use the old one.
- **Cookie tossing between previews:** a cookie with `Domain=wsm-preview.test` from preview A reaches the app of preview B (`PREVIEW-CROSS-TOSS`, INFO). Options for M5: strip `Domain` from the app's `Set-Cookie` in the proxy (does not help against JavaScript), or accept it, because all previews belong to the same owner. ADR 0017 records the choice.

## Decisions

- Dashboard authentication: `docs/adr/0016-dashboard-authentication.md`.
- Preview origins and access: `docs/adr/0017-preview-origins-and-access.md`.
