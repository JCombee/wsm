<script setup lang="ts">
import { ref } from 'vue'
import { register } from '../webauthn'
import { route, navigate } from '../router'

const emit = defineEmits<{ done: [] }>()
const name = ref('')
const busy = ref(false)
const err = ref('')
const token = route.query.get('token') ?? ''

async function go() {
  busy.value = true
  err.value = ''
  try {
    await register(token, name.value)
    navigate('/')
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
    <h1>Register a passkey</h1>
    <p v-if="!token" class="error">This link has no token. Run <span class="mono">wsm auth enroll</span> on the server.</p>
    <template v-else>
      <label><span>Name (optional)</span><input v-model="name" maxlength="64" placeholder="e.g. laptop" /></label>
      <button class="primary" :disabled="busy" @click="go">Create passkey</button>
    </template>
    <p v-if="err" class="error">{{ err }}</p>
  </div>
</template>
