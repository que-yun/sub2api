<template>
  <div>
    <!-- Loading state -->
    <div v-if="props.loading && !props.stats" class="space-y-0.5">
      <div class="h-3 w-12 animate-pulse rounded bg-gray-200 dark:bg-gray-700"></div>
      <div class="h-3 w-16 animate-pulse rounded bg-gray-200 dark:bg-gray-700"></div>
      <div class="h-3 w-10 animate-pulse rounded bg-gray-200 dark:bg-gray-700"></div>
    </div>

    <!-- Error state -->
    <div v-else-if="props.error && !props.stats" class="text-xs text-red-500">
      {{ props.error }}
    </div>

    <!-- Stats data -->
    <div v-else-if="props.stats" class="space-y-1 text-xs">
      <div class="space-y-0.5">
        <div class="text-[10px] font-medium text-gray-400 dark:text-gray-500">
          {{ t('admin.accounts.columns.todayStats') }}
        </div>
        <div class="flex items-center gap-1">
          <span class="text-gray-500 dark:text-gray-400"
            >{{ t('admin.accounts.stats.requests') }}:</span
          >
          <span class="font-medium text-gray-700 dark:text-gray-300">{{
            formatNumber(props.stats.requests)
          }}</span>
        </div>
        <div class="flex items-center gap-1">
          <span class="text-gray-500 dark:text-gray-400"
            >{{ t('admin.accounts.stats.tokens') }}:</span
          >
          <span class="font-medium text-gray-700 dark:text-gray-300">{{
            formatTokens(props.stats.tokens)
          }}</span>
        </div>
        <div class="flex items-center gap-1">
          <span class="text-gray-500 dark:text-gray-400">{{ t('usage.accountBilled') }}:</span>
          <span class="font-medium text-emerald-600 dark:text-emerald-400">{{
            formatCurrency(props.stats.cost)
          }}</span>
        </div>
        <div v-if="props.stats.user_cost != null" class="flex items-center gap-1">
          <span class="text-gray-500 dark:text-gray-400">{{ t('usage.userBilled') }}:</span>
          <span class="font-medium text-gray-700 dark:text-gray-300">{{
            formatCurrency(props.stats.user_cost)
          }}</span>
        </div>
      </div>

      <div v-if="props.lifetimeStats" class="space-y-0.5 border-t border-gray-100 pt-1 dark:border-gray-800">
        <div class="text-[10px] font-medium text-gray-400 dark:text-gray-500">
          {{ t('admin.accounts.columns.lifetimeStats') }}
        </div>
        <div class="flex items-center gap-1">
          <span class="text-gray-500 dark:text-gray-400"
            >{{ t('admin.accounts.stats.requests') }}:</span
          >
          <span class="font-medium text-gray-700 dark:text-gray-300">{{
            formatNumber(props.lifetimeStats.requests)
          }}</span>
        </div>
        <div class="flex items-center gap-1">
          <span class="text-gray-500 dark:text-gray-400"
            >{{ t('admin.accounts.stats.tokens') }}:</span
          >
          <span class="font-medium text-gray-700 dark:text-gray-300">{{
            formatTokens(props.lifetimeStats.tokens)
          }}</span>
        </div>
        <div class="flex items-center gap-1">
          <span class="text-gray-500 dark:text-gray-400">{{ t('usage.accountBilled') }}:</span>
          <span class="font-medium text-emerald-600 dark:text-emerald-400">{{
            formatCurrency(props.lifetimeStats.cost)
          }}</span>
        </div>
        <div v-if="props.lifetimeStats.user_cost != null" class="flex items-center gap-1">
          <span class="text-gray-500 dark:text-gray-400">{{ t('usage.userBilled') }}:</span>
          <span class="font-medium text-gray-700 dark:text-gray-300">{{
            formatCurrency(props.lifetimeStats.user_cost)
          }}</span>
        </div>
      </div>
    </div>

    <!-- No data -->
    <div v-else class="text-xs text-gray-400">-</div>
  </div>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import type { WindowStats } from '@/types'
import { formatNumber, formatCurrency } from '@/utils/format'

const props = withDefaults(
  defineProps<{
    stats?: WindowStats | null
    lifetimeStats?: WindowStats | null
    loading?: boolean
    error?: string | null
  }>(),
  {
    stats: null,
    lifetimeStats: null,
    loading: false,
    error: null
  }
)

const { t } = useI18n()

// Format large token numbers (e.g., 1234567 -> 1.23M)
const formatTokens = (tokens: number): string => {
  if (tokens >= 1000000) {
    return `${(tokens / 1000000).toFixed(2)}M`
  } else if (tokens >= 1000) {
    return `${(tokens / 1000).toFixed(1)}K`
  }
  return tokens.toString()
}
</script>
