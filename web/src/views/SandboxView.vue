<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { get, del, sandboxPath, type Sandbox, type Enabled } from '../api'
import { navigate } from '../router'
import Terminal from '../components/Terminal.vue'
import Files from '../components/Files.vue'
import Audit from './Audit.vue'

const props = defineProps<{ project: string; sandbox: string }>()
const tab = ref<'terminal' | 'files' | 'services' | 'audit'>('terminal')
const info = ref<Sandbox | null>(null)
const services = ref<Enabled[]>([])
const err = ref('')

async function load() {
  try {
    const all = await get<Sandbox[]>(`/v1/sandboxes?project=${encodeURIComponent(props.project)}`)
    info.value = all.find((s) => s.slug === props.sandbox) ?? null
    services.value = await get<Enabled[]>(sandboxPath(props.project, props.sandbox) + '/services')
  } catch (e) {
    err.value = String(e)
  }
}
onMounted(load)

async function revoke(block: string) {
  if (!window.confirm(`Take ${block} away from this sandbox? Its account and databases are dropped.`)) return
  try {
    await del(sandboxPath(props.project, props.sandbox) + '/services/' + encodeURIComponent(block))
  } catch (e) {
    err.value = String(e)
  }
  await load()
}
</script>

<template>
  <div class="row" style="justify-content: space-between">
    <h1>
      <a @click="navigate('/')">{{ project }}</a> / {{ sandbox }}
      <span v-if="info" :class="`status-${info.status}`" style="font-size: .9rem">{{ info.status }}</span>
    </h1>
    <a v-if="info?.preview_url" :href="info.preview_url" target="_blank" rel="noopener noreferrer">Open preview</a>
  </div>
  <p v-if="err" class="error">{{ err }}</p>
  <div class="tabs">
    <button :class="{ active: tab === 'terminal' }" @click="tab = 'terminal'">Terminal</button>
    <button :class="{ active: tab === 'files' }" @click="tab = 'files'">Files</button>
    <button :class="{ active: tab === 'services' }" @click="tab = 'services'; load()">Services</button>
    <button :class="{ active: tab === 'audit' }" @click="tab = 'audit'">Audit</button>
  </div>
  <p v-if="info && info.status !== 'running' && (tab === 'terminal' || tab === 'files')" class="muted">
    The sandbox is not running; start it on the projects page.
  </p>
  <Terminal v-else-if="tab === 'terminal'" :project="project" :sandbox="sandbox" />
  <Files v-else-if="tab === 'files'" :project="project" :sandbox="sandbox" />
  <div v-if="tab === 'services'" class="card">
    <h2>Services this sandbox enabled</h2>
    <p class="muted">Agents enable services through the MCP. Passwords are never shown here.</p>
    <table>
      <thead><tr><th>Block</th><th>Tag</th><th>Address</th><th>User</th><th>Database</th><th></th></tr></thead>
      <tbody>
        <tr v-for="s in services" :key="s.block">
          <td>{{ s.block }}</td><td>{{ s.tag }}</td><td class="mono">{{ s.host }}:{{ s.port }}</td>
          <td class="mono">{{ s.user }}</td><td class="mono">{{ s.database }}</td>
          <td><button class="danger" @click="revoke(s.block)">Revoke</button></td>
        </tr>
      </tbody>
    </table>
  </div>
  <Audit v-if="tab === 'audit' && info" :sandbox-id="info.id" />
</template>
