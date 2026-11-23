# Onderzoek: Paperclip volledig in wsm-sandboxes

Datum: 22 november 2026. Status: onderzoek. Er is nog niets besloten en nog niets gebouwd.

**De vraag van de eigenaar:** hoe sluiten we wsm aan op Paperclip (paperclip.ing), zodat Paperclip volledig in de geïsoleerde rootless Podman-containers van wsm draait en daar ook zijn werk doet? Wat niet kan, beschrijven we met de reden. Een oplossing daarvoor zoeken we nog niet.

**Gebaseerd op:**
- **wsm:** de eisen, het bouwplan, ADR 0001–0018 en de code op `brief/v1` (`380c70e`).
- **Paperclip:** de broncode en documentatie op `master`, commit [`7671e0c`](https://github.com/paperclipai/paperclip/tree/7671e0cf0d718351f3b0c4d6a7d33df9867ffb8a) van 22 november 2026. Bronverwijzingen staan als `[P: pad]`. Ze zijn relatief aan die commit; de lijst staat onderaan.
- **De visuele versie** van dit stuk staat in `paperclip-in-wsm.html`.

## Conclusie

- **Het kan, met één sandbox per Paperclip-instantie.** In de werkcontainer van één wsm-sandbox draaien samen:
  - de Paperclip-server;
  - de ingebouwde PostgreSQL;
  - de plugin-workers;
  - alle agent-CLI's (Claude Code, Codex, …).

  Het board van Paperclip is de preview van die sandbox. Hieronder heet dat de **hoofdkantoor-sandbox**.
- **wsm hoeft er niet voor te veranderen.** Alles past in wat fase 1 al biedt:
  - een projectimage uit een Containerfile;
  - een app-poort met preview;
  - het home-volume per project;
  - code via een git bundle;
  - services via de MCP.

  Er is geen nieuwe invariant nodig en er wordt geen invariant gebroken.
- **Binnen de sandbox is er maar één vertrouwensdomein.**
  - Paperclip draait lokale agents zonder sandbox, als dezelfde gebruiker als de server: "Local CLI adapters run unsandboxed on the host machine" `[P: docs/agents-runtime.md]`. In dit ontwerp is die "host" de wsm-sandbox.
  - Elke agent kan dus bij de database, de sleutels en de sessies van Paperclip. Rollen, goedkeuringen en budgetten van Paperclip zijn daar geen veiligheidsgrens.
  - De echte grens is wsm, en die houdt: de schade blijft binnen het project.
- **Wat niet kan zonder de invarianten te breken** (zie hoofdstuk 5):
  1. Paperclip als orkestrator die andere wsm-sandboxes aanmaakt en daar agents in draait (sandbox-provider, SSH-omgeving, Runner).
  2. Paperclips eigen insluiting van vijandige invoer (`low_trust_review`). Die vereist een sandbox-provider.
  3. Scheiding tussen agents of tussen companies binnen één instantie.
  4. Een eigen wsm-identiteit per Paperclip-agent, voor MCP-token, audit en grants.
  5. Code naar buiten: push, pull requests en merges vanuit Paperclip.
  6. Inkomende webhooks, en daarmee de meeste chatkoppelingen. Discord werkt wel.
  7. Previews van de apps die de agents bouwen, naast het board.
  8. Containers, Tailscale, Paperclips eigen Bubblewrap-opsluiting en andere functies die rechten in de sandbox vragen.
  9. Paperclip bijwerken zonder nieuw project of verlies van `/workspace`.
- **Advies:**
  1. Eerst een spike `05-paperclip` op een wegwerp-VM, met het gescripte nepmodel in plaats van echte LLM-sleutels.
  2. Daarna een ADR en een voorbeeld-Containerfile in `examples/paperclip/`.

## 1. Wat Paperclip is

Paperclip is een open-source control plane (MIT) voor teams van AI-agents, "zero-human companies".
- **Begrippen:** je richt een *company* in met doelen, een organigram van agents, projecten en issues.
- **Heartbeats:** agents werken niet continu. Ze draaien in korte runs die een wekker start: een timer, een toewijzing, een vermelding of de knop `[P: docs/agents-runtime.md]`.
- **Sessies:** een run die eindigt, kan in de volgende heartbeat zijn sessie hervatten.

### 1.1 Onderdelen die voor wsm tellen

| Onderdeel | Wat het is | In een wsm-sandbox |
|---|---|---|
| Server | Node.js 24.11 of nieuwer, poort 3100 `[P: package.json]` | Een proces in de werkcontainer, als UID 1000 |
| Board-UI | React, geserveerd door de server; live updates over WebSocket | Via de preview van de sandbox |
| Database | Ingebouwde PostgreSQL 18 (`embedded-postgres`) als `DATABASE_URL` ontbreekt; anders een externe PostgreSQL `[P: doc/DATABASE.md]` | Een child process in dezelfde container |
| Data | Database, bijlagen, back-ups, master key en logs onder `PAPERCLIP_HOME/instances/<id>/`; plugins in `~/.paperclip/plugins` `[P: packages/shared/src/home-paths.ts, server/src/services/plugin-loader.ts]` | Op het home-volume (§4.7) |
| Adapters | Hoe een agent draait: `claude_local`, `codex_local`, `gemini_local`, `opencode_local`, `pi_local`, `cursor`, `hermes_*`, `openclaw_gateway`, `process`, `http` | De lokale adapters starten een CLI als child process |
| Omgevingen | Waar een run draait: `local`, `ssh`, `sandbox` of `plugin` `[P: packages/shared/src/constants.ts]` | Alleen `local` is bruikbaar (U1) |
| Plugins | npm-pakketten met eigen worker-processen, waaronder sandbox-providers voor Daytona, E2B, Kubernetes, Modal, Cloudflare en meer `[P: packages/plugins/sandbox-providers/]` | De workers draaien in dezelfde container |
| Runner | Een Rust-daemon (`paperclip-runnerd`) voor runs op afstand | Niet nodig voor lokaal werk |
| Telemetrie | Staat standaard aan en gaat naar `telemetry.paperclip.ing` `[P: packages/shared/src/telemetry/]` | Uitzetten met `PAPERCLIP_TELEMETRY_DISABLED=1` |

### 1.2 Paperclips eigen beveiligingsmodel

Paperclip kent twee situaties.
- **Lokaal.** De agent-CLI is een child process van de server, zonder sandbox en met de rechten van de server.
  - Voor Claude staat `--dangerously-skip-permissions` standaard aan `[P: packages/adapters/claude-local/src/server/execute.ts]`.
  - De agent krijgt de omgeving van de server mee. Paperclip haalt daar alleen de `PAPERCLIP_*`-variabelen uit `[P: packages/adapter-utils/src/server-utils.ts, sanitizeInheritedPaperclipEnv]`. `BETTER_AUTH_SECRET` en de `WSM_*`-variabelen gaan dus mee.
  - Optioneel draait een lokale run in Bubblewrap (`filesystemScope`, `networkScope`). Dat staat standaard uit `[P: packages/adapters/claude-local/src/index.ts]`, en in wsm werkt het waarschijnlijk niet (U8).
- **Sandbox-provider.** Nu is de server de vertrouwde host en de sandbox onbetrouwbaar `[P: packages/plugins/sandbox-providers/SANDBOX-REQUIREMENTS.md]`.
  - Paperclip bewaakt dan maar twee bevoegdheden:
    - bestanden terugschrijven naar de host, via workspace-synchronisatie;
    - de Paperclip-API aanroepen, via de HTTP-bridge.
  - Binnen de sandbox beschermt Paperclip niets: "Paperclip does not protect sandbox files, processes, credentials, or code from other code in the same sandbox."

## 2. Wat wsm daartegenover zet

Kort, en alleen wat voor Paperclip telt:
- **Een project** is een Linux-gebruiker met rootless Podman, een projectimage, limieten en een plafond van blokken.
- **Een sandbox** is een pod. De werkcontainer:
  - draait als `1000:1000`, zonder capabilities, met `no-new-privileges`, seccomp en CPU-, geheugen- en pids-limieten (invariant 9);
  - heeft geen Podman-socket en geen hostmappen, alleen de read-only `wsm-fs` (invariant 1).
- **Volumes:** `/workspace` is een volume per sandbox. `/home/agent` is een volume per project, dat alle sandboxes van dat project delen (plan 3.4).
- **Starten:** de werkcontainer start met `--init` en houdt het eigen `CMD`/`ENTRYPOINT` van het image. Eindigt dat proces, dan stopt de sandbox (ADR 0007).
- **Omgeving:** `WSM_PROJECT`, `WSM_SANDBOX`, `WSM_BRANCH`, `WSM_PREVIEW_URL`, `WSM_MCP_URL`, `WSM_MCP_TOKEN` en `HOME=/home/agent`.
- **Preview:** één app-poort per project.
  - wsm publiceert die op `127.0.0.1`, in de reeks 47000–48999.
  - De preview-proxy stuurt `https://<sandbox>--<project>.<preview-domein>/` daarheen, maar pas na inloggen met de passkey (ADR 0017).
- **Uitgaand verkeer** (ADR 0001):
  - internet is open;
  - LAN, cloud-metadata, de loopback en de overige adressen van de host en andere sandboxes zijn dicht;
  - de MCP op `10.255.0.1:7443` mag altijd, een gedeelde service pas na `service_enable`.
- **Code** komt binnen als git bundle op de branch `agent/<sandbox>`, zonder remote en zonder credentials (ADR 0010). Code naar buiten brengen is fase 2.
- **Beheer:** alleen de eigenaar maakt sandboxes aan en verwijdert ze (aanname 4). De MCP heeft geen beheertools (invariant 2).

## 3. De kernvraag: waar ligt de grens?

Paperclip en wsm leggen de grens op verschillende plekken:

```
Paperclip met sandbox-provider:  [ Paperclip-server ]  ══ grens ══  [ agent-sandbox ]
Paperclip lokaal:                [ Paperclip-server + agents, zelfde gebruiker, geen grens ]
wsm:                             [ manager · helper · host ]  ══ grens ══  [ sandbox ]
Dit ontwerp:                     [ manager · helper · host ]  ══ grens ══  [ Paperclip-server + agents ]
```

"Paperclip volledig in wsm" zet de Paperclip-server aan de onbetrouwbare kant van de wsm-grens. Daarvoor zijn twee vormen denkbaar:
- **A. Alles in één sandbox.**
  - Server en agents delen één vertrouwensdomein, en wsm is de grens eromheen.
  - Dit is Paperclips lokale model, in een afgesloten doos.
- **B. De server in sandbox P, de agents in andere sandboxes.**
  - Dan moet P andere sandboxes aanmaken, er commando's in draaien en er bestanden uit halen.
  - Dat zijn precies de beheerbevoegdheden die wsm alleen aan de eigenaar geeft.
  - B kan dus niet (uitzondering U1).

De rest van dit stuk werkt vorm A uit.

## 4. Ontwerp: de hoofdkantoor-sandbox

### 4.1 Overzicht

```
Browser van de eigenaar
   │  https://hq--acme.<preview-domein>/
   ▼
Caddy (TLS) ──► wsm serve: preview-proxy (127.0.0.1:8081)
                  │  geen __Host-wsm_preview? dan eerst via het dashboard en de passkey
                  ▼
          127.0.0.1:47xxx  (gepubliceerde app-poort van de pod)
                  │  pasta
┌─────────────────▼──────── pod sbx-<id>, als gebruiker wsm-p-<uid> ───────────────┐
│ werkcontainer: Paperclip-image + tmux · 1000:1000 · cap-drop ALL · limieten      │
│                                                                                  │
│   Paperclip-server :3100   board-UI, REST-API, WebSocket                         │
│     ├── embedded PostgreSQL 18 (alleen loopback in de pod)                       │
│     ├── plugin-workers                                                           │
│     └── heartbeat ──► claude / codex / … (child processes, UID 1000)             │
│                          ├── cwd: /workspace of een git-worktree daarvan         │
│                          ├── API: http://127.0.0.1:3100 met de run-sleutel       │
│                          └── MCP: $WSM_MCP_URL met $WSM_MCP_TOKEN                │
│                                                                                  │
│   /workspace   volume per sandbox: repo op branch agent/hq                       │
│   /home/agent  volume per project: .paperclip/instances/hq, logins van de CLI's  │
└──────────────────────────────────────────────────────────────────────────────────┘
        │  uitgaand, nftables per project-UID (ADR 0001)
        ├──► internet: LLM-API's, npm, GitHub ............. open
        ├──► 10.255.0.1:7443  wsm-MCP ...................... open, met bearer-token
        ├──► 10.255.0.1:20xxx MariaDB ...................... pas na service_enable
        └──► LAN, metadata, host, andere sandboxes ......... dicht
```

### 4.2 Paperclip-begrippen in wsm

| Paperclip | In wsm | Toelichting |
|---|---|---|
| Instantie (server + database) | Eén sandbox, bijvoorbeeld `acme/hq` | Server, PostgreSQL, workers en agents in één werkcontainer |
| Company | Eén wsm-project per company (advies) | De schade van een injectie stopt bij het project, niet bij de company (U3) |
| Board-UI | De preview van de sandbox | App-poort 3100 |
| Board-login (Better Auth) | Een tweede laag achter de wsm-passkey | §4.6 |
| Agent in het organigram | Een child process in de werkcontainer, als UID 1000 | Geen scheiding op OS-niveau (U3) |
| Projectworkspace | `/workspace`, als lokaal pad | Gevuld met de git bundle van wsm, branch `agent/<sandbox>` |
| Execution-workspace (worktree per issue) | Een git-worktree binnen de sandbox | Paperclip draait git zelf, in de sandbox (§4.8) |
| Omgeving `local` | De werkcontainer | De enige bruikbare omgeving |
| Omgevingen `ssh`, `sandbox`, `plugin` | Niet beschikbaar | U1 |
| Secrets van Paperclip | In de database, met de master key op het home-volume | De secret-store van wsm komt in fase 2; ook dan leesbaar in de sandbox |
| Goedkeuringen, budgetten, rollen | Alleen binnen Paperclip, geen grens | De goedkeuringen van wsm zelf zijn fase 2 |
| Activity log | Paperclips database, in de sandbox (agents kunnen erin schrijven) | De wsm-audit staat buiten de sandbox, maar dekt alleen MCP-calls (U4) |
| Plugins | Een npm-install in de home; workers als child processes | Uitgaand verkeer naar npm is open |
| Runtime services (dev-servers) | Processen in dezelfde werkcontainer | Niet zichtbaar in de browser (U7) |

### 4.3 Het projectimage

**Eisen:**
- **Van wsm** (`docs/guide/images.md`):
  - `tmux`, `git` en `bash`;
  - gebruiker 1000 met home `/home/agent`;
  - een hoofdproces dat blijft draaien.
- **Van Paperclip** `[P: SANDBOX-REQUIREMENTS.md, Required on PATH]`:
  - `node` 24.11 of nieuwer;
  - de gebruikte agent-CLI's op `PATH`;
  - `sh` en `tar`;
  - een schrijfbare home, cache en tmp.

**Draaien zonder root kan al.** Het entrypoint van Paperclip voert bij een gebruiker zonder root direct het commando uit, zonder `usermod` of `gosu` `[P: scripts/docker-entrypoint.sh]`.

Er zijn twee routes:

| | Route A: afgeleid van het officiële image (advies) | Route B: eigen image met npm |
|---|---|---|
| Basis | `ghcr.io/paperclipai/paperclip:sha-<commit>`, publiek, amd64 en arm64 `[P: doc/DOCKER.md]` | `node:24-trixie-slim` plus `npm install -g paperclipai@<versie>`. Dat pakket bevat de server en `embedded-postgres`; `paperclipai run` doet onboard en doctor en start de server `[P: cli/package.json, cli/src/index.ts]` |
| Voordeel | Het artefact dat Paperclip zelf test. Claude Code, Codex, OpenCode, Gemini CLI en Kimi zitten er al in `[P: Dockerfile]` | Kleiner, en je kiest zelf de CLI's |
| Nadeel | Groot: ongeveer 2,9 GB aan dependencies plus een provider-pack van ongeveer 2,5 GB, dat we niet gebruiken `[P: Dockerfile]` | Niet de route die Paperclip test; de spike moet die controleren |
| Wat erbij moet | `tmux`, de home van UID 1000 naar `/home/agent`, en het startscript | Alles |

Een schets voor route A. Die is nog niet getest; de spike moet hem bewijzen.
- De build-context van wsm bevat alleen de Containerfile (ADR 0008). Het startscript staat daarom inline.
- Pin het basisimage op een vaste `sha-<commit>`-tag. Een vaste Containerfile levert dan altijd hetzelfde image op.

```Dockerfile
# Paperclip als hoofdkantoor-sandbox (schets, nog niet getest).
FROM ghcr.io/paperclipai/paperclip:sha-<volledige commit-sha>

USER root
RUN apt-get update \
 && apt-get install -y --no-install-recommends tmux bash \
 && rm -rf /var/lib/apt/lists/* \
 && usermod --login agent --home /home/agent node \
 && groupmod --new-name agent node \
 && mkdir -p /home/agent && chown 1000:1000 /home/agent

# Startscript: zet WSM_* om naar instellingen van Paperclip.
RUN cat > /usr/local/bin/wsm-paperclip <<'EOF'
#!/bin/sh
set -eu
: "${WSM_PREVIEW_URL:?het project heeft --app-port 3100 en een preview-domein nodig}"
export PAPERCLIP_HOME="$HOME/.paperclip"
export PAPERCLIP_INSTANCE_ID="$WSM_SANDBOX"
inst="$PAPERCLIP_HOME/instances/$PAPERCLIP_INSTANCE_ID"
export PAPERCLIP_CONFIG="$inst/config.json"
export HOST=0.0.0.0 PORT=3100
export PAPERCLIP_DEPLOYMENT_MODE=authenticated PAPERCLIP_DEPLOYMENT_EXPOSURE=private
export PAPERCLIP_PUBLIC_URL="${WSM_PREVIEW_URL%/}"
export PAPERCLIP_API_URL="http://127.0.0.1:3100"
export PAPERCLIP_TELEMETRY_DISABLED=1 DO_NOT_TRACK=1
export PAPERCLIP_DISABLE_CWD_ENV_FILE=true
umask 077
mkdir -p "$inst"
if [ ! -f "$inst/.env" ]; then
  node -e 'const r=()=>require("crypto").randomBytes(32).toString("hex");
    console.log("BETTER_AUTH_SECRET="+r()+"\nPAPERCLIP_TOOL_ACTION_SIGNING_SECRET="+r())' > "$inst/.env"
fi
# Houd de sandbox in leven als Paperclip herstart, en geef een stop door.
trap 'kill -TERM "$pid" 2>/dev/null; wait "$pid"; exit 0' TERM INT
while :; do
  "$@" & pid=$!
  wait "$pid" || true
  sleep 5
done
EOF
RUN chmod 0755 /usr/local/bin/wsm-paperclip

USER 1000:1000
WORKDIR /workspace
# wsm start de container al met --init, dus tini is hier niet nodig.
ENTRYPOINT ["/usr/local/bin/wsm-paperclip"]
CMD ["node", "/app/server/dist-runtime/index.mjs"]
```

**De herstart-lus is een keuze.**
- *Zonder lus* (`exec node …`): een crash van Paperclip stopt de hele sandbox (ADR 0007). De eigenaar start hem dan weer met `wsm sandbox start`.
- *Met lus:* het board blijft beschikbaar. Een agent kan de server dan wel steeds laten herstarten. Dat kan hij ook zonder lus, want hij draait als dezelfde gebruiker.

### 4.4 Configuratie: van `WSM_*` naar Paperclip

| Instelling | Waarde | Waarom |
|---|---|---|
| `HOST`, `PORT` | `0.0.0.0`, `3100` | De preview komt via pasta binnen, en wsm vraagt dat de app op `0.0.0.0` luistert. `3100` is gelijk aan `--app-port`. |
| `PAPERCLIP_DEPLOYMENT_MODE` en `…_EXPOSURE` | `authenticated` en `private` | `local_trusted` bindt alleen op loopback en heeft geen login. `public` is bedoeld voor een open internetadres en zet onder andere de claim van de eerste admin in de browser uit `[P: doc/DEPLOYMENT-MODES.md]`. Achter de passkey-poort van wsm past `private`, net als in Paperclips eigen Docker- en Quadlet-bestanden `[P: docker/quadlet/paperclip.container]`. |
| `PAPERCLIP_PUBLIC_URL` | `WSM_PREVIEW_URL` zonder de `/` aan het eind | Het is de basis-URL voor de login. De hostnaam komt daarmee vanzelf op de lijst met toegestane hostnamen `[P: server/src/config.ts]`. |
| `PAPERCLIP_API_URL` | `http://127.0.0.1:3100` | Zonder deze instelling krijgen agents de publieke URL als API-adres `[P: server/src/runtime-api.ts, server/src/index.ts]`. Die loopt via het publieke adres van de host, en ADR 0001 blokkeert dat. Bovendien wil de preview een cookie. |
| `PAPERCLIP_HOME` | `/home/agent/.paperclip` | Op het home-volume, zodat de data het verwijderen van de sandbox overleeft. |
| `PAPERCLIP_INSTANCE_ID` | `WSM_SANDBOX` | Sandboxes van één project delen `/home/agent`. Met een eigen ID per sandbox draaien twee instanties nooit op één PostgreSQL-datamap. Paperclip gebruikt het ID ook in de namen van de login-cookies `[P: doc/worktrees.md]`. Een sandboxnaam van wsm (`^[a-z]([a-z0-9-]{0,30}[a-z0-9])?$`, `internal/sandbox/sandbox.go`) is altijd een geldig ID (`^[a-zA-Z0-9_-]+$`) `[P: packages/shared/src/home-paths.ts]`. |
| `PAPERCLIP_DISABLE_CWD_ENV_FILE` | `true` | Anders laadt de server ook een `.env` uit zijn werkmap `[P: server/src/config.ts, server/src/env-file-policy.ts]`. Die werkmap is `/workspace`, waar agents schrijven en waar de app vaak een eigen `.env` heeft. Een `DATABASE_URL` daaruit zou Paperclip naar een andere database sturen. |
| `BETTER_AUTH_SECRET` en `PAPERCLIP_TOOL_ACTION_SIGNING_SECRET` | Eén keer willekeurig gemaakt, in `<instance>/.env` met mode 0600 | wsm heeft nog geen secret-store (fase 2). Paperclip leest het `.env`-bestand naast `config.json` `[P: server/src/config.ts, server/src/paths.ts]`. |
| `PAPERCLIP_TELEMETRY_DISABLED` en `DO_NOT_TRACK` | `1` | Telemetrie staat anders aan; dit zijn de twee uitschakelaars `[P: packages/shared/src/telemetry/config.ts]`. |
| `PAPERCLIP_AUTH_DISABLE_SIGN_UP` | `true`, zodra de eerste admin er is | Alleen de eigenaar komt bij de preview. Maar via loopback kan ook een agent zich registreren. |

**De hostname-controle van Paperclip** staat aan bij `private` `[P: server/src/app.ts]`. Ze laat drie soorten verzoeken door `[P: server/src/middleware/private-hostname-guard.ts]`:
- loopback-namen;
- de toegestane hostnamen;
- directe numerieke verzoeken naar `/api` op het eigen adres.

De preview-proxy van wsm houdt `Host` gelijk aan de preview-host en zet `X-Forwarded-Host` en `X-Forwarded-Proto: https`. Paperclip leest eerst `X-Forwarded-Host`. Beide kloppen dus.

### 4.5 Netwerk

| Van → naar | Pad | Welke regel | Resultaat |
|---|---|---|---|
| Browser → board | Caddy → preview-proxy → `127.0.0.1:47xxx` → pasta → `:3100` | Passkey en `__Host-wsm_preview` (ADR 0016, 0017) | Open na inloggen |
| Agent → Paperclip-API | `127.0.0.1:3100` in de pod | Blijft in de network namespace van de pod en raakt geen wsm-regel (ADR 0006) | Open, met de run-sleutel |
| Server → PostgreSQL | Loopback in de pod, `127.0.0.1:54329` `[P: server/src/config.ts]` | Idem | Open |
| Agent → LLM-API's (Anthropic, OpenAI, …) | Internet | Regel 4: open | Open |
| Server → npm, GitHub, `companies.sh` (plugins, skills, templates) | Internet | Open | Open |
| Server → `telemetry.paperclip.ing` | Internet | Open | Zet uit (§4.4) |
| Server → Discord-gateway (chat) | Internet, een uitgaande WebSocket | Regel 4: open | Open (U6) |
| Agent → wsm-MCP | `10.255.0.1:7443` | Regel 1 | Open, met bearer-token |
| Agent → MariaDB | `10.255.0.1:20000–20999` | Alleen na `service_enable` (ADR 0014) | Per project |
| Sandbox → andere sandbox of preview-poort | Loopback of publiek adres van de host | Regel 3 (`TestOtherProjectPreviewPort`) | Dicht |
| Sandbox → LAN, `169.254.169.254`, manager-API, Podman-socket | — | Regel 3, invariant 1 (`TestManagerAPINotReachable`, `TestNoPodmanSocketInSandbox`) | Dicht |
| Internet → Paperclip zonder wsm-login (webhooks) | Preview-proxy | Redirect of 401 (ADR 0017) | Dicht (U6) |

**WebSockets** gaan door de preview-proxy:
- `httputil.ReverseProxy` geeft `Upgrade` door.
- De preview-listener heeft geen `WriteTimeout`, alleen `ReadHeaderTimeout` (10 s) en `IdleTimeout` (2 min) (`cmd/wsm/serve.go`).
- Paperclip zet zijn keep-alive op 185 s "to outlive reverse-proxy idle timeouts" `[P: server/src/index.ts]`. De proxy sluit een ongebruikte verbinding naar de app al na 90 s (`IdleConnTimeout` in `internal/preview/preview.go`). Hij hergebruikt dus nooit een verbinding die Paperclip net sluit.
- De proxy wacht hoogstens 60 s op de response-headers van de app (`responseHeaderTimeout`). Het board en de WebSocket sturen hun headers meteen. Een API-call die langer rekent voordat hij antwoordt, krijgt een 502.

De spike moet de live updates via de preview bevestigen, en letten op API-calls die langer dan 60 s duren.

### 4.6 Inloggen, origins en cookies

- **Twee lagen:**
  1. de passkey van wsm geeft een dashboard-sessie;
  2. het dashboard geeft een eenmalig token aan de preview-host, die `__Host-wsm_preview` zet (12 uur, per host);
  3. daarna logt de eigenaar in bij Paperclip zelf (Better Auth, met e-mail en wachtwoord).
- **Cookies:** de preview-proxy haalt alleen zijn eigen cookie weg. De cookies van Paperclip gaan gewoon door en zijn host-only voor de preview-host.
- **De eerste admin:** zolang er geen instance-admin is, wint de eerste ingelogde browser die het board claimt `[P: doc/DEPLOYMENT-MODES.md §8]`. Een agent kan via loopback ook een account aanmaken en claimen. Twee manieren om dat voor te zijn:
  - direct na de eerste start `paperclipai auth bootstrap-ceo` draaien in `wsm shell`; dat geeft een eenmalige invite-URL;
  - of meteen in de browser "Claim this instance" kiezen.

  Doe het in elk geval vóórdat er agents draaien. Zet daarna `PAPERCLIP_AUTH_DISABLE_SIGN_UP=true`.
- **Invariant 10 blijft heel.**
  - De cookies van Paperclip staan op de preview-origin.
  - De dashboard-cookies hebben het `__Host-`-prefix.
  - De passkey hoort alleen bij de dashboard-host (ADR 0016).
- **Invariant 11** gaat over het wsm-dashboard. Paperclip toont agent-content (Markdown, HTML-artefacten in een `sandbox`-iframe zonder `allow-same-origin`) op zijn eigen preview-origin `[P: doc/html-artifact-previews.md]`. Dat raakt het dashboard niet.
- **Een bekende beperking:** cookie tossing tussen previews (ADR 0017, punt 8). Een andere preview-app kan een cookie voor het hele preview-domein zetten, en Paperclip ontvangt die. Dat is geaccepteerd zolang er één eigenaar is. Een apart registreerbaar preview-domein blijft het advies.

### 4.7 Data en levensduur

| Data | Plek | Na stop/start | Na het verwijderen van de sandbox | Na het verwijderen van het project |
|---|---|---|---|---|
| Paperclip-database (embedded PG) | `/home/agent/.paperclip/instances/<sandbox>/db` | blijft | blijft (home per project) | weg |
| Bijlagen en back-ups van de database | `…/instances/<sandbox>/data/storage` en `data/backups` | blijft | blijft | weg |
| Logs | `…/instances/<sandbox>/logs` | blijft | blijft | weg |
| Master key en `.env` met de sleutels | `…/instances/<sandbox>/secrets/master.key` en `.env` | blijft | blijft | weg |
| Werkmappen van agents zonder project, beheerde checkouts, Codex-homes per agent | `…/instances/<sandbox>/workspaces/<agent>`, `projects/…` en `companies/<company>/agents/<agent>/codex-home` | blijft | blijft | weg |
| Plugins | `/home/agent/.paperclip/plugins`, voor alle instanties van het project samen | blijft | blijft | weg |
| Logins van de agent-CLI's | `/home/agent/.claude`, `.codex`, … | blijft | blijft | weg |
| Code (repo en commits) en de git-worktrees van Paperclip | `/workspace` en `/workspace/.paperclip/worktrees/<branch>` | blijft | **weg** | weg |
| Schrijflaag van de container (`/tmp`, …) | de container | blijft | weg | weg |
| Paperclip zelf | het image (alleen-lezen voor UID 1000) | — | — | — |

**Let op:**
- **Nooit twee processen op één datamap.** Twee Paperclip-processen op dezelfde datamap betekent corrupte PostgreSQL-data. Een eigen `PAPERCLIP_INSTANCE_ID` per sandbox voorkomt dat. Twee sandboxes van één project zijn daarmee twee losse Paperclip-instanties.
- **De back-ups zijn geen echte back-up.** Paperclip zet zijn automatische database-back-ups op hetzelfde volume. Een echte back-up gaat buiten de sandbox om, met een export van het home-volume als projectgebruiker. Dat is handwerk.
- **Hergebruik van de data.** Na het verwijderen van `acme/hq` vindt een nieuwe sandbox met dezelfde naam (`acme/hq`) de Paperclip-data terug, maar niet de code en niet de worktrees.
- **Plugins zijn per project, niet per instantie.** Paperclip installeert ze met `npm install --ignore-scripts` in `~/.paperclip/plugins` `[P: server/src/services/plugin-loader.ts]`. Alle sandboxes van het project delen die map.

### 4.8 Code, workspaces en git

- **De code komt van wsm.** Met `--repo` vult wsm bij de eerste start `/workspace` (ADR 0010):
  - de historie van de basisbranch;
  - de branch `agent/<sandbox>`;
  - geen remote en geen credentials.
- **De projectworkspace in Paperclip is het lokale pad `/workspace`, niet een repo-URL.**
  - Bij een workspace met alleen een repo-URL clonet en fetcht de Paperclip-server zelf. Voor privérepo's gebruikt hij dan een GitHub-token uit de secrets of de omgeving `[P: docs/guides/board-operator/execution-workspaces-and-runtime-services.md]`.
  - Dat zou een git-credential in de sandbox brengen, en dat sluit de eis "In de sandbox staan geen git-credentials" uit.
- **Execution-workspaces** (`isolated_workspace` met `git_worktree`) maken worktrees van `/workspace`, binnen de sandbox.
  - Ze staan standaard in `/workspace/.paperclip/worktrees/<branch>` (instelbaar met `worktreeParentDir`) `[P: server/src/services/workspace-runtime.ts]`. Ze verdwijnen dus met de sandbox.
  - Paperclip draait daar zelf git. Dat mag: invariant 3 gaat over de manager op de host, en in de sandbox is alles al terrein van de agent.
  - De issue-branches zijn gewone lokale branches, met `agent/<sandbox>` als basis.
- **Een agent zonder projectworkspace** werkt in `…/instances/<sandbox>/workspaces/<agent-id>` op het home-volume `[P: server/src/home-paths.ts]`.
- **Er komt geen nieuw werk van boven binnen.** `/workspace` heeft geen remote, dus nieuwe commits upstream komen alleen in een nieuwe sandbox (een nieuwe seed).
- **Code naar buiten** kan in fase 1 niet (U5).

### 4.9 Agents, harnassen en credentials

- **Lokale adapters** zijn child processes van de server, in dezelfde container en als UID 1000.
  - Ze krijgen de omgeving van de server mee, dus ook `WSM_MCP_TOKEN`. Alleen de `PAPERCLIP_*`-variabelen haalt Paperclip eruit (§1.2).
  - Per run zet Paperclip daar `PAPERCLIP_API_URL` en `PAPERCLIP_API_KEY` bij `[P: packages/adapter-utils/src/server-utils.ts]`. Een expliciete `PAPERCLIP_API_URL` van de server gaat voor, dus agents krijgen `127.0.0.1:3100`.
  - De run-sleutel is een JWT die standaard 48 uur geldig is (`PAPERCLIP_AGENT_JWT_TTL_SECONDS`). Paperclip tekent hem met `PAPERCLIP_AGENT_JWT_SECRET`, of anders met `BETTER_AUTH_SECRET` `[P: server/src/agent-auth-jwt.ts]`. In dit ontwerp is dat dus het geheim uit `<instance>/.env`.
- **`--dangerously-skip-permissions`** staat standaard aan voor `claude_local`. In wsm is dat aanvaardbaar.
  - De grens is wsm, niet de permissievragen van het harnas.
  - De eisen zeggen dit al: de harnasregels staan in de projecthome, waar de agent kan schrijven.
- **LLM-credentials** kunnen op twee manieren:
  1. **Abonnementslogin:** `claude login` of `codex login` in `wsm shell`. Die belandt in `/home/agent`, gedeeld per project. Dat volgt de eis "Harnas-home (met login) per project". Eén login geldt voor alle agents:
     - `claude_local` gebruikt de gedeelde `~/.claude`, of `CLAUDE_CONFIG_DIR` als die gezet is `[P: packages/adapters/claude-local/src/server/claude-config.ts]`;
     - `codex_local` geeft elke agent een eigen `CODEX_HOME` onder de instantie. Paperclip kopieert daar `config.toml`, `config.json` en `instructions.md` uit `~/.codex` in, en maakt van `auth.json` een symlink `[P: packages/adapters/codex-local/src/server/codex-home.ts]`.
  2. **API-sleutels als company-secret in Paperclip:** versleuteld in de database, met de master key op hetzelfde volume.

  Bij beide kan elke agent in de sandbox ze lezen; ook de secret-store van wsm (fase 2) verandert dat niet. Met `ANTHROPIC_API_KEY` in de omgeving gebruikt Claude API-billing in plaats van het abonnement `[P: docs/agents-runtime.md §8]`.
- **Tests** gebruiken nooit echte sleutels, maar het gescripte nepmodel uit spike 03 (ADR 0011, punt 7):
  - voor Claude Code `ANTHROPIC_BASE_URL` met `ENABLE_TOOL_SEARCH=false`;
  - voor Codex een eigen `model_provider`.

### 4.10 Services via de wsm-MCP

- **Eén identiteit:** alle agents in de sandbox delen de MCP-identiteit van de sandbox (U4).
- **Configuratie van de client:** een `.mcp.json` in `/workspace` of `claude mcp add` in de home. Beide verwijzen naar `${WSM_MCP_URL}` en `Bearer ${WSM_MCP_TOKEN}` en kopiëren het token niet (ADR 0011, punt 6).
- **Codex** vult geen variabelen in de URL in (open punt in ADR 0012). Omdat de home per project gedeeld is, past één vaste URL niet bij alle sandboxes. Paperclip kopieert `~/.codex/config.toml` naar de `CODEX_HOME` van elke Codex-agent (§4.9), dus daar geldt hetzelfde.
- **Een database voor de app die de agents bouwen:** `service_enable mariadb` geeft een eigen account per sandbox.
- **Paperclips eigen database blijft embedded.** Een PostgreSQL-blok in wsm komt pas met "overige engines" in fase 2. Dan kan `DATABASE_URL`, maar het helpt hier niet:
  - de credentials staan dan toch in dezelfde sandbox, dus het levert geen isolatie op;
  - het verwijderen van een sandbox gooit zijn databases met prefix weg (invariant 7), en dan gaat de Paperclip-data mee.

  Embedded op het home-volume past beter.

### 4.11 Resources en limieten

- **De standaard is te krap.** Dat is 2 CPU's, 2 GiB en 1024 pids per sandbox.
  - De server en PostgreSQL kosten samen al een flink deel van het geheugen.
  - Elke gelijktijdige Claude Code- of Codex-run kost honderden MB's, plus child processes (node, ripgrep, git, MCP-servers).
- **Een startpunt** dat de spike moet meten: `--cpus 4 --memory 8g --pids 4096`. Beperk daarnaast in Paperclip hoeveel runs er tegelijk lopen.
- **Kies ruim.** De limieten liggen vast na `project create`; er is geen commando om ze te wijzigen.
- **Zombies:** Paperclip waarschuwt dat zonder init de zombieprocessen van agent-runs zich ophopen tot de pids-limiet op is `[P: Dockerfile]`. wsm start de werkcontainer altijd met `--init`, dus dat is gedekt.
- **Schijf:** een quotum per sandbox is er nog niet. Run-logs en database-back-ups groeien op het home-volume.

### 4.12 Beheer

**De eerste start:**

```sh
wsm project create --slug acme \
  --containerfile examples/paperclip/Containerfile \
  --repo git@github.com:acme/product.git --branch main \
  --cpus 4 --memory 8g --pids 4096 --app-port 3100
wsm project key acme                      # read-only deploy key toevoegen
wsm project block set acme mariadb --tag 11.8   # optioneel, voor de app van de agents
wsm sandbox create acme/hq
wsm sandbox start acme/hq
wsm shell acme/hq   # eenmalig: paperclipai auth bootstrap-ceo, claude login, codex login
```

Open daarna de preview vanuit het dashboard, neem de invite aan en richt de company en de agents in. Zet de projectworkspace op `/workspace`.

**Verder:**
- **Na een herstart van de host** starten de pods niet vanzelf (open punt in `briefs/aandachtspunten.md`). Paperclip is dan weg tot `wsm sandbox start`.
- **Bijwerken** kan in fase 1 alleen met een nieuw project of een nieuwe sandbox (U9).
- **Back-up:** zie §4.7.

### 4.13 Wat wsm hiervoor moet veranderen

**Niets verplicht.** Alles hierboven gebruikt wat er al is.

Al geplande verbeteringen die dit ontwerp beter maken:
- een secret-store (fase 2);
- de app-poort, de limieten en de Containerfile van een project kunnen wijzigen;
- sandboxes opnieuw aanmaken met behoud van code (aanname 9);
- pods automatisch starten na een herstart van de host;
- `/mcp` accepteren met de identiteit alleen uit het token, voor Codex (open punt in ADR 0012).

## 5. Uitzonderingen: wat niet lukt, en waarom

Elke uitzondering hieronder beschrijft:
- wat Paperclip wil;
- waarom dat niet kan;
- waar het botst.

Oplossingen zijn bewust nog niet onderzocht.

### U1. Paperclip als orkestrator van andere wsm-sandboxes

**Wat Paperclip wil.** Een run elders laten draaien:
- in een `ssh`-omgeving;
- via een `sandbox`-provider (Daytona, E2B, Kubernetes, Modal, Cloudflare, …);
- via een `plugin`-omgeving;
- via de Runner.

Een provider-plugin implementeert daarvoor onder meer `[P: doc/plugins/SANDBOX_PROVIDER_CAPABILITIES.md]`:
- `environmentAcquireLease`: een sandbox maken;
- `environmentExecute`: er commando's in draaien;
- `environmentSyncIn` en `environmentSyncOut`: bestanden erin en eruit;
- `environmentReleaseLease` en `environmentDestroyLease`: hem opruimen.

Met wsm als provider zou de Paperclip-server in sandbox P per agent of per issue een eigen wsm-sandbox maken.

**Waarom het niet kan:**
- **Aanmaken en verwijderen is van de eigenaar.** Sandboxes aanmaken en verwijderen mag alleen de eigenaar (aanname 4). Dat gaat via de API-socket (groep `wsm-admin`, ADR 0005) of het dashboard, en een sandbox komt nooit bij de API-socket (`TestManagerAPINotReachable`).
- **De MCP heeft geen beheertools.** Hij kent alleen de eigen sandbox (invariant 2). Een tool om een sandbox te maken of in een andere sandbox iets te draaien, is precies zo'n beheertool.
- **Draaien en bestanden kopiëren in een andere sandbox vraagt Podman-toegang.**
  - Dat is de Podman van dat project. Die toegang heeft alleen de manager (ADR 0002, groep `wsm`).
  - In een sandbox komt nooit een Podman-socket (invariant 1).
- **Het netwerk staat het niet toe.**
  - Verkeer van sandbox naar sandbox is dicht (ADR 0001, regel 3), en een preview vraagt de wsm-cookie.
  - Dus ook de omgekeerde vorm werkt niet: een runner in sandbox S die P peilt, of de `http`-adapter.
- **SSH:** sandboxes hebben geen sshd en geen inkomend pad. Het LAN is dicht.

**Gevolgen:**
- Alle agents draaien in de sandbox van de server.
- Scheiding per agent op OS-niveau is er niet.

**Let op, het internet staat open.** Twee sandboxes kunnen elkaar via een publieke tunneldienst bereiken. Dat omzeilt de scheiding die wsm bewust aanbrengt, en het zet Paperclips API op het internet. Dat valt buiten dit ontwerp.

**Wel opvallend:** Paperclip verplaatst code naar runs op afstand al als git bundle, zonder remote ("no-remote-git contract"). Dat is hetzelfde idee als in wsm. Wat ontbreekt is dus de bevoegdheid, niet het mechanisme.

### U2. Paperclips insluiting van vijandige invoer (`low_trust_review`)

**Wat Paperclip wil.** Een preset voor werk op onbetrouwbare invoer, zoals externe pull requests, tickets of dependency-diffs. Paperclip weigert zo'n run, tenzij `[P: doc/LOW-TRUST-PRESETS.md, Runtime Containment]`:
- de omgeving de `sandbox`-driver gebruikt;
- de workspace `isolated_workspace` is.

**Waarom het niet kan.** Er is geen `sandbox`-driver (U1). Zulke runs starten dus niet.

**Gevolg.** De insluiting is dan alleen die van wsm, per project. Zet werk op vijandige invoer daarom in een eigen wsm-project met een eigen Paperclip, zonder waardevolle credentials of data.

### U3. Scheiding tussen agents en tussen companies binnen één instantie

**Wat Paperclip biedt:**
- rollen, goedkeuringen en budgetten;
- data per company;
- agent-sleutels per agent;
- in `local_trusted` een controle op de procesboom.

**Waarom dat binnen de sandbox geen grens is.** Alles draait als UID 1000 in één container. Een agent kan:
- de PostgreSQL-bestanden lezen, en met de database verbinden. De ingebouwde PostgreSQL luistert op `127.0.0.1:54329` met de vaste login `paperclip`/`paperclip` `[P: server/src/index.ts]`;
- de master key en het `.env`-bestand met `BETTER_AUTH_SECRET` lezen. Dat hoeft niet eens, want hij erft `BETTER_AUTH_SECRET` uit de omgeving van de server (§1.2). Daarmee tekent hij zelf run-sleutels voor elke agent (§4.9);
- `/proc/<pid>/environ` van de server lezen;
- de server stoppen of vervangen.

Paperclip zegt zelf dat het binnen een sandbox niets beschermt, en dat lokale adapters zonder sandbox draaien. Paperclips optionele Bubblewrap-opsluiting per run zou hier iets kunnen scheiden, maar werkt in een wsm-container waarschijnlijk niet (U8).

**Waar het botst.** Niet met een invariant van wsm, wel met een verwachting die je zou kunnen hebben. De eis "bij een prompt-injectie reikt de schade nooit verder dan het eigen project" blijft waar. Binnen het project ligt alles open.

**Gevolgen:**
- Neem één company per wsm-project.
- Zie de governance van Paperclip als werkproces, niet als beveiliging.
- Budgetten van Paperclip zijn omzeilbaar. De echte limiet zit bij de LLM-aanbieder, als bestedingslimiet op de sleutel of het abonnement.

### U4. Een wsm-identiteit per Paperclip-agent

**Wat je zou willen.**
- Agent A krijgt wel een database, agent B niet.
- De audit log laat zien welke agent wat deed.

**Waarom het niet kan:**
- De identiteit komt alleen uit het MCP-token per sandbox (invariant 2), en alle agents erven dat token.
- Grants gelden per sandbox (ADR 0014). Netwerktoegang geldt per project-UID (ADR 0001).
- De audit noteert `mcp:<sandbox-id>` (ADR 0012).

**Gevolgen:**
- wsm kan geen least privilege per agent afdwingen.
- Wie wat deed, is alleen te herleiden door tijden te vergelijken met de run-log van Paperclip, en daar kunnen agents in schrijven.

### U5. Code naar buiten: push, pull requests en merges

**Wat Paperclip wil.** Agents die zelf code naar buiten brengen:
- branches pushen;
- PR's openen en mergen, met `gh` in het image en een GitHub-token als secret;
- de PR-merge-review-webhook gebruiken.

**Waarom het niet kan:**
- In de sandbox staan geen git-credentials (eisen, "Git, review en signing"; ADR 0009, 0010; `TestNoCredentialsInSandbox`).
- Code gaat alleen als bundle naar buiten, via de manager, met review en hersigneren. Dat is fase 2 en bestaat nog niet.

**Technisch kan het wel.** Het internet staat open, en niets houdt een token in de home of in de Paperclip-secrets tegen. Maar dat gaat in tegen de eisen en het model van review en ondertekening, en hoort daarom niet in dit ontwerp.

**Gevolg.** In fase 1 blijft het werk in de sandbox (zie ook U9).

### U6. Inkomende webhooks en chatkoppelingen

**Wat Paperclip wil.** Verzoeken die van buiten naar Paperclip komen:
- chatkoppelingen via `POST /api/chat-webhooks/<id>/<provider>`: Slack, Telegram, Microsoft Teams en GitHub `[P: server/src/routes/chat-channels.ts]`. Voor Slack noemt Paperclip "webhook ingress that Slack can reach" als voorwaarde `[P: doc/chat-connector-development.md]`;
- e-mail via AgentMail en spraak, ook via webhooks `[P: server/src/routes/email.ts, server/src/routes/voice-sessions.ts]`;
- webhooks van plugins (`…/plugins/<plugin>/webhooks/<endpoint>`) `[P: server/src/routes/plugins.ts]`;
- PR-webhooks en routines met een webhook-trigger.

**Waarom het niet kan.**
- De enige weg naar binnen is de preview-proxy.
- Zonder geldige `__Host-wsm_preview`-cookie krijgt een verzoek anders dan GET een 401, en een GET een redirect naar het dashboard (ADR 0017).
- Een externe dienst heeft geen passkey.

**Wat wel werkt.** Koppelingen die zelf een verbinding naar buiten openhouden, want uitgaand internet staat open. Paperclip houdt voor twee chatdiensten zo'n eigen ontvanger in de server `[P: server/src/services/chat-channels/runtime/receiver-policy.ts]`:
- Discord, via de gateway van discord.js: een uitgaande WebSocket `[P: server/src/services/chat-sdk-runtime.ts]`;
- iMessage via Photon. De spike moet bevestigen dat die ontvanger alleen naar buiten verbindt.

### U7. Previews van wat de agents bouwen

**Wat Paperclip wil.** Dev-servers per workspace starten (`port: auto`) en linken via `expose.urlTemplate`, bijvoorbeeld `https://{{workspace.branchName}}.dev.example.com` `[P: docs/guides/board-operator/execution-workspaces-and-runtime-services.md]`.

**Waarom het niet kan:**
- wsm heeft één app-poort per project en één preview-host per sandbox (ADR 0017, punt 2).
- In de hoofdkantoor-sandbox is die poort het board van Paperclip.
- Andere poorten in de pod zijn niet bereikbaar vanuit de browser.
- De app-poort ligt vast bij `project create`.

**Gevolg.**
- Agents kunnen hun app binnen de sandbox draaien en testen, bijvoorbeeld met curl of een headless browser.
- De eigenaar kan de app niet in de browser openen.
- De HTML-artefacten van Paperclip werken wel; dat zijn statische pagina's in een sandboxed iframe.

### U8. Functies die rechten of een container-runtime in de sandbox vragen

- **Tailscale** (in Paperclips image, `bind=tailnet`):
  - vraagt een TUN-device en `NET_ADMIN`, en die zijn weg door `cap-drop ALL`;
  - de userspace-modus zou een tweede inkomend pad openen dat wsm niet beheert. Binnenkomen gaat alleen via de preview (ADR 0017).
- **Docker- of Podman-flows:**
  - het betreft de review van onbetrouwbare PR's via `docker compose`, de Kubernetes-provider en de agent-runtime-images;
  - er is geen container-runtime en geen socket in de sandbox (invariant 1);
  - geneste rootless Podman vraagt user namespaces en rechten die `cap-drop ALL`, `no-new-privileges` en seccomp blokkeren.
- **Paperclips eigen opsluiting per run** (`filesystemScope: "workspace"` en `networkScope` bij `claude_local` en `codex_local`, standaard uit) `[P: packages/adapters/claude-local/src/index.ts, packages/adapter-utils/src/local-process-sandbox.ts]`:
  - Paperclip start de CLI dan in Bubblewrap, met eigen pid-, ipc- en uts-namespaces, een nieuwe `/proc` en bij `networkScope` een eigen netwerk;
  - `bwrap` zit niet in Paperclips image `[P: Dockerfile]`;
  - zonder root maakt `bwrap` daarvoor een geneste user namespace en mount hij een nieuwe `/proc`. Podman schermt delen van `/proc` af, en dan weigert de kernel zo'n mount meestal. Ook het seccomp-profiel kan het tegenhouden;
  - het werkt dus waarschijnlijk alleen als we de hardening van de werkcontainer verzwakken (invariant 9). De spike moet bevestigen dat het nu faalt.
- **Software installeren tijdens het draaien:** er is geen root in de container (plan 3.4). Installeer alles in het image.

### U9. Paperclip bijwerken

**Wat Paperclip wil.** Paperclip brengt vaak nieuwe versies uit, met CalVer-tags als `v2026.1023.0` en daarnaast canaries.

**Waarom het nu niet kan:**
- Een project is na het aanmaken niet meer te wijzigen, ook de Containerfile niet (`docs/guide/images.md`, `docs/reference/cli.md`).
- `wsm project image` bouwt dezelfde Containerfile opnieuw.
- Bestaande sandboxes houden hun image (ADR 0008, punt 8).
- Aanname 9 (sandboxes opnieuw aanmaken met behoud van code) is nog niet gebouwd.

**Gevolgen:**
- Bijwerken kan alleen met een nieuw project, en dan moet de data mee via de export en import van companies in Paperclip.
- Een nieuwe sandbox met dezelfde naam houdt de Paperclip-data, maar verliest `/workspace`.
- Paperclip in de home installeren (`npm -g`) maakt bijwerken ter plekke mogelijk. Dat kost reproduceerbaarheid en geeft een orkestrator die agents kunnen aanpassen.

## 6. Risico's binnen het ontwerp

- **Eén geïnjecteerde agent raakt de hele instantie:** de database, de sleutels, de LLM-logins, het MCP-token en alle code van het project. Het blijft wel binnen het project.
- **Exfiltratie:** het internet staat open, dus een geïnjecteerde agent kan data naar buiten sturen. Dat is een bekende kanttekening in de eisen.
- **Kosten:** de budgetten van Paperclip zijn in de sandbox te omzeilen (U3). Zet een bestedingslimiet bij de LLM-aanbieder.
- **Gedeelde logins:** de LLM-logins staan in de home van het project, en elke sandbox van het project kan ze lezen. Dat is in wsm altijd zo.
- **Twee processen op één datamap:** zie §4.7.
- **De race om de eerste admin:** zie §4.6.
- **Telemetrie** staat standaard aan: zie §4.4.
- **Resources:** pids, geheugen, schijf en de groei van de run-logs (§4.11).
- **Lange API-calls:** de preview-proxy geeft een 502 als de app niet binnen 60 s antwoordt (§4.5).
- **Een groot image:** een flinke eerste pull per project; dat lost route B op.

## 7. Voorstel: zo pakken we het op, binnen het stramien

1. **Spike `spikes/05-paperclip/`.**
   - Wegwerpcode, met een `FINDINGS.md`.
   - Op een wegwerp-VM van `debian-13-public` via test-on-vm.
   - Met het nepmodel en nooit met echte sleutels.

   **Klaar als:**
   - het image bouwt via `wsm project create --containerfile` (de context bevat alleen de Containerfile);
   - Paperclip start als `1000:1000` zonder capabilities, en de embedded PostgreSQL start binnen de pids- en geheugenlimieten;
   - het board werkt via de preview, met login en live updates over WebSocket, en is zonder wsm-login onbereikbaar;
   - een heartbeat van een `claude_local`-agent met het nepmodel draait in een worktree van `/workspace`, roept de API aan via `127.0.0.1` en schakelt via de MCP MariaDB in;
   - een tweede sandbox van hetzelfde project draait een eigen instantie naast de eerste;
   - stop/start en verwijderen plus opnieuw aanmaken gedragen zich zoals tabel 4.7 zegt;
   - er gaat geen verkeer naar `telemetry.paperclip.ing`;
   - de server leest geen `.env` uit `/workspace` (`PAPERCLIP_DISABLE_CWD_ENV_FILE`);
   - het geheugen, de pids en de schijf per gelijktijdige run zijn gemeten.
2. **Daarna:**
   - een ADR, bijvoorbeeld "Paperclip in a sandbox";
   - `examples/paperclip/Containerfile`;
   - een pagina in de docs.
3. **Isolatietests.** De bestaande tests dekken de grenzen al: `TestPreview`, `TestOtherProjectPreviewPort`, `TestManagerAPINotReachable`, `TestNoPodmanSocketInSandbox` en `TestContainerHardening`. Een run met het Paperclip-image als testimage laat zien dat ze ook voor dit image gelden.
4. **De uitzonderingen U1–U9** zijn input voor besluiten in fase 2 en later. Nu bouwen we er niets voor.

## 8. Open vragen voor de spike

- **De verbinding via pasta:** komt een preview-verbinding binnen als loopback of als extern adres? Dat bepaalt of `TRUST_PROXY` nodig is, en hoe de process-boundary-controle van Paperclip zich gedraagt.
- **Cookies:** zet Better Auth achter de proxy Secure-cookies met alleen een expliciete `https`-`PAPERCLIP_PUBLIC_URL`?
- **Inloggen zonder browser:** werken `claude login` en `codex login` zonder lokale browser (met een code of device-flow) in `wsm shell`?
- **PostgreSQL in de container:** draait PostgreSQL 18 met de standaard `/dev/shm` van 64 MB, `cap-drop ALL` en de pids-limiet?
- **Chatkoppelingen:** werkt de Discord-gateway vanuit de sandbox, en verbindt de Photon-ontvanger alleen naar buiten (U6)?
- **Bubblewrap:** faalt `filesystemScope` in de container zoals verwacht, en met welke fout (U8)?
- **Route B:** werkt `npm install -g paperclipai` met `paperclipai run` als kleiner image, en welke npm-versie hoort bij welke commit?

Al beantwoord uit de broncode, en verwerkt in §4.7–§4.9 en U3: waar de worktrees staan, hoe de agent-CLI's hun login vinden, en welke server-variabelen agents erven.

## Bronnen

**wsm**
- `docs/plan/eisen-workspace-manager.md` en `docs/plan/bouwplan-fase-1.md`.
- ADR 0001 (egress), 0002 (Podman-sockets), 0005 (helper en API), 0006 (isolatietests), 0007 (containercommando), 0008 (images), 0009 en 0010 (git), 0011 en 0012 (MCP), 0013, 0014 en 0015 (services), 0016 (dashboard-login) en 0017 (previews).
- `docs/guide/images.md`, `docs/reference/cli.md`, `docs/dashboard.md` en `briefs/aandachtspunten.md`.
- Code: `internal/sandbox/sandbox.go` (`WorkSpec`, `PodSpec`, `workEnv`, `slugPattern`), `internal/preview/preview.go` (de timeouts van de proxy), `cmd/wsm/serve.go` en `internal/netpolicy/netpolicy.go`.

**Paperclip**, commit `7671e0cf0d718351f3b0c4d6a7d33df9867ffb8a`. Basis-URL: `https://github.com/paperclipai/paperclip/blob/7671e0cf0d718351f3b0c4d6a7d33df9867ffb8a/`.
- `Dockerfile`: de `production`-stage met de CLI's, `ENV`, `ENTRYPOINT` en `CMD`.
- `scripts/docker-entrypoint.sh`: het pad zonder root.
- `docker/quadlet/paperclip.container`, `paperclip.pod` en `paperclip-db.container`.
- `doc/DEPLOYMENT-MODES.md`: de modi, de eerste admin en `bootstrap-ceo`.
- `doc/DATABASE.md` en `server/package.json`: `embedded-postgres` `^18.1.0-beta.16`.
- `doc/DOCKER.md`: het gepubliceerde image en de twee sleutels.
- `docs/agents-runtime.md`: heartbeats, de adapters en "Local CLI adapters run unsandboxed on the host machine".
- `server/src/config.ts`: `PAPERCLIP_PUBLIC_URL`, `PAPERCLIP_ALLOWED_HOSTNAMES`, `PAPERCLIP_AUTH_DISABLE_SIGN_UP`, de `.env`-bestanden en de PostgreSQL-poort 54329.
- `server/src/env-file-policy.ts`: `PAPERCLIP_DISABLE_CWD_ENV_FILE`.
- `server/src/app.ts`: `shouldEnablePrivateHostnameGuard`.
- `server/src/middleware/private-hostname-guard.ts`.
- `server/src/runtime-api.ts`: `choosePrimaryRuntimeApiUrl`.
- `server/src/index.ts`: de override met `PAPERCLIP_API_URL`, de keep-alive en de start van de ingebouwde PostgreSQL (poort en login).
- `packages/shared/src/home-paths.ts` en `server/src/home-paths.ts`: de mappen per instantie en de regel voor `PAPERCLIP_INSTANCE_ID`.
- `server/src/services/workspace-runtime.ts`: de plek van de git-worktrees.
- `server/src/services/plugin-loader.ts`: de pluginmap en de installatie met npm.
- `server/src/agent-auth-jwt.ts`: de run-sleutel.
- `packages/adapter-utils/src/server-utils.ts`: `runChildProcess`, `sanitizeInheritedPaperclipEnv` en `PAPERCLIP_API_URL` per run.
- `packages/adapter-utils/src/local-process-sandbox.ts` en `packages/adapters/claude-local/src/index.ts`: de Bubblewrap-opsluiting.
- `packages/adapters/claude-local/src/server/claude-config.ts` en `packages/adapters/codex-local/src/server/codex-home.ts`: hoe de CLI's hun login vinden.
- `server/src/routes/chat-channels.ts`, `email.ts`, `voice-sessions.ts` en `plugins.ts`: de webhook-routes.
- `server/src/services/chat-channels/runtime/receiver-policy.ts` en `server/src/services/chat-sdk-runtime.ts`: de eigen ontvangers voor Discord en Photon.
- `doc/chat-connector-development.md`: Slack vraagt inkomende webhooks.
- `cli/package.json` en `cli/src/index.ts`: het npm-pakket `paperclipai` en `paperclipai run`.
- `packages/adapters/claude-local/src/server/execute.ts`: `dangerouslySkipPermissions` en `effectiveEnv`.
- `packages/shared/src/constants.ts`: `ENVIRONMENT_DRIVERS`.
- `packages/plugins/sandbox-providers/SANDBOX-REQUIREMENTS.md`.
- `doc/plugins/SANDBOX_PROVIDER_CAPABILITIES.md`.
- `doc/LOW-TRUST-PRESETS.md`.
- `docs/guides/board-operator/execution-workspaces-and-runtime-services.md`.
- `doc/html-artifact-previews.md`.
- `packages/shared/src/telemetry/config.ts` en `client.ts`.
- Website en documentatie: https://paperclip.ing en https://paperclip.ing/docs.
