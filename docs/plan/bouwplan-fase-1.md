# Bouwplan fase 1: Development Workspace Manager

Status: concept, 1 november 2026. Gebaseerd op `eisen-workspace-manager.md` en `onderzoek-bestaande-tools.md`; in de repo staan alle drie in `docs/plan/`.
Werknaam in dit plan: `wsm`. Die vervangen we zodra de echte naam vastligt.

## 1. Doel van fase 1
Eén werkende doorsnede van begin tot eind, vanuit de browser:
1. Inloggen met een passkey.
2. Een project aanmaken: repo-URL, Containerfile of registry-image, app-poort, en de blokken MariaDB (gedeeld) en Redis (per sandbox).
3. Een sandbox aanmaken op een eigen branch.
4. In de webterminal een harnas starten, bijvoorbeeld Claude Code uit het projectimage.
5. De agent zet via de MCP MariaDB aan, krijgt een eigen account en database, en draait migraties.
6. De app openen op het preview-adres van de sandbox.
7. In het dashboard alle MCP-calls teruglezen.
8. Alle isolatietests zijn groen.

**Niet in fase 1** (dat volgt in fase 2 en 3):
- goedkeuringsverzoeken en meldingen;
- push, hersigneren en review;
- secret-store;
- TOTP en step-up;
- overige engines;
- tools voor status, reset en inspectie;
- auto-stop en een maximum aantal sandboxes;
- gedeelde Containerfile-bibliotheek en import/export.

Eenvoudige CPU- en RAM-limieten per sandbox zitten er wél al in. Dat is één vlag per container, en het voorkomt dat een ontspoorde agent de server platlegt.

## 2. Stack
- **Backend:** Go (één module, de actuele stabiele Go-versie).
- **Frontend:** Vue 3 + TypeScript + Vite, met Monaco en xterm.js. Wordt gebouwd naar statische bestanden en met `go:embed` in de binary opgenomen. Vue is een voorstel; React of Svelte kan ook.
- **Opslag:** SQLite met de pure-Go-driver `modernc.org/sqlite` (geen cgo nodig) en migraties via goose.
- **Bibliotheken:**
  - MCP: de officiële `github.com/modelcontextprotocol/go-sdk` (Streamable HTTP);
  - passkeys: `github.com/go-webauthn/webauthn`;
  - Podman: de officiële bindings `github.com/containers/podman/v5/pkg/bindings` (te valideren in spike 2, met als alternatief een dunne eigen REST-client);
  - WebSocket: `github.com/coder/websocket`;
  - MariaDB: `github.com/go-sql-driver/mysql`;
  - HTTP: `net/http` uit de standaardbibliotheek.
- **Uitgangspunt:** zo weinig mogelijk afhankelijkheden, en de standaardbibliotheek waar het kan.

## 3. Architectuur

### 3.1 Processen en Linux-gebruikers
| Proces | Draait als | Taak |
|---|---|---|
| `wsm-helper` | root | Het enige geprivilegieerde onderdeel: projectgebruikers aanmaken en verwijderen, de interface `wsm0` beheren en nftables-regels toepassen. Luistert op `/run/wsm/helper.sock` en accepteert alleen gebruiker `wsm` (gecontroleerd via peer-credentials). Neemt nooit paden of commando's aan, alleen gevalideerde ID's en regels. |
| `wsm serve` | `wsm` | De manager: API, dashboard, MCP-server, terminal-bridge, preview-proxy, git-mirror en broker. De CLI praat met de manager via `/run/wsm/api.sock` (groep `wsm-admin`). |
| Gedeelde services | `wsm-svc` | Rootless Podman met Quadlet-units. In fase 1 alleen MariaDB. |
| Sandboxes | `wsm-p-<id>` (één per project) | Rootless Podman; elke sandbox is een pod. |
| Caddy | `caddy` | TLS en doorsturen naar de manager op `127.0.0.1`. |

Projectgebruikers krijgen UID's uit een vaste reeks (bijv. 50000–59999). Zo dekt één set nftables-regels de hele reeks.
Elke projectgebruiker heeft een eigen Podman-socket. De manager krijgt daar toegang toe via groep `wsm` (met tmpfiles.d en een override van de socket-unit). Spike 2 bevestigt de precieze aanpak.

### 3.2 Domeinen en origins
- `dash.<domein>`: dashboard, API en passkeys.
- `<sandbox>--<project>.<preview-domein>`: app-preview. Door het `--`-scheidingsteken dekt één wildcard-certificaat alle previews.
- **Advies:** gebruik voor previews een apart registreerbaar domein, zoals GitHub dat doet met githubusercontent.com. Dan kan content uit een sandbox nooit cookies van het dashboard raken.
- **Kan dat niet,** gebruik dan dashboard-cookies met `__Host-`-prefix (een subdomein kan die niet overschrijven) en zet de passkey-RP-ID exact op de dashboard-host. Een preview-origin kan de passkey dan niet aanspreken.

### 3.3 Netwerk (de kern van de isolatie)
- `wsm0` is een dummy-interface op de host met `10.255.0.1/32` (het adres is instelbaar).
- Gedeelde services publiceren alleen op `10.255.0.1:<poort>`.
- De MCP-server luistert op `10.255.0.1:7443`.
- Voor de preview publiceert elke sandbox-pod zijn app-poort op `127.0.0.1:<vrije poort>`. Alleen de manager maakt daar verbinding mee.
- Rootless containers (via pasta) maken uitgaande verbindingen via de host, als de projectgebruiker. Daarom filtert nftables op UID (`meta skuid`), in een eigen tabel `inet wsm`:
  1. **Altijd toegestaan:** de DNS-resolver van de host en `10.255.0.1:7443` (de MCP).
  2. **Per project toegestaan na `service_enable`:** `10.255.0.1:<poort van die service>`.
  3. **Geblokkeerd voor de hele project-UID-reeks:**
     - loopback (127.0.0.0/8, ::1);
     - privé-reeksen (10/8, 172.16/12, 192.168/16, 100.64/10, fc00::/7);
     - link-local en cloud-metadata (169.254.0.0/16, fe80::/10);
     - alle overige adressen van de host zelf (`fib daddr type local`), zoals een publiek IP. Podman koppelt `169.254.1.2` aan de host, dus zonder deze regel bereikt een sandbox hostdiensten die op alle adressen luisteren (spike 01).

     Omdat `10.255.0.1` binnen 10/8 valt, werken daar alleen de expliciet toegestane poorten.
  4. **Al het andere (internet) blijft open.** Dat valt onder de harnasregels.
- **Gevolg:** netwerktoegang tot een gedeelde service geldt per project, omdat alle sandboxes van een project één UID delen. Binnen een project houdt het eigen account per sandbox de data gescheiden.
- Het Podman-socket wordt nooit in een sandbox gemount.

### 3.4 Een sandbox is een pod
- De pod `sbx-<id>` (pasta-netwerk) gebruikt `podman pod create --userns=keep-id:uid=1000,gid=1000`. In de pod draaien:
  - de werkcontainer, gebouwd op het projectimage;
  - per-sandbox services als containers in dezelfde pod. In fase 1 is dat Redis op `localhost:6379`.
- **Volumes:**
  - `sbx-<id>-code` → `/workspace`: de eigen working copy;
  - `proj-<id>-home` → `/home/agent`: gedeeld per project, voor harnas-login en -config;
  - de `wsm-fs`-helper, read-only → `/opt/wsm/bin` (zie 3.7).
- **Hardening op de werkcontainer:**
  - `--user 1000:1000`;
  - `--cap-drop=ALL`, `--security-opt=no-new-privileges` en het standaard seccomp-profiel;
  - `--memory`, `--cpus` en `--pids-limit`, met standaardwaarden per project;
  - geen host-mounts behalve de read-only helper, geen `--privileged`, geen host-netwerk.
- De werkcontainer start met `--init`. Heeft het image geen CMD, dan draait `sleep infinity`; anders het CMD van het image, bijvoorbeeld supervisord met nginx en php-fpm.
- **Gevolg:** er is geen root in de container. Tools komen uit het projectimage; de agent kan alleen op gebruikersniveau iets installeren (npm, pip --user en dergelijke). Dat verkleint het aanvalsoppervlak van de kernel. Een optionele "root in container"-schakelaar per project kan later.
- **Env-variabelen:** `WSM_PROJECT`, `WSM_SANDBOX`, `WSM_BRANCH`, `WSM_PREVIEW_URL`, `WSM_MCP_URL`, `WSM_MCP_TOKEN`.

### 3.5 Git zonder credentials in de sandbox
- De manager maakt per project een SSH-deploy-key (ed25519), die je in GitLab of GitHub toevoegt. In fase 1 heeft die alleen leesrechten; in fase 2 krijgt hij schrijfrechten voor de push.
- De manager houdt per project een mirror bij in `/var/lib/wsm/git/<project>.git`.
- Code gaat de sandbox alleen in en uit als **git bundle**, via exec en stdin:
  - **in:** een bundle van de basisbranch → `git clone` in de sandbox → branch `agent/<sandbox>`;
  - **uit (fase 2):** `git bundle create` in de sandbox → de manager importeert de bundle in de mirror, hersigneert en pusht.
- Zo draait de manager nooit git in een map die de agent beheert. Hooks, fsmonitor en symlink-trucs uit de sandbox kunnen de manager niet raken. Op de mirror zelf staan hooks en fsmonitor ook uit (aanname 5).

### 3.6 Broker (fase 1: MariaDB en Redis)
Elke engine implementeert dezelfde interface:
```go
type Provisioner interface {
    Enable(ctx context.Context, sb Sandbox, profile Profile) (Grant, error)
    Revoke(ctx context.Context, sb Sandbox, g Grant) error
}
```
MariaDB-profiel "dev":
```sql
CREATE USER 'sbx_<id>'@'%' IDENTIFIED BY '<random>' WITH MAX_USER_CONNECTIONS 50;
GRANT ALL PRIVILEGES ON `sbx\_<id>\_%`.* TO 'sbx_<id>'@'%';
CREATE DATABASE `sbx_<id>_app`;
```
- `ALL` op databaseniveau omvat triggers, views en routines. Het geeft géén globale rechten (SUPER, FILE, PROCESS, CREATE USER) en geen GRANT OPTION.
- De agent kan zelf extra databases met het eigen prefix aanmaken, bijvoorbeeld een testdatabase.
- Op de gedeelde dev-MariaDB staat de binlog uit (standaard in het officiële image), zodat triggers zonder SUPER werken.
- Dumps met een vreemde `DEFINER` moet het project bij de import zelf strippen.
- Bij verwijderen van een sandbox: `DROP USER` en alle databases met het prefix.
- **Redis:** een container in de pod. Er is geen account nodig, want het is een eigen instantie.
- **Netwerk:** `Enable` voor een gedeelde service zet ook de nftables-regel voor het project, via de helper. De verbindingsgegevens gaan pas terug nadat de regel actief is.

### 3.7 Bestanden in het dashboard
- `wsm-fs` is een statische Go-binary, read-only gemount in elke sandbox. Commando's: `list`, `read`, `write` (atomisch) en `stat`, met JSON over stdin/stdout.
- De manager roept `wsm-fs` aan via exec (zonder TTY), behandelt de uitvoer als onbetrouwbaar en begrenst de grootte.
- Dit werkt met elk image, omdat er geen tools in het image nodig zijn.

### 3.8 Terminal
- Een WebSocket op de dashboard-origin is verbonden met een Podman-exec met TTY: `tmux new-session -A -s main` in de werkcontainer.
- Sessies blijven bestaan als je de browser sluit, en zelfs als de manager herstart, want tmux draait in de container.
- Resize-berichten gaan naar de exec-resize van Podman.
- **Eis aan projectimages:** `tmux`, `git`, `bash`, en een gebruiker 1000 met home `/home/agent`. Voorbeeld-Containerfiles voor Laravel en Node komen in `examples/`.
- **CLI:** `wsm shell <project>/<sandbox>` doet hetzelfde via de lokale API-socket.

### 3.9 MCP
- Streamable HTTP op `http://10.255.0.1:7443/mcp/<sandbox-id>`.
- **Authenticatie:** een bearer-token per sandbox (32 willekeurige bytes); de database bewaart alleen de hash.
  - De identiteit komt uit het token, en het pad moet bij het token passen.
  - Geen enkele tool accepteert een project- of sandbox-argument.

**Tools in fase 1:**

| Tool | Wat het doet |
|---|---|
| `sandbox_info` | Project, sandbox, branch, preview-URL en actieve services. |
| `services_available` | Blokken binnen het plafond, met hun status. |
| `service_enable` | Maakt account, database en netwerktoegang aan, of start de per-sandbox container. Geeft de verbindingsgegevens terug. |

- Vraagt een agent iets buiten het plafond, dan volgt een weigering met een duidelijke melding. Het verzoekmechanisme komt in fase 2.
- Elke call, ook een geweigerde, gaat naar de audit log.

### 3.10 Audit log
- Tabel `audit_events`, append-only: SQLite-triggers blokkeren UPDATE en DELETE.
- **Hash-chain:** `hash = sha256(prev_hash || canonieke JSON van het event)`. `wsm audit verify` controleert de keten.
- Wachtwoorden en tokens worden vóór opslag geredigeerd.

### 3.11 Auth en sessies
- Passkey-login met go-webauthn. De RP-ID is de dashboard-host; de enige toegestane origin is `https://dash.<domein>`.
- Sessiecookie `__Host-wsm_session` (Secure, HttpOnly, SameSite=Lax), een CSRF-token voor API-calls en een Origin-check op de WebSocket.
- **Herstel** (zolang TOTP nog niet bestaat; dat komt in fase 2): `wsm auth enroll` op de server maakt een eenmalige registratielink.
- **Preview-toegang:**
  1. Zonder cookie stuurt de proxy je via het dashboard.
  2. Het dashboard geeft een kortlevend, eenmalig token terug aan de preview-host.
  3. De preview-host zet daarmee een eigen host-only cookie.
  4. De proxy haalt dat cookie weg voordat het verzoek de sandbox bereikt.

### 3.12 Datamodel (SQLite)
```
users            (id, name, created_at)
passkeys         (id, user_id, credential_id, public_key, sign_count, aaguid,
                  backup_eligible, backup_state, transports, name, created_at, last_used_at)
sessions         (id_hash, user_id, created_at, expires_at, last_seen_at, user_agent)
projects         (id, slug, name, repo_url, base_branch, linux_user, linux_uid,
                  image_source, containerfile, image_ref, app_port,
                  cpu_limit, mem_limit, deploy_key_pub, deploy_key_priv_enc, created_at)
project_blocks   (project_id, block_id, profile, config_json)        -- het plafond
sandboxes        (id, project_id, slug, branch, status, pod_name, preview_host,
                  host_port, mcp_token_hash, created_at, last_activity_at)
service_grants   (id, sandbox_id, block_id, status, account, databases_json,
                  secret_enc, created_at, revoked_at)
audit_events     (seq, ts, actor, sandbox_id, tool, args_json, result, detail,
                  prev_hash, hash)
jobs             (id, type, ref_id, status, log_path, created_at, finished_at)
```
- De blokcatalogus zit in fase 1 als ingebakken YAML in de binary (`internal/blocks/catalog/*.yaml`). Eigen blokken en import/export volgen in fase 3.
- Geheimen staan versleuteld op schijf, met een sleutelbestand in `/var/lib/wsm` (rechten 0600).
- `last_activity_at` wordt al bijgehouden (terminal en MCP), zodat auto-stop in fase 3 alleen nog logica is.

## 4. Repostructuur
```
CLAUDE.md         instructies voor Claude Code en andere agents
cmd/
  wsm/            serve + CLI-subcommando's
  wsm-helper/     geprivilegieerde helper (root)
  wsm-fs/         bestandshelper in de sandbox
internal/
  api/            HTTP-API voor dashboard en CLI
  audit/          append-only log met hash-chain
  auth/           passkeys, sessies, preview-auth
  blocks/         catalogus en rechtenprofielen
  broker/         provisioners (mariadb, redis)
  db/             SQLite en migraties
  gitmgr/         deploy keys, mirror, bundles
  helper/         protocol en client voor wsm-helper
  mcp/            MCP-server en tools
  netpolicy/      berekent de gewenste nftables-regels
  podman/         wrapper rond de Podman-API per projectgebruiker
  preview/        reverse proxy
  sandbox/        levenscyclus en jobs
  terminal/       WebSocket ↔ exec
web/              Vue 3 + TypeScript (Monaco, xterm.js)
deploy/           systemd-units, Quadlet, Caddyfile-voorbeeld, install.sh
examples/         voorbeeld-Containerfiles (Laravel, Node)
spikes/           01-network, 02-podman-api, 03-mcp, 04-passkeys
test/isolation/   isolatietests
docs/
  plan/           eisen, onderzoek en dit bouwplan
  adr/            architectuurbesluiten uit spikes en mijlpalen
```

## 5. Volgorde: eerst de risico's

### Spikes (vóór al het andere)
Elke spike krijgt een eigen map in `spikes/` en eindigt met een `FINDINGS.md`. Blijvende besluiten gaan als ADR naar `docs/adr/`.

1. **Netwerk.** Een rootless pod met pasta onder twee projectgebruikers, MariaDB op `wsm0` en nftables op de UID-reeks.
   *Klaar als:*
   - DNS werkt;
   - MariaDB alleen bereikbaar is nadat de regel is gezet;
   - LAN, metadata en loopback geblokkeerd zijn;
   - project A niet bij de preview-poort van project B kan.
2. **Podman-API.** De manager (gebruiker `wsm`) bestuurt de rootless Podman van een projectgebruiker via diens socket.
   *Te bewijzen:*
   - een pod met userns op pod-niveau en de hardening-vlaggen;
   - een exec met TTY doorgestuurd naar een WebSocket;
   - de vraag of de officiële bindings bouwen zonder C-afhankelijkheden (build-tags `remote` en `containers_image_openpgp`), of dat we een eigen dunne REST-client nemen.
3. **MCP.** De go-sdk met Streamable HTTP en een bearer-token, getest met Claude Code en Codex vanuit een sandbox.
4. **Passkeys.** go-webauthn met strikte RP-ID en origin.
   *Te bewijzen:*
   - een preview-origin kan de passkey niet gebruiken;
   - `__Host-`-cookies doen wat ze moeten doen.

### Mijlpalen
| | Oplevering |
|---|---|
| M1 | Helper en manager-skelet, SQLite, audit log. CLI: project en sandbox aanmaken, starten, stoppen en verwijderen, plus `wsm shell`. |
| M2 | Deploy key, mirror en bundle in de sandbox op `agent/<sandbox>`. Image bouwen uit een Containerfile of ophalen uit een registry. |
| M3 | MariaDB via Quadlet met provisioner, Redis per sandbox, nftables per grant, en de MCP-server met de drie tools. |
| M4 | Dashboard: passkey-login, projecten en sandboxes, terminal, bestanden met Monaco, services en het audit-overzicht. |
| M5 | Preview-proxy met eigen auth per host, limieten, en de complete isolatietests in CI (in een VM met Podman 5). |

## 6. Isolatietests (vanaf M1, uitgebreid per mijlpaal)
**Vanuit sandbox A (project A) moet dit allemaal mislukken:**
- verbinden met de preview-poort of de services van project B;
- MariaDB bereiken vóór `service_enable`;
- na `service_enable`: databases van anderen zien of gebruiken, `CREATE DATABASE` buiten het eigen prefix, of globale rechten gebruiken;
- LAN-adressen, `169.254.169.254`, de manager-API, en poorten op `wsm0` buiten de toegestane bereiken;
- hostdiensten die op alle adressen luisteren, ook via het publieke IP en `169.254.1.2`;
- met het MCP-token van A iets doen voor sandbox B;
- het Podman-socket vinden, of bestanden buiten de eigen volumes schrijven.

**Op de host:** `wsm-p-A` kan niet bij de bestanden of het socket van `wsm-p-B`.

**In de container:** `NoNewPrivs=1`, seccomp is actief en er zijn geen capabilities.

## 7. Installatie (fase 1)
- **Doelplatform:** een Linux-server met systemd, cgroups v2, nftables en Podman 5 of nieuwer (met pasta als rootless netwerk).
- **`deploy/install.sh`:**
  - zet de binaries in `/usr/local/bin`;
  - maakt de gebruikers `wsm` en `wsm-svc` aan;
  - installeert de systemd-units voor `wsm-helper` en `wsm` en de Quadlet-unit voor MariaDB;
  - levert een voorbeeld-Caddyfile.
- **DNS:** `dash.<domein>` en een wildcard voor het preview-domein wijzen naar de server.
- **TLS via Caddy:** een wildcard-certificaat via DNS-01, of `tls internal` met de lokale root-CA op je apparaten. Passkeys werken alleen via HTTPS.
- **Later:** de Caddy-functionaliteit in de binary zelf (certmagic), voor een installatie met echt één binary.

## 8. Open punten
- Naam en licentie (voorstel: MIT; naam bijvoorbeeld "Sluis", nog te controleren).
- De exacte MariaDB-versie en de standaardlimieten per sandbox.
- De keuze voor het frontend-framework (voorstel: Vue 3).
