// catalog.mjs <distribution-dir> <key> <plugin-key> <directory>
//
// Writes <distribution-dir>/catalog.json, Paperclip's image-owned plugin
// catalog (server/src/services/distribution-plugin-catalog.ts at 2026.1117.0).
// Paperclip reads it at every boot under PAPERCLIP_BUNDLED_PLUGIN_ROOT, checks
// the bundle against the digest and refuses to start on a mismatch. With
// `plugins.autoInstall: [<key>]` in PAPERCLIP_MANAGED_CONFIG it installs the
// bundle itself, and a managed instance activates no other plugin.
//
// The digest is Paperclip's distributionBundleDigest: the bundle's files in
// sorted order as [relative path, sha256 of the bytes, mode & 0o777], hashed
// as JSON. It covers the file modes, so this runs after the final chmod. The
// limits are Paperclip's: no symlinks, at most 10,000 files and 256 MiB.
import fs from "node:fs";
import path from "node:path";
import { createHash } from "node:crypto";

const [root, key, pluginKey, directory] = process.argv.slice(2);
if (!root || !key || !pluginKey || !directory) {
  console.error("usage: catalog.mjs <distribution-dir> <key> <plugin-key> <directory>");
  process.exit(2);
}
const bundle = path.join(root, directory);
const hash = (value) => `sha256:${createHash("sha256").update(value).digest("hex")}`;
const inventory = [];
let bytes = 0;

function walk(dir, relative = "") {
  if (relative.split("/").length > 32) throw new Error("bundle exceeds Paperclip's directory depth limit");
  for (const name of fs.readdirSync(dir).sort()) {
    const relativePath = relative ? `${relative}/${name}` : name;
    const file = path.join(dir, name);
    const stat = fs.lstatSync(file);
    if (stat.isSymbolicLink()) throw new Error(`bundles cannot hold symlinks: ${relativePath}`);
    if (stat.isDirectory()) walk(file, relativePath);
    else if (stat.isFile()) {
      bytes += stat.size;
      if (bytes > 256 * 1024 * 1024 || inventory.length >= 10_000) throw new Error("bundle exceeds Paperclip's verification limits");
      inventory.push([relativePath, hash(fs.readFileSync(file)), stat.mode & 0o777]);
    } else throw new Error(`bundles hold only files and directories: ${relativePath}`);
  }
}

walk(bundle);
if (inventory.length === 0) throw new Error("bundle is empty");
const pkg = JSON.parse(fs.readFileSync(path.join(bundle, "package.json"), "utf8"));
const digest = hash(JSON.stringify(inventory));
const catalog = { schemaVersion: 1, plugins: [{ key, pluginKey, version: pkg.version, directory, digest }] };
fs.writeFileSync(path.join(root, "catalog.json"), `${JSON.stringify(catalog, null, 2)}\n`);
console.log(`key=${key} pluginKey=${pluginKey} version=${pkg.version} digest=${digest} files=${inventory.length} bytes=${bytes}`);
