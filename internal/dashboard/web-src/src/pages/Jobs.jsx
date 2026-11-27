// #/jobs - runners alive, the queue, what runlight has learned, and recent results.
import { useLive } from '../lib/live.js';
import { gb, dur, hms } from '../lib/format.js';
import { Card, Table, Badge, RepoJob, Spinner } from '../components/ui.jsx';

export const meta = { title: 'Jobs', icon: 'list-checks' };

const result = (r) => (r === 'succeeded' ? <Badge kind="good">Correcto</Badge> : r === 'failed' ? <Badge kind="crit">Falló</Badge> : <Badge kind="warn" icon="ban">Cancelado</Badge>);

export default function Jobs() {
  const { state } = useLive();
  if (!state) return <Spinner block label="Esperando datos…" />;

  const runners = state.runners.map((r) => [
    r.name,
    r.state === 'busy' ? <Badge kind="rl" icon="play">En job</Badge> : <Badge kind="info" icon="circle">Libre</Badge>,
    r.job ? <RepoJob repo={r.repo} job={r.job} /> : '—',
    dur((state.now - r.startedMs) / 1000),
    (r.cpuPct / 100).toFixed(2) + ' núcleos',
    r.memMb ? gb(r.memMb) + ' (pico ' + gb(r.peakMb) + ')' : '—',
    r.why,
  ]);

  return (
    <div className="stack">
      <Card title="Runners" sub="Efímeros: nacen para un job y mueren al terminar. Su CPU y memoria salen de su propio cgroup.">
        <Table stack cols={[{ h: 'Runner' }, { h: 'Estado' }, { h: 'Job' }, { h: 'Vive hace', r: true }, { h: 'CPU', r: true }, { h: 'Memoria', r: true }, { h: 'Por qué nació' }]}
          rows={runners} empty="Sin runners. Es lo normal en reposo: runlight escala a cero y arranca solo cuando hay trabajo." />
      </Card>
      <section className="grid2">
        <Card title="Cola" sub="Jobs esperando runner. GitHub los retiene hasta que runlight sube el límite.">
          <Table stack cols={[{ h: 'Job' }, { h: 'Espera', r: true }, { h: 'Etiquetas' }]}
            rows={state.queue.slice(0, 15).map((q) => [<RepoJob repo={q.repo} job={q.job} />, dur((state.now - q.queuedMs) / 1000), (q.labels || []).join(', ')])} empty="Cola vacía." />
        </Card>
        <Card title="Lo que ha aprendido" sub="Memoria pico real de cada job (media móvil). Con esto decide cuántos runners caben.">
          <Table stack cols={[{ h: 'Job' }, { h: 'Veces', r: true }, { h: 'Duración', r: true }, { h: 'Memoria pico', r: true }]}
            rows={state.profiles.slice(0, 12).map((p) => [p.key, String(p.runs), dur(p.avgRunSec), gb(p.peakMemMb) + ' (máx ' + gb(p.maxMemMb) + ')'])}
            empty="Aún no hay perfiles: se aprenden tras cada job que termina bien." />
        </Card>
      </section>
      <Card title="Últimos jobs">
        <Table stack cols={[{ h: 'Resultado' }, { h: 'Job' }, { h: 'Duración', r: true }, { h: 'En cola', r: true }, { h: 'Memoria pico', r: true }, { h: 'Terminó' }]}
          rows={state.jobs.slice(0, 12).map((j) => [result(j.result), <RepoJob repo={j.repo} job={j.job} />, dur(j.runSec), dur(j.queueSec), j.peakMemMb ? gb(j.peakMemMb) : '—', hms(j.finishedMs)])}
          empty="Todavía no ha terminado ningún job desde el arranque." />
      </Card>
    </div>
  );
}
