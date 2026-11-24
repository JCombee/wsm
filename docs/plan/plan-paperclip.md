# Plan: Paperclip met de waarden van wsm

Status: besloten op 24 november 2026. De eigenaar nam alle voorstellen uit §13.1 over. Spike S1 is klaar (`spikes/05-paperclip-provider/FINDINGS.md`); de besluiten daarna staan in §13.6.

Gebaseerd op:
- de broncode van Paperclip op commit `cb4129a` (23 november 2026). Die is alleen gelezen, nooit uitgevoerd;
- `eisen-workspace-manager.md`, `bouwplan-fase-1.md`, `docs/security.md`, de invarianten in `CLAUDE.md` en de ADR's;
- het eerdere onderzoek `onderzoek-paperclip.md`: de hoofdkantoor-sandbox, met de uitzonderingen U1–U9.

Verwijzingen naar de broncode van Paperclip staan als `[P: pad]`.

**Begrippen in dit plan**
- **Regiekamer:** de Paperclip-installatie van één project: server, database en plugins. In het Engels heet dit de control plane.
- **Lease:** Paperclips woord voor een sandbox die een run gebruikt.
- **Tenant-API:** de beperkte API waarmee een regiekamer de sandboxes van haar eigen project beheert.
- **Voordeur:** het deel van de wsm-manager dat het internet ziet: inloggen, board, webhooks en previews.

## Conclusie

1. **Draai de rollen om.**
   - In het eerdere onderzoek draaide Paperclip ín wsm. Dit plan zet Paperclip erboven: Paperclip wordt de regiekamer, wsm de uitvoeringslaag eronder.
   - wsm levert dan aan Paperclip wat het nu aan de eigenaar levert: geharde sandboxes, het netwerkfilter, services via de MCP, git zonder credentials in de sandbox, en de audit.
2. **Paperclip is hierop ontworpen.**
   - Paperclips eigen grensmodel voor sandbox-providers is dat van wsm: de host is vertrouwd en de sandbox niet. Er gaan maar twee bevoegdheden over de grens: bestanden terugschrijven via synchronisatie, en de Paperclip-API aanroepen via de HTTP-bridge `[P: packages/plugins/sandbox-providers/SANDBOX-REQUIREMENTS.md]`.
   - De bridge loopt over het exec-kanaal van de provider. Een sandbox heeft dus geen netwerkpad naar de regiekamer nodig `[P: packages/adapter-utils/src/sandbox-callback-bridge.ts]`.
   - Paperclips eigen Kubernetes-provider past al bijna dezelfde hardening toe als wsm `[P: packages/plugins/sandbox-providers/kubernetes/README.md]`.
3. **Wat het oplevert,** vergeleken met de hoofdkantoor-sandbox:
   - een eigen sandbox per agent en werkruimte;
   - de geheimen van de regiekamer buiten bereik van de agents;
   - een wsm-identiteit per sandbox, en dus per agent;
   - `low_trust_review` werkt;
   - webhooks en previews per sandbox.

   Van U1–U9 lossen er zes op, en U3 grotendeels. U8 en U9 worden deels beter (§11).
4. **Wat het kost.**
   - Paperclip komt aan de vertrouwde kant, en het is een grote, jonge codebase met plugins.
   - Daarom sluiten we Paperclip zelf ook in: één regiekamer per project, als eigen Linux-gebruiker in een geharde container. Ze krijgt geen root en geen Podman, alleen de tenant-API van het eigen project.
   - Een gekaapte regiekamer blijft zo binnen haar project.
5. **Wat wsm moet bouwen:**
   - de tenant-API;
   - regiekamers als beheerd onderdeel;
   - een Paperclip-plugin die wsm als sandbox-provider aanbiedt;
   - push-verzoeken met review en hersigneren;
   - de voordeur voor board en webhooks.
6. **Wat Paperclip moet laten:**
   - agents lokaal op de regiekamer draaien;
   - workspace-services en stdio-MCP op de regiekamer;
   - git-tokens in sandboxes;
   - plugins installeren tijdens runtime;
   - telemetrie.

   Een deel daarvan vraagt een kleine aanpassing in Paperclip (§9).
7. **Wat niet helemaal lukt:** twaalf afwijkingen, in §10. De belangrijkste:
   - agents in één project kunnen elkaar via de Paperclip-API beïnvloeden;
   - Paperclip kent geen passkeys;
   - het board toont HTML van agents in een afgeschermd iframe.
8. **Volgende stap:** vijf spikes op een wegwerp-VM (§13), te beginnen met de provider-plugin en de bridge over wsm-exec.

## 1. Waarom een nieuw plan

- **De hoofdkantoor-sandbox zet alles in één vertrouwensdomein.**
  - Server, database en alle agents draaien als dezelfde gebruiker in één container.
  - Agents kunnen elkaars werk, de database en de geheimen van Paperclip lezen (U3).
  - Ze delen één wsm-identiteit (U4).
  - Een eigen container per agent kan niet, want daarvoor moet een sandbox andere sandboxes beheren (U1).
- **De vraag van de eigenaar:** laat wsm zoals het nu is los. Hoe maken we de waarde en het beveiligingsdenken van wsm verenigbaar met Paperclip?
- **Werkwijze:**
  1. de waarden van wsm losmaken van hun huidige uitvoering (§2);
  2. Paperclip langs die waarden leggen (§3);
  3. een ontwerp kiezen en uitwerken (§4–§6);
  4. per waarde nagaan of hij overeind blijft (§7).
- **Dit plan vervangt het onderzoek niet.** De hoofdkantoor-sandbox blijft bruikbaar als snelle proef met het nepmodel, omdat die geen wijziging in wsm vraagt.
- **Dit plan raakt bevestigde eisen en invarianten** (§8). Bouwen begint pas na akkoord. Werk dan eerst de eisen, `CLAUDE.md` en het bouwplan bij.

## 2. De waarden van wsm

De waarden hieronder staan los van hoe wsm ze nu uitvoert. Ze zijn de maatstaf voor elk ontwerp.

| | Waarde | Waarom | Hoe wsm het nu doet |
|---|---|---|---|
| W1 | **De schade blijft binnen het project** | Een prompt-injectie of een container-escape mag nooit verder reiken dan één project (eisen, "Doel" en "Architectuur") | Een Linux-gebruiker met rootless Podman per project |
| W2 | **Alles uit een sandbox is vijandig** | Wat de agent maakt, kan een aanval zijn | Grootte-limieten en strikt parsen van exec-uitvoer, `wsm-fs`-JSON en bundles (invariant 8) |
| W3 | **De sandbox is gehard** | Een uitbraak moet zo moeilijk mogelijk zijn | `1000:1000`, `--cap-drop=ALL`, `no-new-privileges`, seccomp en limieten; geen Podman-socket en geen hostmappen (invariant 1 en 9) |
| W4 | **Standaard dicht, rechten via een broker** | Een agent krijgt alleen wat hij voor development nodig heeft | De MCP geeft per sandbox een account en netwerktoegang; credentials pas nadat de nftables-regel actief is (invariant 7) |
| W5 | **Identiteit komt uit de verbinding** | Een agent kan zich niet voordoen als een ander | Een MCP-token per sandbox; geen tool neemt een project- of sandbox-ID aan, en de MCP heeft geen beheertools (invariant 2) |
| W6 | **Het plafond is van de eigenaar** | Een agent kan zijn eigen rechten niet verruimen | Blokken, projectconfig en Containerfiles staan in de manager en wijzigen alleen via de API; buiten het plafond volgt een goedkeuringsverzoek (invariant 5) |
| W7 | **Geen git-credentials in de sandbox** | Gestolen of misbruikte credentials reiken anders tot buiten het project | De manager beheert de remote; code gaat als bundle heen en weer; de manager hersigneert en pusht na review, met een mini-MR voor gevoelige bestanden (invariant 3, ADR 0009 en 0010) |
| W8 | **Eerst loggen, dan antwoorden** | Achteraf moet vaststaan wat er gebeurd is | Een append-only, hash-gekoppelde audit buiten de sandboxes, ook voor geweigerde calls (invariant 6) |
| W9 | **Een kleine vertrouwde kern** | Hoe minder vertrouwde code, hoe kleiner het aanvalsoppervlak | Alleen `wsm-helper` is root, met getypeerde verzoeken en `SO_PEERCRED`; Go met weinig afhankelijkheden (invariant 4) |
| W10 | **De eigenaar logt in met een passkey** | Phishing en gestolen wachtwoorden werken dan niet | Passkey met één origin en `__Host-`-cookies; step-up voor goedkeuren, pushen en secrets (fase 2) (invariant 10) |
| W11 | **Sandbox-content op eigen origins** | Content van een agent mag de sessie van de eigenaar niet raken | Previews op aparte origins; het dashboard toont sandbox-content nooit als HTML (invariant 10 en 11) |
| W12 | **Geheimen per project, pas bij de start erin** | Een geheim is alleen zichtbaar waar het nodig is | Secret-store per project (fase 2); registry-credentials blijven in de manager (aanname 8) |
| W13 | **Andere sandboxes, het LAN en de host zijn dicht** | Laterale beweging en cloud-metadata zijn klassieke routes | nftables per project-UID (ADR 0001) |
| W14 | **Opruimen is intrekken** | Een verwijderde sandbox laat geen werkende toegang achter | Bij verwijderen gaan accounts en databases met prefix weg (invariant 7, aanname 4) |

Daarnaast geldt: wsm heeft één gebruiker, de eigenaar (eisen, "Doel").

## 3. Paperclip langs die waarden

### 3.1 Twee manieren om agents te draaien

Paperclip draait een agent lokaal of in een sandbox:

| | Lokaal | Via een sandbox-provider |
|---|---|---|
| Waar de agent draait | Als child process van de server, met de rechten van de server | In een sandbox van een provider-plugin (Daytona, E2B, Kubernetes, Modal, …) |
| Grens | Geen | Host vertrouwd, sandbox niet `[P: SANDBOX-REQUIREMENTS.md]` |
| Wat over de grens mag | Alles | Twee bevoegdheden: bestanden terugschrijven via synchronisatie, en de API aanroepen via de HTTP-bridge |
| Waar de controles draaien | n.v.t. | Buiten de sandbox: de host controleert elk archieflid vóór het uitpakken; de bridge controleert de authenticatie, voegt de run-identiteit toe en laat alleen toegestane routes door |

De sandbox-variant past bij wsm op vier punten:
- **De bridge loopt over exec.** Er zijn twee vormen `[P: packages/adapter-utils/src/sandbox-callback-bridge.ts]`:
  - een wachtrij in bestanden, die de host met exec-calls leegt;
  - één HTTP/2-sessie over stdin en stdout van een exec-proces.

  De sandbox maakt in geen van beide vormen zelf een verbinding met de server.
- **De bridge heeft een allowlist van routes.** Een agent mag bijvoorbeeld wel een aanname aanvragen via het goedkeuringsendpoint, maar zelf geen agent aanmaken.
- **De provider levert de sandbox, Paperclip de regie.** Een plugin implementeert:
  - `environmentAcquireLease`, `environmentResumeLease`, `environmentReleaseLease` en `environmentDestroyLease`;
  - `environmentExecute`;
  - `environmentSyncIn` en `environmentSyncOut`.

  De host vertrouwt een opgegeven capability pas als de worker de bijbehorende methodes echt aanbiedt, en weigert bij twijfel `[P: doc/plugins/SANDBOX_PROVIDER_CAPABILITIES.md]`.
- **De Kubernetes-provider laat zien wat Paperclip van een provider verwacht** `[P: packages/plugins/sandbox-providers/kubernetes/README.md]`:
  - een namespace per company;
  - niet-root, alle capabilities weg, geen privilege-escalatie, een read-only root-filesystem en seccomp;
  - netwerk standaard dicht, met een allowlist;
  - egress-grants per taak;
  - een allowlist voor images;
  - optioneel Kata-microVM's.

### 3.2 Per waarde: wat Paperclip doet en wat ontbreekt

| | Wat Paperclip zelf doet | Wat ontbreekt |
|---|---|---|
| W1 | Data per company in de API. Maar companies zijn geen vertrouwensgrens voor plugins, en plugins gelden voor de hele instantie `[P: doc/plugins/PLUGIN_SPEC.md §2]` | Eén instantie voor meerdere projecten deelt het lot van de server |
| W2 | In de sandbox-variant hetzelfde uitgangspunt, met de controles buiten de sandbox | Lokaal is er geen grens |
| W3 | Laat de hardening over aan de provider | Een provider die hardt zoals wsm |
| W4 | Een MCP-gateway: profielen, beleid (`allow`, `block`, `require_approval`, `rate_limit`), goedkeuringen, redactie en audit `[P: doc/MCP-ACCESS-GOVERNANCE.md]`. Secrets per agent of project gebonden | Geen broker voor databases en andere infrastructuur |
| W5 | De bridge voegt de run-identiteit toe. De gateway geeft kortlevende tokens per run | Een identiteit per sandbox voor infrastructuur |
| W6 | Instellingen staan in Paperclips database en zijn via het board te wijzigen. Een nieuwe agent kan een goedkeuring van het board vereisen (`requireBoardApprovalForNewAgents`) `[P: packages/shared/src/validators/company.ts]` | Geen hard plafond voor infrastructuur. Een omgeving of adapter kiezen die op de server draait, kan gewoon |
| W7 | De server clonet privérepo's met een GitHub-token uit de secrets. Code gaat als bundle naar runs op afstand, zonder remote. Maar agents pushen zelf: met een token in de omgeving, of met een token dat per git-commando wordt opgehaald ("not a security boundary") `[P: doc/execution-github-identity.md; docs/guides/board-operator/execution-workspaces-and-runtime-services.md]` | Credentials bereiken de sandbox; geen hersignering met de sleutel van de eigenaar; geen review van gevoelige bestanden |
| W8 | Activity log, een tool-call-log zonder bewerk- of verwijderroute, en een log van elke secret-resolutie | Niet hash-gekoppeld |
| W9 | Een grote TypeScript-codebase. Plugins draaien als workers met de rechten van de server. Plugin-UI draait als JavaScript op de origin van het board `[P: PLUGIN_SPEC.md, "Current implementation caveats"]` | Een kleine kern; Paperclip zelf moet ingesloten worden |
| W10 | Better Auth met e-mail en wachtwoord. Passkeys of 2FA heb ik niet gevonden. In `private` staat rate limiting standaard uit `[P: doc/DEPLOYMENT-MODES.md]` | Passkey en step-up |
| W11 | HTML van agents in een iframe met `sandbox="allow-scripts"` en een strikte CSP, zonder `allow-same-origin` `[P: doc/html-artifact-previews.md]` | Het board rendert content van agents wél |
| W12 | Secrets versleuteld met een master key; per run geïnjecteerd; gemaskeerd in de logs; elke resolutie gelogd `[P: docs/deploy/secrets.md]` | Niets: dit is zo goed als de geplande secret-store van wsm |
| W13 | "The provider and the operator set the policy for general internet access" `[P: SANDBOX-REQUIREMENTS.md]` | Afhankelijk van de provider |
| W14 | Bij het vrijgeven van een tijdelijke lease ruimt Paperclip de sandbox op, na een geverifieerde copyback | Accounts en data van services intrekken |

## 4. Vier opties

| | Optie | Kern | Oordeel |
|---|---|---|---|
| A | **Hoofdkantoor-sandbox** (eerder onderzoek) | Alles in één wsm-sandbox | Werkt zonder wsm te wijzigen, maar met één vertrouwensdomein (U1–U9) |
| B | **Paperclip met een bestaande provider, zonder wsm** | Bijvoorbeeld k3s met de Kubernetes-provider op dezelfde server | De hardening is er, maar je bouwt de rest van wsm opnieuw: de broker, de git-custody, de passkey-voordeur en de audit. Daarbij komt Kubernetes op één server, en de standaardbackend is nog alpha |
| C | **wsm als provider onder Paperclip** | Paperclip is de regiekamer, wsm doet uitvoering, netwerk, services, git en audit | **Aanbevolen.** Hergebruikt wat wsm al heeft, via het extensiepunt dat Paperclip daarvoor heeft |
| D | **Paperclip als beheerder van wsm** | Paperclip krijgt de volledige beheer-API | Afgewezen: een systeem dat agents aansturen krijgt dan beheermacht over alle projecten (W1, W6, W9) |

Het verschil tussen C en D is waar het om draait. In C mag de regiekamer alleen de sandboxes van haar eigen project beheren, binnen een plafond dat zij niet kan wijzigen. In D mag ze alles.

## 5. Het ontwerp: wsm onder Paperclip

### 5.1 Overzicht

```
                      Eigenaar (browser met passkey)
                                   │ HTTPS
┌──────────────────────────────────▼────────────────────────────────────┐
│ Voordeur (wsm-manager)        passkey · __Host-cookies · step-up      │
│   board van project A     webhooks van project A     previews          │
└───────────┬─────────────────────────┬────────────────────┬────────────┘
            │                         │                    │
┌───────────▼─────────────────────────▼──────┐             │
│ Regiekamer project A   (gebruiker wsm-c-A) │             │
│   Paperclip-server · PostgreSQL · plugins  │             │
│   wsm-provider-plugin                       │             │
└───────────┬────────────────────────────────┘             │
            │ tenant-API: Unix-socket, peer = wsm-c-A       │
┌───────────▼─────────────────────────────────────────┐     │
│ wsm-kern   manager: plafond · audit · MCP · git ·   │     │
│            broker · voordeur     helper (root)       │     │
└───────────┬─────────────────────────────────────────┘     │
            │ Podman van project A                           │
┌───────────▼────────────────────────────────────────────────▼──────────┐
│ Uitvoering project A   (gebruiker wsm-p-A)                             │
│   [sandbox agent 1]    [sandbox agent 2]    [sandbox low-trust]        │
└───────────────────────────────────────────────────────────────────────┘
```

Een project B heeft een eigen regiekamer (`wsm-c-B`) en eigen sandboxes (`wsm-p-B`). Het deelt alleen de wsm-kern met A.

### 5.2 Zones

| Zone | Wat er draait | Vertrouwen | Wie het wijzigt |
|---|---|---|---|
| Eigenaar | De browser, met de passkey | Vertrouwd | De eigenaar |
| Voordeur | Inloggen, het board doorsturen, webhooks doorsturen, de preview-proxy | Vertrouwd: wsm-code | Alleen een wsm-release |
| Regiekamer | Paperclip-server, PostgreSQL, plugin-workers en de provider-plugin, per project | **Halfvertrouwd**: vertrouwd voor het eigen project, ingesloten voor de rest | De eigenaar via wsm (image, plugins, versie) en via het board (werk) |
| wsm-kern | Manager (Go) en `wsm-helper` (root) | Vertrouwd: de kleine kern | Alleen een wsm-release |
| Uitvoering | Sandboxes per project, met per-sandbox-services | **Vijandig** | Niemand: ze bestaan binnen het plafond |
| Services | Gedeelde services per versie (MariaDB, …) | Vertrouwd, per sandbox afgeschermd | wsm |

### 5.3 Begrippen: Paperclip naast wsm

| Paperclip | wsm | Toelichting |
|---|---|---|
| Instantie (met in principe één company) | Project | Eén vertrouwensdomein. Meerdere companies in één instantie delen dat domein |
| Server, database en plugins | Regiekamer van het project | Eigen Linux-gebruiker `wsm-c-<id>`, eigen container |
| Omgeving met driver `wsm` | Het project als provider | De standaardomgeving voor alle agents |
| Lease | Sandbox | Tijdelijk, of warm per agent en werkruimte (§5.5) |
| Run-identiteit (JWT, via de bridge) | MCP-token per sandbox | Paperclip regelt de identiteit tegenover de eigen API, wsm die tegenover services |
| Execution workspace | Werkruimte in de regiekamer, als bundle in de sandbox | De regiekamer bewaart de doorlopende git-staat |
| Goedkeuring in het board | Goedkeuringsverzoek in wsm | Werk keurt het board goed; het plafond en pushen keurt de eigenaar goed in wsm, met step-up |
| Secrets en AI-verbindingen | Geen eigen secret-store meer | Zie §6.6 |

### 5.4 Nieuwe invarianten

Naast de bestaande invarianten komen er vijf bij:

1. **De regiekamer voert nooit projectcode uit.** Er draaien geen lokale adapters, geen workspace-services en geen stdio-MCP in de regiekamer.
2. **Een sandbox heeft geen netwerkpad naar een regiekamer.** Al het verkeer tussen beide loopt via de tenant-API van wsm, als exec of bestandsoverdracht.
3. **Een regiekamer kan alleen bij de sandboxes van het eigen project, en alleen binnen het plafond.** Haar identiteit komt uit de verbinding, nooit uit een argument.
4. **Git-credentials en signeersleutels staan alleen in de wsm-kern.** Ze staan niet in de regiekamer en niet in een sandbox.
5. **De Paperclip-versie en de plugins horen bij het plafond.** Alleen de eigenaar wijzigt ze, en ze staan vast op een digest.

### 5.5 Een run van begin tot eind

1. **Aanleiding.** Een heartbeat, een issue of een routine start een run in de regiekamer.
2. **Lease.**
   - Paperclip vraagt de provider-plugin om een sandbox: een nieuwe, of de warme sandbox van deze agent en werkruimte.
   - De plugin vraagt die aan bij wsm via de tenant-API.
   - wsm controleert het plafond, maakt de pod met de vaste hardening, zet de nftables-regels, maakt een MCP-token en schrijft dat alles in de audit.
3. **Code erin.** Paperclip pakt de werkruimte in als git bundle, zonder remote en zonder credentials. Die gaat via `environmentSyncIn` de sandbox in, als tar-stroom over exec.
4. **Start.**
   - Paperclip start de agent-CLI via `environmentExecute`.
   - De omgeving bevat alleen wat voor deze run gebonden is: de run-gegevens, de gekoppelde secrets en de LLM-toegang.
   - wsm voegt `WSM_MCP_URL`, `WSM_MCP_TOKEN` en `WSM_PREVIEW_URL` toe.
5. **Werk.**
   - De agent praat met Paperclip via de bridge: een kleine gateway op loopback in de sandbox, die de regiekamer over exec bedient.
   - De bridge voegt de identiteit van de run toe en laat alleen toegestane routes door.
   - Services vraagt de agent aan bij de wsm-MCP, met het token van zijn eigen sandbox.
6. **Code eruit.**
   - Aan het eind pakt de sandbox de nieuwe commits in als bundle. Paperclip haalt die op via `environmentSyncOut`, controleert elk archieflid en zet de commits in de werkruimte van de regiekamer.
   - Moet er iets naar de remote, dan vraagt de agent een push aan bij de wsm-MCP (§6.4).
7. **Einde.** Paperclip geeft de lease vrij.
   - Een tijdelijke sandbox gaat weg. wsm trekt zijn accounts in en gooit zijn databases weg.
   - Een warme sandbox blijft voor de volgende beurt. Hij gaat pas weg als de werkruimte sluit.

**Het leasemodel.** Paperclip kent drie leasebeleiden `[P: packages/shared/src/types/environment.ts; server/src/services/environment-runtime.ts]`:
- `ephemeral`;
- `reuse_by_environment`: een warme sandbox. De sleutel bevat onder meer de company, de omgeving, de execution workspace, het issue, de agent en het adaptertype;
- `retain_on_failure`: een gestopte sandbox als enige kopie van een mislukte run.

Voorstel:
- **Standaard warm.** De sandbox van agent X op werkruimte Y blijft bestaan tussen beurten, met zijn eigen `/workspace`, home, caches en service-accounts. Dit is de "eigen container per agent".
- **Tijdelijk voor `low_trust_review`.** Werk op vijandige invoer begint altijd schoon.

### 5.6 De tenant-API

Een nieuwe, beperkte API naast de bestaande beheer-API (ADR 0005).

**Identiteit.**
- Elk project krijgt een eigen socket, bijvoorbeeld `/run/wsm/tenant/<id>.sock`.
- wsm controleert met `SO_PEERCRED` dat de peer de regiekamer-gebruiker van dat project is, zoals de helper dat nu doet.
- Een route bevat nooit een project-ID (W5).

**Voorgestelde routes:**

| Route | Wat | Controle door wsm |
|---|---|---|
| `POST /v1/tenant/sandboxes` | Sandbox maken, met labels voor agent, issue, run en lease | Image in de allowlist van het plafond, limieten binnen het plafond, aantal binnen het maximum |
| `POST /v1/tenant/sandboxes/{id}/start` en `/stop` | Starten en stoppen | Alleen sandboxes van het eigen project |
| `DELETE /v1/tenant/sandboxes/{id}` | Verwijderen, met intrekken van accounts en databases | Idem |
| `GET /v1/tenant/sandboxes` | Eigen sandboxes en hun status | Idem |
| `POST /v1/tenant/sandboxes/{id}/exec` | Een stroom met stdin, stdout, stderr en exitcode, en een omgeving per exec | Grootte- en tijdslimieten; omgevingswaarden nooit in de audit |
| `GET /v1/tenant/repo/bundle` | De basisbranch als bundle, uit de wsm-mirror | Alleen het eigen project |

Wat er bewust niet in zit:
- routes voor het plafond, voor projecten of voor andere projecten;
- service-grants: die vraagt een sandbox zelf aan bij de MCP, met zijn eigen identiteit;
- pushen: dat loopt via de MCP met review (§6.4).

Elke call komt in de audit vóór het antwoord, ook een geweigerde (W8).

**Bestanden** gaan als tar-stroom over exec. Paperclip noemt dit de command-fallback. De sandbox maakt het archief, en de regiekamer controleert elk lid. Dat volgt het patroon van `wsm-fs`: een hulpprogramma in de sandbox levert data, en de ontvanger parset die strikt.

Podmans archive-API gebruiken we niet. Daarmee zou wsm bestanden uit een vijandige container lezen, met de symlink-risico's die daarbij horen.

### 5.7 De provider-plugin

- **Wat het is:** een Paperclip-plugin met een omgevingsdriver `driverKey: "wsm"`, `kind: "sandbox_provider"`.
- **Taal:** TypeScript, omdat Paperclip-plugins Node-pakketten zijn. Dat is een uitzondering op "Go". De plugin houdt dat klein:
  - hij is een dunne client van de tenant-API;
  - alle beveiligingslogica zit in wsm;
  - de enige afhankelijkheid is de plugin-SDK van Paperclip.
- **Capabilities:**
  - `reusableLeases` (resume, release en destroy);
  - `nativeSyncIn` en `nativeSyncOut` (tar over exec);
  - `persistentProcessSessions`, `independentControlCommands` en `incrementalSessionOutput`;
  - de stop-hook om een werkruimte te bewaren (`environmentStopLease`).
- **Configuratie:** geen geheimen. De identiteit komt uit de socket. De config kan het plafond alleen verder beperken, bijvoorbeeld tot een kleiner image of lagere limieten.
- **Plaats:** een eigen map in deze repo, bijvoorbeeld `integrations/paperclip-provider/`, met een eigen `package.json`.
- **Voorbeeld:** de Kubernetes-provider van Paperclip, met zijn pod-spec, lease-levensloop, exec, bestandssynchronisatie, image-allowlist en egress-beleid.

## 6. Uitwerking

### 6.1 De regiekamer insluiten

Paperclip is groot en verandert snel. Dit plan vertrouwt het daarom alleen binnen het eigen project, en richt de regiekamer zo in dat een gekaapte server weinig kan.

- **Eén regiekamer per project.**
  - Een eigen Linux-gebruiker `wsm-c-<id>`. De helper leidt naam en UID af uit de project-UID, bijvoorbeeld project 50012 → regiekamer 60012.
  - De regiekamer draait als Quadlet-unit onder die gebruiker, zoals de gedeelde services onder `wsm-svc`.
  - Ze staat los van de projectgebruiker. Een uitbraak uit een sandbox landt dus niet bij de master key van de regiekamer.
- **Een gehard image.** Het officiële Paperclip-image op een vaste digest, met de provider-plugin erin gebakken. wsm bouwt het, en het hoort bij het plafond.
- **Een geharde container:**
  - niet-root, `--cap-drop=ALL`, `no-new-privileges`, seccomp en limieten;
  - een read-only root-filesystem met één datavolume;
  - geen Podman-socket en geen hostmappen;
  - alleen de tenant-socket van het eigen project is erin gemount.
- **Netwerk:**
  - binnenkomend alleen van de voordeur, op een poort op `127.0.0.1`;
  - uitgaand standaard dicht, met een allowlist per functie, bijvoorbeeld de Discord-gateway of een e-maildienst. Die allowlist hoort bij het plafond.
- **Instellingen en aanpassingen:** zie §9.

**Als de regiekamer van project A toch gekaapt wordt:**

| Kan wel | Kan niet |
|---|---|
| De sandboxes van A maken, verwijderen en erin draaien, binnen het plafond | Het plafond verhogen, of een image buiten de allowlist gebruiken |
| De secrets en AI-verbindingen van A lezen | Bij project B, bij de host of bij andere regiekamers komen |
| De werkruimtes en de database van A wijzigen | Pushen naar de remote zonder review door wsm |
| Activiteit van A vervalsen in Paperclips eigen logboek | De wsm-audit wijzigen; elke tenant-call staat daar al in |

De schade blijft binnen het project (W1). Dat is dezelfde grens als nu, alleen nu ook voor een fout in Paperclip zelf.

### 6.2 Netwerk

| Van | Naar | Regel |
|---|---|---|
| Sandbox | Internet | Open, zoals in ADR 0001, of standaard dicht met een allowlist (besluit B5) |
| Sandbox | MCP op `10.255.0.1:7443` | Toegestaan |
| Sandbox | Gedeelde services | Pas na een grant (ADR 0014) |
| Sandbox | Een regiekamer, eigen of andere | **Dicht.** De bridge loopt over exec |
| Sandbox | Andere sandboxes, LAN, metadata, host | Dicht (ADR 0001) |
| Regiekamer | wsm-kern | Alleen de tenant-socket: een Unix-socket, geen netwerk |
| Regiekamer | Internet | Standaard dicht, met een allowlist per functie |
| Regiekamer | Sandboxes | Geen netwerk, alleen via de tenant-API |
| Voordeur | Regiekamer | Een poort op `127.0.0.1` |
| Internet | Voordeur | Board na passkey; webhooks alleen op webhook-routes; previews na de preview-cookie |

**Besluit B5, internet in sandboxes.**
- De eisen zeggen "Standaard is alles dicht", maar ADR 0001 zet internet open, omdat agents registries en LLM-API's nodig hebben.
- Paperclips Kubernetes-provider laat zien dat het ook anders kan: standaard dicht, met domeinen per taak.
- Voorstel: begin open, zoals nu, en maak een allowlist per project mogelijk als optie in het plafond.

**De native runner** (`paperclip-runnerd`) maakt zelf een WSS-verbinding met de server, of luistert in de sandbox op een inkomende verbinding `[P: packages/paperclip-runner/README.md]`. Dat botst met de tweede nieuwe invariant. Gebruik daarom de adapters die de bridge over exec gebruiken (zie E7).

### 6.3 Inloggen, identiteit en origins

- **De eigenaar** logt in met een passkey bij de voordeur (ADR 0016).
  - Elk board krijgt een eigen origin per project. De voordeur gebruikt dezelfde stroom als voor previews (ADR 0017): eerst inloggen op het dashboard, daarna een host-cookie voor het board.
  - De voordeur haalt haar eigen cookie weg voordat ze een verzoek doorstuurt (invariant 10).
- **Paperclips eigen login** blijft daarachter: `authenticated` met `private`. Dat is een dubbele login. Paperclip kent geen passkeys en, voor zover gevonden, geen login via een vertrouwde proxy (E3).
- **Step-up** bij het goedkeuren van een push of een verzoek buiten het plafond gebeurt in wsm, met de passkey. Goedkeuringen in het board, zoals een aanname of een budget, hebben geen step-up.
- **Agents:**
  - de bridge zet de run-identiteit bij elk verzoek;
  - het geheim dat de run-sleutels tekent, staat in de regiekamer, dus een agent kan geen sleutels meer namaken (U3);
  - een sandbox-run krijgt alleen de omgeving die Paperclip voor die run samenstelt. De spike controleert dat er geen servergeheimen in zitten.
- **Services:** een MCP-token per sandbox (invariant 2 ongewijzigd).
- **Regiekamer naar wsm:** de peer-UID op de tenant-socket.

### 6.4 Code: custody, review en ondertekening

De git-custody blijft bij wsm (W7). Paperclips eigen GitHub-functies staan uit.

```
upstream ──(deploy key, wsm-kern)──▶ mirror ──bundle──▶ regiekamer-werkruimte ──bundle──▶ sandbox
                                        ▲                                                  │
                                        │                                         commits, lokaal
                                        └── push-verzoek via de wsm-MCP: bundle uit de ────┘
                                            sandbox, review, hersigneren, pushen
```

- **Code erin.**
  - wsm haalt de code op met de deploy key van het project (ADR 0009).
  - De plugin haalt de basisbranch op als bundle via de tenant-API, en houdt in de regiekamer een kale repo bij als lokale "origin".
  - Paperclip werkt daarmee zoals altijd: worktrees per issue in de regiekamer, en bundles naar de sandbox.
- **Doorlopende staat.** Na elke run zet Paperclips restore de commits in de werkruimte van de regiekamer. Dat is Paperclips "no-remote-git contract": code gaat tussen runs nooit via een remote `[P: docs/guides/board-operator/execution-workspaces-and-runtime-services.md]`.
- **Naar de remote** gaat code alleen met een push-verzoek via de wsm-MCP. Dat staat al in de eisen ("git via de manager: review + push aanvragen"):
  1. de agent roept de MCP-tool aan; de identiteit komt uit het token van zijn sandbox;
  2. wsm haalt een bundle uit die sandbox, als onbetrouwbare invoer, en zet die op een quarantaine-ref in de mirror;
  3. zonder gevoelige bestanden: hersigneren met de sleutel van de eigenaar, pushen naar `agent/<…>`, en in de audit;
  4. met gevoelige bestanden: een mini-MR in wsm, met step-up voor goedkeuren;
  5. de agent ziet de status via de MCP en meldt die in het issue.
- **Wat uit staat:**
  - de managed GitHub-koppeling. Die vraagt voor een self-hosted instantie bovendien een goedkeuring van Paperclip Cloud `[P: doc/connections/GITHUB.md]`;
  - `GH_TOKEN` voor agents;
  - de credential-route van de bridge (`/runtime-tools/github/credentials`);
  - het clonen door de server met een company-secret.
- **Git in de regiekamer** werkt op bundles die agents maakten. Eis daarom hetzelfde als op de wsm-mirror (aanname 5): hooks en fsmonitor uit, en geen submodules volgen. De spike controleert hoe Paperclip git aanroept.

### 6.5 Services via de wsm-MCP

- **Ongewijzigd.** Elke sandbox heeft een eigen MCP-endpoint en -token. Met warme leases heeft elke agent zo een eigen identiteit, eigen accounts en eigen data (U4 opgelost).
- **Twee soorten tools, twee poortwachters:**
  - **infrastructuur** (databases, Redis, push-verzoeken): de wsm-MCP, met het plafond en de audit van wsm;
  - **externe diensten** (bijvoorbeeld Linear, of leesrechten op GitHub): Paperclips MCP-gateway, met profielen, beleid en goedkeuringen in het board. Alleen `remote_http`, nooit `local_stdio` (§9).
- **Buiten het plafond:** een verzoek bij de wsm-MCP wordt "pending" en komt in wsm ter goedkeuring (aanname 3). Later kan de plugin zulke verzoeken ook in het board tonen.

### 6.6 Secrets en LLM-toegang

- **Paperclips secrets worden de secret-store** `[P: docs/deploy/secrets.md]`:
  - versleuteld met een master key, per regiekamer en dus per project;
  - per agent, project of omgeving gebonden;
  - per run in de omgeving gezet;
  - gemaskeerd in de logs;
  - elke resolutie gelogd.

  De geplande secret-store van wsm (fase 2) is dan niet meer nodig (besluit B10).
- **De master key staat in de regiekamer.** In de hoofdkantoor-sandbox kon elke agent hem lezen; nu niet meer.
- **LLM-toegang** via Paperclips AI-verbindingen: een abonnementstoken of API-sleutel per gebruiker of company, per run geleverd `[P: doc/connections/AI-CONNECTIONS.md]`. Er staat geen login meer in een gedeelde projecthome. Dat vervangt de eis "Harnas-home (met login) per project" (besluit B9).
- **Blijft zo:** tijdens de run kan de agent zijn eigen sleutel lezen (E10). Een LLM-proxy die de sleutel buiten de sandbox houdt, kan later (besluit B6).
- **Registry-credentials** blijven in de wsm-kern (aanname 8).
- **Tests** gebruiken het gescripte nepmodel, nooit echte sleutels (ADR 0011, punt 7).

### 6.7 Data en levensduur

| Wat | Waar | Levensduur |
|---|---|---|
| Paperclips database (ingebouwde PostgreSQL) | Datavolume van de regiekamer | Zo lang als het project |
| Master key, `BETTER_AUTH_SECRET`, overige servergeheimen | Datavolume van de regiekamer | Idem |
| Werkruimtes met de doorlopende git-staat | Datavolume van de regiekamer | Tot de werkruimte sluit |
| Run-logs, bijlagen, back-ups | Datavolume van de regiekamer | Volgens Paperclip |
| `/workspace`, home en caches van een sandbox | Volumes per sandbox | Tijdelijk: met de run weg. Warm: tot de werkruimte sluit |
| Data van per-sandbox-services | Bij de sandbox | Met de sandbox weg (invariant 7) |
| Mirror, deploy key, signeersleutel | wsm-kern | Zo lang als het project |
| wsm-audit | wsm-kern | Append-only |

Eén PostgreSQL per regiekamer houdt de projecten los van elkaar. Een gedeelde PostgreSQL bespaart geheugen, maar maakt de databaseserver tot een gedeeld lot. Dat kan later, als die engine in wsm komt.

### 6.8 Audit

- **wsm-audit (hash-gekoppeld):** elke tenant-call, elke MCP-call, elk push-verzoek en elke beslissing over het plafond.
- **Paperclip:** de activity log, de tool-call-log van de gateway (zonder bewerk- of verwijderroute) en de secret-resoluties. Agents kunnen daar niet meer bij, want ze draaien niet meer naast de server. Hash-gekoppeld is dit niet (E6).
- **Optioneel:** de plugin abonneert zich op Paperclip-events en schrijft een samenvatting naar de wsm-audit (besluit B8).

### 6.9 Vertrouwen binnen een project

- **De standaard (`standard`)** is zichtbaarheid binnen de company. Agents lezen het werk van de company, maken taken voor zichzelf en vragen aannames aan via een goedkeuring.
  - Een gekaapte agent kan dus proberen andere agents via issues en comments te sturen (E1).
  - Dat blijft binnen het project, en dus binnen W1.
- **Voor vijandige invoer: `low_trust_review`** `[P: doc/LOW-TRUST-PRESETS.md]`:
  - Paperclip weigert zo'n run zonder `sandbox`-driver en `isolated_workspace`. Beide zijn er nu;
  - de agent leest en wijzigt dan alleen binnen zijn grens;
  - geen inline geheimen, geen comment in het hogere issue, en geen workspace-services.
  - Gebruik dit voor externe PR's, tickets en webhook-invoer.
- **Verschillende vertrouwensniveaus** horen in verschillende projecten, met elk een eigen regiekamer.
- **Budgetten** zijn nu echt: agents kunnen niet meer bij de database van Paperclip. De harde grens blijft de bestedingslimiet bij de LLM-aanbieder.

### 6.10 Webhooks en chat

- **De voordeur heeft per project een webhook-host.** Die laat alleen de webhook-routes van Paperclip door:
  - chat-webhooks;
  - e-mail;
  - voice;
  - plugin-webhooks;
  - PR- en routine-webhooks.
- **Zonder passkey,** want afzenders kunnen die niet gebruiken. Wel met limieten op grootte en tempo in de voordeur. Paperclip controleert de handtekening.
- **De inhoud is onbetrouwbaar.** Laat zulke invoer afhandelen door agents met `low_trust_review`.
- **Uitgaande chat**, zoals de Discord-gateway, gaat via de allowlist van de regiekamer.

### 6.11 Previews en content

- **Previews:** een agent start zijn dev-server in zijn eigen sandbox. Die is bereikbaar op `<sandbox>--<project>.<preview-domein>`, achter de passkey-cookie (ADR 0017). Dat lost U7 op.
- **Paperclips workspace-services staan uit.** Die start de server zelf als lokaal proces, met projectcode `[P: server/src/services/local-service-supervisor.ts; server/src/services/heartbeat/runtime-services.ts]`. Dat botst met de eerste nieuwe invariant (E9).
- **HTML van agents in het board:** een iframe met een ondoorzichtige origin en een strikte CSP. Dat is een afwijking van invariant 11 (E4, besluit B7).
- **Plugin-UI** draait als vertrouwde JavaScript op de origin van het board. Installeer daarom alleen eigen, vastgepinde plugins (E5).

## 7. De waarden-check

| | Waarde | In dit ontwerp | Status |
|---|---|---|---|
| W1 | Schade binnen het project | Project = regiekamer + sandboxes onder eigen gebruikers; een tenant-socket per project | Volledig, ook voor fouten in Paperclip |
| W2 | Alles uit een sandbox is vijandig | Bridge en sync controleren buiten de sandbox; wsm begrenst exec en bundles | Volledig |
| W3 | Geharde sandbox | Ongewijzigd, nu per lease | Volledig |
| W4 | Dicht, rechten via een broker | wsm-MCP voor infrastructuur, Paperclips gateway voor externe tools | Volledig |
| W5 | Identiteit uit de verbinding | MCP-token per sandbox; peer-UID voor de regiekamer; run-identiteit via de bridge | Volledig |
| W6 | Het plafond is van de eigenaar | wsm controleert elke tenant-call. Paperclip kan alleen beperken. Versie en plugins horen bij het plafond | Volledig, mits lokale uitvoering echt uit staat (E11) |
| W7 | Geen git-credentials in de sandbox | Custody, review en hersigneren in wsm; GitHub-functies van Paperclip uit | Volledig |
| W8 | Eerst loggen | wsm-audit voor alles wat wsm doet; Paperclips logboeken voor werk | Deels (E6) |
| W9 | Kleine vertrouwde kern | De wsm-kern blijft klein. Paperclip is groot, maar ingesloten per project | Deels (E2) |
| W10 | Passkey | Passkey bij de voordeur; step-up voor push en plafond in wsm | Deels (E3) |
| W11 | Eigen origins voor sandbox-content | Previews en boards op eigen origins; HTML in een afgeschermd iframe | Afwijking (E4, E5) |
| W12 | Geheimen per project | Paperclip-secrets per regiekamer, per run geïnjecteerd | Volledig |
| W13 | Andere sandboxes, LAN en host dicht | Ongewijzigd, en sandboxes kunnen niet bij regiekamers | Volledig |
| W14 | Opruimen is intrekken | Lease vrij → sandbox weg → accounts en databases weg | Volledig |

## 8. Wat er verandert in wsm

| Onderdeel | Wat er gebeurt | Toelichting |
|---|---|---|
| `wsm-helper` | Behouden, uitbreiden | Nieuwe verzoektypes: een regiekamer-gebruiker maken en verwijderen, en egress-regels voor regiekamers. Nog steeds getypeerd en gevalideerd |
| Projectgebruikers en rootless Podman | Behouden | |
| Sandbox-hardening | Behouden | Nu per lease |
| nftables (ADR 0001) | Behouden, uitbreiden | Regels voor regiekamers; optioneel een internet-allowlist per project |
| Image-builds en plafond | Behouden, uitbreiden | Images moeten voldoen aan Paperclips contract: `node`, `sh`, `tar` en de CLI's op het `PATH` `[P: SANDBOX-REQUIREMENTS.md]`. Het plafond krijgt een maximum aantal sandboxes en de Paperclip-versie |
| MCP, broker, grants, per-sandbox-services | Behouden | Plus de tool voor push-verzoeken |
| Audit | Behouden, uitbreiden | Tenant-calls en push-verzoeken |
| Beheer-API (ADR 0005) | Behouden | Voor de eigenaar en de CLI |
| **Tenant-API** | **Nieuw** | §5.6 |
| **Regiekamer-beheer** | **Nieuw** | Gebruiker, Quadlet-unit, image, volume en tenant-socket per project; upgrades door de eigenaar |
| **Provider-plugin** | **Nieuw** | TypeScript, dun (§5.7) |
| Git-mirror en deploy keys | Behouden, uitbreiden | Bundles voor de regiekamer; push-verzoeken met review en hersigneren. Dat laatste was al fase 2 |
| Preview-proxy | Behouden | Per sandbox. Het board is geen preview: het gaat via de voordeur |
| Passkey-login | Behouden, uitbreiden | Beschermt ook de boards (zelfde stroom als previews) en de goedkeuringen met step-up |
| **Webhook-host** | **Nieuw** | In de voordeur, alleen webhook-routes |
| Dashboard | Aanpassen | Voor beheer: projecten, plafond, regiekamers, audit, goedkeuringen en een terminal in een sandbox om te debuggen. Het dagelijkse werk gaat naar het board |
| CLI | Behouden | Ook voor regiekamers |
| Secret-store (fase 2) | Vervalt (voorstel) | Paperclip-secrets per regiekamer (B10) |
| Goedkeuringen (fase 2) | Opsplitsen | Werk in het board; plafond en push in wsm |
| Harnas-home per project | Vervalt (voorstel) | Een home per sandbox; logins via AI-verbindingen (B9) |
| Aanname 4 ("alleen de gebruiker maakt sandboxes") | Aanpassen | De regiekamer maakt ze, binnen het plafond, met audit (B1) |

## 9. Wat Paperclip moet laten of anders doen

| Wat | Waarom | Hoe |
|---|---|---|
| Agents lokaal op de server (`local`, `process`, lokale adapters) | Eerste nieuwe invariant | Standaardomgeving `wsm` voor de instantie en per agent (`defaultEnvironmentId`) `[P: server/src/services/environments.ts]`. Een harde blokkade heb ik niet gevonden: er is een kleine patch of een bijdrage aan Paperclip nodig, met een isolatietest (E11). Zet `requireBoardApprovalForNewAgents` aan |
| Workspace-services | Ze draaien projectcode op de server (E9) | Geen services configureren; patch of instelling, te bevestigen in de spike |
| Stdio-MCP (`local_stdio`) | Code van derden op de server | Alleen `remote_http`; geen `PAPERCLIP_TRUSTED_MCP_RUNTIME_HOST` `[P: doc/MCP-ACCESS-GOVERNANCE.md]` |
| Managed GitHub, `GH_TOKEN` en de credential-route | Vierde nieuwe invariant | Geen GitHub-verbinding; de route weigeren in een patch of via instellingen |
| Plugins installeren tijdens runtime | Vijfde nieuwe invariant | Een read-only pluginmap in het image; geen egress naar npm |
| Telemetrie | Privacy | `PAPERCLIP_TELEMETRY_DISABLED=1` en `DO_NOT_TRACK=1` |
| Native runner | Tweede nieuwe invariant | Adapters met de bridge over exec (E7) |
| De bridge in bestandsmodus | Elke poll is een exec, en dus een audit-regel | Bij voorkeur HTTP/2 over stdio: één exec-sessie per run |
| Paperclip Cloud-functies | Externe afhankelijkheid | Niet gebruiken |

Elke patch is onderhoud bij elke upgrade. Bied daarom wat breder bruikbaar is aan Paperclip aan:
- een allowlist voor omgevingen per instantie;
- workspace-services uit kunnen zetten;
- login via een vertrouwde proxy, of passkeys.

## 10. Uitzonderingen en afwijkingen

Elke afwijking hieronder beschrijft wat niet (helemaal) lukt, waarom niet, en wat het gevolg is.

**E1. Agents in één project kunnen elkaar via de Paperclip-API beïnvloeden.**
- Waarom: `standard` maakt het werk zichtbaar binnen de company, en agents mogen comments en taken schrijven.
- Gevolg: een gekaapte agent kan andere agents proberen te sturen. Dat blijft binnen het project (W1).
- Wat helpt: `low_trust_review` voor vijandige invoer, en aparte projecten voor aparte vertrouwensniveaus.

**E2. Paperclip is vertrouwde code.**
- Waarom: de bridge en de synchronisatie van Paperclip zíjn de grenscontroles. Een fout daarin is een gat in de grens tussen sandbox en regiekamer.
- Gevolg: de vertrouwde code is veel groter dan de wsm-kern (W9).
- Wat helpt: insluiting per project (§6.1), vaste versies, en isolatietests op de grens.

**E3. Geen passkey en geen step-up in het board.**
- Waarom: Paperclip gebruikt Better Auth met e-mail en wachtwoord. Passkeys of login via een vertrouwde proxy heb ik niet gevonden.
- Gevolg: een dubbele login (eerst de voordeur, dan Paperclip), en goedkeuringen in het board zonder step-up.
- Wat helpt: push en plafond keurt de eigenaar goed in wsm; een bijdrage aan Paperclip.

**E4. Het board toont content van agents.**
- Waarom: HTML-artefacten draaien in een iframe met `sandbox="allow-scripts"`, zonder `allow-same-origin`, met een strikte CSP.
- Gevolg: een afwijking van invariant 11. Het risico is klein: er is geen toegang tot cookies of het board. Misleidende inhoud blijft wel mogelijk.
- Besluit B7: toestaan, of uitzetten met een patch. Hoe het board Markdown rendert, controleert de spike.

**E5. Plugin-UI is vertrouwde code op de origin van het board.**
- Gevolg: een kwaadaardige plugin kan alles wat de ingelogde gebruiker kan.
- Wat helpt: alleen eigen, vastgepinde plugins, als deel van het plafond.

**E6. Paperclips logboeken zijn niet hash-gekoppeld.**
- Gevolg: wie de regiekamer kaapt, kan Paperclips logboek van dat project vervalsen. De wsm-audit blijft betrouwbaar, maar dekt het werk van Paperclip niet in detail.
- Wat helpt: de samenvatting via de plugin (B8).

**E7. De native runner vraagt een netwerkpad.**
- Waarom: `paperclip-runnerd` maakt een WSS-verbinding met de server, of wacht op een inkomende verbinding.
- Gevolg: harnassen die alleen via de native runner werken, vallen af, of vragen een smalle regel van de sandbox naar de eigen regiekamer.
- Besluit: in eerste instantie alleen adapters met de bridge over exec.

**E8. De GitHub-functies van Paperclip vallen weg.**
- Wat wegvalt: de managed GitHub-koppeling (bovendien afhankelijk van Paperclip Cloud), PR-reviewbots, merge-verificatie via GitHub, en agents die zelf PR's openen.
- Gevolg: een PR maken is een stap na de push, door de eigenaar of later door wsm.

**E9. Paperclips workspace-services vallen weg.**
- Gevolg: geen dev-servers die Paperclip zelf start, en geen automatische service-URL in het board. Een agent start zijn dev-server in zijn sandbox, en meldt `WSM_PREVIEW_URL` in het issue.

**E10. LLM-credentials zijn tijdens de run leesbaar.**
- Waarom: het harnas heeft de sleutel nodig, in de sandbox.
- Gevolg: een gekaapte agent kan de sleutel van zijn run lekken. Dat is wel beter dan nu: geen blijvende login in een gedeelde home, en per agent alleen wat gebonden is.
- Wat helpt: bestedingslimieten bij de aanbieder, en later een LLM-proxy (B6).

**E11. Lokale uitvoering is niet hard uit te zetten zonder aanpassing.**
- Waarom: Paperclip kent een standaardomgeving, maar ik vond geen allowlist. Een beheerder kan dus een lokale omgeving kiezen, en een goedgekeurde aanname kan er een vragen.
- Gevolg: zonder patch rust de eerste nieuwe invariant op discipline.
- Wat helpt: een patch of bijdrage, een isolatietest die bewijst dat geen run lokaal start, en een regiekamer-image zonder agent-CLI's.

**E12. Paperclip is multi-user, wsm niet.**
- Waarom: Paperclip kent uitnodigingen en meerdere board-gebruikers. wsm heeft één eigenaar.
- Voorstel: één board-gebruiker per regiekamer, uitnodigingen uit. Multi-user blijft geparkeerd, zoals in de eisen.

## 11. Wat er wordt van U1–U9

| | Uitzondering uit het onderzoek | In dit plan |
|---|---|---|
| U1 | Orkestrator van andere sandboxes | **Opgelost:** de regiekamer heeft een tenant-API voor het eigen project |
| U2 | `low_trust_review` | **Opgelost:** de `sandbox`-driver bestaat nu |
| U3 | Geen scheiding tussen agents en companies | **Grotendeels opgelost:** een sandbox per agent, servergeheimen buiten bereik. Binnen de company blijft de API gedeeld (E1) |
| U4 | Geen wsm-identiteit per agent | **Opgelost:** een MCP-token per sandbox |
| U5 | Code naar buiten | **Opgelost** met push-verzoeken via de MCP. Dit is de fase 2-flow van wsm, die nu gebouwd moet worden |
| U6 | Inkomende webhooks | **Opgelost** via de webhook-host, met een bewust open oppervlak |
| U7 | Previews van de apps van agents | **Opgelost:** een preview per sandbox |
| U8 | Rechten of een container-runtime in de sandbox | **Deels:** Docker in een sandbox kan nog steeds niet. Bubblewrap is niet meer nodig. Tailscale-omgevingen gebruiken we niet |
| U9 | Paperclip bijwerken | **Makkelijker:** de eigenaar wisselt het image van de regiekamer, met de bijbehorende databasemigraties en een plugintest |

## 12. Risico's

- **Paperclip verandert snel.** Het provider-contract, de bridge en de leaselogica zijn nog in beweging. Pin een versie, en houd de plugin dun en goed getest.
- **Prestaties van de bridge over exec.** Exec loopt via de manager naar Podman. Meet de doorvoer en de latency, en kies HTTP/2 over stdio.
- **Opstarttijd.** Een sandbox met eigen services maken kost seconden. Warme leases beperken dat tot de eerste beurt.
- **Geheugen.** Elke regiekamer is een Node-server met een PostgreSQL. Meet in de spike wat dat per project kost.
- **Twee UI's.** Werk gebeurt in het board, beheer in wsm. Houd die verdeling scherp, en link waar nodig.
- **Volume in de audit.** Veel exec-calls kunnen de audit laten groeien. Log sessies, niet bytes.
- **Toeleveringsketen.** Paperclip heeft veel afhankelijkheden. Vaste digests, en upgrades alleen door de eigenaar.
- **Licentie.** Paperclip is MIT `[P: LICENSE]`, en past dus bij een open-source wsm.

## 13. Aanpak

### 13.1 Besluiten voor de eigenaar

De eigenaar nam op 24 november 2026 elk voorstel hieronder over.

| | Besluit | Voorstel |
|---|---|---|
| B1 | De rollen omdraaien, met een regiekamer die sandboxes maakt binnen het plafond (aanname 4 aanpassen) | Ja |
| B2 | Eén regiekamer per project, of één voor alles | Eén per project |
| B3 | Het leasemodel | Warm per agent en werkruimte; tijdelijk voor `low_trust_review` |
| B4 | De git-custody | Bij wsm; Paperclips GitHub-functies uit |
| B5 | Internet in sandboxes | Open zoals nu, met een allowlist per project als optie |
| B6 | Een LLM-proxy | Later; eerst per-run injectie door Paperclip |
| B7 | HTML-artefacten in het board | Toestaan, met de afgeschermde iframe; heroverwegen na de spike |
| B8 | Paperclip-events naar de wsm-audit | Ja, als samenvatting via de plugin |
| B9 | Harnas-home per project laten vallen | Ja, een home per sandbox |
| B10 | Secret-store van wsm laten vallen | Ja, Paperclip-secrets |

### 13.2 Spikes

Alle spikes draaien op een wegwerp-VM, met het gescripte nepmodel en zonder echte sleutels. Elke spike eindigt met een `FINDINGS.md` in `spikes/`.

1. **S1: provider en bridge.** Een minimale provider-plugin, Paperclip op een vaste digest, en een `claude_local`-agent in een geharde pod.
   *Klaar als:*
   - acquire, execute, sync-in, sync-out en release werken;
   - de bridge werkt in beide vormen over exec, terwijl de sandbox geen netwerkpad naar de regiekamer heeft;
   - een run haalt een issue op, commit lokaal, en de commit komt via restore in de werkruimte van de regiekamer;
   - een archief met een symlink of `../` wordt geweigerd.
2. **S2: de regiekamer insluiten.** Paperclip in een rootless container onder een eigen gebruiker, zonder Podman-socket, met alleen een tenant-socket.
   *Klaar als:*
   - een run met een lokale omgeving of de `process`-adapter niet start;
   - workspace-services en stdio-MCP niet starten;
   - plugins installeren tijdens runtime faalt;
   - uitgaand verkeer alleen naar de allowlist gaat;
   - de omgeving van een sandbox-run geen servergeheimen bevat;
   - het geheugengebruik per regiekamer gemeten is.
3. **S3: de tenant-API.** Een Go-prototype met `SO_PEERCRED` door rootless Podman heen.
   *Klaar als:*
   - regiekamer A alleen sandboxes van A kan maken, gebruiken en verwijderen;
   - images buiten de allowlist, te veel sandboxes en te hoge limieten worden geweigerd;
   - elke call in de audit staat, ook een geweigerde, zonder omgevingswaarden;
   - lange en parallelle exec-stromen werken, met de exitcode.
4. **S4: de voordeur.** Een passkey-proxy voor het board op een eigen origin, en een webhook-host.
   *Klaar als:*
   - het board alleen na passkey-login bereikbaar is, en de proxy zijn cookie weghaalt;
   - de WebSockets van het board door de proxy werken;
   - de webhook-host alleen de toegestane routes doorlaat;
   - de cookies van Paperclip en wsm niet botsen.
5. **S5: git-custody.**
   *Klaar als:*
   - de regiekamer de code als bundle uit de mirror krijgt;
   - een push-verzoek via de MCP tot een hersigneerde push naar een testremote leidt;
   - er in de sandbox en in de regiekamer geen git-credential staat.

### 13.3 Mijlpalen (na de spikes)

| | Oplevering |
|---|---|
| P1 | Tenant-API met peer-identiteit, plafondcontrole en audit; helper-uitbreiding voor regiekamer-gebruikers |
| P2 | Regiekamer als beheerd onderdeel: image, Quadlet, volume, egress, upgrades, en de CLI |
| P3 | Provider-plugin met leases, exec en synchronisatie; images volgens het contract; een run van begin tot eind met het nepmodel |
| P4 | Voordeur: boards en webhook-hosts; previews per sandbox |
| P5 | Git-custody: bundles naar de regiekamer, push-verzoeken met review, hersigneren en step-up |
| P6 | `low_trust_review`, de koppeling met de MCP-gateway, de audit-samenvatting, en alle isolatietests groen |

### 13.4 ADR's

Leg na de spikes elk besluit vast:
- Paperclip als regiekamer boven wsm (vervangt aanname 4);
- de tenant-API;
- een regiekamer per project;
- de provider-plugin en het leasemodel;
- push-verzoeken;
- de voordeur voor boards en webhooks;
- secrets en LLM-toegang via Paperclip (vervangt de secret-store en de harnas-home per project).

### 13.5 Nieuwe isolatietests

**Vanuit sandbox A moet dit mislukken:**
- verbinden met regiekamer A of B, op welke poort ook, of met een tenant-socket;
- een servergeheim van Paperclip vinden in de omgeving of in bestanden;
- via de bridge een route buiten de allowlist aanroepen, of een andere run-identiteit gebruiken;
- een archief met een symlink of `../` terugsturen;
- de GitHub-credential-route gebruiken.

**Vanuit regiekamer A moet dit mislukken:**
- een Podman-socket of een hostmap vinden;
- een sandbox van project B maken, gebruiken of verwijderen;
- het plafond wijzigen, een image buiten de allowlist gebruiken, of het maximum overschrijden;
- uitgaand verkeer buiten de allowlist;
- een run lokaal starten.

**Vanaf het internet moet dit mislukken:**
- het board bereiken zonder passkey-sessie;
- via de webhook-host iets anders bereiken dan een webhook-route.

De bestaande tests blijven gelden, zoals `TestNoCredentialsInSandbox` en `TestManagerAPINotReachable`.

### 13.6 Besluiten na spike S1

De eigenaar besloot op 24 november 2026:

- **Volgorde:** S3, S2, S4 en S5, daarna P1 tot en met P6. S3 gaat vóór S2, omdat de uitkomst van `SO_PEERCRED` door rootless Podman bepaalt hoe de container van de regiekamer moet draaien.
- **Scope:** de onderdelen uit fase 2 die dit plan nodig heeft, horen nu bij de scope:
  - push-verzoeken met review en hersigneren;
  - step-up;
  - goedkeuringen voor pushes en voor verzoeken buiten het plafond;
  - een maximum aantal sandboxes in het plafond.
- **Paperclip patchen mag,** met kleine, aparte patches in het regiekamer-image.
  - Vooral om lokale uitvoering te blokkeren (E11), en waar een instelling niet genoeg is ook voor workspace-services en de GitHub-credential-route (§9).
  - Elke patch wordt bij elke upgrade opnieuw getest.
  - Een bijdrage aan Paperclip zelf doet de eigenaar.
- **De open vragen uit S1:**
  1. *Gecommitte symlink naar een hostpad:* S2 test dit met een eigen scenario. Lekt hij, dan blokkeert de provider of een patch het.
  2. *Een geweigerd archief* kost de hele run (fail closed).
  3. *Geheimen in de sandbox:* alleen wat de run echt nodig heeft, en nooit GitHub-tokens (B4). S2 bepaalt per naam wat blijft.
  4. *Duplex* staat standaard aan. Een terugval op de bestandsbridge telt als fout.
  5. *Geen voorverwarming.* Alleen de allereerste sandbox op een nieuwe host was traag.

## 14. Open vragen

- Kan Paperclip workspace-services voor sandbox-omgevingen in de sandbox starten, in plaats van op de server? (E9)
- Is lokale uitvoering per instantie uit te zetten zonder patch? (E11)
- Hoe bereiken MCP-gatewaycalls van een agent de regiekamer: via de bridge of anders?
- Hoe roept Paperclip git aan op bundles van agents: met hooks, fsmonitor en submodules uit?
- Kan Paperclip een login van een vertrouwde proxy aannemen, zodat de dubbele login verdwijnt?
- Hoe rendert het board Markdown van agents: zonder ruwe HTML?
- Werkt `SO_PEERCRED` door rootless Podman heen met de juiste host-UID?
- Hoeveel geheugen en opstarttijd kost een regiekamer per project?
- Welke harnassen werken met de bridge over exec, en welke alleen met de native runner?

## Bronnen

**wsm**
- `docs/plan/eisen-workspace-manager.md`, `docs/plan/bouwplan-fase-1.md`, `docs/security.md`, de invarianten in `CLAUDE.md`.
- ADR 0001 (egress), 0005 (helper en beheer-API), 0009 (deploy keys), 0010 (code in de sandbox), 0011 en 0012 (MCP), 0014 (grants), 0016 (dashboard-login), 0017 (previews).
- `docs/plan/onderzoek-paperclip.md` (de hoofdkantoor-sandbox, U1–U9).

**Paperclip** (commit `cb4129a`, 23 november 2026)
- Grensmodel: `packages/plugins/sandbox-providers/SANDBOX-REQUIREMENTS.md`, `doc/plugins/SANDBOX_PROVIDER_CAPABILITIES.md`.
- Bridge en allowlist: `packages/adapter-utils/src/sandbox-callback-bridge.ts`.
- Leases: `packages/shared/src/types/environment.ts`, `server/src/services/environment-runtime.ts`, `server/src/services/environments.ts`.
- Kubernetes-provider: `packages/plugins/sandbox-providers/kubernetes/README.md`.
- Native runner: `packages/paperclip-runner/README.md`, `doc/runner-internals.md`.
- Git en GitHub: `doc/execution-github-identity.md`, `docs/guides/board-operator/execution-workspaces-and-runtime-services.md`, `doc/connections/GITHUB.md`.
- Vertrouwen: `doc/LOW-TRUST-PRESETS.md`, `doc/MCP-ACCESS-GOVERNANCE.md`, `doc/prompt-injection-prevention.md`.
- Login en modi: `doc/DEPLOYMENT-MODES.md`.
- Secrets en LLM-toegang: `docs/deploy/secrets.md`, `doc/connections/AI-CONNECTIONS.md`.
- Content en plugins: `doc/html-artifact-previews.md`, `doc/plugins/PLUGIN_SPEC.md`.
- Workspace-services: `server/src/services/local-service-supervisor.ts`, `server/src/services/heartbeat/runtime-services.ts`.
- Aannames goedkeuren: `packages/shared/src/validators/company.ts`.
- Licentie: `LICENSE` (MIT).
