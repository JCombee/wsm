# Onderzoek: bestaande tools vergeleken met de eisen

Datum: 1 november 2026. De eisen staan in `eisen-workspace-manager.md`.

## Conclusie
Geen enkele bestaande tool dekt de kern van de eisen:
- least-privilege accounts per sandbox op gedeelde services, uitgedeeld via MCP;
- goedkeuringsverzoeken voor alles buiten het plafond;
- een manager die git beheert, commits hersigneert en alleen bij gevoelige paden om een menselijke review vraagt.

Als platform komt Coder het dichtst bij. Advies: zelf bouwen (open source) en waar het kan bestaande onderdelen hergebruiken.

## Vergelijking

| Tool | Agent in sandbox | Webdashboard | Isolatie | Service-broker + goedkeuring | Git zonder creds / review / signing | Open source |
|---|---|---|---|---|---|---|
| Coder Community | ja | ja (web-UI, terminal, VS Code) | Docker/K8s; geen gebruiker per project | nee (alleen statisch via Terraform) | nee | AGPL-3.0; audit log en auto-stop alleen in Premium |
| Docker Sandboxes (sbx) | ja | nee (lokale CLI/TUI) | microVM met eigen kernel | nee (wel credential-proxy) | deels (creds buiten de VM; geen review of signing) | nee, closed source + Docker-login |
| container-use (Dagger) | nee, harnas op de host via MCP-tools | nee | rootful (Dagger) | deels (services per omgeving; geen gedeelde accounts) | deels (branch per omgeving; geen signing) | Apache-2.0, experimenteel |
| OpenSandbox | ja | nee (SDK/CLI) | optioneel gVisor/Kata/Firecracker | nee (Credential Vault voor HTTP) | nee | Apache-2.0, nog geen stabiele API |
| poddle | ja | deels (audit-dashboard) | Podman | deels (relay voor Postgres/Redis; geen MySQL/OpenSearch/RabbitMQ; geen goedkeuring) | deels (git-creds gebrokerd; geen review of signing) | AGPL-3.0, erg jong |
| Kleine rootless-Podman-tools | ja | deels (één heeft een webterminal) | rootless onder de eigen gebruiker | nee | nee | ja (diverse licenties) |

Daytona valt af: het is sinds juni 2026 closed source en het repo is op 3 oktober 2026 gearchiveerd.

## Notities per tool
- **Coder** (github.com/coder/coder): de Community-editie bevat web-UI, CLI, API, agent-taken en OIDC. Audit logging, resource-quota's en auto-stop van inactieve workspaces zitten in Premium. Workspaces worden gedefinieerd met Terraform-templates. Agent Boundaries (nu agent-firewall) heeft een open-source CLI: github.com/coder/boundary.
- **Docker Sandboxes** (docs.docker.com/ai/sandboxes): elke agent draait in een microVM met eigen kernel en eigen Docker-engine. Credentials worden buiten de VM geproxied, YAML-kits leggen de configuratie vast en MCP-servers zijn per sandbox aan te zetten. Nadelen: closed source, een Docker-login is nodig, en op Linux wordt alleen Ubuntu 24.04+ met KVM genoemd.
- **container-use** (github.com/dagger/container-use): MCP-server die per agent een container plus een eigen git-branch aanmaakt, met services per omgeving (environment_add_service). Het harnas draait op de host. Met Podman vereist Dagger rootful uitvoering.
- **OpenSandbox** (github.com/opensandbox-group/OpenSandbox): control plane met SDK's, CLI, MCP-tools, Docker/K8s, egress-policy en Credential Vault. Multi-tenancy werkt alleen op Kubernetes.
- **poddle** (github.com/datadir-lab/poddle): een broker houdt de echte credentials en pods krijgen alleen handles. Er is een relay op verbindingsniveau voor Postgres en Redis, en de audit log is hash-chained.
- **Rootless-Podman-tools:**
  - agent-pod: een eigen home-volume per project;
  - another-ai-sandbox: GPL-3.0, met een webdashboard en browserterminal;
  - agentsbox en ags;
  - ca-mantis-shrimp/agent-sandbox: legt een harnas-laag over de Containerfile van de repo;
  - agentic-sandbox: KVM of rootless, met een webdashboard.

## Herbruikbare onderdelen
- **OpenBao** (Vault-fork): ingebouwde plugins voor dynamische credentials voor MySQL, PostgreSQL, Redis/Valkey en RabbitMQ. Elasticsearch/OpenSearch zit er niet in. Advies voor v1: de adapters direct in de manager bouwen; OpenBao eventueel later toevoegen voor leases en intrekking.
- **coder/boundary**: open-source netwerkfilter per proces. Kan als optionele egress-laag in de projectimages, passend bij de keuze dat internetverkeer onder de harnasregels valt.
- **Ideeën om over te nemen:**
  - container-use: de MCP-toolset en een branch per omgeving;
  - agent-pod: een home per project en een verificatiescript;
  - Docker Sandboxes: de credential-proxy en YAML-kits;
  - DDEV-MCP: default-deny whitelist voor inspectietools;
  - db-mcp-gateway: geauditeerde read-only query's.

## Alternatief
Coder Community als basis gebruiken en zelf alleen de broker en de git-manager bouwen. Dat scheelt dashboard- en terminalwerk, maar kost rootless Podman per project, auto-stop (Premium) en de keuze voor alleen Monaco.

## Volgende stap
De uitgestelde bouwbeslissingen: stack, licentie, naam en installatie.
