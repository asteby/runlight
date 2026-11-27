// Shared UI kit. Status is always icon + text (never colour alone). Props are documented per component.
// Kinds: 'good' | 'warn' | 'crit' | 'info' | 'rl' (runlight orange) | 'muted'.
import { useEffect, useRef, useState, useSyncExternalStore } from 'react';
import { Icon } from './Icon.jsx';

const cx = (...a) => a.filter(Boolean).join(' ');

export const STATUS_ICON = {
  good: 'circle-check', warn: 'triangle-alert', crit: 'circle-x', info: 'info', rl: 'play', muted: 'circle',
};

// Card({ title, sub, actions, id, class }) - a surface with an optional header row.
export function Card({ title, sub, actions, id, class: cls, children }) {
  return (
    <section className={cx('card', cls)} id={id} aria-label={typeof title === 'string' ? title : undefined}>
      {(title || actions) && (
        <div className="cardhead">
          <div>{title && <h2>{title}</h2>}{sub && <p className="sub">{sub}</p>}</div>
          {actions && <div className="cardactions">{actions}</div>}
        </div>
      )}
      {children}
    </section>
  );
}

// Section({ title, sub, actions }) - a heading group on the page background (no surface).
export function Section({ title, sub, actions, id, class: cls, children }) {
  return (
    <section className={cx('sect', cls)} id={id} aria-label={typeof title === 'string' ? title : undefined}>
      <div className="secthead">
        <div><h2>{title}</h2>{sub && <p className="sub">{sub}</p>}</div>
        {actions && <div className="cardactions">{actions}</div>}
      </div>
      {children}
    </section>
  );
}

export function Spinner({ label = 'Cargando…', block = false }) {
  return (
    <span className={cx('spinner', block && 'block')} role="status">
      <Icon name="loader" class="spin" />
      <span className={block ? undefined : 'sr'}>{label}</span>
    </span>
  );
}

// Button({ variant: 'default'|'primary'|'danger'|'ghost', icon, busy, ...native props })
export function Button({ variant = 'default', icon, busy = false, class: cls, children, disabled, type = 'button', ...rest }) {
  return (
    <button type={type} className={cx('btn', variant !== 'default' && variant, cls)} disabled={disabled || busy} aria-busy={busy ? 'true' : undefined} {...rest}>
      {busy ? <Icon name="loader" size={16} class="spin" /> : icon && <Icon name={icon} size={16} />}
      {children != null && <span>{children}</span>}
    </button>
  );
}

// IconButton({ icon, label, count }) - square 40px target; `label` is the accessible name (and tooltip).
export function IconButton({ icon, label, count, class: cls, ...rest }) {
  return (
    <button type="button" className={cx('iconbtn', cls)} aria-label={label} title={label} {...rest}>
      <Icon name={icon} size={20} />
      {count > 0 && <span className="count" aria-hidden="true">{count > 99 ? '99+' : count}</span>}
    </button>
  );
}

// Badge({ kind, icon }) - small status marker. The icon defaults per kind.
export function Badge({ kind = 'info', icon, class: cls, children }) {
  return <span className={cx('badge', kind, cls)}><Icon name={icon || STATUS_ICON[kind] || 'info'} size={14} />{children}</span>;
}

// Chip({ pressed, onClick }) - toggle chip used for filters and tabs.
export function Chip({ pressed, children, ...rest }) {
  return <button type="button" className="chip" aria-pressed={!!pressed} {...rest}>{children}</button>;
}

// Toggle({ checked, onChange(next), label, disabled, ariaLabel }) - switch with a visible label.
export function Toggle({ checked, onChange, label, ariaLabel, disabled, class: cls, ...rest }) {
  return (
    <button type="button" role="switch" className={cx('toggle', cls)} aria-checked={!!checked} aria-label={label ? undefined : ariaLabel}
      disabled={disabled} onClick={() => onChange && onChange(!checked)} {...rest}>
      <span className="track" aria-hidden="true"><span className="thumb" /></span>
      {label && <span className="tlabel">{label}</span>}
    </button>
  );
}

const decimals = (step) => { const s = String(step); const i = s.indexOf('.'); return i < 0 ? 0 : s.length - i - 1; };

// NumberField({ value, onChange(number), min, max, step, unit, label, help, error, disabled, id })
// Free typing is kept as text; onChange fires only with finite numbers. Blur restores the committed value.
let fieldSeq = 0;
export function NumberField({ value, onChange, min, max, step = 1, unit, label, help, error, disabled, id }) {
  const uid = useRef(id || 'nf' + ++fieldSeq).current;
  const d = decimals(step);
  const fmt = (v) => (v == null || Number.isNaN(v) ? '' : Number(v).toFixed(d));
  const [text, setText] = useState(fmt(value));
  const typing = useRef(false);
  useEffect(() => { if (!typing.current) setText(fmt(value)); }, [value]); // eslint-disable-line react-hooks/exhaustive-deps
  const clamp = (n) => Math.min(max ?? Infinity, Math.max(min ?? -Infinity, n));
  const bump = (dir) => {
    const base = Number.isFinite(value) ? value : (min ?? 0);
    const n = clamp(Number((Math.round(base / step) * step + dir * step).toFixed(d)));
    setText(fmt(n));
    if (onChange) onChange(n);
  };
  const onInput = (e) => {
    typing.current = true;
    setText(e.target.value);
    const n = parseFloat(String(e.target.value).replace(',', '.'));
    if (Number.isFinite(n) && onChange) onChange(n);
  };
  const hint = [help, min != null && max != null ? `Entre ${min} y ${max}${unit ? ' ' + unit : ''}.` : null].filter(Boolean).join(' ');
  return (
    <div className={cx('field', error && 'invalid')}>
      {label && <label htmlFor={uid}>{label}</label>}
      <div className="numfield">
        <button type="button" className="step" aria-label="Disminuir" disabled={disabled || (min != null && value <= min)} onClick={() => bump(-1)}><Icon name="minus" size={16} /></button>
        <input id={uid} type="number" inputMode="decimal" value={text} min={min} max={max} step={step} disabled={disabled}
          aria-invalid={error ? 'true' : undefined} aria-describedby={uid + '-d'} onChange={onInput}
          onBlur={() => { typing.current = false; setText(fmt(value)); }} />
        {unit && <span className="unit">{unit}</span>}
        <button type="button" className="step" aria-label="Aumentar" disabled={disabled || (max != null && value >= max)} onClick={() => bump(1)}><Icon name="plus" size={16} /></button>
      </div>
      <div id={uid + '-d'} className="fieldnote">
        {error ? <span className="err" role="alert"><Icon name="circle-alert" size={14} />{error}</span> : hint}
      </div>
    </div>
  );
}

// Meter({ value 0-100, kind: 'rl'|'prod'|'good'|'warn'|'crit'|'muted', mark 0-100, label })
// Decorative unless `label` is given (the number is always shown in text next to it).
export function Meter({ value, kind = 'rl', mark, label, class: cls }) {
  const v = Math.min(100, Math.max(0, Number.isFinite(value) ? value : 0));
  const a11y = label ? { role: 'meter', 'aria-label': label, 'aria-valuemin': 0, 'aria-valuemax': 100, 'aria-valuenow': Math.round(v) } : { 'aria-hidden': 'true' };
  return (
    <div className={cx('meter', kind, cls)} {...a11y}>
      <i style={{ width: v + '%' }} />
      {mark != null && <b style={{ left: Math.min(100, Math.max(0, mark)) + '%' }} />}
    </div>
  );
}

export function EmptyState({ icon = 'info', title, children, class: cls }) {
  return (
    <div className={cx('emptystate', cls)}>
      <Icon name={icon} size={22} />
      {title && <p className="et">{title}</p>}
      {children && <p className="ed">{children}</p>}
    </div>
  );
}

// Table({ cols: [{ h, r }], rows: [[cell, ...]], empty, stack })
// Cells are strings or elements (never HTML). `r` right-aligns numbers. `stack` turns rows into
// labelled cards under 640 px instead of scrolling sideways.
export function Table({ cols, rows, empty = 'Sin datos.', stack = false, class: cls }) {
  if (!rows.length) return <EmptyState icon="circle-dot">{empty}</EmptyState>;
  return (
    <div className="tablewrap">
      <table className={cx(stack && 'stacked', cls)}>
        <thead><tr>{cols.map((c) => <th key={c.h} className={c.r ? 'r' : undefined}>{c.h}</th>)}</tr></thead>
        <tbody>
          {rows.map((r, ri) => (
            <tr key={ri}>{r.map((cell, i) => <td key={i} className={cols[i].r ? 'r num' : undefined} data-label={cols[i].h}>{cell}</td>)}</tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export function RepoJob({ repo, job }) {
  return <span><span className="repo">{repo} · </span>{job}</span>;
}

// ---- toasts ----
const KIND_ALIAS = { ok: 'good', success: 'good', error: 'crit', danger: 'crit', warning: 'warn' };
let toasts = [];
let toastSeq = 0;
const toastSubs = new Set();
const emitToasts = () => toastSubs.forEach((f) => f());
const subscribeToasts = (fn) => { toastSubs.add(fn); return () => toastSubs.delete(fn); };

// toast('Guardado', 'good') - kinds: good | info | warn | crit (aliases: ok, success, error, danger, warning)
export function toast(msg, kind = 'info', ms = 4500) {
  const t = { id: ++toastSeq, msg, kind: KIND_ALIAS[kind] || kind };
  toasts = [...toasts, t].slice(-4);
  emitToasts();
  setTimeout(() => dismiss(t.id), ms);
  return t.id;
}
function dismiss(id) { toasts = toasts.filter((t) => t.id !== id); emitToasts(); }

export function ToastHost() {
  const list = useSyncExternalStore(subscribeToasts, () => toasts);
  return (
    <div className="toasts" role="region" aria-label="Avisos" aria-live="polite">
      {list.map((t) => (
        <div className={'toast ' + t.kind} key={t.id}>
          <Icon name={STATUS_ICON[t.kind] || 'info'} size={18} />
          <span className="grow">{t.msg}</span>
          <button type="button" className="toastx" aria-label="Cerrar aviso" onClick={() => dismiss(t.id)}><Icon name="x" size={16} /></button>
        </div>
      ))}
    </div>
  );
}
