import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import AccountTableFilters from '../AccountTableFilters.vue'

describe('AccountTableFilters plan type', () => {
  it('exposes SuperGrok under plan filter instead of auth type', async () => {
    const i18n = createI18n({
      legacy: false,
      locale: 'en',
      missingWarn: false,
      fallbackWarn: false,
      messages: { en: {} }
    })
    const wrapper = mount(AccountTableFilters, {
      props: {
        searchQuery: '',
        filters: {
          platform: '',
          type: '',
          plan_type: '',
          status: '',
          privacy_mode: '',
          group: ''
        },
        groups: []
      },
      global: {
        plugins: [i18n],
        stubs: {
          Select: {
            props: ['modelValue', 'options'],
            emits: ['update:modelValue', 'change'],
            template: `<select :value="modelValue" @change="onChange">
              <option v-for="opt in options" :key="String(opt.value)" :value="opt.value">{{ opt.label }}</option>
            </select>`,
            methods: {
              onChange(event) {
                this.$emit('update:modelValue', event.target.value)
                this.$emit('change')
              }
            }
          },
          SearchInput: true
        }
      }
    })

    const selects = wrapper.findAll('select')
    expect(selects.length).toBeGreaterThanOrEqual(3)
    const typeOptions = selects[1].findAll('option').map((o) => o.attributes('value'))
    const planOptions = selects[2].findAll('option').map((o) => o.attributes('value'))
    expect(typeOptions).toContain('oauth')
    expect(typeOptions).not.toContain('SuperGrok')
    expect(planOptions).toContain('SuperGrok')
    expect(planOptions).toContain('')

    await selects[2].setValue('SuperGrok')
    const emitted = wrapper.emitted('update:filters')
    expect(emitted).toBeTruthy()
    expect(emitted?.[0]?.[0]).toMatchObject({ plan_type: 'SuperGrok' })
  })
})
