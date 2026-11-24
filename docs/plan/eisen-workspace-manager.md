# Eisen: Development Workspace Manager (MCP)

Status: bevestigd op 1 november 2026, na een grill-sessie. Bijgewerkt op 24 november 2026 met de besluiten over Paperclip (zie onderaan en `plan-paperclip.md`).

## Doel
- Self-hosted systeem dat afgeschermde ontwikkelomgevingen (sandboxes) beheert, puur bedoeld voor AI-agents.
- Bij een prompt-injectie reikt de schade nooit verder dan het eigen project.
- Eén gebruiker (alleen de eigenaar). Multi-user met ACL is geparkeerd.
- Wordt open source als we het zelf bouwen.

## Architectuur
- Draait op een eigen Linux-server (systemd, cgroups v2).
- In het midden zit een manager (core-API). Webdashboard en CLI zijn allebei clients van die API. De CLI kan volledig beheren en valt buiten de audit log.
- Container-runtime: rootless Podman, met een eigen Linux-gebruiker per project. Manager en gedeelde services draaien onder aparte gebruikers. Een container-escape raakt hooguit dat ene project.
- Het harnas (Claude Code, Codex, Gemini CLI, OpenCode, lokale Ollama-agents) draait ín de sandbox en wordt via het projectimage geïnstalleerd. De tool start of beheert geen agentsessies.
- Code leeft op de server. Op de client draait alleen een browser.

## Projecten, images en sandboxes
- Een project = repo + projectimage + gekozen service-blokken.
- Het projectimage (runtime, tooling, webserver, harnas) komt uit een van drie bronnen:
  1. een gedeeld image uit de bibliotheek (bijv. 'Laravel', 'Node'), optioneel met een eigen laag erbovenop (`FROM` het gedeelde image); per project een vaste versie pinnen of altijd de nieuwste volgen;
  2. een volledig eigen Containerfile, beheerd in het dashboard;
  3. een kant-en-klaar image uit een eigen registry.
- Een gedeeld image wordt één keer gebouwd en door alle projecten gebruikt die het kiezen. Builds draaien rootless en met dezelfde afscherming als een sandbox.
- Service-blokken zijn alleen nog services, elk met een eigen rechtenprofiel.
- Blokken, projectconfig en Containerfiles vormen samen het rechtenplafond. Ze staan in de manager, buiten bereik van agents, en zijn alleen via dashboard of CLI te wijzigen. Import/export maakt delen mogelijk.
- Meerdere sandboxes per project (per branch of taak), elk met eigen service-accounts en eigen data.
- Sandboxes starten leeg; het project haalt zelf zijn data op.
- Een sandbox bestaat uit de werkcontainer (gebouwd op het projectimage), de service-containers per sandbox en toegang tot de gedeelde services via de MCP.

## Services (v1)
- Gedeeld per versie, met per sandbox een eigen gebruiker/rol en eigen databases, index-prefix of vhost: MySQL, MariaDB, PostgreSQL, OpenSearch, Elasticsearch, RabbitMQ.
- Per sandbox: Redis/Valkey, Varnish, Mailpit.
- Standaard is alles dicht. Via de MCP krijgt een sandbox een account, netwerktoegang en credentials, met alleen de rechten die nodig zijn voor development.
- Binnen het plafond regelt de agent het zelf. Daarbuiten wordt het een goedkeuringsverzoek in het dashboard.

## MCP-tools voor agents
- Kern: zien wat binnen het plafond beschikbaar is, een service aanzetten, sandbox-info opvragen (o.a. de URL) en een verzoek indienen voor iets buiten het plafond.
- Extra:
  - status en logs van de eigen services;
  - eigen data resetten;
  - git via de manager (fetch/rebase, review + push aanvragen);
  - inspectietools (gestructureerde query's op eigen database, indexen en queues).

## Git, review en signing
- De manager beheert de remote. In de sandbox staan geen git-credentials; de agent commit alleen lokaal op de eigen sandbox-branch.
- Raakt een push geen gevoelige bestanden, dan hersigneert en pusht de manager automatisch en legt dat vast in de audit log.
- Raakt een push wel gevoelige bestanden, dan volgt een mini-MR in het dashboard: diff per commit en totaal, opmerkingen globaal en per regel. 'Request changes' gaat via de MCP terug naar de agent; 'approve' signeert en pusht.
- Standaard gevoelig:
  - CI/CD-config (.gitlab-ci.yml, .github/workflows/**, bitbucket-pipelines.yml, Deployer/Envoy);
  - env-templates en app-config (.env.example, Magento app/etc/config.php);
  - container- en buildbestanden (Dockerfile, compose, Makefile, Husky).
- De lijst met gevoelige bestanden is globaal, en per blok of project aan te vullen.
- Signeren gebeurt bij de push (de hashes veranderen pas dan), standaard met de eigen SSH/GPG-key. Per project is een andere key of identiteit in te stellen.

## Netwerk en toegang
- De tool sluit af: andere sandboxes, de interne services (behalve via MCP-toegang), het LAN en cloud-metadata.
- Internetverkeer valt onder de regels van het harnas, die de gebruiker zelf instelt.
- App-preview: elke sandbox krijgt een eigen HTTPS-subdomein (wildcard-certificaat), alleen bereikbaar na login. De URL staat als env-variabele in de sandbox.
- Dashboard en sandbox-content draaien op aparte origins.

## Dashboard
- Bestandsbrowser, Monaco-editor en terminal (geen volledige VS Code).
- Terminalsessies lopen server-side door (zoals bij tmux); opnieuw aansluiten kan vanaf elk apparaat.
- Inloggen met een passkey, met TOTP als fallback. Step-up voor goedkeuren, pushen en secrets.
- Meldingen via dashboard + Web Push, ntfy/Gotify, e-mail en chat (Telegram/Slack/Discord). Goedkeuren kan alleen in het dashboard.
- Secret-store per project, die geheimen als env-variabelen of bestanden (bijv. auth.json) injecteert bij de start van een sandbox. *Vervallen: zie de besluiten over Paperclip.*
- Harnas-home (met login) per project, gedeeld door de sandboxes van dat project. *Vervallen: zie de besluiten over Paperclip.*

## Resources en audit
- CPU-, RAM- en schijflimieten per sandbox.
- Auto-stop bij inactiviteit (geen proces-, terminal- of MCP-activiteit).
- Gedeelde services draaien alleen als ze nodig zijn.
- Maximum aantal actieve sandboxes.
- Audit log: append-only en buiten de sandboxes, met alle MCP-calls (ook geweigerde), alle verzoeken en de beslissingen daarover.

## Aannames (bevestigd)
2. Elke sandbox heeft een eigen MCP-endpoint. De projectidentiteit hangt aan de verbinding, nooit aan een tool-argument, en de MCP bevat geen beheertools.
3. Een verzoek geeft 'pending' terug; de agent kan de status opvragen of erop wachten.
4. De gebruiker maakt sandboxes aan en verwijdert ze. Binnen het plafond doet de regiekamer van het project dat ook, met audit (aangepast op 24 november 2026, besluit B1 in `plan-paperclip.md`). Bij verwijderen worden accounts, databases, indexen en vhosts opgeruimd; de remote branch blijft bestaan.
5. De manager draait git met hooks en fsmonitor uitgeschakeld.
6. Git werkt generiek via SSH of HTTPS met elke host; signeren kan met SSH of GPG.
7. Het databaseprofiel staat op de eigen databases triggers, views en routines toe (nodig voor Magento).
8. Registry-credentials voor private images staan in de manager, niet in de sandbox.
9. Wijzigt de Containerfile of de image-tag, dan worden sandboxes bij een herstart opnieuw aangemaakt. Code, data en de projecthome blijven bewaard.
10. De bibliotheek met Containerfiles en service-blokken is te exporteren en importeren.

(Aanname 1 is vervallen: er wordt niets uit blokken samengevoegd.)

## Besluiten na de zoektocht (1 november 2026)
- Zelf bouwen (open source); geen bestaande tool als basis.
- Backend in Go. Frontend in TypeScript met Monaco en xterm.js; Vue 3 als framework is een voorstel, nog te bevestigen.
- Het bouwplan voor fase 1 staat in `bouwplan-fase-1.md`.

## Besluiten over Paperclip (24 november 2026)
Het plan staat in `plan-paperclip.md`, met de besluiten in §13.1 en §13.6.
- Paperclip wordt de regiekamer boven wsm (B1, B2).
  - Er is één regiekamer per project, met een eigen gebruiker en container.
  - Ze krijgt alleen de tenant-API van het eigen project.
- De git-custody blijft bij wsm. Paperclips GitHub-functies staan uit (B4).
- Goedkeuringen worden gesplitst:
  - werk keurt de eigenaar goed in het board;
  - het plafond en pushen keurt de eigenaar goed in wsm, met step-up.
- Paperclips secrets en AI-verbindingen vervangen de secret-store en de harnas-home per project (B9, B10). Elke sandbox krijgt een eigen home.
- Het dashboard is voor beheer. Het dagelijkse werk gebeurt in het board.

## Uitgesteld
- Nog te kiezen: licentie (voorstel MIT), naam (voorstel "Sluis", nog te controleren) en installatie/distributie (voorstel in het bouwplan).
- Later: een database ophalen als basis voor een sandbox; multi-user/ACL.

## Kanttekeningen
- De harnasregels staan in de projecthome, waar de agent kan schrijven. Een geïnjecteerde agent kan ze dus losser zetten en credentials of secrets van het eigen project naar internet sturen. Andere projecten blijven buiten schot.
- Dependencies (composer/npm) vallen niet onder de review, terwijl CI gewoon projectcode uitvoert. Zet CI-secrets daarom alleen op protected branches.
