// stages.test.js — применение серверных названий этапов к STAGES.
import { afterEach, describe, expect, it } from 'vitest'
import { STAGES, applyNames, stageById } from './stages.js'

const originalTitles = STAGES.map((s) => s.title)

afterEach(() => {
  STAGES.forEach((s, i) => {
    s.title = originalTitles[i]
  })
})

describe('applyNames', () => {
  it('переименовывает по id, остальные не трогает', () => {
    applyNames({ 1: 'Новые заявки', 7: 'Оплачен договор' })
    expect(stageById(1).title).toBe('Новые заявки')
    expect(stageById(7).title).toBe('Оплачен договор')
    expect(stageById(2).title).toBe('Живые лиды')
  })

  it('понимает строковые ключи и игнорирует пустые/мусорные значения', () => {
    applyNames({ 2: '  ', 3: '', 4: 'Ждут оплату', 99: 'мимо' })
    expect(stageById(2).title).toBe('Живые лиды')
    expect(stageById(3).title).toBe('Оплачено: ждут ссылку')
    expect(stageById(4).title).toBe('Ждут оплату')
  })

  it('null/undefined — no-op', () => {
    applyNames(null)
    applyNames(undefined)
    expect(stageById(1).title).toBe('Серые лиды')
  })
})
