<script setup lang="ts">
import { computed, onUnmounted, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { mbnService, type MBNAsset, type MBNPolicy, type MBNSnapshot } from '../services/mbn'
const props = defineProps<{ deviceId: string; snapshot: MBNSnapshot; assets: MBNAsset[]; blocked: boolean }>()
const emit = defineEmits<{ error: [unknown] }>()
const scope = ref('sim'), mode = ref('inherit'), profile = ref(''), asset = ref(''), allowImport = ref(false), saving = ref(false)
const policies = ref<MBNPolicy[]>([])
const key = computed(() => scope.value === 'sim' ? `sim:${props.snapshot.owner.iccid}` : `device:${props.snapshot.owner.imei}`)
const simTail = computed(() => props.snapshot.owner.iccid.slice(-6))
let epoch = 0
function populate() {
  const policy = policies.value.find(item => item.key === key.value)
  mode.value = policy?.mode || 'inherit'; profile.value = policy?.profile || ''; asset.value = policy?.asset_id || ''; allowImport.value = policy?.allow_import || false
}
watch(() => [props.deviceId, props.snapshot.owner.iccid, props.snapshot.owner.imei], async () => {
  const current = ++epoch
  saving.value = false; policies.value = []; populate()
  try { const result = await mbnService.policies(props.deviceId); if (current === epoch) { policies.value = result; populate() } }
  catch (err) { if (current === epoch) emit('error', err) }
}, { immediate: true })
watch(key, populate)
async function save() {
  if (saving.value || props.blocked) return
  const id = props.deviceId, current = epoch, owner = props.snapshot.owner
  const selected = props.snapshot.profiles.find(item => item.name === profile.value)
  const policy = { key: key.value, mode: mode.value, model: owner.model, firmware: owner.firmware, profile: profile.value, version: selected?.version || '', asset_id: asset.value, allow_import: allowImport.value }
  saving.value = true
  try {
    await ElMessageBox.confirm('保存后会在身份就绪时应用；固定配置不匹配型号/固件会明确报错，不会自动猜其他配置。', '保存 MBN 策略', { type: 'warning' })
    if (current !== epoch) return
    await mbnService.savePolicy(id, policy)
    if (current !== epoch) return
    policies.value = [...policies.value.filter(item => item.key !== policy.key), policy]
    ElMessage.success('策略已保存')
  } catch (err) { if (err !== 'cancel' && err !== 'close' && current === epoch) emit('error', err) }
  finally { if (current === epoch) saving.value = false }
}
onUnmounted(() => { epoch++ })
</script>
<template>
  <el-form label-position="top" :disabled="blocked || saving">
    <p>当前 SIM 尾号 {{ simTail }}。策略按这张卡保存，不按运营商品牌共享。</p>
    <el-form-item label="策略范围"><el-radio-group v-model="scope"><el-radio-button value="sim">本 SIM</el-radio-button><el-radio-button value="device">模组默认</el-radio-button></el-radio-group></el-form-item>
    <el-form-item label="配置方式"><el-select v-model="mode"><el-option :label="scope === 'sim' ? '继承模组默认' : '不托管，保持现有行为'" value="inherit" /><el-option label="模组 AutoSel" value="autosel" /><el-option label="固定 MBN" value="fixed" /></el-select></el-form-item>
    <template v-if="mode === 'fixed'"><el-form-item label="固定配置"><el-select v-model="profile" filterable><el-option v-for="item in snapshot.profiles" :key="item.name" :label="item.name" :value="item.name" /></el-select></el-form-item><el-form-item label="可恢复的原始文件（可选）"><el-select v-model="asset" clearable><el-option v-for="item in assets" :key="item.id" :label="item.name" :value="item.id" /></el-select></el-form-item><el-checkbox v-model="allowImport" :disabled="!asset">配置缺失时允许导入此文件；我确认来源和适配固件</el-checkbox></template>
    <el-button type="primary" :loading="saving" :disabled="mode === 'fixed' && !profile" @click="save">保存 MBN 策略</el-button>
  </el-form>
</template>
<style scoped>
.el-form { display: grid; gap: 12px; margin-bottom: 20px; } .el-button { min-height: 44px; justify-self: start; } :deep(.el-checkbox__label) { white-space: normal; line-height: 1.6; }
</style>
