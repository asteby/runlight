// #/servidor - the shared machine: memory split, swap/disk/load, top processes, CPU and memory charts, caches.
import { useState } from 'react';
import { useLive, useProcs } from '../lib/live.js';
import { gb, hms, pct } from '../lib/format.js';
import { LEVEL_ICON, LEVEL_METER } from '../lib/decision.js';
import { Icon } from '../components/Icon.jsx';
import { Card, Table, Meter, Chip, Spinner } from '../components/ui.jsx';
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

function Facts({ p, c }) {
  const swapPct = p.swapTotalMb ? (p.swapUsedMb / p.swapTotalMb) * 100 : 0;
  const sl = swapPct >= 90 ? 'bad' : swapPct >= 60 ? 'watch' : 'ok';
  const diskUsed = p.diskTotalMb ? (1 - p.diskFreeMb / p.diskTotalMb) * 100 : 0;
  const dl = diskUsed >= 90 ? 'bad' : diskUsed >= 80 ? 'watch' : 'ok';
  const loadPct = (p.load1 / c.cores) * 100;
  const ll = loadPct >= 150 ? 'bad' : loadPct >= 100 ? 'watch' : 'ok';
  return (
    <div className="facts">
      <Fact level={sl} label="Swap" value={`${gb(p.swapUsedMb)} de ${gb(p.swapTotalMb)}`} meter={swapPct}
        note={sl === 'bad' ? 'Casi lleno: el sistema está paginando a disco y todo se vuelve lento.' : sl === 'watch' ? 'Uso alto de swap.' : 'Sin problema.'} />
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
      {p && (
        <Card title="Servidor compartido" sub={`${c.cores} núcleos · ${gb(c.memTotalMb)} de RAM compartidos entre producción, runlight y el resto. Lo que no usa uno lo aprovecha otro.`}>
          <MemorySplit p={p} c={c} />
          <div className="grid2">
            <Procs />
            <Facts p={p} c={c} />
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
