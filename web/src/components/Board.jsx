// Board — 8 колонок стадий §3.1 (задача M10-2). Drag-and-drop нативный
// (HTML5 DnD): зависимость не нужна, а тесты дёргают те же DOM-события.
import { useEffect, useMemo, useState } from 'react'
import { STAGES } from '../lib/stages.js'
import { useStore } from '../hooks.js'
import LeadCard from './LeadCard.jsx'

export default function Board({ onOpenLead, onMoveLead }) {
  const { leadsById } = useStore()

  // Минутный тик — индикаторы TTL на карточках считаются от
  // last_activity_at и должны стареть без событий.
  const [, setTick] = useState(0)
  useEffect(() => {
    const t = setInterval(() => setTick((v) => v + 1), 60_000)
    return () => clearInterval(t)
  }, [])

  const byStage = useMemo(() => {
    const cols = new Map(STAGES.map((s) => [s.id, []]))
    for (const lead of Object.values(leadsById)) {
      const col = cols.get(lead.stage_id)
      if (col) col.push(lead)
    }
    for (const col of cols.values()) {
      col.sort((a, b) => new Date(b.last_activity_at) - new Date(a.last_activity_at))
    }
    return cols
  }, [leadsById])

  return (
    <main className="board">
      {STAGES.map((stage) => (
        <Column
          key={stage.id}
          stage={stage}
          leads={byStage.get(stage.id)}
          onOpenLead={onOpenLead}
          onMoveLead={onMoveLead}
        />
      ))}
    </main>
  )
}

function Column({ stage, leads, onOpenLead, onMoveLead }) {
  const [dragOver, setDragOver] = useState(false)

  function handleDrop(e) {
    e.preventDefault()
    setDragOver(false)
    const id = Number(e.dataTransfer.getData('text/lead-id'))
    if (id) onMoveLead(id, stage.id)
  }

  return (
    <section
      className={'column' + (dragOver ? ' column-dragover' : '')}
      data-stage-id={stage.id}
      aria-label={stage.title}
      onDragOver={(e) => {
        e.preventDefault() // разрешаем drop
        setDragOver(true)
      }}
      onDragLeave={() => setDragOver(false)}
      onDrop={handleDrop}
    >
      <header className="column-head" title={stage.hint}>
        <span className="column-title">
          {stage.id}. {stage.title}
        </span>
        <span className="column-count">{leads.length}</span>
      </header>
      <div className="column-cards">
        {leads.map((lead) => (
          <LeadCard key={lead.id} lead={lead} onOpen={() => onOpenLead(lead.id)} />
        ))}
      </div>
    </section>
  )
}
