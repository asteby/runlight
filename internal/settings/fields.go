package settings

import (
	"fmt"
	"sort"

	"github.com/dannyjp49/runlight/internal/alerts"
)

const (
	groupScaling  = "Escalado"
	groupPressure = "Presión"
	groupCaches   = "Cachés"
	groupAlerts   = "Alertas"
)

// Defaults carries the effective startup values (from the daemon's flags) that
// become the defaults of the static fields.
type Defaults struct {
	MaxRunners, MinRunners, MemPerRunnerMB, MemReserveMB, DiskMinFreeMB, IdleTTLSec, JanitorIntervalMin int
	PSICPU, PSIMem, PSIIO                                                                               float64
}

// StaticFields is the schema that does not depend on configuration discovered
// at runtime (cache names, alert rules).
func StaticFields(d Defaults) []Field {
	return []Field{
		{Key: "scaling.maxRunners", Label: "Máximo de runners", Help: "Tope de runners que pueden existir a la vez. Nunca se cancela un job en marcha: bajar el tope solo impide arrancar nuevos.",
			Group: groupScaling, Kind: KindInt, Min: 1, Max: 16, Step: 1, Default: d.MaxRunners},
		{Key: "scaling.minRunners", Label: "Runners en caliente", Help: "Runners que se mantienen listos aunque no haya trabajo, para que los jobs arranquen al instante.",
			Group: groupScaling, Kind: KindInt, Min: 0, Max: 8, Step: 1, Default: d.MinRunners},
		{Key: "scaling.memPerRunnerMb", Label: "Memoria estimada por runner", Help: "Memoria que se calcula que consumirá un runner más. Si no cabe junto a la reserva, no se arranca.",
			Group: groupScaling, Unit: "MB", Kind: KindInt, Min: 256, Max: 16384, Step: 128, Default: d.MemPerRunnerMB},
		{Key: "scaling.memReserveMb", Label: "Reserva de memoria intocable", Help: "Memoria disponible que runlight nunca toca, para dejarla a producción.",
			Group: groupScaling, Unit: "MB", Kind: KindInt, Min: 256, Max: 16384, Step: 128, Default: d.MemReserveMB},
		{Key: "scaling.diskMinFreeMb", Label: "Disco libre mínimo", Help: "Por debajo de este espacio libre no se arrancan runners nuevos.",
			Group: groupScaling, Unit: "MB", Kind: KindInt, Min: 1024, Max: 102400, Step: 512, Default: d.DiskMinFreeMB},
		{Key: "scaling.idleTtlSec", Label: "Segundos antes de retirar un runner libre", Help: "Tiempo que un runner sin trabajo espera antes de retirarse.",
			Group: groupScaling, Unit: "s", Kind: KindInt, Min: 10, Max: 3600, Step: 10, Default: d.IdleTTLSec},

		{Key: "pressure.cpuHigh", Label: "CPU de producción desde la que frena", Help: "Porcentaje de tiempo con la CPU de producción bloqueada a partir del cual se frena el escalado.",
			Group: groupPressure, Unit: "%", Kind: KindFloat, Min: 5, Max: 95, Step: 1, Default: d.PSICPU},
		{Key: "pressure.memHigh", Label: "Presión de memoria desde la que frena", Help: "Porcentaje de tiempo con tareas esperando memoria a partir del cual se frena el escalado.",
			Group: groupPressure, Unit: "%", Kind: KindFloat, Min: 1, Max: 50, Step: 0.5, Default: d.PSIMem},
		{Key: "pressure.ioHigh", Label: "Disco (E/S) desde el que frena", Help: "Porcentaje de tiempo con tareas esperando al disco a partir del cual se frena el escalado.",
			Group: groupPressure, Unit: "%", Kind: KindFloat, Min: 5, Max: 95, Step: 1, Default: d.PSIIO},

		{Key: "janitor.intervalMin", Label: "Cada cuántos minutos revisa las cachés", Help: "Frecuencia con la que se comprueban los topes de las cachés. Si el disco está justo, revisa antes.",
			Group: groupCaches, Unit: "min", Kind: KindInt, Min: 1, Max: 240, Step: 1, Default: d.JanitorIntervalMin},
	}
}

// CacheFields builds one cap field per cache ("cache.<name>.capGb"); caps are
// in GB. The order is stable (by name).
func CacheFields(caps map[string]float64) []Field {
	names := make([]string, 0, len(caps))
	for n := range caps {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]Field, 0, len(names))
	for _, n := range names {
		out = append(out, Field{
			Key: "cache." + n + ".capGb", Label: "Tope de " + n,
			Help:  fmt.Sprintf("Tamaño máximo de la caché %s; al pasarlo se borra lo menos usado.", n),
			Group: groupCaches, Unit: "GB", Kind: KindFloat, Min: 0.5, Max: 200, Step: 0.5, Default: caps[n],
		})
	}
	return out
}

// AlertFields builds "alert.<rule>.enabled" and, for rules that have a
// tunable threshold, "alert.<rule>.threshold" from the alert engine's catalog.
func AlertFields(rules []alerts.RuleInfo) []Field {
	out := make([]Field, 0, 2*len(rules))
	for _, r := range rules {
		id := string(r.ID)
		out = append(out, Field{
			Key: "alert." + id + ".enabled", Label: "Aviso: " + r.Title, Help: r.Description,
			Group: groupAlerts, Kind: KindBool, Default: r.Config.Enabled,
		})
		if r.Max <= r.Min { // rule without a tunable threshold
			continue
		}
		label := r.ThresholdLabel
		if label == "" {
			label = "Umbral de " + r.Title
		}
		step := r.Step
		if step <= 0 {
			step = 1
		}
		out = append(out, Field{
			Key: "alert." + id + ".threshold", Label: label, Help: r.Description,
			Group: groupAlerts, Unit: r.Unit, Kind: KindFloat, Min: r.Min, Max: r.Max, Step: step, Default: r.Config.Threshold,
		})
	}
	return out
}
