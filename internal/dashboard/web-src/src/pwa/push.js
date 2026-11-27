// Web Push helpers for this device. All calls go through the dashboard API.
import { api } from '../lib/api';

export function supported() {
  return typeof window !== 'undefined' && 'serviceWorker' in navigator && 'PushManager' in window && 'Notification' in window;
}

async function currentSub() {
  let reg = await navigator.serviceWorker.getRegistration('/');
  if (!reg) reg = await navigator.serviceWorker.register('/sw.js', { scope: '/' });
  await navigator.serviceWorker.ready;
  return { reg, sub: await reg.pushManager.getSubscription() };
}

// 'unsupported' | 'denied' | 'default' | 'subscribed' | 'unsubscribed'
export async function status() {
  if (!supported()) return 'unsupported';
  if (Notification.permission === 'denied') return 'denied';
  try {
    const { sub } = await currentSub();
    if (sub) return 'subscribed';
  } catch (e) { /* treat as not subscribed */ }
  return Notification.permission === 'granted' ? 'unsubscribed' : 'default';
}

function b64urlToBytes(s) {
  const pad = '='.repeat((4 - (s.length % 4)) % 4);
  const raw = atob((s + pad).replace(/-/g, '+').replace(/_/g, '/'));
  const out = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i);
  return out;
}

// Asks permission (call it from a user gesture), subscribes and registers this device.
export async function subscribe(label) {
  if (!supported()) throw new Error('Este navegador no admite notificaciones push.');
  const perm = await Notification.requestPermission();
  if (perm !== 'granted') throw new Error('Permiso de notificaciones denegado. Actívalo en los ajustes del navegador.');
  const { publicKey } = await api.get('/api/push/key');
  if (!publicKey) throw new Error('El servidor no tiene las notificaciones configuradas.');
  const { reg, sub: existing } = await currentSub();
  const sub = existing || (await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: b64urlToBytes(publicKey) }));
  return api.post('/api/push/subscribe', {
    subscription: sub.toJSON(),
    label: (label || '').trim() || 'Este dispositivo',
  });
}

// Removes this browser's subscription and, when an id is given, the server record.
export async function unsubscribe(deviceId) {
  let sub = null;
  try { sub = (await currentSub()).sub; } catch (e) { /* ignore */ }
  if (deviceId) {
    try { await api.post('/api/push/unsubscribe', { id: deviceId }); } catch (e) { if (!sub) throw e; }
  }
  if (sub) await sub.unsubscribe();
}

// Sends a test notification to every device, or to one.
export function test(id) {
  return api.post('/api/push/test', id ? { id } : {});
}
