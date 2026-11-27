// Configuración: a form driven by the schema of /api/settings. Edits stay as
// pending changes until saved; the server validates again and reports per field.
import { useState, useEffect, useMemo, useCallback } from 'react';
import { CircleAlert, Pause, Play, Eraser, RotateCcw, ChevronUp, ChevronDown, History, Undo2, Save } from 'lucide-react';
import { api, ApiError } from '../lib/api';
import { useLive } from '../lib/live';
import { toast } from '../components/ui';

export const meta = { title: 'Configuración', icon: 'settings' };

function fmt(v) {
  if (typeof v === 'boolean') return v ? 'activa' : 'apagada';
  if (typeof v === 'number') return Number.isInteger(v) ? String(v) : String(Math.round(v * 100) / 100);
  return String(v);
}
function stamp(iso) {
  return new Date(iso).toLocaleString('es', { day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit', hour12: false });
}
const same = (a, b) => (typeof a === 'number' && typeof b === 'number' ? Math.abs(a - b) < 1e-9 : a === b);

// Returns an error message or '' for a draft entry of a field.
function check(f, raw) {
  if (f.kind === 'bool') return '';
  const s = String(raw).trim();
  if (s === '') return 'Escribe un número';
  const n = Number(s.replace(',', '.'));
  if (!Number.isFinite(n)) return 'Debe ser un número';
  if (f.kind === 'int' && !Number.isInteger(n)) return 'Debe ser un número entero';
  if (n < f.min || n > f.max) return 'Debe estar entre ' + fmt(f.min) + ' y ' + fmt(f.max) + (f.unit ? ' ' + f.unit : '');
  return '';
}
const toValue = (f, raw) => (f.kind === 'bool' ? !!raw : Number(String(raw).trim().replace(',', '.')));

function Field({ f, value, draft, error, onChange, onReset }) {
  const id = 'f-' + f.key.replace(/[^a-z0-9]/gi, '-');
  const shown = draft !== undefined ? draft : value;
  const dirty = draft !== undefined;
  const isDefault = same(dirty && f.kind !== 'bool' ? toValue(f, draft) : shown, f.default);
  const hid = id + '-h';
  const eid = id + '-e';
  const describedBy = [f.help ? hid : '', error ? eid : ''].join(' ').trim() || undefined;
  return (
    <div className={'x-cfg' + (dirty ? ' dirty' : '') + (error ? ' bad' : '')}>
      <div className="x-cfg-text">
        <label htmlFor={id}>{f.label}</label>
        {f.help && <span className="x-muted" id={hid}>{f.help}</span>}
        {error && <span className="x-err" id={eid} role="alert"><CircleAlert size={14} aria-hidden="true" />{error}</span>}
      </div>
      <div className="x-cfg-ctl">
        {f.kind === 'bool' ? (
          <button type="button" id={id} role="switch" aria-checked={shown ? 'true' : 'false'} aria-describedby={describedBy}
            className={'x-switch' + (shown ? ' on' : '')} onClick={() => onChange(!shown)}>
            <span className="x-switch-knob" /><span className="x-switch-text">{shown ? 'Activa' : 'Apagada'}</span>
          </button>
        ) : (
          <div className="x-input-wrap">
            <input id={id} className={'x-input' + (error ? ' bad' : '')} type="number" inputMode={f.kind === 'int' ? 'numeric' : 'decimal'}
              min={f.min} max={f.max} step={f.step || (f.kind === 'int' ? 1 : 'any')} value={shown}
              aria-invalid={error ? 'true' : 'false'} aria-describedby={describedBy}
              onChange={(e) => onChange(e.currentTarget.value)} />
            {f.unit && <span className="x-unit">{f.unit}</span>}
          </div>
        )}
        <button type="button" className="x-btn sm x-ghost" disabled={isDefault} onClick={onReset}
          aria-label={'Restablecer ' + f.label + ' al valor por defecto (' + fmt(f.default) + ')'} title={'Por defecto: ' + fmt(f.default)}>
          <RotateCcw size={14} aria-hidden="true" />Por defecto</button>
      </div>
    </div>
  );
}

function Actions({ paused }) {
  const [busy, setBusy] = useState('');
  const act = async (name, okMsg) => {
    setBusy(name);
    try { const r = await api.post('/api/actions/' + name, {}); toast((r && r.message) || okMsg, 'ok'); }
    catch (e) { toast((e && e.message) || 'No se pudo ejecutar la acción', 'error'); }
    finally { setBusy(''); }
  };
  return (
    <section className="x-card" aria-labelledby="x-actn-h">
      <div className="x-card-head"><h2 id="x-actn-h">Acciones</h2>
        {paused === true && <span className="x-sev x-sev-warn"><Pause size={15} aria-hidden="true" />En pausa</span>}</div>
      <div className="x-actions">
        {paused
          ? <button type="button" className="x-btn x-primary" disabled={!!busy} onClick={() => act('resume', 'runlight reanudado')}><Play size={16} aria-hidden="true" />Reanudar</button>
          : <button type="button" className="x-btn" disabled={!!busy} onClick={() => act('pause', 'runlight en pausa')}><Pause size={16} aria-hidden="true" />Pausar</button>}
        <button type="button" className="x-btn" disabled={!!busy} onClick={() => act('janitor-now', 'Limpieza lanzada')}><Eraser size={16} aria-hidden="true" />Limpiar ahora</button>
      </div>
      <p className="x-muted">Pausar no arranca runners nuevos; los que están corriendo terminan su trabajo. Limpiar ahora lanza una pasada de limpieza de cachés sin esperar al intervalo.</p>
    </section>
  );
}

function Audit({ audit, labels }) {
  const [open, setOpen] = useState(false);
  return (
    <section className="x-card" aria-labelledby="x-aud-h">
      <div className="x-card-head"><h2 id="x-aud-h">Cambios recientes</h2>
        <button type="button" className="x-btn sm x-ghost" aria-expanded={open ? 'true' : 'false'} aria-controls="x-aud-list" onClick={() => setOpen(!open)}>
          {open ? <ChevronUp size={15} aria-hidden="true" /> : <ChevronDown size={15} aria-hidden="true" />}{open ? 'Ocultar' : 'Mostrar ' + audit.length}</button></div>
      {audit.length === 0 && <p className="x-empty"><History size={18} aria-hidden="true" />Aún no se ha cambiado nada.</p>}
      {open && audit.length > 0 && (
        <ul className="x-list" id="x-aud-list">
          {audit.slice(0, 50).map((e, i) => (
            <li className="x-hist" key={i}>
              <History size={18} aria-hidden="true" />
              <div className="x-alert-main"><strong>{stamp(e.at)} · {e.actor}</strong>
                <ul className="x-changes">{(e.changes || []).map((c) => (
                  <li key={c.key}>{labels[c.key] || c.key}: <span className="x-muted">{fmt(c.from)}</span> → <strong>{fmt(c.to)}</strong></li>
                ))}</ul></div>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

export default function Config() {
  const live = useLive();
  const [data, setData] = useState(null);
  const [loadErr, setLoadErr] = useState('');
  const [draft, setDraft] = useState({});
  const [srvErr, setSrvErr] = useState({});
  const [saving, setSaving] = useState(false);

  const load = useCallback(async () => {
    try { setData(await api.get('/api/settings')); setLoadErr(''); } catch (e) { setLoadErr((e && e.message) || 'No se pudo cargar'); }
  }, []);
  useEffect(() => { load(); }, [load]);

  const schema = useMemo(() => (data && data.schema) || [], [data]);
  const byKey = useMemo(() => Object.fromEntries(schema.map((f) => [f.key, f])), [schema]);
  const groups = useMemo(() => {
    const out = []; const idx = {};
    for (const f of schema) { if (!(f.group in idx)) { idx[f.group] = out.length; out.push({ name: f.group, fields: [] }); } out[idx[f.group]].fields.push(f); }
    return out;
  }, [schema]);
  const labels = useMemo(() => Object.fromEntries(schema.map((f) => [f.key, f.label])), [schema]);

  const clientErr = useMemo(() => {
    const e = {};
    for (const k of Object.keys(draft)) { const f = byKey[k]; if (f) { const m = check(f, draft[k]); if (m) e[k] = m; } }
    return e;
  }, [draft, byKey]);

  const pending = Object.keys(draft);
  const hasClientErr = Object.keys(clientErr).length > 0;

  useEffect(() => {
    if (!pending.length) return undefined;
    const h = (e) => { e.preventDefault(); e.returnValue = ''; };
    window.addEventListener('beforeunload', h);
    return () => window.removeEventListener('beforeunload', h);
  }, [pending.length]);

  const setField = (f, raw) => {
    const cur = data.values[f.key];
    setSrvErr((p) => { if (!(f.key in p)) return p; const n = { ...p }; delete n[f.key]; return n; });
    setDraft((d) => {
      const n = { ...d };
      const unchanged = f.kind === 'bool' ? raw === cur : (check(f, raw) === '' && same(toValue(f, raw), cur));
      if (unchanged) delete n[f.key]; else n[f.key] = raw;
      return n;
    });
  };
  const discard = () => { setDraft({}); setSrvErr({}); };
  const resetGroup = (g) => {
    setDraft((d) => {
      const n = { ...d };
      for (const f of g.fields) { if (same(data.values[f.key], f.default)) delete n[f.key]; else n[f.key] = f.default; }
      return n;
    });
    setSrvErr({});
  };

  const save = async () => {
    if (hasClientErr || !pending.length) return;
    const changes = {};
    for (const k of pending) changes[k] = toValue(byKey[k], draft[k]);
    setSaving(true);
    try {
      const r = await api.put('/api/settings', { changes });
      setDraft({}); setSrvErr({});
      toast(r.applied && r.applied.length ? 'Cambios guardados y aplicados (' + r.applied.length + ')' : 'No había nada que cambiar', 'ok');
      await load();
    } catch (e) {
      if (e instanceof ApiError && e.fields && Object.keys(e.fields).length) {
        setSrvErr(e.fields);
        toast('Revisa los campos marcados: no se guardó nada', 'error');
      } else { toast((e && e.message) || 'No se pudo guardar', 'error'); }
    } finally { setSaving(false); }
  };

  if (!data) {
    return (
      <div className="x-page">
        {loadErr
          ? <p className="x-empty x-err" role="alert"><CircleAlert size={18} aria-hidden="true" />{loadErr} <button type="button" className="x-btn sm" onClick={load}>Reintentar</button></p>
          : <p className="x-empty" role="status">Cargando configuración…</p>}
      </div>
    );
  }

  const paused = live && live.state ? !!live.state.paused : undefined;
  const effective = (f) => {
    const d = draft[f.key];
    if (d === undefined) return data.values[f.key];
    if (f.kind === 'bool') return d;
    return check(f, d) ? d : toValue(f, d);
  };
  const nonDefault = (g) => g.fields.some((f) => !same(effective(f), f.default));

  return (
    <div className="x-page x-config">
      <Actions paused={paused} />
      {groups.map((g) => (
        <section className="x-card" key={g.name} aria-labelledby={'x-g-' + g.name}>
          <div className="x-card-head"><h2 id={'x-g-' + g.name}>{g.name}</h2>
            <button type="button" className="x-btn sm x-ghost" disabled={!nonDefault(g)} onClick={() => resetGroup(g)}><RotateCcw size={14} aria-hidden="true" />Restablecer grupo</button></div>
          <div className="x-cfg-list">{g.fields.map((f) => (
            <Field key={f.key} f={f} value={data.values[f.key]} draft={draft[f.key]}
              error={clientErr[f.key] || srvErr[f.key] || ''} onChange={(v) => setField(f, v)}
              onReset={() => setField(f, f.default)} />
          ))}</div>
        </section>
      ))}
      <Audit audit={data.audit || []} labels={labels} />
      <div className={'x-savebar' + (pending.length ? ' show' : '')} role="region" aria-label="Cambios pendientes" aria-hidden={pending.length ? 'false' : 'true'}>
        <span className="x-savebar-text" role="status" aria-live="polite">
          {hasClientErr ? <CircleAlert size={18} aria-hidden="true" /> : <Save size={18} aria-hidden="true" />}
          {pending.length} {pending.length === 1 ? 'cambio pendiente' : 'cambios pendientes'}{hasClientErr ? ': corrige los campos marcados' : ''}</span>
        <button type="button" className="x-btn" disabled={saving || !pending.length} tabIndex={pending.length ? 0 : -1} onClick={discard}><Undo2 size={16} aria-hidden="true" />Descartar</button>
        <button type="button" className="x-btn x-primary" disabled={saving || hasClientErr || !pending.length} tabIndex={pending.length ? 0 : -1} onClick={save}><Save size={16} aria-hidden="true" />{saving ? 'Guardando…' : 'Guardar'}</button>
      </div>
    </div>
  );
}
