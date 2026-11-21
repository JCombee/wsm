<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { get, post, put, del, enc, sandboxPath, type Project, type Sandbox, type Block, type Service } from '../api'
import { navigate } from '../router'

const projects = ref<Project[]>([])
const sandboxes = ref<Sandbox[]>([])
const blocks = ref<Record<string, Block[]>>({})
const services = ref<Service[]>([])
const err = ref('')
const busy = ref('')

async function load() {
  try {
    projects.value = await get<Project[]>('/v1/projects')
    sandboxes.value = await get<Sandbox[]>('/v1/sandboxes')
    const b: Record<string, Block[]> = {}
    for (const p of projects.value) b[p.slug] = await get<Block[]>(`/v1/projects/${enc(p.slug)}/blocks`)
    blocks.value = b
    services.value = await get<Service[]>('/v1/services')
  } catch (e) {
    err.value = String(e)
  }
}
onMounted(load)

// run performs one action and reloads; long ones (image builds) show a
// busy label.
async function run(label: string, fn: () => Promise<unknown>) {
  busy.value = label
  err.value = ''
  try {
    await fn()
  } catch (e) {
    err.value = `${label}: ${e instanceof Error ? e.message : e}`
  } finally {
    busy.value = ''
    await load()
  }
}

const form = reactive({ slug: '', name: '', source: 'image', image: '', containerfile: '', repo: '', branch: '',
  cpus: '', memoryGiB: '', pids: '', appPort: '' })
const showCreate = ref(false)

function createProject() {
  const body: Record<string, unknown> = { slug: form.slug, name: form.name || undefined }
  if (form.source === 'image') body.image = form.image
  else body.containerfile = form.containerfile
  if (form.repo) { body.repo_url = form.repo; if (form.branch) body.base_branch = form.branch }
  if (form.cpus) body.cpu_millis = Math.round(parseFloat(form.cpus) * 1000)
  if (form.memoryGiB) body.mem_bytes = Math.round(parseFloat(form.memoryGiB) * 1024 ** 3)
  if (form.pids) body.pids_limit = parseInt(form.pids, 10)
  if (form.appPort) body.app_port = parseInt(form.appPort, 10)
  return run(`create project ${form.slug}`, async () => {
    await post('/v1/projects', body)
    showCreate.value = false
  })
}

const newSandbox = reactive<Record<string, string>>({})
const newBlock = reactive<Record<string, { block: string; tags: string; profile: string }>>({})
function blockForm(slug: string) {
  return (newBlock[slug] ??= { block: '', tags: '', profile: '' })
}
const keys = reactive<Record<string, string>>({})
const ask = (msg: string) => window.confirm(msg)

function sandboxesOf(slug: string) {
  return sandboxes.value.filter((s) => s.project === slug)
}
function gib(n: number) {
  return (n / 1024 ** 3).toFixed(n % 1024 ** 3 ? 2 : 0)
}
</script>

<template>
  <div class="row" style="justify-content: space-between">
    <h1>Projects</h1>
    <button class="primary" @click="showCreate = !showCreate">New project</button>
  </div>
  <p v-if="busy" class="muted">{{ busy }}… (building or pulling an image can take minutes)</p>
  <p v-if="err" class="error">{{ err }}</p>

  <div v-if="showCreate" class="card">
    <h2>New project</h2>
    <label><span>Slug</span><input v-model="form.slug" placeholder="shop" /></label>
    <label><span>Name</span><input v-model="form.name" /></label>
    <label><span>Image source</span>
      <select v-model="form.source"><option value="image">Registry image</option><option value="containerfile">Containerfile</option></select>
    </label>
    <label v-if="form.source === 'image'"><span>Image</span><input v-model="form.image" size="50" placeholder="docker.io/library/node:22" /></label>
    <label v-else><span>Containerfile</span><textarea v-model="form.containerfile" rows="10"></textarea></label>
    <label><span>Repository (SSH)</span><input v-model="form.repo" size="50" placeholder="git@github.com:org/repo.git" /></label>
    <label v-if="form.repo"><span>Base branch</span><input v-model="form.branch" placeholder="main" /></label>
    <label><span>CPUs</span><input v-model="form.cpus" placeholder="2" size="6" /></label>
    <label><span>Memory (GiB)</span><input v-model="form.memoryGiB" placeholder="2" size="6" /></label>
    <label><span>Process limit</span><input v-model="form.pids" placeholder="1024" size="6" /></label>
    <label><span>App port (preview)</span><input v-model="form.appPort" placeholder="none" size="6" /></label>
    <button class="primary" :disabled="!!busy" @click="createProject">Create</button>
  </div>

  <div v-for="p in projects" :key="p.id" class="card">
    <div class="row" style="justify-content: space-between">
      <h2>{{ p.name }} <span class="muted mono">{{ p.slug }}</span></h2>
      <div class="row">
        <button :disabled="!!busy" @click="run(`image ${p.slug}`, () => post(`/v1/projects/${enc(p.slug)}/image`))">Rebuild image</button>
        <button v-if="p.repo_url" @click="run(`key ${p.slug}`, async () => { keys[p.slug] = (await get<{ public_key: string }>(`/v1/projects/${enc(p.slug)}/key`)).public_key })">Deploy key</button>
        <button class="danger" :disabled="!!busy" @click="ask(`Delete project ${p.slug}?`) && run(`delete ${p.slug}`, () => del(`/v1/projects/${enc(p.slug)}`))">Delete</button>
      </div>
    </div>
    <p class="muted">
      {{ p.image }} · {{ p.cpu_millis / 1000 }} CPU · {{ gib(p.mem_bytes) }} GiB · {{ p.pids_limit }} pids · user {{ p.linux_user }}
      <template v-if="p.repo_url"> · {{ p.repo_url }} ({{ p.base_branch }})</template>
      <template v-if="p.app_port"> · app port {{ p.app_port }}</template>
    </p>
    <p v-if="keys[p.slug]" class="mono" style="word-break: break-all">{{ keys[p.slug] }}</p>

    <table>
      <thead><tr><th>Sandbox</th><th>Status</th><th>Branch</th><th>Preview</th><th></th></tr></thead>
      <tbody>
        <tr v-for="s in sandboxesOf(p.slug)" :key="s.id">
          <td><a @click="navigate(`/p/${enc(p.slug)}/s/${enc(s.slug)}`)">{{ s.slug }}</a></td>
          <td :class="`status-${s.status}`">{{ s.status }}</td>
          <td class="mono">{{ s.branch }}</td>
          <td><a v-if="s.preview_url" :href="s.preview_url" target="_blank" rel="noopener noreferrer">{{ s.preview_url }}</a></td>
          <td class="row">
            <button v-if="s.status !== 'running'" :disabled="!!busy" @click="run(`start ${s.slug}`, () => post(sandboxPath(p.slug, s.slug) + '/start'))">Start</button>
            <button v-else :disabled="!!busy" @click="run(`stop ${s.slug}`, () => post(sandboxPath(p.slug, s.slug) + '/stop'))">Stop</button>
            <button class="danger" :disabled="!!busy" @click="ask(`Delete sandbox ${s.slug}?`) && run(`delete ${s.slug}`, () => del(sandboxPath(p.slug, s.slug)))">Delete</button>
          </td>
        </tr>
      </tbody>
    </table>
    <div class="row" style="margin-top: .6rem">
      <input v-model="newSandbox[p.slug]" placeholder="new sandbox slug" />
      <button :disabled="!!busy || !newSandbox[p.slug]" @click="run(`create sandbox ${newSandbox[p.slug]}`, () => post(`/v1/projects/${enc(p.slug)}/sandboxes`, { slug: newSandbox[p.slug] }))">Create sandbox</button>
    </div>

    <h2 style="margin-top: 1rem">Services allowed (ceiling)</h2>
    <table v-if="blocks[p.slug]?.length">
      <thead><tr><th>Block</th><th>Profile</th><th>Tags</th><th></th></tr></thead>
      <tbody>
        <tr v-for="b in blocks[p.slug]" :key="b.block">
          <td>{{ b.block }}</td><td>{{ b.profile }}</td><td>{{ b.tags.join(', ') }}</td>
          <td><button class="danger" @click="run(`remove ${b.block}`, () => del(`/v1/projects/${enc(p.slug)}/blocks/${enc(b.block)}`))">Remove</button></td>
        </tr>
      </tbody>
    </table>
    <div class="row" style="margin-top: .4rem">
      <input v-model="blockForm(p.slug).block" placeholder="block, e.g. mariadb" size="12" />
      <input v-model="blockForm(p.slug).tags" placeholder="tags, e.g. 11.4, 10.11" size="18" />
      <input v-model="blockForm(p.slug).profile" placeholder="profile (default)" size="14" />
      <button :disabled="!!busy || !blockForm(p.slug).block" @click="run(`set ${blockForm(p.slug).block}`, () => put(`/v1/projects/${enc(p.slug)}/blocks/${enc(blockForm(p.slug).block)}`, { profile: blockForm(p.slug).profile || undefined, tags: blockForm(p.slug).tags.split(',').map((t) => t.trim()).filter(Boolean) }))">Allow</button>
    </div>
  </div>

  <div class="card">
    <h2>Shared service instances</h2>
    <table>
      <thead><tr><th>Block</th><th>Tag</th><th>Address</th><th>State</th><th>Version</th></tr></thead>
      <tbody>
        <tr v-for="s in services" :key="s.block + s.tag">
          <td>{{ s.block }}</td><td>{{ s.tag }}</td><td class="mono">{{ s.address }}:{{ s.port }}</td>
          <td :class="s.state === 'ready' ? 'status-running' : 'status-failed'">{{ s.state }}</td><td>{{ s.version }}</td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
