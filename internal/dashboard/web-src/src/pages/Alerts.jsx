// Alertas: active alerts, rules (switch + editable threshold), history and this
// device's push notifications.
import { useState, useEffect, useRef, useCallback } from 'react';
import {
  OctagonAlert, TriangleAlert, Info, CircleAlert, CircleCheck, Bell, BellOff, BellRing,
  Smartphone, Laptop, Download, Send, Trash2, History,
} from 'lucide-react';
import { api, ApiError } from '../lib/api';
import { toast } from '../components/ui';
import * as push from '../pwa/push';
import * as install from '../pwa/install';

export const meta = { title: 'Alertas', icon: 'bell' };

const SEV = {
  critical: { label: 'Crítica', Icon: OctagonAlert, cls: 'crit' },
  warning: { label: 'Aviso', Icon: TriangleAlert, cls: 'warn' },
  info: { label: 'Info', Icon: Info, cls: 'info' },
};
const sev = (s) => SEV[s] || SEV.info;
const THIS_DEVICE_KEY = 'runlight.pushDeviceId';

function readLocal(k) { try { return localStorage.getItem(k) || ''; } catch (e) { return ''; } }
function writeLocal(k, v) { try { if (v) localStorage.setItem(k, v); else localStorage.removeItem(k); } catch (e) { /* ignore */ } }

function since(iso, now) {
  const s = Math.max(0, Math.round((now - new Date(iso).getTime()) / 1000));
  if (s < 60) return 'hace unos segundos';
  if (s < 3600) return 'hace ' + Math.floor(s / 60) + ' min';
  if (s < 86400) return 'hace ' + Math.floor(s / 3600) + ' h';
  return 'hace ' + Math.floor(s / 86400) + ' d';
}
function stamp(iso) {
  return new Date(iso).toLocaleString('es', { day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit', hour12: false });
}
function span(a) {
  if (!a.resolvedAt) return '';
  const m = Math.max(1, Math.round((new Date(a.resolvedAt) - new Date(a.since)) / 60000));
  return m < 60 ? m + ' min' : Math.floor(m / 60) + ' h ' + String(m % 60).padStart(2, '0') + ' min';
}
function errText(e) {
  if (e instanceof ApiError && e.fields) {
    const v = Object.values(e.fields)[0];
    if (v) return v;
  }
  return (e && e.message) || 'Algo salió mal';
}
function shortUA(ua) {
  if (!ua) return 'Navegador desconocido';
  const os = /Android/.test(ua) ? 'Android' : /iPhone|iPad/.test(ua) ? 'iOS' : /Windows/.test(ua) ? 'Windows' : /Mac OS/.test(ua) ? 'macOS' : /Linux/.test(ua) ? 'Linux' : '';
  const br = /Edg\//.test(ua) ? 'Edge' : /Firefox\//.test(ua) ? 'Firefox' : /Chrome\//.test(ua) ? 'Chrome' : /Safari\//.test(ua) ? 'Safari' : 'Navegador';
  return os ? br + ' en ' + os : br;
}

function SevBadge({ s }) {
  const v = sev(s);
  return <span className={'x-sev x-sev-' + v.cls}><v.Icon size={15} aria-hidden="true" />{v.label}</span>;
}

function Switch({ on, onChange, label, busy }) {
  return (
    <button type="button" role="switch" aria-checked={on ? 'true' : 'false'} aria-label={label}
      className={'x-switch' + (on ? ' on' : '')} disabled={busy} onClick={() => onChange(!on)}>
      <span className="x-switch-knob" /><span className="x-switch-text">{on ? 'Activa' : 'Apagada'}</span>
    </button>
  );
}

function RuleRow({ rule, onSave }) {
  const cfg = rule.config;
  const hasThr = rule.max > 0;
  const [draft, setDraft] = useState(String(cfg.threshold));
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);
  useEffect(() => { setDraft(String(cfg.threshold)); setErr(''); }, [cfg.threshold]);

  const save = async (changes) => {
    setBusy(true);
    try { await onSave(changes); setErr(''); } catch (e) { setErr(errText(e)); } finally { setBusy(false); }
  };
  const commit = () => {
    if (!hasThr) return;
    const n = Number(draft.replace(',', '.'));
    if (draft.trim() === '' || !Number.isFinite(n)) { setErr('Escribe un número'); return; }
    if (n < rule.min || n > rule.max) { setErr('Debe estar entre ' + rule.min + ' y ' + rule.max + ' ' + rule.unit); return; }
    if (n === cfg.threshold) { setErr(''); return; }
    save({ ['alert.' + rule.id + '.threshold']: n });
  };
  const id = 'thr-' + rule.id.replace(/[^a-z0-9]/gi, '-');
  return (
    <li className={'x-rule' + (cfg.enabled ? '' : ' off')}>
      <div className="x-rule-main">
        <div className="x-rule-head"><strong>{rule.title}</strong><SevBadge s={rule.severity} /></div>
        <p className="x-muted">{rule.description}</p>
      </div>
      <div className="x-rule-ctl">
        {hasThr && (
          <div className="x-field">
            <label htmlFor={id}>{rule.thresholdLabel}</label>
            <div className="x-input-wrap">
              <input id={id} className={'x-input' + (err ? ' bad' : '')} type="number" inputMode="decimal"
                min={rule.min} max={rule.max} step={rule.step} value={draft} disabled={busy || !cfg.enabled}
                aria-invalid={err ? 'true' : 'false'} aria-describedby={err ? id + '-e' : undefined}
                onChange={(e) => setDraft(e.currentTarget.value)} onBlur={commit}
                onKeyDown={(e) => {
                  if (e.key === 'Enter') { e.preventDefault(); commit(); }
                  if (e.key === 'Escape') { setDraft(String(cfg.threshold)); setErr(''); }
                }} />
              <span className="x-unit">{rule.unit}</span>
            </div>
            {err && <span className="x-err" id={id + '-e'} role="alert"><CircleAlert size={14} aria-hidden="true" />{err}</span>}
          </div>
        )}
        <Switch on={cfg.enabled} busy={busy} label={'Regla ' + rule.title}
          onChange={(v) => save({ ['alert.' + rule.id + '.enabled']: v })} />
      </div>
    </li>
  );
}

function Devices({ devices, reload }) {
  const [state, setState] = useState('unsupported');
  const [label, setLabel] = useState('');
  const [busy, setBusy] = useState('');
  const [inst, setInst] = useState(install.getState());
  const mine = readLocal(THIS_DEVICE_KEY);

  const refresh = useCallback(() => push.status().then(setState).catch(() => setState('unsupported')), []);
  useEffect(() => { refresh(); return install.onChange(setInst); }, [refresh]);

  const run = async (name, fn, ok) => {
    setBusy(name);
    try { const r = await fn(); if (ok) toast(typeof ok === 'function' ? ok(r) : ok, 'ok'); }
    catch (e) { toast(errText(e), 'error'); }
    finally { setBusy(''); await refresh(); reload(); }
  };
  const enable = () => run('on', async () => {
    const d = await push.subscribe(label);
    writeLocal(THIS_DEVICE_KEY, d.id);
    setLabel('');
  }, 'Notificaciones activadas en este dispositivo');
  const disable = () => run('off', async () => { await push.unsubscribe(mine); writeLocal(THIS_DEVICE_KEY, ''); }, 'Este dispositivo ya no recibirá avisos');
  const testAll = (id) => run('test' + (id || ''), () => push.test(id), (r) => {
    if (r.sent && !r.failed) return 'Prueba enviada: revisa el dispositivo';
    if (r.sent) return 'Enviada a ' + r.sent + ', falló en ' + r.failed;
    return 'No se pudo entregar la prueba';
  });
  const removeDevice = (d) => run('rm' + d.id, async () => {
    if (d.id === mine) { await push.unsubscribe(d.id); writeLocal(THIS_DEVICE_KEY, ''); } else { await api.post('/api/push/unsubscribe', { id: d.id }); }
  }, 'Dispositivo quitado');

  const [StatusIcon, statusText] = {
    unsupported: [CircleAlert, 'Este navegador no admite notificaciones push.'],
    denied: [BellOff, 'Bloqueadas en este navegador. Actívalas en los ajustes del sitio y vuelve aquí.'],
    default: [Bell, 'Aún no activadas en este dispositivo.'],
    unsubscribed: [Bell, 'Permiso concedido, pero este dispositivo no está suscrito.'],
    subscribed: [BellRing, 'Activadas en este dispositivo.'],
  }[state] || [Bell, ''];

  return (
    <section className="x-card" aria-labelledby="x-dev-h">
      <div className="x-card-head"><h2 id="x-dev-h">Notificaciones en tus dispositivos</h2></div>
      <p className={'x-status x-status-' + state} role="status"><StatusIcon size={18} aria-hidden="true" /><span>{statusText}</span></p>
      {inst.kind === 'ios' && (
        <p className="x-hint"><Smartphone size={16} aria-hidden="true" /><span>{inst.hint} En iPhone las notificaciones solo funcionan con la app añadida a la pantalla de inicio.</span></p>
      )}
      {inst.kind === 'prompt' && (
        <p className="x-hint"><Download size={16} aria-hidden="true" /><span>Instala runlight para recibir avisos como una app.</span>
          <button type="button" className="x-btn" onClick={() => install.install()}>Instalar</button></p>
      )}
      <div className="x-actions">
        {(state === 'default' || state === 'unsubscribed') && (
          <>
            <div className="x-field x-grow"><label htmlFor="x-dev-label">Nombre de este dispositivo</label>
              <input id="x-dev-label" className="x-input" maxLength={40} placeholder="Mi móvil" value={label} onChange={(e) => setLabel(e.currentTarget.value)} /></div>
            <button type="button" className="x-btn x-primary" disabled={busy === 'on'} onClick={enable}><BellRing size={16} aria-hidden="true" />Activar en este dispositivo</button>
          </>
        )}
        {state === 'subscribed' && (
          <>
            <button type="button" className="x-btn" disabled={!!busy} onClick={() => testAll(mine || '')}><Send size={16} aria-hidden="true" />Enviar prueba</button>
            <button type="button" className="x-btn" disabled={!!busy} onClick={disable}><BellOff size={16} aria-hidden="true" />Desactivar aquí</button>
          </>
        )}
        {devices.length > 1 && (
          <button type="button" className="x-btn" disabled={!!busy} onClick={() => testAll('')}><Send size={16} aria-hidden="true" />Probar en todos</button>
        )}
      </div>
      <h3 className="x-sub">Dispositivos registrados</h3>
      {devices.length === 0 ? (
        <p className="x-empty"><Smartphone size={18} aria-hidden="true" />Ningún dispositivo recibe avisos todavía.</p>
      ) : (
        <ul className="x-list">
          {devices.map((d) => {
            const DevIcon = /Android|iPhone|iOS/.test(d.userAgent || '') ? Smartphone : Laptop;
            return (
              <li className="x-device" key={d.id}>
                <DevIcon size={20} aria-hidden="true" />
                <div className="x-device-main">
                  <strong>{d.label || 'Dispositivo'}{d.id === mine && <> <span className="x-tag">este dispositivo</span></>}</strong>
                  <span className="x-muted">{shortUA(d.userAgent)} · alta {stamp(d.createdAt)}</span>
                  {d.lastError
                    ? <span className="x-err"><TriangleAlert size={14} aria-hidden="true" />Último error: {d.lastError}</span>
                    : d.lastOk
                      ? <span className="x-ok"><CircleCheck size={14} aria-hidden="true" />Último envío correcto {stamp(d.lastOk)}</span>
                      : <span className="x-muted">Sin envíos todavía</span>}
                </div>
                <div className="x-device-act">
                  <button type="button" className="x-btn sm" disabled={!!busy} onClick={() => testAll(d.id)} aria-label={'Enviar prueba a ' + d.label}><Send size={15} aria-hidden="true" />Probar</button>
                  <button type="button" className="x-btn sm danger" disabled={!!busy} onClick={() => removeDevice(d)} aria-label={'Quitar ' + d.label}><Trash2 size={15} aria-hidden="true" />Quitar</button>
                </div>
              </li>
            );
          })}
        </ul>
      )}
    </section>
  );
}

export default function Alerts() {
  const [data, setData] = useState(null);
  const [devices, setDevices] = useState([]);
  const [error, setError] = useState('');
  const [now, setNow] = useState(Date.now());
  const alive = useRef(true);

  const load = useCallback(async () => {
    try {
      const [a, d] = await Promise.all([api.get('/api/alerts'), api.get('/api/push/devices').catch(() => ({ devices: [] }))]);
      if (!alive.current) return;
      setData(a); setDevices(d.devices || []); setError(''); setNow(Date.now());
    } catch (e) { if (alive.current) setError(errText(e)); }
  }, []);

  useEffect(() => {
    alive.current = true;
    load();
    const t = setInterval(() => { if (!document.hidden) load(); }, 5000);
    return () => { alive.current = false; clearInterval(t); };
  }, [load]);

  const saveRule = async (changes) => { await api.put('/api/settings', { changes }); await load(); };

  if (!data) {
    return (
      <div className="x-page">
        {error
          ? <p className="x-empty x-err" role="alert"><CircleAlert size={18} aria-hidden="true" />{error} <button type="button" className="x-btn sm" onClick={load}>Reintentar</button></p>
          : <p className="x-empty" role="status">Cargando alertas…</p>}
      </div>
    );
  }
  const active = data.active || [];
  const history = data.history || [];
  const rules = data.rules || [];

  return (
    <div className="x-page">
      {error && <p className="x-banner" role="alert"><TriangleAlert size={16} aria-hidden="true" />No se pudo actualizar ({error}). Mostrando lo último recibido.</p>}
      <section className="x-card" aria-labelledby="x-act-h">
        <div className="x-card-head"><h2 id="x-act-h">Alertas activas</h2>
          <span className="x-count">{active.length === 0 ? 'Ninguna' : active.length}</span></div>
        {active.length === 0 ? (
          <p className="x-empty x-empty-ok" role="status"><CircleCheck size={20} aria-hidden="true" />Todo en orden: ninguna regla está disparada.</p>
        ) : (
          <ul className="x-list">
            {active.map((a) => {
              const v = sev(a.severity);
              return (
                <li key={a.id} className={'x-alert x-alert-' + v.cls}>
                  <v.Icon size={22} aria-hidden="true" />
                  <div className="x-alert-main">
                    <div className="x-rule-head"><strong>{a.title}</strong><SevBadge s={a.severity} /></div>
                    <p>{a.body}</p>
                    <span className="x-muted">Desde {since(a.since, now)} · {a.notifications} {a.notifications === 1 ? 'aviso enviado' : 'avisos enviados'}</span>
                  </div>
                </li>
              );
            })}
          </ul>
        )}
      </section>

      <Devices devices={devices} reload={load} />

      <section className="x-card" aria-labelledby="x-rules-h">
        <div className="x-card-head"><h2 id="x-rules-h">Reglas</h2></div>
        <p className="x-muted x-lead">Los cambios se aplican al instante. Una regla apagada resuelve sus alertas sin avisar.</p>
        <ul className="x-list">{rules.map((r) => <RuleRow key={r.id} rule={r} onSave={saveRule} />)}</ul>
      </section>

      <section className="x-card" aria-labelledby="x-hist-h">
        <div className="x-card-head"><h2 id="x-hist-h">Historial</h2><span className="x-count">{history.length}</span></div>
        {history.length === 0 ? (
          <p className="x-empty"><History size={18} aria-hidden="true" />Todavía no hay alertas resueltas.</p>
        ) : (
          <ul className="x-list">
            {history.map((a) => {
              const v = sev(a.severity);
              return (
                <li key={a.id + a.since} className="x-hist">
                  <v.Icon size={18} aria-hidden="true" />
                  <div className="x-alert-main">
                    <div className="x-rule-head"><strong>{a.title}</strong><SevBadge s={a.severity} /></div>
                    <p className="x-muted">{a.body}</p>
                    <span className="x-muted">{stamp(a.since)}{a.resolvedAt && <> · <span className="x-ok"><CircleCheck size={13} aria-hidden="true" />Resuelta tras {span(a)}</span></>}</span>
                  </div>
                </li>
              );
            })}
          </ul>
        )}
      </section>
    </div>
  );
}
