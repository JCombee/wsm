import { runWorker } from "@paperclipai/plugin-sdk";
import plugin from "./plugin.ts";

export default plugin;
runWorker(plugin, import.meta.url);
