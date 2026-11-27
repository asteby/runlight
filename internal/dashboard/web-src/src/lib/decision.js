// Wording of the governor's decision (state.decision), shared by the KPI row and the decision card.
export const CHECK_LABEL = {
  disk: 'Disco libre', mem_reserve: 'Memoria disponible (reserva intocable)',
  psi_cpu: 'Presión de CPU en producción', psi_mem: 'Presión de memoria en producción', psi_io: 'Presión de disco (E/S)',
  load: 'Carga por núcleo', mem_fit: 'Cabe otro runner en memoria', demand: 'Demanda',
};

// -> [headline, explanation]
export function narrative(d) {
  const first = d.checks.find((c) => !c.ok);
  switch (d.reason) {
    case 'grow': return ['Puede subir la capacidad.', 'Producción está tranquila y hay memoria para otro runner, así que sube de uno en uno.'];
    case 'pressure': return ['Frenando para proteger producción.', first ? `${CHECK_LABEL[first.key]}: ${first.value.toFixed(0)}${first.unit} frente a un límite de ${first.limit.toFixed(0)}${first.unit}. Reduce a la mitad y deja los jobs en la cola de GitHub.` : 'Producción sufre presión.'];
    case 'low-memory': return ['Sin margen de memoria.', 'Queda menos memoria que la reserva intocable. No arranca nada nuevo, pero tampoco mata jobs en curso.'];
    case 'low-disk': return ['Disco casi lleno.', 'No arranca runners nuevos hasta que haya espacio.'];
    case 'no-headroom': return ['Estable: no cabe otro runner.', 'La memoria libre no alcanza para el siguiente runner con la reserva.'];
    default:
      return d.inUse >= d.limit
        ? ['Estable al límite actual.', 'Todos los cupos están en uso y producción está dentro de lo normal; sube si la presión baja.']
        : ['Capacidad suficiente.', 'Hay cupos libres para la demanda actual.'];
  }
}

// Level (ok | watch | bad) -> icon name and meter colour.
export const LEVEL_ICON = { ok: 'circle-check', watch: 'triangle-alert', bad: 'circle-x' };
export const LEVEL_METER = { ok: 'prod', watch: 'warn', bad: 'crit' };
// Level -> status kind (badges, notes).
export const LEVEL_KIND = { ok: 'good', watch: 'warn', bad: 'crit' };
