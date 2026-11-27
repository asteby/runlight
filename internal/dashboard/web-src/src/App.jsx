// App shell: header, navigation (sidebar on desktop, bottom tabs on mobile) and the hash router.
// Alerts and Config belong to another agent: they are imported with the `optional:` prefix (see build.mjs),
// so the build works whether or not those files exist yet, and a missing page shows a placeholder.
import { Component, useEffect, useRef, useState } from 'react';
import { api } from './lib/api.js';
import { useLive } from './lib/live.js';
import { useInstall } from './lib/install.js';
import { gb, dur } from './lib/format.js';
import { Icon } from './components/Icon.jsx';
import { IconButton, Button, EmptyState, ToastHost, toast } from './components/ui.jsx';
import Overview, { meta as overviewMeta } from './pages/Overview.jsx';
import Jobs, { meta as jobsMeta } from './pages/Jobs.jsx';
import Server, { meta as serverMeta } from './pages/Server.jsx';
import * as alertsMod from 'optional:./pages/Alerts.jsx';
import * as configMod from 'optional:./pages/Config.jsx';

const fromModule = (path, mod, fallback) => ({
  path,
  Page: typeof mod.default === 'function' ? mod.default : null,
  ...fallback,
  ...(mod.meta || {}),
});

const ROUTES = [
  { path: '/', Page: Overview, ...overviewMeta },
  { path: '/jobs', Page: Jobs, ...jobsMeta },
  { path: '/servidor', Page: Server, ...serverMeta },
  fromModule('/alertas', alertsMod, { title: 'Alertas', icon: 'bell' }),
  fromModule('/config', configMod, { title: 'Configuración', icon: 'settings' }),
];

const parseHash = () => {
  const p = (location.hash.replace(/^#/, '').split('?')[0] || '/').replace(/\/+$/, '') || '/';
  return p.startsWith('/') ? p : '/' + p;
};

function useRoute() {
  const [path, setPath] = useState(parseHash);
  useEffect(() => {
    const f = () => setPath(parseHash());
    addEventListener('hashchange', f);
    return () => removeEventListener('hashchange', f);
  }, []);
  return ROUTES.find((r) => r.path === path) || ROUTES[0];
}

// ---- theme ----
const isDark = () => {
  const t = document.documentElement.dataset.theme;
  return t ? t === 'dark' : matchMedia('(prefers-color-scheme: dark)').matches;
};
function toggleTheme() {
  const next = isDark() ? 'light' : 'dark';
  document.documentElement.dataset.theme = next;
  try { localStorage.setItem('rl-theme', next); } catch { /* storage may be blocked */ }
}

// ---- pages ----
class Boundary extends Component {
  state = { err: null };
  static getDerivedStateFromError(err) { return { err }; }
  componentDidCatch(err) { console.error('page crashed', err); }
  render() {
    return this.state.err
      ? <EmptyState icon="octagon-alert" title="Esta sección ha fallado">Recarga la página. Si sigue pasando, mira la consola del navegador.</EmptyState>
      : this.props.children;
  }
}

let navigated = false; // the first page view keeps focus where it is
function PageView({ route }) {
  const main = useRef(null);
  useEffect(() => { document.title = route.title + ' · runlight'; }, [route.title]);
  useEffect(() => {
    scrollTo(0, 0);
    if (!navigated) { navigated = true; return; }
    if (main.current) main.current.focus({ preventScroll: true });
  }, [route.path]);
  const Cmp = route.Page;
  return (
    <main id="main" tabIndex={-1} ref={main}>
      <h1 className="pagetitle"><Icon name={route.icon} size={22} />{route.title}</h1>
      {Cmp
        ? <Boundary key={route.path}><Cmp /></Boundary>
        : <EmptyState icon="rocket" title="Esta sección llega pronto">Todavía no está disponible en esta versión.</EmptyState>}
    </main>
  );
}

// ---- header ----
function StatusPill({ state, online }) {
  let kind = 'good';
  let icon = 'circle-check';
  let text = 'Estable';
  if (!online) { kind = 'crit'; icon = 'wifi-off'; text = 'Sin conexión'; }
  else if (state && state.paused) { kind = 'warn'; icon = 'pause'; text = 'En pausa'; }
  else if (state && state.decision && ['pressure', 'low-memory', 'low-disk'].includes(state.decision.reason)) { kind = 'warn'; icon = 'shield-alert'; text = 'Protegiendo producción'; }
  else if (!state) { kind = 'muted'; icon = 'loader'; text = 'Conectando'; }
  return <span className={'pill ' + kind} role="status"><Icon name={icon} size={16} /><span className="txt">{text}</span></span>;
}

function useMe() {
  const [me, setMe] = useState(null);
  useEffect(() => { api.get('/api/me').then(setMe).catch(() => {}); }, []);
  return me;
}

export default function App() {
  const { state, online } = useLive();
  const route = useRoute();
  const me = useMe();
  const install = useInstall();
  const [, redraw] = useState(0);
  const [collapsed, setCollapsed] = useState(() => { try { return localStorage.getItem('rl_side') === '1'; } catch { return false; } });
  const toggleSide = () => setCollapsed((v) => { const n = !v; try { localStorage.setItem('rl_side', n ? '1' : '0'); } catch { /* optional */ } return n; });
  const alerts = (state && state.alerts) || { active: 0, critical: 0 };
  const c = state && state.config;
  const dark = isDark();

  const logout = async () => {
    try { await api.post('/api/logout', {}); } catch { /* leave anyway */ }
    location.replace('/login');
  };
  const onInstall = () => {
    if (install.available) install.install();
    else toast('En iPhone o iPad: pulsa Compartir y luego «Añadir a pantalla de inicio».', 'info', 9000);
  };

  return (
    <div className="app">
      <header className="top">
        <a className="brand" href="#/" aria-label="runlight, ir al resumen">
          <picture>
            <source media="(prefers-color-scheme: dark)" srcSet="/logo-dark.svg" />
            <img className="mark" src="/logo-light.svg" alt="" width="26" height="26" decoding="async" />
          </picture>
          <span className="word">run<b>light</b></span>
          <span className="subtitle">{c ? `${c.host} · ${c.cores} núcleos · ${gb(c.memTotalMb)} RAM · backend ${c.backend}` : 'cargando…'}</span>
        </a>
        <div className="spacer" />
        <StatusPill state={state} online={online} />
        {(install.available || install.ios) && <Button icon="download" class="installbtn" onClick={onInstall}>Instalar</Button>}
        <a className={'iconbtn' + (alerts.critical > 0 ? ' crit' : '')} href="#/alertas" title="Alertas"
          aria-label={alerts.active ? `Alertas: ${alerts.active} activas` : 'Alertas: ninguna activa'}>
          <Icon name={alerts.active ? 'bell-ring' : 'bell'} size={20} />
          {alerts.active > 0 && <span className="count" aria-hidden="true">{alerts.active}</span>}
        </a>
        <IconButton icon={dark ? 'sun' : 'moon'} label={dark ? 'Cambiar a tema claro' : 'Cambiar a tema oscuro'} onClick={() => { toggleTheme(); redraw((n) => n + 1); }} />
        {me && me.auth && <IconButton icon="log-out" label={me.user ? 'Salir (sesión de ' + me.user + ')' : 'Salir'} onClick={logout} />}
      </header>
      <div className={'shell' + (collapsed ? ' collapsed' : '')}>
        <nav className="side" aria-label="Secciones">
          {ROUTES.map((r) => (
            <a key={r.path} href={'#' + r.path} title={collapsed ? r.title : undefined} aria-current={r.path === route.path ? 'page' : undefined}>
              <Icon name={r.icon} size={20} /><span>{r.title}</span>
              {r.path === '/alertas' && alerts.active > 0 && <span className={'count' + (alerts.critical > 0 ? ' crit' : '')} aria-label={alerts.active + ' activas'}>{alerts.active}</span>}
            </a>
          ))}
          <button type="button" className="sidetoggle" onClick={toggleSide} aria-expanded={!collapsed} title={collapsed ? 'Expandir menú' : 'Colapsar menú'}>
            <Icon name={collapsed ? 'panel-left-open' : 'panel-left-close'} size={20} /><span>{collapsed ? 'Expandir' : 'Colapsar'}</span>
          </button>
        </nav>
        <div className="content">
          <PageView key={route.path} route={route} />
          <footer>{c ? `runlight ${c.version} · scale set “${c.scaleSet}” en ${c.url} · en marcha hace ${dur((state.now - state.startedMs) / 1000)}` : ''}</footer>
        </div>
      </div>
      <ToastHost />
    </div>
  );
}
