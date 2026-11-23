// Sandbox lifecycle through the management API, and the mapping from a lease to
// an exec target. Leases are always ephemeral: acquire creates a fresh sandbox
// pc-<12 hex>, release deletes it.
import { AdminClient, podmanSocketFor } from "./admin.ts";
import { runExec, type ExecLog, type ExecTarget } from "./exec.ts";
import { formatLeaseId, newSandboxSlug, parseLeaseId, type LeaseRef } from "./lease.ts";

export const READY_TIMEOUT_MS = 60_000;

export interface ResolvedLease extends LeaseRef {
  leaseId: string;
  target: ExecTarget;
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

export class Sandboxes {
  admin: AdminClient;
  log: ExecLog;
  private cache = new Map<string, ResolvedLease>();

  constructor(admin: AdminClient, log: ExecLog) {
    this.admin = admin;
    this.log = log;
  }

  /** acquire creates and starts a sandbox and waits until exec works. */
  async acquire(project: string): Promise<ResolvedLease> {
    const t0 = Date.now();
    const p = await this.admin.project(project);
    const slug = newSandboxSlug();
    const sb = await this.admin.createSandbox(project, slug);
    const lease: ResolvedLease = {
      project,
      sandbox: slug,
      leaseId: formatLeaseId({ project, sandbox: slug }),
      target: { socket: podmanSocketFor(p.linuxUser), container: sb.container },
    };
    this.log.info(`s05 lease acquire begin sandbox=${slug} project=${project}`);
    try {
      await this.admin.startSandbox(project, slug);
      await this.waitReady(lease.target);
    } catch (e) {
      await this.admin.deleteSandbox(project, slug).catch(() => false);
      this.log.warn(`s05 lease acquire failed sandbox=${slug}; deleted`);
      throw e;
    }
    this.cache.set(lease.leaseId, lease);
    this.log.info(`s05 lease acquire sandbox=${slug} project=${project} ms=${Date.now() - t0}`);
    return lease;
  }

  private async waitReady(target: ExecTarget): Promise<void> {
    const deadline = Date.now() + READY_TIMEOUT_MS;
    for (;;) {
      const r = await runExec({ target, command: "true", timeoutMs: 10_000 }).catch(() => null);
      if (r && r.exitCode === 0) return;
      if (Date.now() > deadline) throw new Error("wsm sandbox did not accept exec within 60 s");
      await sleep(1000);
    }
  }

  /** resolve maps a lease ID to its exec target; the sandbox must exist. */
  async resolve(leaseId: unknown, project: string, requireRunning = false): Promise<ResolvedLease> {
    const ref = parseLeaseId(leaseId, project);
    const id = formatLeaseId(ref);
    const cached = this.cache.get(id);
    if (cached && !requireRunning) return cached;
    const [p, sb] = await Promise.all([this.admin.project(project), this.admin.sandbox(project, ref.sandbox)]);
    if (!sb) throw new Error(`wsm sandbox ${ref.sandbox} does not exist`);
    if (requireRunning && sb.status !== "running") throw new Error(`wsm sandbox ${ref.sandbox} is ${sb.status}, not running`);
    const lease: ResolvedLease = { ...ref, leaseId: id, target: { socket: podmanSocketFor(p.linuxUser), container: sb.container } };
    this.cache.set(id, lease);
    return lease;
  }

  /** release deletes the lease's sandbox. A missing sandbox is fine. */
  async release(leaseId: unknown, project: string, hook: string): Promise<LeaseRef> {
    const ref = parseLeaseId(leaseId, project);
    const id = formatLeaseId(ref);
    this.cache.delete(id);
    const deleted = await this.admin.deleteSandbox(ref.project, ref.sandbox);
    this.log.info(`s05 lease ${hook} sandbox=${ref.sandbox} project=${ref.project} result=${deleted ? "deleted" : "absent"}`);
    return ref;
  }

  /** inspect reads the sandbox state only. */
  async inspect(leaseId: unknown, project: string): Promise<"active" | "destroyed"> {
    const ref = parseLeaseId(leaseId, project);
    const sb = await this.admin.sandbox(ref.project, ref.sandbox);
    return sb ? "active" : "destroyed";
  }

  forget(leaseId: string): void {
    this.cache.delete(leaseId);
  }
}
