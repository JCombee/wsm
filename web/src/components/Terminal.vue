<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'
import { sandboxPath } from '../api'

// The terminal protocol (internal/terminal): binary frames carry terminal
// bytes both ways; text frames from us are {"type":"resize",...}.
const props = defineProps<{ project: string; sandbox: string }>()
const el = ref<HTMLDivElement>()
const state = ref('connecting…')
let term: Terminal | undefined
let ws: WebSocket | undefined
let ro: ResizeObserver | undefined

onMounted(() => {
  term = new Terminal({ cursorBlink: true, fontSize: 13, fontFamily: 'ui-monospace, monospace', scrollback: 5000 })
  const fit = new FitAddon()
  term.loadAddon(fit)
  term.open(el.value!)
  fit.fit()
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
  const url = `${proto}//${location.host}/api${sandboxPath(props.project, props.sandbox)}/terminal?rows=${term.rows}&cols=${term.cols}`
  ws = new WebSocket(url)
  ws.binaryType = 'arraybuffer'
  const encoder = new TextEncoder()
  ws.onopen = () => {
    state.value = ''
    term!.focus()
  }
  ws.onmessage = (ev) => {
    if (ev.data instanceof ArrayBuffer) term!.write(new Uint8Array(ev.data))
  }
  ws.onclose = (ev) => {
    state.value = `disconnected${ev.reason ? ': ' + ev.reason : ''}; the tmux session keeps running — reload to reconnect`
  }
  term.onData((d) => ws?.readyState === WebSocket.OPEN && ws.send(encoder.encode(d)))
  term.onBinary((d) => {
    const b = new Uint8Array(d.length)
    for (let i = 0; i < d.length; i++) b[i] = d.charCodeAt(i) & 0xff
    if (ws?.readyState === WebSocket.OPEN) ws.send(b)
  })
  term.onResize(({ rows, cols }) => {
    if (ws?.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: 'resize', rows, cols }))
  })
  ro = new ResizeObserver(() => fit.fit())
  ro.observe(el.value!)
})

onBeforeUnmount(() => {
  ro?.disconnect()
  ws?.close()
  term?.dispose()
})
</script>

<template>
  <p v-if="state" class="muted">{{ state }}</p>
  <div ref="el" style="height: 70vh; background: #000; padding: 4px; border-radius: 4px"></div>
</template>
