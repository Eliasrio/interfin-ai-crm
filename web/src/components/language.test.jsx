// language.test.jsx — M14: бейдж языка на карточке, выпадающий выбор в
// модалке (LanguageSelect) и событие lead_language в store.
import { afterEach, describe, expect, it } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import LeadCard from './LeadCard.jsx'
import LanguageSelect from './LanguageSelect.jsx'
import * as store from '../lib/store.js'
import * as auth from '../lib/auth.js'
import { languageBadge } from '../lib/language.js'
import { makeJWT, makeLead, mockFetch } from '../test/helpers.js'

afterEach(() => {
  cleanup()
  store.reset()
  auth.logout()
})

describe('languageBadge', () => {
  it('тройка → бейдж, null/мусор → null (бейдж не показывается)', () => {
    expect(languageBadge('ru')).toBe('🇷🇺 RU')
    expect(languageBadge('en')).toBe('🇬🇧 EN')
    expect(languageBadge('es')).toBe('🇪🇸 ES')
    expect(languageBadge(null)).toBeNull()
    expect(languageBadge('pt')).toBeNull()
  })
})

describe('LeadCard: бейдж языка', () => {
  it('language=es → бейдж 🇪🇸 ES виден', () => {
    render(<LeadCard lead={makeLead({ id: 1, language: 'es' })} onOpen={() => {}} />)
    expect(screen.getByTestId('lang-badge').textContent).toContain('🇪🇸 ES')
  })

  it('language=null → бейджа нет', () => {
    render(<LeadCard lead={makeLead({ id: 1 })} onOpen={() => {}} />)
    expect(screen.queryByTestId('lang-badge')).toBeNull()
  })
})

describe('store: событие lead_language', () => {
  it('автодетекция/ручная смена обновляют язык лида live', () => {
    store.applyLeads([makeLead({ id: 1 })])
    const applied = store.applyEvent({
      type: 'lead_language',
      lead_id: 1,
      language: 'en',
      ts: new Date().toISOString(),
    })
    expect(applied).toBe(true)
    expect(store.getState().leadsById[1].language).toBe('en')
  })

  it('неизвестный тип события по-прежнему игнорируется молча', () => {
    store.applyLeads([makeLead({ id: 1, language: 'ru' })])
    const applied = store.applyEvent({
      type: 'language_reset_v99',
      lead_id: 1,
      ts: new Date().toISOString(),
    })
    expect(applied).toBe(true)
    expect(store.getState().leadsById[1].language).toBe('ru')
  })
})

async function setupSelect(lead, patchReply) {
  mockFetch([
    { match: (u) => u === '/auth/login', reply: { body: { access_token: makeJWT('manager'), expires_in: 900 } } },
  ])
  await auth.login('m@x', 'pw')
  store.applyLeads([lead])
  const fetch = mockFetch([
    { match: (u, o) => u === `/api/leads/${lead.id}/language` && o.method === 'PATCH', reply: patchReply },
  ])
  render(<LanguageSelect lead={lead} />)
  return { fetch }
}

describe('LanguageSelect (модалка)', () => {
  it('язык не определён: селект на placeholder, бейджа нет', async () => {
    await setupSelect(makeLead({ id: 1 }), { body: {} })
    expect(screen.getByTestId('lang-select').value).toBe('')
    expect(screen.queryByTestId('lang-badge-modal')).toBeNull()
  })

  it('выбор языка зовёт PATCH и обновляет store ответом', async () => {
    const fresh = makeLead({ id: 1, language: 'es' })
    const { fetch } = await setupSelect(makeLead({ id: 1, language: 'ru' }), { body: { lead: fresh } })
    fireEvent.change(screen.getByTestId('lang-select'), { target: { value: 'es' } })
    await waitFor(() => {
      const call = fetch.calls.find((c) => c.url === '/api/leads/1/language')
      expect(call).toBeTruthy()
      expect(JSON.parse(call.opts.body)).toEqual({ language: 'es' })
    })
    await waitFor(() => expect(store.getState().leadsById[1].language).toBe('es'))
  })

  it('ошибка PATCH — тост, язык в store не меняется', async () => {
    await setupSelect(makeLead({ id: 1, language: 'ru' }), {
      status: 404,
      body: { error: 'лид не найден', code: 'ERR_NOT_FOUND' },
    })
    fireEvent.change(screen.getByTestId('lang-select'), { target: { value: 'en' } })
    await waitFor(() => expect(store.getState().alerts.some((a) => a.kind === 'error')).toBe(true))
    expect(store.getState().leadsById[1].language).toBe('ru')
  })

  it('бейдж в модалке следует за языком лида', async () => {
    await setupSelect(makeLead({ id: 1, language: 'en' }), { body: {} })
    expect(screen.getByTestId('lang-badge-modal').textContent).toContain('🇬🇧 EN')
    expect(screen.getByTestId('lang-select').value).toBe('en')
  })
})
