<script setup lang="ts">
import { ref } from 'vue'
import { login } from '../webauthn'
import { route, navigate, safeNext } from '../router'

defineProps<{ enrolled: boolean }>()
const emit = defineEmits<{ done: [] }>()
const busy = ref(false)
const err = ref('')

async function go() {
  busy.value = true
  err.value = ''
  try {
    await login()
    const next = safeNext(route.query.get('next'))
    // /preview-auth is served by the server, not by this app.
    if (next.startsWith('/preview-auth')) {
      location.assign(next)
      return
    }
    navigate(next === '/login' ? '/' : next)
    emit('done')
  } catch (e) {
    err.value = e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="center card">
    <h1>wsm</h1>
    <p v-if="!enrolled" class="muted">
      No passkey is registered yet. On the server, run <span class="mono">wsm auth enroll</span> and open the link it prints.
    </p>
    <button class="primary" :disabled="busy" @click="go">Log in with a passkey</button>
    <p v-if="err" class="error">{{ err }}</p>
  </div>
</template>
