// Provider lease IDs have the form wsm:<sandbox>. The sandbox ID is the one the
// tenant API handed out: "t" and ten lowercase letters or digits. The project
// is the tenant socket's, so a lease ID never names one.

export const SANDBOX_ID_RE = /^t[a-z0-9]{10}$/;
const LEASE_RE = /^wsm:(t[a-z0-9]{10})$/;

export class LeaseError extends Error {}

export function formatLeaseId(sandbox: string): string {
  if (!SANDBOX_ID_RE.test(sandbox)) throw new LeaseError("invalid sandbox id");
  return `wsm:${sandbox}`;
}

/** parseLeaseId returns the sandbox ID of a provider lease ID. */
export function parseLeaseId(id: unknown): string {
  if (typeof id !== "string" || id.length > 64) throw new LeaseError("malformed wsm lease id");
  const m = LEASE_RE.exec(id);
  if (!m || !m[1]) throw new LeaseError("malformed wsm lease id");
  return m[1];
}
