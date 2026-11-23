// The wsm sandbox provider: one ephemeral wsm sandbox per Paperclip lease,
// commands over `podman --remote exec` on the project's socket.
import { definePlugin, type PluginContext } from "@paperclipai/plugin-sdk";
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
  PluginEnvironmentTerminationReceipt,
  PluginEnvironmentValidateConfigParams,
  PluginEnvironmentValidationResult,
} from "@paperclipai/plugin-sdk";
import { AdminClient } from "./admin.ts";
import { parseConfig as parseStrict, type WsmConfig } from "./config.ts";
import { describe, runExec, type ExecLog, type ExecSpec } from "./exec.ts";
import { Sandboxes } from "./sandboxes.ts";

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

const sandboxes = new Sandboxes(new AdminClient(), log);
const seenExtraKeys = new Set<string>();

/** parseConfig parses the host-built config of a lease or exec hook. */
function parseConfig(raw: unknown): WsmConfig {
  const extraKeys: string[] = [];
  const cfg = parseStrict(raw, { strict: false, extraKeys });
  const fresh = extraKeys.filter((k) => !seenExtraKeys.has(k));
  if (fresh.length > 0) {
    for (const k of fresh) seenExtraKeys.add(k);
    log.info(`s05 config: ignoring host-added keys [${fresh.sort().join(",")}]`);
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

async function release(params: PluginEnvironmentReleaseLeaseParams, hook: string): Promise<PluginEnvironmentTerminationReceipt | void> {
  if (params.providerLeaseId === null) return;
  const cfg = parseConfig(params.config);
  await sandboxes.release(params.providerLeaseId, cfg.project, hook);
  return { providerLeaseId: params.providerLeaseId, state: "destroyed" };
}

const plugin = definePlugin({
  async setup(ctx) {
    pluginContext = ctx;
    ctx.logger.info("s05 wsm sandbox provider ready");
  },

  async onHealth() {
    return { status: "ok", message: "wsm sandbox provider loaded" };
  },

  async onEnvironmentValidateConfig(params: PluginEnvironmentValidateConfigParams): Promise<PluginEnvironmentValidationResult> {
    try {
      const cfg = parseStrict(params.config);
      return { ok: true, normalizedConfig: { project: cfg.project, timeoutMs: cfg.timeoutMs } };
    } catch (e) {
      return { ok: false, errors: [errorText(e)] };
    }
  },

  async onEnvironmentProbe(params: PluginEnvironmentProbeParams): Promise<PluginEnvironmentProbeResult> {
    try {
      const cfg = parseConfig(params.config);
      const p = await sandboxes.admin.project(cfg.project);
      return { ok: true, summary: `wsm project ${p.slug} is available.`, metadata: { provider: "wsm", project: p.slug } };
    } catch (e) {
      return { ok: false, summary: errorText(e) };
    }
  },

  async onEnvironmentAcquireLease(params: PluginEnvironmentAcquireLeaseParams): Promise<PluginEnvironmentLease> {
    const cfg = parseConfig(params.config);
    const lease = await sandboxes.acquire(cfg.project);
    return {
      providerLeaseId: lease.leaseId,
      metadata: { provider: "wsm", project: lease.project, sandbox: lease.sandbox, remoteCwd: WORKSPACE_DIR },
    };
  },

  async onEnvironmentResumeLease(params: PluginEnvironmentResumeLeaseParams): Promise<PluginEnvironmentLease> {
    const cfg = parseConfig(params.config);
    const lease = await sandboxes.resolve(params.providerLeaseId, cfg.project, true);
    log.info(`s05 lease resume sandbox=${lease.sandbox}`);
    return {
      providerLeaseId: lease.leaseId,
      metadata: { provider: "wsm", project: lease.project, sandbox: lease.sandbox, remoteCwd: WORKSPACE_DIR, resumedLease: true },
    };
  },

  async onEnvironmentReleaseLease(params: PluginEnvironmentReleaseLeaseParams) {
    return await release(params, "release");
  },

  async onEnvironmentDestroyLease(params: PluginEnvironmentDestroyLeaseParams) {
    return await release(params, "destroy");
  },

  async onEnvironmentInspectLease(params: PluginEnvironmentInspectLeaseParams): Promise<PluginEnvironmentLeaseObservation> {
    const cfg = parseConfig(params.config);
    const state = await sandboxes.inspect(params.providerLeaseId, cfg.project);
    return { providerLeaseId: params.providerLeaseId, state };
  },

  async onEnvironmentRealizeWorkspace(params: PluginEnvironmentRealizeWorkspaceParams): Promise<PluginEnvironmentRealizeWorkspaceResult> {
    const cfg = parseConfig(params.config);
    const lease = await sandboxes.resolve(params.lease.providerLeaseId, cfg.project);
    const r = await runExec({ target: lease.target, command: "mkdir", args: ["-p", "--", WORKSPACE_DIR], cwd: "/workspace", timeoutMs: 30_000 });
    if (r.exitCode !== 0) throw new Error(`could not create ${WORKSPACE_DIR} (exit ${r.exitCode})`);
    log.info(`s05 realize sandbox=${lease.sandbox} cwd=${WORKSPACE_DIR}`);
    return { cwd: WORKSPACE_DIR, metadata: { provider: "wsm", remoteCwd: WORKSPACE_DIR } };
  },

  async onEnvironmentExecute(params: PluginEnvironmentExecuteParams): Promise<PluginEnvironmentExecuteResult> {
    const cfg = parseConfig(params.config);
    const lease = await sandboxes.resolve(params.lease.providerLeaseId, cfg.project);
    const spec: ExecSpec = {
      target: lease.target,
      command: params.command,
      args: params.args ?? [],
      cwd: params.cwd || WORKSPACE_DIR,
      env: params.env ?? {},
      interactive: params.stdin !== undefined && params.stdin !== null,
      timeoutMs: pickTimeout(cfg, params),
    };
    log.info(`s05 exec hook=execute sandbox=${lease.sandbox} ${describe(spec)} stdin=${spec.interactive ? "yes" : "no"}`);
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
});

export default plugin;
