// Provider lease IDs have the form wsm:<project>/<sandbox>. The sandbox slug is
// always pc-<12 hex>, created by this plugin. Parsing is strict: a lease that
// names another project than the driver config is an error.
import { randomBytes } from "node:crypto";
import { SLUG_RE } from "./config.ts";

export const SANDBOX_SLUG_RE = /^pc-[0-9a-f]{12}$/;
const LEASE_RE = /^wsm:([a-z0-9-]{1,32})\/(pc-[0-9a-f]{12})$/;

export interface LeaseRef {
  project: string;
  sandbox: string;
}

export class LeaseError extends Error {}

export function newSandboxSlug(): string {
  return `pc-${randomBytes(6).toString("hex")}`;
}

export function formatLeaseId(ref: LeaseRef): string {
  if (!SLUG_RE.test(ref.project) || !SANDBOX_SLUG_RE.test(ref.sandbox)) {
    throw new LeaseError("invalid lease reference");
  }
  return `wsm:${ref.project}/${ref.sandbox}`;
}

/** parseLeaseId parses a provider lease ID and checks it belongs to project. */
export function parseLeaseId(id: unknown, project: string): LeaseRef {
  if (typeof id !== "string" || id.length > 64) throw new LeaseError("malformed wsm lease id");
  const m = LEASE_RE.exec(id);
  if (!m || !m[1] || !m[2] || !SLUG_RE.test(m[1])) throw new LeaseError("malformed wsm lease id");
  if (m[1] !== project) throw new LeaseError("wsm lease belongs to another project");
  return { project: m[1], sandbox: m[2] };
}
