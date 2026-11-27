// Install prompt handling (Chromium) and the iOS hint (Safari has no install event).
let deferred = null;
const listeners = new Set();

export function isStandalone() {
  return (window.matchMedia && window.matchMedia('(display-mode: standalone)').matches) || window.navigator.standalone === true;
}

export function isIOS() {
  const ua = navigator.userAgent || '';
  return /iphone|ipad|ipod/i.test(ua) || (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1);
}

// kind: 'installed' | 'prompt' (can install now) | 'ios' (manual steps) | 'none'
export function getState() {
  if (isStandalone()) return { kind: 'installed', hint: '' };
  if (deferred) return { kind: 'prompt', hint: 'Instalar runlight como aplicación.' };
  if (isIOS()) return { kind: 'ios', hint: 'En Safari: toca Compartir y luego «Añadir a pantalla de inicio».' };
  return { kind: 'none', hint: '' };
}

function notify() {
  listeners.forEach((fn) => { try { fn(getState()); } catch (e) { /* ignore */ } });
}

export function onChange(fn) {
  listeners.add(fn);
  fn(getState());
  return () => listeners.delete(fn);
}

export async function install() {
  if (!deferred) return 'unavailable';
  const p = deferred;
  deferred = null;
  p.prompt();
  const choice = await p.userChoice.catch(() => ({ outcome: 'dismissed' }));
  notify();
  return choice.outcome;
}

if (typeof window !== 'undefined') {
  window.addEventListener('beforeinstallprompt', (e) => { e.preventDefault(); deferred = e; notify(); });
  window.addEventListener('appinstalled', () => { deferred = null; notify(); });
}
