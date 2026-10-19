<script setup lang="ts">
import { onMounted, onUnmounted, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import {
  Call24Regular,
  CallInbound24Regular,
  CallMissed24Regular,
  CallOutbound24Regular,
  PersonAdd24Regular
} from '@vicons/fluent'
import type { PhoneRecord } from '../services/phone'
import { usePhoneStore } from '../stores/phone'
import { usePhoneIdentity } from '../composables/usePhoneIdentity'
import { phoneContactsService } from '../services/phone-contacts'
import { formatCallTime, formatRecordDuration, phoneErrorMessage, phoneRecordStatusLabel } from '../utils/phone'

const props = defineProps<{ records: PhoneRecord[] }>()
const identities = usePhoneIdentity()

const phone = usePhoneStore()

watch(() => props.records.map((item) => `${item.device_id}\u0000${item.peer}`).join('|'), () => {
  for (const record of props.records) {
    if (record.peer) void identities.resolve(record.peer, record.device_id)
  }
}, { immediate: true })

async function saveContact(record: PhoneRecord) {
  const peer = String(record.peer || '').trim()
  if (!peer) return
  const current = identities.identityFor(peer, record.device_id)
  try {
    const { value } = await ElMessageBox.prompt('保存后，来电和通话记录会显示这个名字', '加到联系人', {
      confirmButtonText: '保存',
      cancelButtonText: '取消',
      inputPlaceholder: '联系人名字',
      inputValue: current?.name || '',
      inputValidator: (v) => !!String(v || '').trim() || '请填写名字'
    })
    const ident = await phoneContactsService.save({
      number: peer,
      name: String(value).trim(),
      deviceId: record.device_id,
      contactId: current?.contact_id
    })
    identities.upsertLocal(ident, peer, record.device_id)
    ElMessage.success('已保存联系人')
  } catch (error) {
    if (error === 'cancel' || error === 'close') return
    ElMessage.error(error instanceof Error ? error.message : '保存联系人失败')
  }
}
// 录音播放：用原生 <audio controls> 展示，进度条/拖动/音量/时长都是浏览器
// 自带的，不再自己维护播放状态。录音接口需要 Bearer 鉴权，<audio src> 带不了
// Authorization 头，所以 src 仍是 blob：滚动到可见时才异步换取，避免一次拉上百
// 条录音。
const recordingSources = ref<Record<number, string>>({})
const recordingErrors = ref<Record<number, string>>({})
const loadingRecordings = new Map<number, string>()
const recordingNames = new Map<number, string>()
const recordingErrorNames = new Map<number, string>()
const audioElements = new Map<number, HTMLAudioElement>()
const redialing = ref(false)
let observer: IntersectionObserver | null = null

function withoutRecord<T>(values: Record<number, T>, id: number): Record<number, T> {
  return Object.fromEntries(Object.entries(values).filter(([key]) => Number(key) !== id)) as Record<number, T>
}

function statusIcon(record: PhoneRecord) {
  if (record.status === 'missed' || record.status === 'rejected' || record.status === 'busy' || record.status === 'failed') {
    return CallMissed24Regular
  }
  return record.direction === 'inbound' ? CallInbound24Regular : CallOutbound24Regular
}

// 回拨：复用拨号流程，直接以该记录的对端号码发起双向呼叫。
function redial(record: PhoneRecord) {
  const peer = String(record.peer || '').trim()
  if (!peer || redialing.value || phone.currentCall) return
  redialing.value = true
  void phone.startCall(record.device_id, peer)
    .catch((error) => {
      ElMessage.error(error instanceof Error ? error.message : '回拨失败')
    })
    .finally(() => { redialing.value = false })
}

async function ensureRecording(record: PhoneRecord) {
  const name = String(record.recording_name || '').trim()
  if (!name) return
  if (recordingSources.value[record.id] && recordingNames.get(record.id) === name) return
  if (loadingRecordings.get(record.id) === name) return
  loadingRecordings.set(record.id, name)
  recordingErrors.value = withoutRecord(recordingErrors.value, record.id)
  recordingErrorNames.delete(record.id)
  try {
    const url = await phone.recordingURL(name)
    const current = props.records.find((item) => item.id === record.id)
    if (loadingRecordings.get(record.id) !== name || current?.recording_name !== name) {
      URL.revokeObjectURL(url)
      return
    }
    releaseRecording(record.id)
    recordingNames.set(record.id, name)
    recordingSources.value = { ...recordingSources.value, [record.id]: url }
  } catch (error) {
    if (loadingRecordings.get(record.id) === name) {
      recordingErrors.value = {
        ...recordingErrors.value,
        [record.id]: phoneErrorMessage(error, '录音加载失败')
      }
      recordingErrorNames.set(record.id, name)
    }
  } finally {
    if (loadingRecordings.get(record.id) === name) loadingRecordings.delete(record.id)
  }
}

function releaseRecording(id: number) {
  const url = recordingSources.value[id]
  if (url) URL.revokeObjectURL(url)
  recordingSources.value = withoutRecord(recordingSources.value, id)
  recordingNames.delete(id)
}

function syncRecordingState() {
  const current = new Map(props.records.map((record) => [record.id, String(record.recording_name || '').trim()]))
  for (const [id, name] of recordingNames) {
    if (current.get(id) !== name) releaseRecording(id)
  }
  const errors = Object.fromEntries(Object.entries(recordingErrors.value).filter(([value]) => {
    const id = Number(value)
    return current.get(id) === recordingErrorNames.get(id)
  }))
  recordingErrors.value = errors
  for (const id of recordingErrorNames.keys()) {
    if (current.get(id) !== recordingErrorNames.get(id)) recordingErrorNames.delete(id)
  }
  observeRecordings()
}

function registerAudio(record: PhoneRecord, element: unknown) {
  const audio = element as HTMLAudioElement | null
  if (!audio) {
    const previous = audioElements.get(record.id)
    if (previous) observer?.unobserve(previous)
    audioElements.delete(record.id)
    return
  }
  audioElements.set(record.id, audio)
  observer?.observe(audio)
}

function observeRecordings() {
  if (!observer) return
  for (const element of audioElements.values()) observer.observe(element)
}

onMounted(() => {
  if (typeof IntersectionObserver === 'undefined') {
    for (const record of props.records) void ensureRecording(record)
    return
  }
  observer = new IntersectionObserver((entries) => {
    for (const entry of entries) {
      if (!entry.isIntersecting) continue
      const element = entry.target as HTMLAudioElement
      const id = Number(element.dataset.recordId || '')
      const record = props.records.find((item) => item.id === id)
      if (record) void ensureRecording(record)
      observer?.unobserve(element)
    }
  }, { rootMargin: '200px' })
  observeRecordings()
})

watch(() => props.records, syncRecordingState, { flush: 'post' })

onUnmounted(() => {
  observer?.disconnect()
  observer = null
  for (const url of Object.values(recordingSources.value)) URL.revokeObjectURL(url)
  recordingSources.value = {}
  recordingErrors.value = {}
  recordingNames.clear()
  recordingErrorNames.clear()
  loadingRecordings.clear()
  audioElements.clear()
})
</script>

<template>
  <section class="history-panel" aria-labelledby="phone-history-title">
    <header>
      <div>
        <span>CALL LOG</span>
        <h2 id="phone-history-title">最近通话</h2>
      </div>
      <strong>{{ records.length }}</strong>
    </header>
    <div v-if="records.length" class="history-list">
      <article v-for="record in records" :key="record.call_id" class="history-item">
        <div class="history-icon" :class="`is-${record.status}`">
          <el-icon><component :is="statusIcon(record)" /></el-icon>
        </div>
        <div class="history-copy">
          <div class="history-primary">
            <strong>{{ identities.titleFor(record.peer, record.device_id) }}</strong>
            <time>{{ formatCallTime(record.started_at) }}</time>
          </div>
          <div class="history-secondary">
            <span :class="`status-${record.status}`">{{ phoneRecordStatusLabel(record) }}</span>
            <span>{{ formatRecordDuration(record) }}</span>
            <span>{{ record.device_id }}</span>
          </div>
          <p v-if="identities.subtitleFor(record.peer, record.device_id)" class="history-attribution">{{ identities.subtitleFor(record.peer, record.device_id) }}</p>
          <p v-if="record.recording_error" class="recording-error">录音失败：{{ record.recording_error }}</p>
          <p v-else-if="recordingErrors[record.id]" class="recording-error">{{ recordingErrors[record.id] }}</p>
          <audio
            v-if="record.recording_name"
            :ref="(el) => registerAudio(record, el)"
            :data-record-id="record.id"
            class="recording-player"
            controls
            preload="none"
            :src="recordingSources[record.id] || undefined"
          />
        </div>
        <div class="history-actions">
          <el-tooltip content="加到联系人" placement="left">
            <button type="button" class="play-button" :aria-label="`把 ${record.peer || '号码'} 加到联系人`" @click="saveContact(record)">
              <el-icon><PersonAdd24Regular /></el-icon>
            </button>
          </el-tooltip>
          <el-tooltip v-if="record.peer" content="回拨" placement="left">
            <button
              type="button"
              class="play-button"
              :disabled="redialing || !!phone.currentCall"
              :aria-label="`回拨 ${record.peer}`"
              @click="redial(record)"
            >
              <el-icon><Call24Regular /></el-icon>
            </button>
          </el-tooltip>
        </div>
      </article>
    </div>
    <div v-else class="history-empty">暂无通话记录</div>
  </section>
</template>

<style scoped>
.history-panel { min-height: 320px; overflow: hidden; background: transparent; }
.history-panel > header { min-height: 68px; padding: 14px 16px; display: flex; align-items: center; justify-content: space-between; border-bottom: 1px solid var(--ui-border); }
.history-panel header span { color: var(--ui-primary); font-family: "v-mono", monospace; font-size: 9px; font-weight: 700; letter-spacing: .12em; }
.history-panel h2 { margin: 2px 0 0; color: var(--ui-text); font-size: 16px; }
.history-panel header > strong { min-width: 28px; padding: 3px 8px; border-radius: 20px; background: var(--ui-surface-muted); color: var(--ui-text-muted); text-align: center; }
.history-list { max-height: 560px; overflow-y: auto; }
.history-item { min-height: 76px; padding: 12px 14px; display: flex; align-items: flex-start; gap: 11px; border-bottom: 1px solid var(--ui-border-muted); }
.history-icon { width: 34px; height: 34px; flex: 0 0 34px; display: grid; place-items: center; border-radius: 50%; background: color-mix(in srgb, var(--ui-primary) 10%, var(--ui-surface)); color: var(--ui-primary); }
.history-icon.is-missed, .history-icon.is-rejected, .history-icon.is-failed { color: var(--ui-danger); background: color-mix(in srgb, var(--ui-danger) 10%, var(--ui-surface)); }
.history-attribution { margin: 4px 0 0; color: var(--ui-text-muted); font-size: 12px; }
.history-icon.is-busy { color: var(--ui-warning); }
.history-copy { min-width: 0; flex: 1; }
.history-primary { display: flex; justify-content: space-between; gap: 8px; }
.history-primary strong { overflow: hidden; text-overflow: ellipsis; color: var(--ui-text); font-family: "v-mono", monospace; font-size: 13px; white-space: nowrap; }
.history-primary time { color: var(--ui-text-muted); font-size: 11px; white-space: nowrap; }
.history-secondary { margin-top: 4px; display: flex; flex-wrap: wrap; gap: 5px 10px; color: var(--ui-text-muted); font-size: 11px; }
.status-missed, .status-rejected, .status-failed { color: var(--ui-danger); }
.status-busy { color: var(--ui-warning); }
.recording-error { margin: 5px 0 0; color: var(--ui-danger); font-size: 11px; line-height: 1.35; }
.play-button { width: 40px; height: 40px; flex: 0 0 40px; display: grid; place-items: center; border: 1px solid var(--ui-border); border-radius: 50%; background: var(--ui-surface); color: var(--ui-primary); cursor: pointer; }
.play-button:disabled { cursor: not-allowed; opacity: .45; }
.history-actions { display: flex; flex-direction: column; gap: 6px; flex: 0 0 auto; }
.recording-player { width: 100%; max-width: 320px; margin-top: 6px; display: block; }
@media (max-width: 640px) {
  .history-item { padding: 10px 12px; gap: 8px; }
  .history-actions { flex-direction: row; }
  .play-button { width: 34px; height: 34px; flex: 0 0 34px; }
  .recording-player { max-width: 100%; }
}
.history-empty { min-height: 250px; display: grid; place-items: center; color: var(--ui-text-muted); }
</style>
