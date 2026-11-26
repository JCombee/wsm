// The wsm sandbox provider for a contained control room: one ephemeral wsm
// sandbox per Paperclip lease, everything through wsm's tenant API on the
// control room's only socket. The plugin holds no security logic: the tenant
// API decides what this control room may do.
import { decodeChannelBytes, definePlugin, type PluginContext } from "@paperclipai/plugin-sdk";
import type {
  PluginEnvironmentAcquireLeaseParams,
  PluginEnvironmentDestroyLeaseParams,
  PluginEnvironmentExecuteParams,
  PluginEnvironmentExecuteResult,
  PluginEnvironmentInspectLeaseParams,
  PluginEnvironmentLease,
  PluginEnvironmentLeaseObservation,
  PluginEnvironmentProbeParams,
  PluginEnvironmentProbeResult,
  PluginEnvironmentRealizeWorkspaceParams,
  PluginEnvironmentRealizeWorkspaceResult,
  PluginEnvironmentReleaseLeaseParams,
  PluginEnvironmentResumeLeaseParams,
  PluginEnvironmentSyncInParams,
  PluginEnvironmentSyncOutParams,
  PluginEnvironmentSyncResult,
  PluginEnvironmentTerminationReceipt,
  PluginEnvironmentValidateConfigParams,
  PluginEnvironmentValidationResult,
} from "@paperclipai/plugin-sdk";
import { parseConfig as parseStrict, type WsmConfig } from "./config.ts";
import { DuplexChannels } from "./duplex.ts";
import { describe, runExec, setTenantClient, type ExecLog, type ExecSpec } from "./exec.ts";
import { Sandboxes } from "./sandboxes.ts";
import { syncIn, syncOut, type SyncContext } from "./sync.ts";
import { TenantClient } from "./tenant.ts";

export const WORKSPACE_DIR = "/workspace/paperclip";

let pluginContext: PluginContext | null = null;

const log: ExecLog = {
  info(msg) {
    pluginContext?.logger.info(msg);
  },
  warn(msg) {
    pluginContext?.logger.warn(msg);
  },
};

const tenant = new TenantClient();
setTenantClient(tenant);
const sandboxes = new Sandboxes(tenant, log);
const duplex = new DuplexChannels({
  sink: {
    data(hostRouteId, workerSessionId, chunk) {
      pluginContext?.duplexChannel.data(hostRouteId, workerSessionId, chunk);
    },
    exit(hostRouteId, workerSessionId, exitCode, transportClosed) {
      if (transportClosed === undefined) pluginContext?.duplexChannel.exit(hostRouteId, workerSessionId, exitCode);
      else pluginContext?.duplexChannel.exit(hostRouteId, workerSessionId, exitCode, transportClosed);
    },
  },
  log,
  cwd: WORKSPACE_DIR,
  deps: { open: (sandbox, spec, handlers) => tenant.openExec(sandbox, spec, handlers) },
});
const seenExtraKeys = new Set<string>();

/** parseConfig parses the host-built config of a lease or exec hook. */
function parseConfig(raw: unknown): WsmConfig {
  const extraKeys: string[] = [];
  const cfg = parseStrict(raw, { strict: false, extraKeys });
  const fresh = extraKeys.filter((k) => !seenExtraKeys.has(k));
  if (fresh.length > 0) {
    for (const k of fresh) seenExtraKeys.add(k);
    log.info(`s07 config: ignoring host-added keys [${fresh.sort().join(",")}]`);
  }
  return cfg;
}

function errorText(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

function pickTimeout(cfg: WsmConfig, params: { timeoutMs?: number; commandTimeoutMs?: number }): number {
  if (typeof params.timeoutMs === "number" && params.timeoutMs > 0) return params.timeoutMs;
  if (typeof params.commandTimeoutMs === "number" && params.commandTimeoutMs > 0) return params.commandTimeoutMs;
  return cfg.timeoutMs;
}

async function syncContext(params: PluginEnvironmentSyncInParams | PluginEnvironmentSyncOutParams, hook: string): Promise<SyncContext> {
  const cfg = parseConfig(params.config);
  const lease = await sandboxes.resolve(params.lease.providerLeaseId);
  const minimum = typeof params.minimumTimeoutMs === "number" && params.minimumTimeoutMs > 0 ? params.minimumTimeoutMs : 0;
  log.info(`s07 ${hook} sandbox=${lease.sandbox} operations=${params.operations.length}`);
  return { target: lease.target, timeoutMs: Math.max(cfg.timeoutMs, minimum), log };
}

async function release(params: PluginEnvironmentReleaseLeaseParams, hook: string): Promise<PluginEnvironmentTerminationReceipt | void> {
  if (params.providerLeaseId === null) return;
  duplex.closeLease(params.providerLeaseId);
  await sandboxes.release(params.providerLeaseId, hook);
  return { providerLeaseId: params.providerLeaseId, state: "destroyed" };
}

const plugin = definePlugin({
  async setup(ctx) {
    pluginContext = ctx;
    ctx.logger.info("s07 wsm sandbox provider ready (tenant API)");
  },

  async onHealth() {
    return { status: "ok", message: "wsm sandbox provider loaded" };
  },

  async onEnvironmentValidateConfig(params: PluginEnvironmentValidateConfigParams): Promise<PluginEnvironmentValidationResult> {
    try {
      const cfg = parseStrict(params.config);
      return { ok: true, normalizedConfig: { timeoutMs: cfg.timeoutMs } };
    } catch (e) {
      return { ok: false, errors: [errorText(e)] };
    }
  },

  async onEnvironmentProbe(params: PluginEnvironmentProbeParams): Promise<PluginEnvironmentProbeResult> {
    try {
      parseConfig(params.config);
      const me = await tenant.whoami();
      return { ok: true, summary: `wsm project ${me.project} through the tenant socket.`, metadata: { provider: "wsm", project: me.project } };
    } catch (e) {
      return { ok: false, summary: errorText(e) };
    }
  },

  async onEnvironmentAcquireLease(params: PluginEnvironmentAcquireLeaseParams): Promise<PluginEnvironmentLease> {
    parseConfig(params.config);
    const lease = await sandboxes.acquire();
    return {
      providerLeaseId: lease.leaseId,
      metadata: { provider: "wsm", sandbox: lease.sandbox, remoteCwd: WORKSPACE_DIR },
    };
  },

  async onEnvironmentResumeLease(params: PluginEnvironmentResumeLeaseParams): Promise<PluginEnvironmentLease> {
    parseConfig(params.config);
    const lease = await sandboxes.resolve(params.providerLeaseId, true);
    log.info(`s07 lease resume sandbox=${lease.sandbox}`);
    return {
      providerLeaseId: lease.leaseId,
      metadata: { provider: "wsm", sandbox: lease.sandbox, remoteCwd: WORKSPACE_DIR, resumedLease: true },
    };
  },

  async onEnvironmentReleaseLease(params: PluginEnvironmentReleaseLeaseParams) {
    return await release(params, "release");
  },

  async onEnvironmentDestroyLease(params: PluginEnvironmentDestroyLeaseParams) {
    return await release(params, "destroy");
  },

  async onEnvironmentInspectLease(params: PluginEnvironmentInspectLeaseParams): Promise<PluginEnvironmentLeaseObservation> {
    const state = await sandboxes.inspect(params.providerLeaseId);
    return { providerLeaseId: params.providerLeaseId, state };
  },

  async onEnvironmentRealizeWorkspace(params: PluginEnvironmentRealizeWorkspaceParams): Promise<PluginEnvironmentRealizeWorkspaceResult> {
    parseConfig(params.config);
    const lease = await sandboxes.resolve(params.lease.providerLeaseId);
    const r = await runExec({ target: lease.target, command: "mkdir", args: ["-p", "--", WORKSPACE_DIR], cwd: "/workspace", timeoutMs: 30_000 });
    if (r.exitCode !== 0) throw new Error(`could not create ${WORKSPACE_DIR} (exit ${r.exitCode})`);
    log.info(`s07 realize sandbox=${lease.sandbox} cwd=${WORKSPACE_DIR}`);
    return { cwd: WORKSPACE_DIR, metadata: { provider: "wsm", remoteCwd: WORKSPACE_DIR } };
  },

  async onEnvironmentExecute(params: PluginEnvironmentExecuteParams): Promise<PluginEnvironmentExecuteResult> {
    const cfg = parseConfig(params.config);
    const lease = await sandboxes.resolve(params.lease.providerLeaseId);
    const spec: ExecSpec = {
      target: lease.target,
      command: params.command,
      args: params.args ?? [],
      cwd: params.cwd || WORKSPACE_DIR,
      env: params.env ?? {},
      interactive: params.stdin !== undefined && params.stdin !== null,
      timeoutMs: pickTimeout(cfg, params),
    };
    log.info(`s07 exec hook=execute sandbox=${lease.sandbox} ${describe(spec)} stdin=${spec.interactive ? "yes" : "no"}`);
    const r = await runExec(spec, { stdin: params.stdin ?? undefined, log });
    return {
      exitCode: r.exitCode,
      signal: r.signal,
      timedOut: r.timedOut,
      stdout: r.stdout.toString("utf8"),
      stderr: r.stderr.toString("utf8"),
      metadata: { durationMs: r.durationMs },
    };
  },

  // Native sync: tar over exec, with every outbound archive member checked by
  // the strict reader in tar.ts before anything lands in the control room.
  async onEnvironmentSyncIn(params: PluginEnvironmentSyncInParams): Promise<PluginEnvironmentSyncResult> {
    const ctx = await syncContext(params, "sync-in");
    return { operations: await syncIn(ctx, params.operations) };
  },

  async onEnvironmentSyncOut(params: PluginEnvironmentSyncOutParams): Promise<PluginEnvironmentSyncResult> {
    const ctx = await syncContext(params, "sync-out");
    return { operations: await syncOut(ctx, params.operations) };
  },

  // Duplex bridge: one tenant exec stream per host route (duplex.ts). The open
  // params carry no driver config, so only a lease this worker acquired or
  // resolved before can open a channel.
  async onDuplexChannelOpen(params) {
    if (params.driverKey !== "wsm") throw new Error("wsm duplex channel: unexpected driver key");
    const lease = sandboxes.cached(params.providerLeaseId);
    if (!lease) throw new Error("wsm duplex channel: no lease of this worker matches the provider lease id");
    return await duplex.open(params, lease);
  },

  // Writes arrive as base64; a malformed or empty frame applies no bytes.
  async onDuplexChannelWrite(params) {
    const data = decodeChannelBytes(params.data);
    if (data === null) return;
    await duplex.write(params.hostRouteId, params.workerSessionId, data);
  },

  async onDuplexChannelStop(params) {
    await duplex.stop(params.hostRouteId, params.workerSessionId);
  },

  async onDuplexChannelClose(params) {
    return await duplex.close(params.hostRouteId);
  },

  async onShutdown() {
    await duplex.closeAll();
  },
});

export default plugin;
