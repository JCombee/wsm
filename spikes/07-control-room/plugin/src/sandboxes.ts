// Sandbox lifecycle through the tenant API. Leases are always ephemeral:
// acquire creates a fresh sandbox, release deletes it. The tenant API chooses
// the sandbox ID and answers a create only once the sandbox runs a command.
import type { ExecLog, ExecTarget } from "./exec.ts";
import { formatLeaseId, parseLeaseId } from "./lease.ts";
import type { TenantClient } from "./tenant.ts";

export interface ResolvedLease {
  leaseId: string;
  sandbox: string;
  target: ExecTarget;
}

function leaseFor(sandbox: string): ResolvedLease {
  return { leaseId: formatLeaseId(sandbox), sandbox, target: { sandbox } };
}

export class Sandboxes {
  tenant: TenantClient;
  log: ExecLog;
  private cache = new Map<string, ResolvedLease>();

  constructor(tenant: TenantClient, log: ExecLog) {
    this.tenant = tenant;
    this.log = log;
  }

  /** acquire creates a sandbox; the tenant API returns it running. */
  async acquire(): Promise<ResolvedLease> {
    const t0 = Date.now();
    const sb = await this.tenant.create();
    if (sb.status !== "running") {
      await this.tenant.remove(sb.id).catch(() => false);
      throw new Error(`wsm sandbox ${sb.id} is ${sb.status} after create, not running`);
    }
    const lease = leaseFor(sb.id);
    this.cache.set(lease.leaseId, lease);
    this.log.info(`s07 lease acquire sandbox=${sb.id} ms=${Date.now() - t0}`);
    return lease;
  }

  /** resolve maps a lease ID to its sandbox; the tenant must have it. */
  async resolve(leaseId: unknown, requireRunning = false): Promise<ResolvedLease> {
    const sandbox = parseLeaseId(leaseId);
    const lease = leaseFor(sandbox);
    const cached = this.cache.get(lease.leaseId);
    if (cached && !requireRunning) return cached;
    const sb = await this.tenant.get(sandbox);
    if (!sb) throw new Error(`wsm sandbox ${sandbox} does not exist`);
    if (requireRunning && sb.status !== "running") throw new Error(`wsm sandbox ${sandbox} is ${sb.status}, not running`);
    this.cache.set(lease.leaseId, lease);
    return lease;
  }

  /** release deletes the lease's sandbox. A missing sandbox is fine. */
  async release(leaseId: unknown, hook: string): Promise<string> {
    const sandbox = parseLeaseId(leaseId);
    this.cache.delete(formatLeaseId(sandbox));
    const deleted = await this.tenant.remove(sandbox);
    this.log.info(`s07 lease ${hook} sandbox=${sandbox} result=${deleted ? "deleted" : "absent"}`);
    return sandbox;
  }

  /** inspect reads the sandbox state only. */
  async inspect(leaseId: unknown): Promise<"active" | "destroyed"> {
    const sb = await this.tenant.get(parseLeaseId(leaseId));
    return sb ? "active" : "destroyed";
  }

  /** cached returns a lease this worker acquired or resolved, or null. */
  cached(leaseId: unknown): ResolvedLease | null {
    return typeof leaseId === "string" ? (this.cache.get(leaseId) ?? null) : null;
  }
}
