<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { get, put, sandboxPath, b64ToBytes, bytesToB64, type Entry } from '../api'
import Monaco from './Monaco.vue'

const props = defineProps<{ project: string; sandbox: string }>()
const dir = ref('/workspace')
const entries = ref<Entry[]>([])
const truncated = ref(false)
const file = ref<{ path: string; text: string } | null>(null)
const dirty = ref(false)
const err = ref('')
const note = ref('')
const editor = ref<InstanceType<typeof Monaco>>()
const base = () => sandboxPath(props.project, props.sandbox) + '/files'

function join(d: string, name: string) {
  return d === '/' ? '/' + name : d + '/' + name
}

async function open(path: string) {
  err.value = ''
  try {
    const r = await get<{ path: string; entries: Entry[]; truncated?: boolean }>(`${base()}/list?path=${encodeURIComponent(path)}`)
    dir.value = path
    entries.value = r.entries
    truncated.value = !!r.truncated
  } catch (e) {
    err.value = String(e)
  }
}

async function openFile(path: string) {
  if (dirty.value && !window.confirm('Discard unsaved changes?')) return
  err.value = ''
  note.value = ''
  try {
    const r = await get<{ content: string }>(`${base()}/content?path=${encodeURIComponent(path)}`)
    const bytes = b64ToBytes(r.content)
    let text: string
    try {
      text = new TextDecoder('utf-8', { fatal: true }).decode(bytes)
    } catch {
      err.value = `${path} is not UTF-8 text; it is not shown.`
      return
    }
    file.value = { path, text }
  } catch (e) {
    err.value = String(e)
  }
}

async function save(text?: string) {
  if (!file.value) return
  const value = text ?? editor.value?.value() ?? ''
  err.value = ''
  try {
    await put(`${base()}/content?path=${encodeURIComponent(file.value.path)}`, {
      content: bytesToB64(new TextEncoder().encode(value)),
    })
    editor.value?.saved()
    note.value = `saved ${new Date().toLocaleTimeString()}`
  } catch (e) {
    err.value = String(e)
  }
}

async function newFile() {
  const name = window.prompt('New file name')
  if (!name || name.includes('/')) return
  const path = join(dir.value, name)
  try {
    await put(`${base()}/content?path=${encodeURIComponent(path)}`, { content: '' })
    await open(dir.value)
    await openFile(path)
  } catch (e) {
    err.value = String(e)
  }
}

function up() {
  const i = dir.value.lastIndexOf('/')
  open(i <= 0 ? '/' : dir.value.slice(0, i))
}

onMounted(() => open(dir.value))
</script>

<template>
  <p v-if="err" class="error">{{ err }}</p>
  <div style="display: grid; grid-template-columns: 300px 1fr; gap: 1rem">
    <div class="card" style="max-height: 75vh; overflow: auto; margin: 0">
      <div class="row" style="margin-bottom: .4rem">
        <button @click="up" :disabled="dir === '/'">..</button>
        <span class="mono" style="word-break: break-all">{{ dir }}</span>
      </div>
      <div class="row" style="margin-bottom: .4rem">
        <button @click="open(dir)">Refresh</button><button @click="newFile">New file</button>
      </div>
      <div v-for="e in entries" :key="e.name">
        <a v-if="e.type === 'dir'" class="mono" @click="open(join(dir, e.name))">{{ e.name }}/</a>
        <a v-else-if="e.type === 'file'" class="mono" @click="openFile(join(dir, e.name))">{{ e.name }}</a>
        <span v-else class="mono muted">{{ e.name }} ({{ e.type }})</span>
      </div>
      <p v-if="truncated" class="muted">Only the first 2000 entries are shown.</p>
    </div>
    <div>
      <template v-if="file">
        <div class="row" style="margin-bottom: .4rem">
          <span class="mono">{{ file.path }}</span>
          <span v-if="dirty" class="muted">(modified)</span>
          <button class="primary" :disabled="!dirty" @click="save()">Save (Ctrl+S)</button>
          <span class="muted">{{ note }}</span>
        </div>
        <Monaco ref="editor" :value="file.text" :path="file.path" @save="save" @dirty="dirty = $event" />
      </template>
      <p v-else class="muted">Pick a file. Files up to 512 KiB of UTF-8 text can be edited.</p>
    </div>
  </div>
</template>
