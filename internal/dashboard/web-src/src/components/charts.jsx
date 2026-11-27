// Time-series chart in plain SVG (lines, stacked areas, steps, reference lines, hover tooltip, legend,
// and a table view for accessibility). Colours are CSS variables so a theme change needs no redraw.
//
// cfg: { title, points, series: [{ label, color, get(p), dash?, area? }], fmt(v), yFmt(v), yMax(points),
//        step?, stacked?, refs?: [{ v, label, color }], legendExtra?: [{ label, value, dash? }], height? }
import { useEffect, useRef, useState } from 'react';
import { Icon } from './Icon.jsx';
import { Table } from './ui.jsx';
import { hms } from '../lib/format.js';

const M = { l: 46, r: 12, t: 10, b: 22 };

export function Chart({ title, sub, cfg }) {
  const [asTable, setAsTable] = useState(false);
  const [width, setWidth] = useState(0);
  const [hover, setHover] = useState(null); // { i, boxW }
  const area = useRef(null);

  useEffect(() => {
    const el = area.current;
    if (!el) return undefined;
    const ro = new ResizeObserver((es) => {
      const w = Math.round(es[0].contentRect.width);
      if (w > 0) setWidth((prev) => (Math.abs(prev - w) > 1 ? w : prev));
    });
    ro.observe(el);
    return () => ro.disconnect();
  }, [asTable]);

  const pts = cfg.points;
  const last = pts[pts.length - 1];

  const legend = (
    <div className="legend">
      {cfg.series.map((s) => (
        <span key={s.label}>
          <span className={'sw' + (s.dash ? ' dash' : '')} style={s.dash ? undefined : { background: s.color }} />
          {s.label}{last && <b className="num">{cfg.fmt(s.get(last))}</b>}
        </span>
      ))}
      {(cfg.legendExtra || []).map((x) => (
        <span key={x.label}><span className={'sw' + (x.dash ? ' dash' : '')} />{x.label}{x.value && <b className="num">{x.value}</b>}</span>
      ))}
    </div>
  );

  const head = (
    <div className="cardhead">
      <div><h2>{title}</h2><p className="sub">{sub}</p></div>
      <button type="button" className="btn" aria-pressed={asTable} onClick={() => { setAsTable(!asTable); setHover(null); }}>
        <Icon name={asTable ? 'chart-line' : 'table-2'} size={16} /><span>{asTable ? 'Ver gráfico' : 'Ver como tabla'}</span>
      </button>
    </div>
  );

  if (asTable) {
    const stride = Math.max(1, Math.floor(pts.length / 14));
    const rows = [];
    for (let i = pts.length - 1; i >= 0 && rows.length < 14; i -= stride) rows.push([hms(pts[i].t), ...cfg.series.map((s) => cfg.fmt(s.get(pts[i])))]);
    return (
      <section className="card" aria-label={title}>
        {head}
        <Table cols={[{ h: 'Hora' }, ...cfg.series.map((s) => ({ h: s.label, r: true }))]} rows={rows} empty="Sin datos" />
        {legend}
      </section>
    );
  }

  return (
    <section className="card" aria-label={title}>
      {head}
      <div className="chart" ref={area}>
        {pts.length && width ? <Plot cfg={cfg} width={width} hover={hover} setHover={setHover} /> : !pts.length ? <div className="empty">Esperando datos…</div> : null}
      </div>
      {legend}
    </section>
  );
}

function Plot({ cfg, width: W, hover, setHover }) {
  const pts = cfg.points;
  const H = cfg.height || 190;
  const t0 = pts[0].t;
  const t1 = pts[pts.length - 1].t;
  const span = Math.max(1, t1 - t0);
  const x = (t) => M.l + ((t - t0) / span) * (W - M.l - M.r);
  const ymax = cfg.yMax(pts);
  const y = (v) => H - M.b - Math.min(1, Math.max(0, v / ymax)) * (H - M.t - M.b);
  const pathOf = (vals, step) => {
    let d = '';
    pts.forEach((p, i) => {
      const px = x(p.t).toFixed(1);
      const py = y(vals[i]).toFixed(1);
      if (i === 0) d += `M${px},${py}`;
      else if (step) d += `L${px},${y(vals[i - 1]).toFixed(1)}L${px},${py}`;
      else d += `L${px},${py}`;
    });
    return d;
  };

  const grid = [];
  for (let i = 0; i <= 4; i++) {
    const v = (ymax * i) / 4;
    grid.push(<line key={'l' + i} x1={M.l} x2={W - M.r} y1={y(v)} y2={y(v)} className={i ? 'gridline' : 'axis'} />);
    grid.push(<text key={'t' + i} x={M.l - 6} y={y(v) + 4} textAnchor="end">{cfg.yFmt(v)}</text>);
  }
  const mins = Math.round(span / 60000);

  const marks = [];
  if (cfg.stacked) {
    let lower = pts.map(() => 0);
    cfg.series.forEach((sr, si) => {
      const upper = pts.map((p, i) => lower[i] + sr.get(p));
      let d = pathOf(upper, false);
      for (let i = pts.length - 1; i >= 0; i--) d += `L${x(pts[i].t).toFixed(1)},${y(lower[i]).toFixed(1)}`;
      marks.push(<path key={'s' + si} d={d + 'Z'} fill={sr.color} fillOpacity="0.92" stroke="var(--surface)" strokeWidth="1.5" strokeLinejoin="round" />);
      lower = upper;
    });
  } else {
    cfg.series.forEach((sr, si) => {
      const d = pathOf(pts.map((p) => sr.get(p)), cfg.step);
      if (sr.area) marks.push(<path key={'a' + si} d={d + `L${x(t1)},${y(0)}L${x(t0)},${y(0)}Z`} fill={sr.color} fillOpacity="0.22" />);
      marks.push(<path key={'p' + si} d={d} fill="none" stroke={sr.color} strokeWidth={sr.area ? 1.5 : 2} strokeLinejoin="round" strokeLinecap="round" strokeDasharray={sr.dash ? '5 4' : undefined} />);
    });
  }
  const refs = (cfg.refs || []).filter((r) => r.v != null && r.v <= ymax).map((r, i) => (
    <g key={'r' + i}>
      <line x1={M.l} x2={W - M.r} y1={y(r.v)} y2={y(r.v)} className="ref" stroke={r.color} />
      <text x={W - M.r - 4} y={y(r.v) - 4} textAnchor="end" className="reflabel">{r.label}</text>
    </g>
  ));

  const move = (ev) => {
    const box = ev.currentTarget.ownerSVGElement.getBoundingClientRect();
    const px = ((ev.clientX - box.left) / box.width) * W;
    const t = t0 + ((px - M.l) / (W - M.l - M.r)) * span;
    let lo = 0;
    let hi = pts.length - 1;
    while (lo < hi) { const mid = (lo + hi) >> 1; if (pts[mid].t < t) lo = mid + 1; else hi = mid; }
    setHover({ i: lo, boxW: box.width });
  };
  const p = hover && pts[Math.min(hover.i, pts.length - 1)];
  const tipLeft = p ? Math.min(Math.max((x(p.t) / W) * hover.boxW + 12, 0), hover.boxW - 170) : 0;

  return (
    <>
      <svg viewBox={`0 0 ${W} ${H}`} role="img" aria-label={cfg.title}>
        {grid}
        <text x={M.l} y={H - 5}>{'−' + mins + ' min'}</text>
        <text x={(M.l + W - M.r) / 2} y={H - 5} textAnchor="middle">{'−' + Math.round(mins / 2) + ' min'}</text>
        <text x={W - M.r} y={H - 5} textAnchor="end">ahora</text>
        {marks}{refs}
        {p && <line className="cursor" x1={x(p.t)} x2={x(p.t)} y1={M.t} y2={H - M.b} />}
        <rect className="hit" x={M.l} y={M.t} width={W - M.l - M.r} height={H - M.t - M.b} fill="transparent"
          onPointerMove={move} onPointerDown={move} onPointerLeave={() => setHover(null)} />
      </svg>
      {p && (
        <div className="tip" style={{ left: tipLeft + 'px', top: '8px' }}>
          <div className="t">{hms(p.t)}</div>
          {cfg.series.map((sr) => (
            <div className="row" key={sr.label}>
              <span className={'sw' + (sr.dash ? ' dash' : '')} style={sr.dash ? undefined : { background: sr.color }} />
              <span className="grow">{sr.label.split(' (')[0]}</span><b className="num">{cfg.fmt(sr.get(p))}</b>
            </div>
          ))}
        </div>
      )}
    </>
  );
}
