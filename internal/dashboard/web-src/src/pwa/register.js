// Registers the service worker. Failure is never fatal: the dashboard works without it.
let pending = null;

export function register() {
  if (pending) return pending;
  if (!('serviceWorker' in navigator)) return Promise.resolve(null);
  pending = navigator.serviceWorker
    .register('/sw.js', { scope: '/' })
    .catch((err) => {
      console.warn('runlight: service worker no registrado', err);
      return null;
    });
  return pending;
}

export default register;
