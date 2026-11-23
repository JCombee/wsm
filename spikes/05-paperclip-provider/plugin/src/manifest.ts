import type { PaperclipPluginManifestV1 } from "@paperclipai/plugin-sdk";

const manifest: PaperclipPluginManifestV1 = {
  id: "wsm.s05-sandbox-provider",
  apiVersion: 1,
  version: "0.1.0",
  displayName: "wsm sandbox provider (spike 05)",
  description: "Runs agent commands in hardened wsm sandboxes over the project's Podman socket. Spike code.",
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
      description: "An ephemeral wsm sandbox per run.",
      defaultAcquireTimeoutMs: 120_000,
      configSchema: {
        type: "object",
        additionalProperties: false,
        required: ["project"],
        properties: {
          project: {
            type: "string",
            description: "The wsm project slug.",
            pattern: "^[a-z]([a-z0-9-]{0,30}[a-z0-9])?$",
          },
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
        nativeSyncIn: false,
        nativeSyncOut: false,
        duplexCommandStream: false,
      },
    },
  ],
};

export default manifest;
