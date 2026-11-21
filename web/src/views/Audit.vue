<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { get, type AuditEvent } from '../api'

const props = defineProps<{ sandboxId?: string }>()
const events = ref<AuditEvent[]>([])
const more = ref(true)
const err = ref('')

async function load(before = 0) {
  try {
    const q = new URLSearchParams({ limit: '100' })
    if (before) q.set('before', String(before))
    if (props.sandboxId) q.set('sandbox', props.sandboxId)
    const page = await get<AuditEvent[]>('/v1/audit?' + q)
    events.value = before ? [...events.value, ...page] : page
    more.value = page.length === 100
  } catch (e) {
    err.value = String(e)
  }
}
onMounted(() => load())
</script>

<template>
  <div class="card">
    <h1 v-if="!sandboxId">Audit log</h1>
    <p class="muted">Every MCP call, including denied ones. The chain can be checked with <span class="mono">wsm audit verify</span>.</p>
    <p v-if="err" class="error">{{ err }}</p>
    <table>
      <thead><tr><th>#</th><th>Time</th><th>Actor</th><th>Tool</th><th>Result</th><th>Arguments</th><th>Detail</th></tr></thead>
      <tbody>
        <tr v-for="e in events" :key="e.seq">
          <td>{{ e.seq }}</td>
          <td class="mono">{{ e.ts.replace('T', ' ').slice(0, 19) }}</td>
          <td class="mono">{{ e.actor }}</td>
          <td>{{ e.tool }}</td>
          <td :class="e.result === 'ok' ? 'status-running' : 'status-failed'">{{ e.result }}</td>
          <td class="mono" style="max-width: 360px; word-break: break-all">{{ e.args }}</td>
          <td>{{ e.detail }}</td>
        </tr>
      </tbody>
    </table>
    <button v-if="more && events.length" style="margin-top: .6rem" @click="load(events[events.length - 1].seq)">Older</button>
  </div>
</template>
