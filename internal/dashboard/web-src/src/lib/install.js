// "Install the app" state for the header button. Chrome/Edge/Android fire beforeinstallprompt;
// iOS has no such event, so we only offer a hint (Share -> Add to Home Screen).
import { useSyncExternalStore } from 'react';

let deferred = null;
let version = 0;
const subs = new Set();
const notify = () => { version++; subs.forEach((f) => f()); };

window.addEventListener('beforeinstallprompt', (e) => { e.preventDefault(); deferred = e; notify(); });
window.addEventListener('appinstalled', () => { deferred = null; notify(); });

const standalone = () => {
  try { return matchMedia('(display-mode: standalone)').matches || navigator.standalone === true; } catch { return false; }
};
const ios = () => /iphone|ipad|ipod/i.test(navigator.userAgent) || (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1);

async function install() {
  if (!deferred) return;
  const ev = deferred;
  deferred = null;
  notify();
  try { await ev.prompt(); await ev.userChoice; } catch { /* user dismissed */ }
}

const subscribe = (fn) => { subs.add(fn); return () => subs.delete(fn); };

export function useInstall() {
  useSyncExternalStore(subscribe, () => version);
  const done = standalone();
  return { available: !!deferred && !done, ios: ios() && !done && !deferred, install };
}
