<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, watch } from 'vue'
import * as monaco from 'monaco-editor/editor/editor.api'
import 'monaco-editor/basic-languages/monaco.contribution'
import EditorWorker from 'monaco-editor/editor/editor.worker?worker'

// Only the core editor and syntax highlighting: file contents come from the
// sandbox and are shown as text, never as HTML (CLAUDE.md invariant 11).
;(self as any).MonacoEnvironment = { getWorker: () => new EditorWorker() }

const props = defineProps<{ value: string; path: string }>()
const emit = defineEmits<{ save: [value: string]; dirty: [dirty: boolean] }>()
const el = ref<HTMLDivElement>()
let editor: monaco.editor.IStandaloneCodeEditor | undefined
let original = ''

function language(path: string): string {
  const name = path.split('/').pop() ?? ''
  const ext = name.includes('.') ? '.' + name.split('.').pop()!.toLowerCase() : ''
  for (const l of monaco.languages.getLanguages()) {
    if (l.filenames?.includes(name) || (ext && l.extensions?.includes(ext))) return l.id
  }
  return 'plaintext'
}

function load(value: string, path: string) {
  original = value
  const old = editor!.getModel()
  editor!.setModel(monaco.editor.createModel(value, language(path)))
  old?.dispose()
  emit('dirty', false)
}

onMounted(() => {
  editor = monaco.editor.create(el.value!, { automaticLayout: true, minimap: { enabled: false }, fontSize: 13 })
  load(props.value, props.path)
  editor.onDidChangeModelContent(() => emit('dirty', editor!.getValue() !== original))
  editor.addCommand(monaco.KeyMod.CtrlCmd | monaco.KeyCode.KeyS, () => emit('save', editor!.getValue()))
})

watch(() => [props.value, props.path] as const, ([v, p]) => editor && load(v, p))

defineExpose({
  value: () => editor?.getValue() ?? '',
  saved: () => {
    original = editor?.getValue() ?? ''
    emit('dirty', false)
  },
})

onBeforeUnmount(() => {
  editor?.getModel()?.dispose()
  editor?.dispose()
})
</script>

<template>
  <div ref="el" style="height: 70vh; border: 1px solid var(--line)"></div>
</template>
