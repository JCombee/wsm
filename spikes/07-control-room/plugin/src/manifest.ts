import type { PaperclipPluginManifestV1 } from "@paperclipai/plugin-sdk";

const manifest: PaperclipPluginManifestV1 = {
  id: "wsm.s07-sandbox-provider",
  apiVersion: 1,
  version: "0.1.0",
  displayName: "wsm sandbox provider (spike 07)",
  description: "Runs agent commands in hardened wsm sandboxes through wsm's tenant API. Spike code.",
  author: "wsm",
  categories: ["automation"],
  capabilities: ["environment.drivers.register"],
  entrypoints: {
    worker: "./dist/worker.js",
  },
  environmentDrivers: [
    {
      driverKey: "wsm",
      kind: "sandbox_provider",
      displayName: "wsm sandbox",
      description: "An ephemeral wsm sandbox per run, through the tenant API.",
      defaultAcquireTimeoutMs: 120_000,
      configSchema: {
        type: "object",
        additionalProperties: false,
        properties: {
          timeoutMs: {
            type: "number",
            description: "Default command timeout in milliseconds.",
            default: 900000,
          },
          reuseLease: { type: "boolean", const: false, description: "Leases are always ephemeral." },
          archiveOnRelease: { type: "boolean", description: "Ignored: sandboxes are always deleted on release." },
        },
      },
      sandboxCapabilities: {
        reusableLeases: false,
        nativeSyncIn: true,
        nativeSyncOut: true,
        duplexCommandStream: true,
      },
    },
  ],
};

export default manifest;
