# Dashboard and previews: setup

How to turn on the web dashboard (M4) and the previews (M5) on a host where
`deploy/install.sh` has run. Background: ADR 0016 (login), 0017 (previews)
and 0018 (frontend).

## 1. Build with the frontend

```sh
make web     # needs Node.js and npm, once per release
make build   # bin/wsm embeds web/dist; bin/wsm-fs is the sandbox file helper
sudo deploy/install.sh --bin bin
```

Without `make web` the dashboard only says that the frontend is not built.

## 2. Names and TLS

- `dash.example.com` points to the server. It is the passkey RP ID, so it
  cannot change later without registering passkeys again.
- `*.example-preview.net` points to the server too. A separate registrable
  domain is advised; a subdomain of the dashboard's domain works, but then
  a sandbox app can set cookies that other previews receive (ADR 0017).
- Caddy terminates TLS and forwards to the loopback listeners; see
  `deploy/Caddyfile.example`. Passkeys work only over HTTPS.

## 3. Turn it on

Edit `/etc/wsm/wsm.env`:

```sh
WSM_SERVE_FLAGS=--dashboard-host dash.example.com --preview-domain example-preview.net
```

Then `sudo systemctl restart wsm`. The dashboard listens on
`127.0.0.1:8080`, the preview proxy on `127.0.0.1:8081`
(`--dashboard-addr`, `--preview-addr`). Both refuse other hosts.

## 4. Register a passkey

```sh
wsm auth enroll
```

prints a link that works once, for 15 minutes. Open it, give the passkey a
name and create it. The same command is the way back in when every passkey
is lost. Later passkeys can be added the same way.

## 5. Previews

Give a project the port its app listens on: `wsm project create ...
--app-port 3000`. Every sandbox created afterwards publishes that port on
`127.0.0.1`, and gets `WSM_PREVIEW_URL`
(`https://<sandbox>--<project>.<preview domain>/`). The app must listen on
`0.0.0.0`. Opening the preview sends the browser once through the dashboard,
which hands over access to that one host for 12 hours.

A sandbox gets no preview when its slug or its project's slug contains
`--`, or when both together are longer than 61 characters.

## 6. Files

The dashboard reads and writes files through `wsm-fs`, mounted read-only at
`/opt/wsm/bin` in every sandbox created after this release. Files up to
512 KiB of UTF-8 text can be edited; a directory shows up to 2000 entries.
Sandboxes created before need to be created again for file access.
