<script setup lang="ts">
import { computed, ref, toRef, watch } from 'vue'
import { ElMessageBox } from 'element-plus'
import { useMBN } from '../composables/useMBN'
import { mbnStatusLabel, mbnStageLabel, mbnKindLabel, type MBNProfile } from '../services/mbn'
import MBNFileLibrary from './MBNFileLibrary.vue'
import MBNPolicyEditor from './MBNPolicyEditor.vue'

const props = defineProps<{ deviceId: string }>()
const { snapshot, operations, assets, error, loading, submitting, active, inspect, refreshJobs, refreshFiles, start, cancel, bindCurrent, report } = useMBN(toRef(props, 'deviceId'))
const selected = ref(''), auto = ref(false), candidates = ref<string[]>([]), goal = ref('registration'), target = ref('')
const recovery = ref(180), registration = ref(120), stability = ref(30), page = ref(1)
const probeURL = ref(''), allowData = ref(false), allowNativeIMS = ref(false)
const blocked = computed(() => !!active.value || submitting.value || loading.value)
const current = computed(() => snapshot.value?.profiles.find(item => item.activated)?.name || '未确认')
const history = computed(() => operations.value.slice((page.value - 1) * 8, page.value * 8))
watch(() => props.deviceId, () => { selected.value = ''; candidates.value = []; page.value = 1 })
async function remove(profile: MBNProfile) {
  if (blocked.value) return
  const original = snapshot.value, id = props.deviceId, replacement = selected.value
  try {
    const result = await ElMessageBox.prompt(`此操作删除模组中的 ${profile.name}，不是删除文件库。内置配置也可能无法恢复。请输入完整名称确认；使用中的配置将先切换到上方选择的替代配置。`, '永久删除 MBN', { inputValidator: value => value === profile.name || '名称必须完全一致', confirmButtonText: '确认无备份删除', cancelButtonText: '取消', type: 'warning' })
    if (props.deviceId !== id || snapshot.value !== original) return
    await start({ kind: 'delete', profile: profile.name, confirm_name: result.value, accept_no_backup: true, replacement }, '确认已了解：配置快照不是二进制备份，删除后不保证能够恢复。')
  } catch (err) { if (err !== 'cancel' && err !== 'close') report(err) }
}
function move(index: number, delta: number) {
  const next = [...candidates.value], other = index + delta
  if (other < 0 || other >= next.length) return
  ;[next[index], next[other]] = [next[other]!, next[index]!]; candidates.value = next
}
</script>

<template>
  <section class="mbn-panel" aria-label="运营商配置 MBN">
    <header><div><h3>运营商配置（MBN）</h3><p>影响模组驻网和漫游能力，不等同于 PLMN 选网，也不用于修复软件 WiFi calling。</p></div><el-button :loading="loading" :disabled="!!active" @click="inspect()">读取模组</el-button></header>
    <el-alert v-if="error" :title="error" type="error" show-icon :closable="false" />
    <el-alert v-if="active" :title="`${mbnStatusLabel[active.status]} · ${mbnStageLabel[active.stage] || active.stage}`" type="info" :closable="false"><p>关闭页面不会取消任务。已发送的命令会等待终态，取消将在安全边界执行。</p><el-button :disabled="active.cancel_requested" @click="cancel">{{ active.cancel_requested ? '已请求取消' : '取消任务' }}</el-button></el-alert>
    <p v-if="!snapshot">尚未查询。点击“读取模组”只读取实际能力和配置，不修改设备。</p>
    <template v-if="snapshot">
      <div class="mbn-summary"><strong>{{ current }}</strong><span>{{ snapshot.owner.model }} · {{ snapshot.owner.firmware }}</span><span>实际 AutoSel：{{ snapshot.autosel ? '开启' : '关闭' }}</span></div>
      <el-tabs>
        <el-tab-pane label="模组配置">
          <el-form label-position="top" class="mbn-controls" :disabled="blocked">
            <el-form-item label="目标配置（删除时也用作替代配置）"><el-select v-model="selected" filterable placeholder="选择模组实际返回的配置"><el-option v-for="p in snapshot.profiles" :key="p.name" :label="p.name" :value="p.name" /></el-select></el-form-item>
            <el-form-item label="模组自动选择 AutoSel"><el-switch v-model="auto" /><span>开启后由模组决定配置，不是自动 PLMN 选网</span></el-form-item>
            <el-button type="primary" :disabled="!snapshot.capabilities.autosel || (!auto && (!selected || !snapshot.capabilities.select))" :loading="submitting" @click="start({ kind: 'apply', profile: selected, autosel: auto }, '应用后将重启模组，电话、短信和数据会暂时中断。不会修改飞行或运营商射频锁。继续？')">应用并重启模组</el-button>
          </el-form>
          <ul class="mbn-profiles"><li v-for="p in snapshot.profiles" :key="p.name"><div><strong>{{ p.name }}</strong><small>{{ p.version || '版本未知' }} · {{ p.date || '日期未知' }} · 来源未知</small><span>selected：{{ p.selected ? '是' : '否' }} / activated：{{ p.activated ? '是' : '否' }}</span></div><el-button type="danger" plain :disabled="blocked || !snapshot.capabilities.delete" @click="remove(p)">删除</el-button></li></ul>
        </el-tab-pane>
        <el-tab-pane label="文件库"><MBNFileLibrary :assets="assets" :blocked="blocked" :can-import="snapshot.capabilities.add" @refresh="refreshFiles" @error="report" @import="id => start({ kind: 'import', asset_id: id, accept_unknown_compatibility: true }, '外部 MBN 可能不兼容此固件。导入不会自动激活；请确认文件来源及型号。继续导入？')" /></el-tab-pane>
        <el-tab-pane label="SIM 策略"><MBNPolicyEditor :device-id="deviceId" :snapshot="snapshot" :assets="assets" :blocked="blocked" @error="report" /><el-button :disabled="blocked" @click="bindCurrent">将当前已生效配置绑定本 SIM</el-button></el-tab-pane>
        <el-tab-pane label="自动试配">
          <p>仅手动开始，每个候选最多尝试一次；首次达到目标即停止。成功保留当前配置，是否绑定 SIM 由你决定。失败或取消会尝试恢复原配置。</p>
          <el-form label-position="top" :disabled="blocked">
            <el-form-item label="候选（勾选顺序为执行顺序）"><el-checkbox-group v-model="candidates"><el-checkbox v-for="p in snapshot.profiles" :key="p.name" :value="p.name">{{ p.name }}</el-checkbox></el-checkbox-group></el-form-item>
            <ol class="mbn-order"><li v-for="(name, index) in candidates" :key="name"><span>{{ name }}</span><el-button :disabled="index === 0" @click="move(index, -1)">上移</el-button><el-button :disabled="index === candidates.length - 1" @click="move(index, 1)">下移</el-button></li></ol>
            <el-form-item label="验证目标"><el-select v-model="goal"><el-option label="稳定驻网（默认）" value="registration" /><el-option label="蜂窝数据连接与 HTTP 探测" value="data" /><el-option label="原生 IMS 注册（需已有 VoLTE 模式）" value="ims" /></el-select></el-form-item>
            <template v-if="goal === 'data'"><el-form-item label="探测 URL（经此模组的数据接口访问）"><el-input v-model="probeURL" placeholder="https://…" /></el-form-item><el-checkbox v-model="allowData">允许建立蜂窝数据连接并产生探测流量</el-checkbox></template>
            <el-checkbox v-if="goal === 'ims'" v-model="allowNativeIMS">允许启用原生 IMS 验证；不发送短信、不拨号</el-checkbox>
            <el-form-item label="限定 PLMN（可选，如 46001）"><el-input v-model="target" /></el-form-item>
            <div class="mbn-times"><el-form-item label="模组恢复（秒）"><el-input-number v-model="recovery" :min="1" /></el-form-item><el-form-item label="驻网等待（秒）"><el-input-number v-model="registration" :min="1" /></el-form-item><el-form-item label="连续稳定（秒）"><el-input-number v-model="stability" :min="1" /></el-form-item></div>
            <el-button type="primary" :disabled="!candidates.length || (goal === 'data' && (!allowData || !probeURL)) || (goal === 'ims' && !allowNativeIMS)" @click="start({ kind: 'trial', candidates, goal, target_plmn: target, recovery_seconds: recovery, registration_seconds: registration, stability_seconds: stability, probe_url: probeURL, allow_data: allowData, allow_native_ims: allowNativeIMS }, '此任务会逐个切换 MBN 并重启模组。不会发送测试短信或拨打电话；注册成功不代表通话和短信已验证。确认开始？')">开始试配</el-button>
          </el-form>
        </el-tab-pane>
      </el-tabs>
    </template>
    <section class="mbn-history"><header><h4>操作记录</h4><el-button @click="refreshJobs()">刷新记录</el-button></header><p v-if="!operations.length">暂无操作记录</p><article v-for="op in history" :key="op.id"><header><strong>{{ mbnKindLabel[op.request.kind] || op.request.kind }} · {{ mbnStatusLabel[op.status] || op.status }}</strong><time>{{ new Date(op.created_at).toLocaleString() }}</time></header><p>{{ mbnStageLabel[op.stage] || op.stage }}</p><p v-if="op.error" role="alert" class="mbn-error">{{ op.error }}</p><p v-if="op.warning">{{ op.warning }}</p><ul v-if="op.results?.length"><li v-for="result in op.results" :key="result.profile">{{ result.profile }}：{{ result.passed ? '达到目标' : result.error }} {{ result.plmn }}</li></ul><el-button v-if="op.original?.profiles?.length && !['queued', 'running', 'cancelling'].includes(op.status)" :disabled="blocked" @click="start({ kind: 'restore', restore_operation: op.id }, '仅在同一模组、同一 SIM 上恢复此任务之前的 MBN 和 AutoSel。可能需要重启；不能恢复已删除的二进制。继续？')">恢复此任务前配置</el-button></article><el-pagination v-if="operations.length > 8" v-model:current-page="page" :page-size="8" :total="operations.length" layout="prev, pager, next" /></section>
  </section>
</template>

<style scoped>
.mbn-panel { display: grid; gap: 20px; color: var(--el-text-color-primary); min-width: 0; }
header { display: flex; gap: 12px; flex-wrap: wrap; align-items: center; justify-content: space-between; }
h3, h4, p { margin: 0; } p { line-height: 1.6; color: var(--el-text-color-regular); }
.mbn-summary { display: grid; gap: 8px; padding: 16px; border-radius: 12px; background: var(--el-fill-color-light); overflow-wrap: anywhere; }
.mbn-controls { margin: 16px 0; } .mbn-controls span { margin-left: 12px; }
.mbn-profiles, .mbn-order { padding: 0; list-style: none; } .mbn-profiles li, .mbn-order li { display: flex; align-items: center; gap: 12px; padding: 12px 0; border-bottom: 1px solid var(--el-border-color-lighter); }
.mbn-profiles li > div { display: grid; gap: 6px; flex: 1; overflow-wrap: anywhere; min-width: 0; } small { font-size: 13px; color: var(--el-text-color-secondary); }
.mbn-order li span { flex: 1; overflow-wrap: anywhere; } .mbn-times { display: flex; gap: 16px; flex-wrap: wrap; }
.mbn-history { display: grid; gap: 12px; } article { display: grid; gap: 8px; padding: 16px; border: 1px solid var(--el-border-color-lighter); border-radius: 12px; overflow-wrap: anywhere; }
.mbn-error { color: var(--el-color-danger); } time { font-size: 13px; color: var(--el-text-color-secondary); }
:deep(.el-button) { min-height: 44px; margin-left: 0; } :deep(.el-checkbox) { height: auto; min-height: 44px; max-width: 100%; } :deep(.el-checkbox__label) { white-space: normal; overflow-wrap: anywhere; }
</style>
