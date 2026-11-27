// #/servidor - the shared machine: memory split, swap/disk/load, top processes, CPU and memory charts, caches.
import { useState } from 'react';
import { useLive, useProcs } from '../lib/live.js';
import { gb, hms, pct } from '../lib/format.js';
import { LEVEL_ICON, LEVEL_METER } from '../lib/decision.js';
import { Icon } from '../components/Icon.jsx';
import { Card, Table, Meter, Chip, Spinner, EmptyState } from '../components/ui.jsx';
import { Chart } from '../components/charts.jsx';

export const meta = { title: 'Servidor', icon: 'server' };

function Fact({ level, label, value, meter, note }) {
  return (
    <div className={'fact ' + level}>
      <div className="row"><span>{label}</span><span className="num">{value}</span></div>
      <Meter value={meter} kind={LEVEL_METER[level]} />
      <div className="note"><Icon name={LEVEL_ICON[level]} size={14} />{note}</div>
    </div>
  );
}

function MemorySplit({ p, c }) {
  const total = p.memTotalMb || c.memTotalMb || 1;
  const used = Math.max(0, total - p.memAvailMb);
  const other = Math.max(0, used - p.memProdMb - p.memRunlightMb);
  const parts = [
    ['Producción', p.memProdMb, 'var(--prod)'],
    ['runlight', p.memRunlightMb, 'var(--rl)'],
    ['Otros (sistema, caché)', other, 'var(--muted)'],
  ].filter((x) => x[1] > 0);
  return (
    <div className="mb">
      <div className="seg" role="img" aria-label="Reparto de la memoria">
        {parts.map(([n, v, col]) => <div key={n} style={{ background: col, flex: String(v / total) }}>{v / total > 0.08 ? n : ''}</div>)}
        <div className="free" style={{ flex: String(p.memAvailMb / total) }}>Disponible {gb(p.memAvailMb)}</div>
      </div>
      <div className="keyrow">
        {parts.map(([n, v, col]) => <span key={n}><span className="sw" style={{ background: col }} />{n}<b className="num">{gb(v)}</b></span>)}
      </div>
    </div>
  );
}

function Facts({ p, c, m }) {
  const swapPct = p.swapTotalMb ? (p.swapUsedMb / p.swapTotalMb) * 100 : 0;
  const quiet = m && m.verdict === 'idle'; // lleno pero sin paginar: no es un problema
  const sl = quiet ? 'ok' : swapPct >= 90 ? 'bad' : swapPct >= 60 ? 'watch' : 'ok';
  const diskUsed = p.diskTotalMb ? (1 - p.diskFreeMb / p.diskTotalMb) * 100 : 0;
  const dl = diskUsed >= 90 ? 'bad' : diskUsed >= 80 ? 'watch' : 'ok';
  const loadPct = (p.load1 / c.cores) * 100;
  const ll = loadPct >= 150 ? 'bad' : loadPct >= 100 ? 'watch' : 'ok';
  return (
    <div className="facts">
      <Fact level={sl} label="Swap" value={`${gb(p.swapUsedMb)} de ${gb(p.swapTotalMb)}`} meter={swapPct}
        note={quiet ? 'Ocupado pero inactivo: sin impacto.' : sl === 'bad' ? 'Casi lleno: el sistema está paginando a disco y todo se vuelve lento.' : sl === 'watch' ? 'Uso alto de swap.' : 'Sin problema.'} />
      <Fact level={dl} label="Disco" value={`${gb(p.diskFreeMb)} libres (${diskUsed.toFixed(0)}% usado)`} meter={diskUsed}
        note={dl === 'ok' ? 'Espacio suficiente.' : 'Poco espacio libre.'} />
      <Fact level={ll} label="Carga (1 min)" value={`${p.load1.toFixed(1)} con ${c.cores} núcleos`} meter={Math.min(100, loadPct / 1.5)}
        note={ll === 'ok' ? 'Dentro de la capacidad.' : 'Más tareas en cola de CPU que núcleos.'} />
    </div>
  );
}

function Procs() {
  const procs = useProcs();
  const [tab, setTab] = useState('cpu');
  const list = procs ? (tab === 'cpu' ? procs.byCpu : procs.byMem) || [] : [];
  return (
    <div>
      <div className="chips" role="group" aria-label="Ordenar procesos">
        <Chip pressed={tab === 'cpu'} onClick={() => setTab('cpu')}>Por CPU</Chip>
        <Chip pressed={tab === 'mem'} onClick={() => setTab('mem')}>Por memoria</Chip>
      </div>
      <Table cols={[{ h: 'PID', r: true }, { h: 'Usuario' }, { h: 'Proceso' }, { h: 'CPU', r: true }, { h: 'Memoria', r: true }]}
        rows={list.map((q) => [String(q.pid), q.user, q.name, q.cpu.toFixed(0) + '%', gb(q.rssMb)])} empty={procs ? 'Sin datos.' : 'Muestreando…'} />
    </div>
  );
}

function Caches({ state }) {
  const cs = state.caches || [];
  const j = state.janitor || {};
  const sub = j.enabled
    ? `El conserje lleva ${j.runs} pasada(s) y ha liberado ${gb(j.freedMb)} desde el arranque${j.dryRun ? ' (modo simulación: no borra nada)' : ''}. Expulsa lo menos usado al pasar el tope, borra las cachés de runners que ya no existen y aprieta más si el disco baja del 12 %.`
    : 'Sin reglas de caché configuradas: nada limita el crecimiento del disco.';
  const rows = cs.map((c) => {
    const use = c.capMb ? (c.sizeMb / c.capMb) * 100 : 0;
    const level = use > 100 ? 'bad' : use > 85 ? 'watch' : 'ok';
    return [
      c.name,
      <div className={'fact ' + level}>
        <div className="row"><span className="cachesize"><Icon name={LEVEL_ICON[level]} size={14} />{gb(c.sizeMb)}</span><span className="num">tope {gb(c.capMb)}</span></div>
        <Meter value={use} kind={LEVEL_METER[level]} />
      </div>,
      c.mode === 'idle' ? 'Se borra entera si no hay jobs' : 'Se expulsa lo menos usado',
      c.freedMb ? gb(c.freedMb) : '—',
      c.lastPruneMs ? hms(c.lastPruneMs) : '—',
    ];
  });
  return (
    <Card title="Cachés y disco" sub={sub}>
      <Table stack cols={[{ h: 'Caché' }, { h: 'Tamaño y tope' }, { h: 'Política' }, { h: 'Liberado la última vez', r: true }, { h: 'Última poda', r: true }]} rows={rows} empty="Sin cachés vigiladas." />
    </Card>
  );
}

const VERDICT = {
  none: { level: 'none', icon: 'circle-minus', title: 'Sin swap en uso', meter: 'muted' },
  idle: { level: 'ok', icon: 'circle-check', title: 'Swap ocupado pero inactivo', meter: 'prod' },
  active: { level: 'watch', icon: 'triangle-alert', title: 'El servidor está paginando', meter: 'warn' },
};
const kbs = (v) => (v >= 1024 ? (v / 1024).toFixed(1) + ' MB/s' : Math.round(v) + ' KB/s');

function Tmpfs({ t }) {
  const use = t.totalMb ? (t.usedMb / t.totalMb) * 100 : 0;
  const level = use >= 85 ? 'bad' : use >= 60 ? 'watch' : 'ok';
  return (
    <div className={'fact ' + level}>
      <div className="row"><span>{t.path} (en RAM)</span><span className="num">{gb(t.usedMb)} de {gb(t.totalMb)}</span></div>
      <Meter value={use} kind={LEVEL_METER[level]} label={t.path + ' en RAM'} />
      <div className="note"><Icon name={LEVEL_ICON[level]} size={14} />{use.toFixed(0)}% de su límite. Es RAM, no disco: lo viejo acaba en swap.</div>
    </div>
  );
}

function MemorySwap({ m }) {
  if (!m) {
    return (
      <Card title="Memoria y swap">
        <EmptyState icon="memory-stick" title="Sin datos de swap">Esta versión del daemon no informa del swap ni de /tmp en RAM. Actualiza runlight para verlos aquí.</EmptyState>
      </Card>
    );
  }
  const v = VERDICT[m.verdict] || VERDICT.none;
  const total = m.swapTotalMb || 0;
  const swapPct = total ? (m.swapUsedMb / total) * 100 : 0;
  const note = (m.note || '').trim();
  const detail = note.startsWith(v.title) ? note.slice(v.title.length).replace(/^[.:s]+/, '') : note;
  const tmpfs = m.tmpfs || [];
  const rows = (m.topSwap || []).map((q) => [q.name, q.unit || '—', String(q.pid), gb(q.mb)]);
  return (
    <Card title="Memoria y swap" sub="Que el swap esté lleno no es un problema por sí solo: importa si el servidor lo está usando de verdad.">
      <div className={'verdict ' + v.level} role="status">
        <Icon name={v.icon} size={22} />
        <div><b>{v.title}</b>{detail && <p>{detail}</p>}</div>
      </div>
      <div className="grid2 mb">
        <div className="facts">
          <div className="fact">
            <div className="row"><span>Swap usado</span><span className="num">{total ? gb(m.swapUsedMb) + ' de ' + gb(total) : 'No hay swap'}</span></div>
            <Meter value={swapPct} kind={v.meter} label="Swap usado" />
          </div>
          <div className="swapio">
            <span><Icon name="download" size={14} />Entra<b className="num">{kbs(m.swapInKbs || 0)}</b></span>
            <span><Icon name="arrow-up-down" size={14} />Sale<b className="num">{kbs(m.swapOutKbs || 0)}</b></span>
          </div>
        </div>
        <div className="facts">
          {tmpfs.length ? tmpfs.map((t) => <Tmpfs key={t.path} t={t} />) : <p className="sub">Ningún directorio temporal vive en RAM.</p>}
        </div>
      </div>
      <h3 className="subhead">Quién usa el swap</h3>
      <Table stack cols={[{ h: 'Proceso' }, { h: 'Unidad' }, { h: 'PID', r: true }, { h: 'En swap', r: true }]} rows={rows}
        empty={m.swapUsedMb ? 'Sin detalle por proceso todavía; se refresca cada 30 s.' : 'Ningún proceso usa swap.'} />
    </Card>
  );
}

export default function Server() {
  const { state, history } = useLive();
  if (!state) return <Spinner block label="Esperando datos…" />;
  const p = state.point;
  const c = state.config;
  const cpu = {
    title: 'CPU', points: history, stacked: true, fmt: (v) => pct(v, 1), yFmt: (v) => Math.round(v) + '%', yMax: () => 100,
    series: [
      { label: 'Producción', color: 'var(--prod)', get: (q) => q.cpuProd },
      { label: 'runlight', color: 'var(--rl)', get: (q) => q.cpuRunlight },
    ],
    legendExtra: [{ label: 'Núcleos', value: String(c.cores) }],
  };
  const last = history[history.length - 1];
  const mem = {
    title: 'Memoria', points: history, stacked: true, fmt: gb, yFmt: gb, yMax: () => c.memTotalMb || (last ? last.memTotalMb : 1),
    series: [
      { label: 'Producción', color: 'var(--prod)', get: (q) => q.memProdMb },
      { label: 'runlight', color: 'var(--rl)', get: (q) => q.memRunlightMb },
    ],
    legendExtra: last ? [{ label: 'Disponible para todos', value: gb(last.memAvailMb) }] : [],
  };
  return (
    <div className="stack">
      <MemorySwap m={state.memory} />
      {p && (
        <Card title="Servidor compartido" sub={`${c.cores} núcleos · ${gb(c.memTotalMb)} de RAM compartidos entre producción, runlight y el resto. Lo que no usa uno lo aprovecha otro.`}>
          <MemorySplit p={p} c={c} />
          <div className="grid2">
            <Procs />
            <Facts p={p} c={c} m={state.memory} />
          </div>
        </Card>
      )}
      <section className="grid2">
        <Chart title="CPU compartida" sub="Quién usa los núcleos, como % de toda la máquina. El CI cede ante producción." cfg={cpu} />
        <Chart title="Memoria compartida" sub="Un solo pozo de RAM para todos. El hueco hasta el techo es memoria disponible." cfg={mem} />
      </section>
      <Caches state={state} />
    </div>
  );
}
