<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { status, logout, type Status } from './api'
import { route, navigate } from './router'
import Login from './views/Login.vue'
import Enroll from './views/Enroll.vue'
import Projects from './views/Projects.vue'
import SandboxView from './views/SandboxView.vue'
import Audit from './views/Audit.vue'

const st = ref<Status | null>(null)
const failed = ref('')

async function refresh() {
  try {
    st.value = await status()
  } catch (e) {
    failed.value = String(e)
  }
}
onMounted(refresh)

const sandboxMatch = computed(() => /^\/p\/([^/]+)\/s\/([^/]+)$/.exec(route.path))

async function doLogout() {
  await logout()
  await refresh()
  navigate('/login')
}
</script>

<template>
  <div v-if="failed" class="center error">{{ failed }}</div>
  <div v-else-if="!st" class="center muted">loading…</div>
  <Enroll v-else-if="route.path === '/enroll'" @done="refresh" />
  <Login v-else-if="!st.logged_in" :enrolled="st.enrolled" @done="refresh" />
  <template v-else>
    <header>
      <strong>wsm</strong>
      <a :class="{ active: route.path === '/' || route.path.startsWith('/p/') }" @click="navigate('/')">Projects</a>
      <a :class="{ active: route.path === '/audit' }" @click="navigate('/audit')">Audit log</a>
      <span class="spacer"></span>
      <a @click="doLogout">Log out</a>
    </header>
    <main>
      <SandboxView v-if="sandboxMatch" :key="route.path" :project="decodeURIComponent(sandboxMatch[1])"
        :sandbox="decodeURIComponent(sandboxMatch[2])" />
      <Audit v-else-if="route.path === '/audit'" />
      <Projects v-else />
    </main>
  </template>
</template>
