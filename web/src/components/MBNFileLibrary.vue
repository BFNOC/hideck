<script setup lang="ts">
import { ref } from 'vue'
import { ElMessageBox } from 'element-plus'
import { mbnService, type MBNAsset } from '../services/mbn'
const props = defineProps<{ assets: MBNAsset[]; blocked: boolean; canImport: boolean }>()
const emit = defineEmits<{ refresh: []; error: [unknown]; import: [string] }>()
const uploading = ref(false), model = ref(''), firmware = ref(''), source = ref('')
async function upload(event: Event) {
  const input = event.target as HTMLInputElement, file = input.files?.[0]
  if (!file || props.blocked || uploading.value) return
  uploading.value = true
  try { await mbnService.upload(file, { model: model.value, firmware: firmware.value, source: source.value }); emit('refresh') }
  catch (err) { emit('error', err) }
  finally { uploading.value = false; input.value = '' }
}
async function download(asset: MBNAsset) {
  try {
    const blob = await mbnService.download(asset.id), url = URL.createObjectURL(blob)
    const link = document.createElement('a'); link.href = url; link.download = asset.name; link.click()
    setTimeout(() => URL.revokeObjectURL(url), 1000)
  } catch (err) { emit('error', err) }
}
async function remove(asset: MBNAsset) {
  try {
    await ElMessageBox.confirm(`删除文件库中的 ${asset.name}？不会删除模组内配置。已被策略或操作记录引用的文件不能删除。`, '删除文件', { type: 'warning' })
    await mbnService.deleteFile(asset.id); emit('refresh')
  } catch (err) { if (err !== 'cancel' && err !== 'close') emit('error', err) }
}
</script>
<template>
  <div class="mbn-files">
    <p>上传只保存原始文件，不导入、不激活。型号和固件是来源声明，不是兼容性验证。模组内置 MBN 不保证支持导出。</p>
    <el-form label-position="top" :disabled="blocked || uploading">
      <el-form-item label="适用型号（可选）"><el-input v-model="model" /></el-form-item>
      <el-form-item label="适用固件（可选）"><el-input v-model="firmware" /></el-form-item>
      <el-form-item label="文件来源"><el-input v-model="source" /></el-form-item>
      <label class="mbn-upload">{{ uploading ? '正在保存文件' : '选择原始 .mbn 文件保存到 HiDeck' }}<input type="file" accept=".mbn" :disabled="blocked || uploading" @change="upload" /></label>
    </el-form>
    <p v-if="!assets.length">文件库为空</p>
    <article v-for="asset in assets" :key="asset.id"><strong>{{ asset.name }}</strong><span>{{ asset.size.toLocaleString() }} 字节 · {{ asset.model || '型号未声明' }} · {{ asset.firmware || '固件未声明' }}</span><small>SHA-256：{{ asset.sha256 }}</small><span>来源：{{ asset.source || '未提供' }}</span><div><el-button @click="download(asset)">下载原文件</el-button><el-button :disabled="blocked || !canImport" @click="emit('import', asset.id)">导入模组</el-button><el-button type="danger" plain :disabled="blocked" @click="remove(asset)">删除文件</el-button></div></article>
  </div>
</template>
<style scoped>
.mbn-files, article { display: grid; gap: 12px; } p { line-height: 1.6; } article { padding: 16px; border: 1px solid var(--el-border-color-lighter); border-radius: 12px; overflow-wrap: anywhere; } article div { display: flex; gap: 8px; flex-wrap: wrap; } small { font-size: 13px; color: var(--el-text-color-secondary); }
.mbn-upload { display: grid; gap: 8px; padding: 16px; border: 1px dashed var(--el-border-color); border-radius: 12px; } input { min-height: 44px; max-width: 100%; } :deep(.el-button) { min-height: 44px; margin-left: 0; }
</style>
