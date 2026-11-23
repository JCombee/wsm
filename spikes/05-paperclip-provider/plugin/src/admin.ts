// Client for the wsm management API on /run/wsm/api.sock. In this spike it
// stands in for the tenant API (plan §5.6): the Paperclip user is in group
// wsm-admin on the disposable VM only. Every name that comes back is checked
// against a strict pattern before it is used in a socket path or an exec.
import http from "node:http";
import { SLUG_RE } from "./config.ts";
import { SANDBOX_SLUG_RE } from "./lease.ts";

export const ADMIN_SOCKET = "/run/wsm/api.sock";
export const LINUX_USER_RE = /^wsm-p-[0-9]+$/;
export const POD_RE = /^sbx-[a-z0-9]{12}$/;

const MAX_RESPONSE = 1 << 20;
const REQUEST_TIMEOUT_MS = 180_000;

export class AdminError extends Error {
  status: number;
  constructor(message: string, status: number) {
    super(message);
    this.status = status;
  }
}

export interface ProjectInfo {
  slug: string;
  linuxUser: string;
}

export interface SandboxInfo {
  slug: string;
  status: string;
  pod: string;
  container: string;
}

/** podmanSocketFor returns the project's Podman socket for a validated user. */
export function podmanSocketFor(linuxUser: string): string {
  if (!LINUX_USER_RE.test(linuxUser)) throw new AdminError("unexpected wsm linux user", 0);
  return `/run/wsm-podman/${linuxUser}/podman.sock`;
}

function isObject(v: unknown): v is Record<string, unknown> {
  return v !== null && typeof v === "object" && !Array.isArray(v);
}

/** parseProject picks one project out of a GET /v1/projects body. */
export function parseProject(body: unknown, slug: string): ProjectInfo | null {
  if (!Array.isArray(body)) throw new AdminError("projects response is not a list", 0);
  for (const p of body) {
    if (!isObject(p) || p.slug !== slug) continue;
    const user = p.linux_user;
    if (typeof user !== "string" || !LINUX_USER_RE.test(user)) {
      throw new AdminError("unexpected wsm linux user", 0);
    }
    return { slug, linuxUser: user };
  }
  return null;
}

/** parseSandbox validates one sandbox object from the management API. */
export function parseSandbox(body: unknown): SandboxInfo {
  if (!isObject(body)) throw new AdminError("sandbox response is not an object", 0);
  const { slug, status, pod } = body;
  if (typeof slug !== "string" || !SANDBOX_SLUG_RE.test(slug)) throw new AdminError("unexpected sandbox slug", 0);
  if (typeof status !== "string" || !/^[a-z]{1,16}$/.test(status)) throw new AdminError("unexpected sandbox status", 0);
  if (typeof pod !== "string" || !POD_RE.test(pod)) throw new AdminError("unexpected pod name", 0);
  return { slug, status, pod, container: `${pod}-work` };
}

interface Reply {
  status: number;
  body: unknown;
}

export class AdminClient {
  socketPath: string;
  constructor(socketPath: string = ADMIN_SOCKET) {
    this.socketPath = socketPath;
  }

  request(method: string, path: string, body?: unknown): Promise<Reply> {
    const payload = body === undefined ? undefined : Buffer.from(JSON.stringify(body));
    return new Promise((resolve, reject) => {
      const req = http.request(
        {
          socketPath: this.socketPath,
          method,
          path,
          host: "wsm",
          headers: payload ? { "content-type": "application/json", "content-length": payload.length } : {},
          timeout: REQUEST_TIMEOUT_MS,
        },
        (res) => {
          const chunks: Buffer[] = [];
          let size = 0;
          res.on("data", (c: Buffer) => {
            size += c.length;
            if (size > MAX_RESPONSE) {
              req.destroy(new AdminError("wsm API response too large", 0));
              return;
            }
            chunks.push(c);
          });
          res.on("end", () => {
            const text = Buffer.concat(chunks).toString("utf8");
            let parsed: unknown = null;
            if (text.length > 0) {
              try {
                parsed = JSON.parse(text);
              } catch {
                reject(new AdminError(`wsm API ${method} returned invalid JSON (status ${res.statusCode})`, res.statusCode ?? 0));
                return;
              }
            }
            resolve({ status: res.statusCode ?? 0, body: parsed });
          });
          res.on("error", reject);
        },
      );
      req.on("timeout", () => req.destroy(new AdminError("wsm API request timed out", 0)));
      req.on("error", reject);
      req.end(payload);
    });
  }

  private static fail(what: string, r: Reply): AdminError {
    const msg = isObject(r.body) && typeof r.body.error === "string" ? r.body.error.slice(0, 200) : "";
    return new AdminError(`wsm API ${what}: status ${r.status}${msg ? `: ${msg}` : ""}`, r.status);
  }

  async project(slug: string): Promise<ProjectInfo> {
    if (!SLUG_RE.test(slug)) throw new AdminError("invalid project slug", 0);
    const r = await this.request("GET", "/v1/projects");
    if (r.status !== 200) throw AdminClient.fail("list projects", r);
    const p = parseProject(r.body, slug);
    if (!p) throw new AdminError(`wsm project ${slug} not found`, 404);
    return p;
  }

  /** sandbox returns the sandbox, or null when it does not exist. */
  async sandbox(project: string, slug: string): Promise<SandboxInfo | null> {
    if (!SLUG_RE.test(project) || !SANDBOX_SLUG_RE.test(slug)) throw new AdminError("invalid slug", 0);
    const r = await this.request("GET", `/v1/sandboxes?project=${project}`);
    if (r.status !== 200) throw AdminClient.fail("list sandboxes", r);
    if (!Array.isArray(r.body)) throw new AdminError("sandboxes response is not a list", 0);
    for (const sb of r.body) {
      if (isObject(sb) && sb.slug === slug) return parseSandbox(sb);
    }
    return null;
  }

  async listSandboxSlugs(project: string): Promise<string[]> {
    if (!SLUG_RE.test(project)) throw new AdminError("invalid slug", 0);
    const r = await this.request("GET", `/v1/sandboxes?project=${project}`);
    if (r.status !== 200) throw AdminClient.fail("list sandboxes", r);
    if (!Array.isArray(r.body)) throw new AdminError("sandboxes response is not a list", 0);
    return r.body.flatMap((sb) => (isObject(sb) && typeof sb.slug === "string" ? [sb.slug] : []));
  }

  async createSandbox(project: string, slug: string): Promise<SandboxInfo> {
    if (!SLUG_RE.test(project) || !SANDBOX_SLUG_RE.test(slug)) throw new AdminError("invalid slug", 0);
    const r = await this.request("POST", `/v1/projects/${project}/sandboxes`, { slug });
    if (r.status !== 201) throw AdminClient.fail("create sandbox", r);
    return parseSandbox(r.body);
  }

  async startSandbox(project: string, slug: string): Promise<void> {
    if (!SLUG_RE.test(project) || !SANDBOX_SLUG_RE.test(slug)) throw new AdminError("invalid slug", 0);
    const r = await this.request("POST", `/v1/projects/${project}/sandboxes/${slug}/start`);
    if (r.status !== 204) throw AdminClient.fail("start sandbox", r);
  }

  /** deleteSandbox deletes a sandbox. It returns false when it was already gone. */
  async deleteSandbox(project: string, slug: string): Promise<boolean> {
    if (!SLUG_RE.test(project) || !SANDBOX_SLUG_RE.test(slug)) throw new AdminError("invalid slug", 0);
    const r = await this.request("DELETE", `/v1/projects/${project}/sandboxes/${slug}`);
    if (r.status === 404) return false;
    if (r.status !== 204) throw AdminClient.fail("delete sandbox", r);
    return true;
  }
}
