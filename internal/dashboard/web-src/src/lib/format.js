// Number and time formatting shared by every page. Output is Spanish.
export const gb = (mb) => (mb >= 1024 ? (mb / 1024).toFixed(1) + ' GB' : Math.round(mb) + ' MB');

export const dur = (s) => {
  s = Math.max(0, Math.round(s));
  if (s < 60) return s + ' s';
  if (s < 3600) return Math.floor(s / 60) + ' min ' + String(s % 60).padStart(2, '0') + ' s';
  return Math.floor(s / 3600) + ' h ' + String(Math.floor((s % 3600) / 60)).padStart(2, '0') + ' min';
};

export const hms = (ms) => new Date(ms).toLocaleTimeString('es', { hour12: false });

export const pct = (v, d = 0) => v.toFixed(d) + '%';

// "hace 5 min" for a unix-ms timestamp (or ISO string). `now` is injectable for tests.
export const ago = (t, now = Date.now()) => {
  const ms = typeof t === 'number' ? t : Date.parse(t);
  if (!Number.isFinite(ms)) return '—';
  const s = Math.max(0, Math.round((now - ms) / 1000));
  if (s < 5) return 'ahora mismo';
  if (s < 60) return 'hace ' + s + ' s';
  if (s < 3600) return 'hace ' + Math.floor(s / 60) + ' min';
  if (s < 86400) return 'hace ' + Math.floor(s / 3600) + ' h';
  return 'hace ' + Math.floor(s / 86400) + (s < 172800 ? ' día' : ' días');
};

// Round an axis maximum up to 1, 2, 2.5, 5 or 10 times a power of ten.
export const niceMax = (v) => {
  if (v <= 0) return 1;
  const p = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 2, 2.5, 5, 10]) if (v <= m * p) return m * p;
  return 10 * p;
};

// plural(3, 'runner', 'runners') -> "3 runners"; plural(1, 'runner', 'runners') -> "1 runner".
export const plural = (n, one, many) => n + ' ' + (n === 1 ? one : many);
