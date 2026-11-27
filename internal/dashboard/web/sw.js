// runlight service worker: push notifications + a deliberately small cache.
// Only static assets are cached. /api/* is NEVER cached: a stale dashboard
// state is worse than an honest "offline" page.
const VERSION = 'v2';
const CACHE = 'runlight-static-' + VERSION;
const STATIC_PREFIXES = ['/fonts/', '/icons/'];
const STATIC_EXACT = ['/logo-light.svg', '/logo-dark.svg'];
const OFFLINE_URL = '/offline.html';

self.addEventListener('install', (event) => {
  event.waitUntil(
    caches.open(CACHE)
      .then((c) => c.add(new Request(OFFLINE_URL, { cache: 'reload' })))
      .catch(() => {})
      .then(() => self.skipWaiting())
  );
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys.filter((k) => k.startsWith('runlight-') && k !== CACHE).map((k) => caches.delete(k))))
      .catch(() => {})
      .then(() => self.clients.claim())
  );
});

function isStatic(url) {
  return STATIC_EXACT.includes(url.pathname) || STATIC_PREFIXES.some((p) => url.pathname.startsWith(p));
}

self.addEventListener('fetch', (event) => {
  const req = event.request;
  if (req.method !== 'GET') return;
  const url = new URL(req.url);
  if (url.origin !== self.location.origin) return;
  if (url.pathname.startsWith('/api/') || url.pathname === '/metrics') return;

  if (req.mode === 'navigate') {
    event.respondWith(
      fetch(req).catch(() => caches.match(OFFLINE_URL).then((r) => r || Response.error()))
    );
    return;
  }

  if (isStatic(url)) {
    event.respondWith(
      caches.match(req).then((hit) => {
        if (hit) return hit;
        return fetch(req).then((res) => {
          // Never cache redirects (e.g. to /login) or failures.
          if (res.ok && !res.redirected && res.type === 'basic') {
            const copy = res.clone();
            caches.open(CACHE).then((c) => c.put(req, copy)).catch(() => {});
          }
          return res;
        });
      })
    );
  }
});

function parsePayload(event) {
  const fallback = { title: 'runlight', body: '' };
  if (!event.data) return fallback;
  try {
    const d = event.data.json();
    if (d && typeof d === 'object') return d;
  } catch (e) {
    try {
      const t = event.data.text();
      if (t) return { title: 'runlight', body: String(t).slice(0, 300) };
    } catch (e2) { /* ignore */ }
  }
  return fallback;
}

// Only same-origin paths are honored; anything else opens the dashboard root.
function safeUrl(u) {
  try {
    if (typeof u !== 'string' || !u) return '/';
    const parsed = new URL(u, self.location.origin);
    if (parsed.origin !== self.location.origin) return '/';
    return parsed.pathname + parsed.search + parsed.hash;
  } catch (e) {
    return '/';
  }
}

self.addEventListener('push', (event) => {
  const d = parsePayload(event);
  const title = typeof d.title === 'string' && d.title ? d.title.slice(0, 120) : 'runlight';
  const tag = typeof d.tag === 'string' && d.tag ? d.tag : undefined;
  event.waitUntil(
    self.registration.showNotification(title, {
      body: typeof d.body === 'string' ? d.body.slice(0, 500) : '',
      tag,
      renotify: !!tag && !!d.renotify,
      icon: '/icons/icon-192.png',
      badge: '/icons/badge-96.png',
      requireInteraction: d.level === 'critical',
      data: { url: safeUrl(d.url) },
    })
  );
});

self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  const target = safeUrl(event.notification.data && event.notification.data.url);
  event.waitUntil(
    self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then((list) => {
      for (const c of list) {
        if (new URL(c.url).origin === self.location.origin && 'focus' in c) {
          return c.focus().then((w) => (w && 'navigate' in w ? w.navigate(target) : w)).catch(() => {});
        }
      }
      return self.clients.openWindow ? self.clients.openWindow(target) : undefined;
    })
  );
});

// The browser rotated the push subscription: re-subscribe and tell the server (best effort).
self.addEventListener('pushsubscriptionchange', (event) => {
  event.waitUntil((async () => {
    try {
      const old = event.oldSubscription;
      const opts = (old && old.options) || (event.newSubscription && event.newSubscription.options);
      let sub = event.newSubscription;
      if (!sub) {
        if (!opts || !opts.applicationServerKey) return;
        sub = await self.registration.pushManager.subscribe({
          userVisibleOnly: true,
          applicationServerKey: opts.applicationServerKey,
        });
      }
      await fetch('/api/push/subscribe', {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json', 'X-Runlight': '1' },
        body: JSON.stringify({ subscription: sub.toJSON(), label: 'Dispositivo' }),
      });
    } catch (e) { /* best effort */ }
  })());
});
