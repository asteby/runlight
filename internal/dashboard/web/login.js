'use strict';
/* Login screen. Talks to POST /api/login; the session lives in an HttpOnly
   cookie the page can never read. Error text is inserted with textContent. */

const $ = (id) => document.getElementById(id);
const form = $('form'), user = $('user'), pw = $('pw'), go = $('go');

// Same theme as the dashboard: the saved choice, or ?theme=light|dark.
(function theme() {
  try { const t = localStorage.getItem('rl-theme'); if (t) document.documentElement.dataset.theme = t; } catch (e) { /* storage may be blocked */ }
  const q = new URLSearchParams(location.search).get('theme');
  if (q === 'light' || q === 'dark') document.documentElement.dataset.theme = q;
})();

function showError(msg) {
  $('errtxt').textContent = msg;
  $('err').hidden = false;
}
function clearError() { $('err').hidden = true; }

$('toggle').addEventListener('click', () => {
  const show = pw.type === 'password';
  pw.type = show ? 'text' : 'password';
  $('toggle').textContent = show ? 'Ocultar' : 'Mostrar';
  $('toggle').setAttribute('aria-pressed', String(show));
  $('toggle').setAttribute('aria-label', show ? 'Ocultar la contraseña' : 'Mostrar la contraseña');
  pw.focus();
});

[user, pw].forEach((i) => i.addEventListener('input', clearError));

form.addEventListener('submit', async (ev) => {
  ev.preventDefault();
  if (!user.value.trim() || !pw.value) {
    showError('Escribe tu usuario y tu contraseña.');
    (user.value.trim() ? pw : user).focus();
    return;
  }
  go.disabled = true;
  go.textContent = 'Entrando…';
  try {
    const r = await fetch('/api/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ user: user.value.trim(), password: pw.value }),
      cache: 'no-store',
    });
    if (r.status === 204) { location.replace('/'); return; }
    let msg = 'No se pudo iniciar sesión.';
    try { msg = (await r.json()).error || msg; } catch (e) { /* non-JSON reply */ }
    showError(msg);
    if (r.status === 401) { pw.value = ''; pw.focus(); }
  } catch (e) {
    showError('No se pudo conectar con el servidor. Revisa tu conexión e inténtalo otra vez.');
  }
  go.disabled = false;
  go.textContent = 'Entrar';
});
