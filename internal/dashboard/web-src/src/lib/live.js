// One shared polling loop for the whole app: /api/state every 2 s (paused while the tab is hidden),
// a 20 min history seeded once, and an optional /api/procs poll that only runs while someone uses it.
import { useEffect, useSyncExternalStore } from 'react';
import { api } from './api.js';

const HISTORY_MAX = 1200;
let snap = { state: null, history: [], online: false, error: null };
const subs = new Set();
let started = false;
let ticking = false;
let resolveReady;
export const ready = new Promise((r) => { resolveReady = r; }); // resolves on the first successful /api/state

const emit = () => subs.forEach((f) => f());
const visible = () => document.visibilityState === 'visible';

function pushPoint(history, p) {
  if (history.length && p.t <= history[history.length - 1].t) return history;
  const next = history.concat(p);
  return next.length > HISTORY_MAX ? next.slice(-HISTORY_MAX) : next;
}

async function tick() {
  if (ticking) return;
  ticking = true;
  try {
    const state = await api.get('/api/state');
    snap = { state, history: state.point ? pushPoint(snap.history, state.point) : snap.history, online: true, error: null };
    resolveReady();
  } catch (e) {
    snap = { ...snap, online: false, error: e.message };
  }
  ticking = false;
  emit();
}

export function start() {
  if (started) return;
  started = true;
  (async () => {
    try {
      const h = await api.get('/api/history?minutes=20');
      snap = { ...snap, history: (h && h.points) || [] };
    } catch { /* the live points will fill it in */ }
    await tick();
  })();
  setInterval(() => { if (visible()) tick(); }, 2000);
  document.addEventListener('visibilitychange', () => { if (visible()) tick(); });
}

const subscribe = (fn) => { subs.add(fn); return () => subs.delete(fn); };

// Outside React (splash screen): fn(snapshot) on every change. Returns the unsubscribe function.
export function watch(fn) {
  return subscribe(() => fn(snap));
}

// -> { state, history, online, error }
export function useLive() {
  useEffect(() => { start(); }, []);
  return useSyncExternalStore(subscribe, () => snap);
}

// ---- processes: shared, polled every 6 s only while at least one component is mounted ----
let procs = null;
const procSubs = new Set();
let procTimer = null;

async function loadProcs() {
  if (!visible()) return;
  try {
    procs = await api.get('/api/procs');
    procSubs.forEach((f) => f());
  } catch { /* keep the last table */ }
}

function subscribeProcs(fn) {
  procSubs.add(fn);
  if (procSubs.size === 1) { loadProcs(); procTimer = setInterval(loadProcs, 6000); }
  return () => {
    procSubs.delete(fn);
    if (!procSubs.size) { clearInterval(procTimer); procTimer = null; }
  };
}

export function useProcs() {
  return useSyncExternalStore(subscribeProcs, () => procs);
}
