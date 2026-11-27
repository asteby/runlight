// #/ - Resumen: what runlight decided and why, at a glance.
import { useState } from 'react';
import { useLive } from '../lib/live.js';
import { gb, dur, hms, pct, niceMax, plural } from '../lib/format.js';
import { CHECK_LABEL, narrative, LEVEL_ICON, LEVEL_METER, LEVEL_KIND } from '../lib/decision.js';
import { Icon } from '../components/Icon.jsx';
import { Card, Meter, Badge, Chip, Spinner, EmptyState } from '../components/ui.jsx';
import { Chart } from '../components/charts.jsx';

export const meta = { title: 'Resumen', icon: 'layout-dashboard' };

function Note({ level, children }) {
  return <span className={'note-i ' + LEVEL_KIND[level]}><Icon name={LEVEL_ICON[level]} size={14} />{children}</span>;
}

function Tile({ label, value, small, note, meter, kind }) {
  return (
    <div className="tile">
      <div className="label">{label}</div>
      <div className="value num">{value}{small && <small>{small}</small>}</div>
      <div className="note">{note}</div>
      {meter != null && <Meter value={meter} kind={kind} />}
    </div>
  );
}

function Kpis({ state }) {
  const p = state.point;
  const d = state.decision;
  const c = state.config;
  const tot = state.totals;
  if (!p) return null;
  const oldest = state.queue.length ? (state.now - state.queue[0].queuedMs) / 1000 : 0;
  const cpu = d && d.checks ? d.checks.find((x) => x.key === 'psi_cpu') : null;
  const done = tot.succeeded + tot.failed + tot.canceled;
  let prodNote = '';
  if (cpu) {
    prodNote = p.prodCpu >= cpu.limit
      ? <Note level="bad">Frena desde {Math.round(cpu.limit)}%</Note>
      : p.prodCpu >= cpu.calm ? <Note level="watch">En vigilancia</Note> : <Note level="ok">Tranquila</Note>;
  }
  return (
    <section className="kpis" aria-label="Indicadores">
      <Tile label="Límite de runners" value={String(p.limit)} small={'de ' + c.max} note={d && d.checks ? narrative(d)[0] : ''} meter={c.max ? (p.limit / c.max) * 100 : 0} kind="rl" />
      <Tile label="Runners vivos" value={String(p.runners)} small={p.busy + ' ocupados'} note={p.runners ? 'Efímeros: uno por job' : 'Escala a cero en reposo'} meter={p.runners ? (p.busy / p.runners) * 100 : 0} kind="rl" />
      <Tile label="Jobs en cola" value={String(state.queue.length)} note={state.queue.length ? 'El más antiguo espera ' + dur(oldest) : 'Nadie esperando'} />
      <Tile label="Presión de producción" value={pct(p.prodCpu, 0)} small="CPU" note={prodNote} meter={cpu ? (p.prodCpu / cpu.limit) * 100 : null} kind="prod" />
      <Tile label="Jobs terminados" value={String(done)} small="desde el arranque"
        note={<>
          <span className="note-i good"><Icon name="check" size={14} />{tot.succeeded}</span>{' '}
          <span className="note-i crit"><Icon name="x" size={14} />{tot.failed}</span>{' '}
          <span className="note-i">cancelados {tot.canceled}</span>
        </>} />
      <Tile label="Memoria disponible" value={gb(p.memAvailMb)} small={'de ' + gb(c.memTotalMb)} note="Compartida por todos" meter={c.memTotalMb ? (p.memAvailMb / c.memTotalMb) * 100 : null} kind="prod" />
    </section>
  );
}

function Check({ c }) {
  const less = ['psi_cpu', 'psi_mem', 'psi_io', 'load'].includes(c.key);
  let level;
  let val;
  let fill;
  let mark;
  let icon;
  if (c.key === 'demand') {
    level = 'ok'; icon = 'activity'; val = `${c.value} en uso / límite ${c.limit}`;
    fill = c.limit ? (c.value / c.limit) * 100 : 0; mark = 100;
  } else if (less) {
    level = c.value >= c.limit ? 'bad' : c.value >= c.calm ? 'watch' : 'ok';
    val = `${c.value.toFixed(1)}${c.unit} · frena en ${c.limit.toFixed(0)}${c.unit}`;
    fill = (c.value / c.limit) * 100; mark = (c.calm / c.limit) * 100;
  } else {
    level = c.ok ? 'ok' : 'bad';
    val = `${gb(c.value)} · mínimo ${gb(c.limit)}`;
    fill = (c.value / (c.limit * 2)) * 100; mark = 50;
  }
  return (
    <li className={'check ' + level}>
      <Icon name={icon || LEVEL_ICON[level]} size={18} />
      <span className="name">{CHECK_LABEL[c.key] || c.key}</span>
      <span className="val num">{val}</span>
      <Meter value={Math.max(1, fill)} kind={LEVEL_METER[level]} mark={mark} />
    </li>
  );
}

function Decision({ state }) {
  const d = state.decision;
  if (!d || !d.checks) return <Card title="Cómo decide ahora"><p className="empty">Esperando la primera medición…</p></Card>;
  const [headline, explain] = narrative(d);
  return (
    <Card title="Cómo decide ahora" sub="Cada 2 segundos runlight mide el servidor, evalúa estas condiciones y fija cuántos runners puede permitirse. Nada se frena por la presión del propio CI: solo por el dolor de producción.">
      <div className="decision">
        <div>
          <p className="headline">{headline}</p>
          <p className="explain">{explain}</p>
          <p className="explain">Límite: {d.limit} de {d.max} runners · {d.inUse} en uso. El próximo runner se estima en {gb(d.memPerRunnerMb)} (aprendido de los jobs en cola; si no los conoce usa el valor por defecto).</p>
        </div>
        <ul className="checks">{d.checks.map((c) => <Check c={c} key={c.key} />)}</ul>
      </div>
    </Card>
  );
}

function Banners({ state }) {
  const a = state.alerts;
  return (
    <>
      {state.paused && (
        <div className="banner warn" role="status">
          <Icon name="pause" size={20} />
          <span><b>Escalado en pausa.</b> No se arrancan runners nuevos; los que están vivos terminan su job.</span>
          <a href="#/config">Reanudar</a>
        </div>
      )}
      {state.memory && state.memory.verdict === 'active' && (
        <div className="banner warn quiet" role="status">
          <Icon name="triangle-alert" size={18} />
          <span><b>Memoria.</b> {state.memory.note || 'El servidor está paginando.'}</span>
          <a href="#/servidor">Ver memoria</a>
        </div>
      )}
      {a && a.active > 0 && (
        <div className={'banner ' + (a.critical > 0 ? 'crit' : 'warn')} role="status">
          <Icon name={a.critical > 0 ? 'octagon-alert' : 'bell-ring'} size={20} />
          <span><b>{plural(a.active, 'alerta activa', 'alertas activas')}</b>{a.critical > 0 ? ` (${a.critical} crítica${a.critical === 1 ? '' : 's'})` : ''}.</span>
          <a href="#/alertas">Ver alertas</a>
        </div>
      )}
    </>
  );
}

const KINDS = {
  capacity: ['arrow-up-down', 'info', 'Capacidad'], scale: ['plus', 'rl', 'Escalado'], job: ['circle-dot', 'info', 'Jobs'],
  reap: ['circle-minus', 'warn', 'Limpieza'], prune: ['eraser', 'good', 'Poda'], keep: ['shield-check', 'good', 'Protección'],
  warn: ['triangle-alert', 'warn', 'Aviso'], alert: ['bell-ring', 'crit', 'Alerta'], info: ['info', 'info', 'Info'],
};

function Log({ state }) {
  const [filter, setFilter] = useState(() => new Set());
  const toggle = (k) => setFilter((s) => { const n = new Set(s); if (n.has(k)) n.delete(k); else n.add(k); return n; });
  const counts = {};
  for (const e of state.events) counts[e.kind] = (counts[e.kind] || 0) + 1;
  const ev = state.events.filter((e) => !filter.size || filter.has(e.kind)).slice(0, 80);
  return (
    <Card title="Registro de decisiones" sub="Qué hizo runlight y por qué, más reciente primero.">
      <div className="chips" role="group" aria-label="Filtrar por tipo">
        {Object.entries(KINDS).map(([k, v]) => (
          <Chip key={k} pressed={filter.has(k)} onClick={() => toggle(k)} data-empty={counts[k] ? undefined : 'true'}>
            {v[2]}<span className="n">{counts[k] || 0}</span>
          </Chip>
        ))}
        {filter.size > 0 && <button type="button" className="chip clear" onClick={() => setFilter(new Set())}>Quitar filtros</button>}
      </div>
      <div className="log" role="table" aria-label="Registro de decisiones">
        {ev.length > 0 && (
          <div className="loghead" role="row">
            <span role="columnheader">Hora</span><span role="columnheader">Tipo</span><span role="columnheader">Qué pasó</span>
          </div>
        )}
        {ev.length ? ev.map((e, i) => {
          const k = KINDS[e.kind] || KINDS.info;
          const m = /^([✓✗])s*(.*)$/s.exec(e.msg);
          return (
            <div className={'line k-' + k[1]} role="row" key={e.t + ':' + i}>
              <span className="time num" role="cell">{hms(e.t)}</span>
              <span role="cell"><Badge kind={k[1]} icon={k[0]}>{k[2]}</Badge></span>
              <span className="msg" role="cell">
                {m && <Icon name={m[1] === '✓' ? 'circle-check' : 'circle-x'} size={15} class={m[1] === '✓' ? 'ok' : 'bad'} />}
                {m ? m[2] : e.msg}
              </span>
            </div>
          );
        }) : <EmptyState icon="scroll-text">Sin eventos con este filtro.</EmptyState>}
      </div>
    </Card>
  );
}

export default function Overview() {
  const { state, history } = useLive();
  if (!state) return <Spinner block label="Esperando la primera medición…" />;
  const d = state.decision;
  const cpu = d && d.checks ? d.checks.find((c) => c.key === 'psi_cpu') : null;
  const ints = (v) => String(Math.round(v));

  const pressure = {
    title: 'Presión', points: history, fmt: (v) => pct(v, 1), yFmt: (v) => Math.round(v) + '%',
    yMax: (ps) => niceMax(Math.max(20, cpu ? cpu.limit * 1.25 : 0, ...ps.map((p) => Math.max(p.prodCpu, p.hostCpu)))),
    series: [
      { label: 'CPU de producción (lo que mide runlight)', color: 'var(--prod)', get: (p) => p.prodCpu },
      { label: 'CPU del host completo (incluye al CI)', color: 'var(--muted)', dash: true, get: (p) => p.hostCpu },
    ],
    refs: cpu ? [
      { v: cpu.limit, label: 'Frena desde ' + Math.round(cpu.limit) + '%', color: 'var(--crit)' },
      { v: cpu.calm, label: 'Sube por debajo de ' + Math.round(cpu.calm) + '%', color: 'var(--good-text)' },
    ] : [],
  };
  const capacity = {
    title: 'Capacidad', points: history, step: true, fmt: ints, yFmt: ints,
    yMax: (ps) => Math.max(4, ...ps.map((p) => Math.max(p.limit, p.runners, p.queued))) + 1,
    series: [
      { label: 'Límite del gobernador', color: 'var(--ink2)', dash: true, get: (p) => p.limit },
      { label: 'Runners vivos', color: 'var(--rl)', get: (p) => p.runners },
      { label: 'Ocupados', color: 'var(--rl)', area: true, get: (p) => p.busy },
      { label: 'Jobs en cola', color: 'var(--queue)', get: (p) => p.queued },
    ],
  };

  return (
    <div className="stack">
      <Banners state={state} />
      <Kpis state={state} />
      <Decision state={state} />
      <section className="grid2">
        <Chart title="Presión: producción vs. host" sub="runlight solo frena por la línea azul. La gris incluye al propio CI esperando CPU, que es normal." cfg={pressure} />
        <Chart title="Capacidad y demanda" sub="El límite lo fija el gobernador; los runners y la cola siguen a la demanda." cfg={capacity} />
      </section>
      <Log state={state} />
    </div>
  );
}
