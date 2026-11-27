// Entry point: theme, mount <App/>, hide the splash after the first data (never before ~1.1 s), register the PWA.
import { createRoot } from 'react-dom/client';
import { ready, start, watch } from './lib/live.js';
import App from './App.jsx';
import * as pwa from 'optional:./pwa/register.js'; // owned by the PWA module; the app works without it

const bootAt = performance.now();
const root = document.documentElement;
try {
  const t = localStorage.getItem('rl-theme');
  if (t === 'light' || t === 'dark') root.dataset.theme = t;
} catch { /* storage may be blocked */ }
const q = new URLSearchParams(location.search).get('theme');
if (q === 'light' || q === 'dark') root.dataset.theme = q;

createRoot(document.getElementById('app')).render(<App />);
start();

const splash = document.getElementById('splash');
const msg = document.getElementById('splashmsg');
const unwatch = watch((s) => { if (!s.online && msg) msg.textContent = 'No se pudo conectar. Reintentando…'; });
ready.then(() => {
  unwatch();
  if (!splash) return;
  setTimeout(() => { splash.classList.add('hide'); setTimeout(() => splash.remove(), 600); }, Math.max(0, 1100 - (performance.now() - bootAt)));
});

const registerPwa = typeof pwa.default === 'function' ? pwa.default : pwa.register;
if (typeof registerPwa === 'function') registerPwa();
