// Command dashdemo serves the dashboard with synthetic data driven through the
// REAL governor, so the checklist, thresholds and wording are genuine. It is a
// development aid for looking at the UI without GitHub or a loaded server.
package main

import (
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"time"

	"github.com/dannyjp49/runlight/internal/dashboard"
	"github.com/dannyjp49/runlight/internal/governor"
	"github.com/dannyjp49/runlight/internal/telemetry"
)

func main() {
	addr := flag.String("listen", "127.0.0.1:9128", "address")
	user := flag.String("user", "", "enable the login screen with this user")
	pass := flag.String("password", "", "password for --user")
	mock := flag.Bool("mock", true, "serve the in-memory alerts/settings/push/actions endpoints in front of the dashboard")
	flag.Parse()

	var opts []dashboard.Option
	if *user != "" {
		h, err := dashboard.HashPassword(*pass)
		if err != nil {
			log.Fatal(err)
		}
		a, err := dashboard.NewAuth(*user, h, nil)
		if err != nil {
			log.Fatal(err)
		}
		opts = append(opts, dashboard.WithAuth(a))
	}

	hub := telemetry.New(telemetry.Config{
		Version: "demo", Backend: "systemd", Host: "vps-496e5e6f", ScaleSet: "runlight",
		URL: "https://github.com/asteby-hq", Cores: 8, MemTotalMB: 23463, Min: 0, Max: 3,
		PSIScope: "system.slice", IdleTTLSec: 30, MaxRunMin: 90,
	})
	gov := governor.New(governor.Params{Max: 3, CPUHigh: 25, MemHigh: 5, IOHigh: 30, MemPerRunnerMB: 3072})

	start := time.Now().Add(-20 * time.Minute)
	runners := 0
	busy := 0
	lastLimit := -1
	step := func(now time.Time) {
		sec := now.Sub(start).Seconds()
		// A production that is calm, with two bursts of pressure.
		prodCPU := 6 + 3*math.Sin(sec/40)
		if (sec > 420 && sec < 520) || (sec > 900 && sec < 960) {
			prodCPU = 31 + 4*math.Sin(sec/7)
		}
		queued := int(8 + 6*math.Sin(sec/180))
		if sec > 1000 {
			queued = int(math.Max(0, 6-(sec-1000)/60))
		}
		sig := governor.Signals{
			HasPSI: true, CPUSome: prodCPU, MemSome: 0.2, IOSome: 0.4,
			HostCPUSome: math.Min(95, prodCPU+45+8*math.Sin(sec/25)),
			MemTotalMB:  23463, MemAvailMB: uint64(11500 + 1500*math.Sin(sec/90)), SwapTotalMB: 6144, SwapUsedMB: 6144,
			DiskFreeMB: 55000, DiskTotalMB: 197000, LoadPerCore: 0.9,
		}
		limit := gov.Update(now, sig, runners)
		runners = min(limit, queued)
		busy = max(0, runners-int(sec/50)%2)
		dec := gov.Decision()
		hub.SetDecision(dec)
		if limit != lastLimit && lastLimit >= 0 {
			hub.Eventf("capacity", "Capacidad %d → %d: %s.", lastLimit, limit, string(dec.Reason))
		}
		lastLimit = limit
		cpuRL := float64(busy) * 22
		hub.AddPoint(telemetry.Point{
			T: now.UnixMilli(), ProdCPU: sig.CPUSome, ProdMem: sig.MemSome, ProdIO: sig.IOSome, HostCPU: sig.HostCPUSome,
			Limit: limit, Runners: runners, Busy: busy, Queued: queued,
			CPUProd: 18 + 6*math.Sin(sec/30), CPURunlight: cpuRL,
			MemTotalMB: 23463, MemProdMB: 9800, MemRunlightMB: uint64(busy) * 2100,
			MemAvailMB: sig.MemAvailMB, SwapUsedMB: 6144, SwapTotalMB: 6144,
			DiskFreeMB: 55000, DiskTotalMB: 197000, Load1: 5.2,
		})
	}
	for t := start; t.Before(time.Now()); t = t.Add(2 * time.Second) {
		step(t)
	}

	hub.Event("info", "Runlight arrancó y reanudó el scale set «runlight» (id 2).")
	hub.Event("scale", "Arranca runlight-5d413d86: 3 job(s) asignado(s), 0 runner(s) vivos, límite 2.")
	hub.Event("capacity", "Capacidad 2 → 1: la presión de CPU en producción es 31% y el límite es 25%, así que se frena.")
	hub.Event("keep", "Conserva runlight-ba523b6e: GitHub confirma que tiene un job en curso.")
	hub.Event("job", "✓ asteby-hq/addons · migraciones ya publicadas no se editan terminó (succeeded) en 17s, pico de memoria 612 MB")
	hub.Event("reap", "Retira runlight-9ded67dd: lleva 30s sin job y GitHub confirma que está libre.")
	hub.Event("scale", "Hay 9 job(s) esperando pero el límite es 3: el resto queda en cola de GitHub hasta que haya margen.")

	hub.SetCaches([]telemetry.CacheStat{
		{Name: "go-build", Dir: "/home/ubuntu/.cache/ci-go/shared/build", Mode: "lru", SizeMB: 6900, CapMB: 8192, FreedMB: 2100, LastPruneMs: time.Now().Add(-4 * time.Minute).UnixMilli()},
		{Name: "go-mod", Dir: "/home/ubuntu/.cache/ci-go/shared/mod", Mode: "idle", SizeMB: 3400, CapMB: 6144, LastPruneMs: time.Now().Add(-4 * time.Minute).UnixMilli()},
		{Name: "pnpm", Dir: "/home/ubuntu/.cache/ci-pnpm", Mode: "lru", SizeMB: 6500, CapMB: 6144, LastPruneMs: time.Now().Add(-4 * time.Minute).UnixMilli()},
		{Name: "go-build (runner)", Dir: "/home/ubuntu/.cache/ci-go/runlight-N", Mode: "lru", SizeMB: 5600, CapMB: 6144, FreedMB: 3600, LastPruneMs: time.Now().Add(-4 * time.Minute).UnixMilli()},
	})
	hub.JanitorRan(23800, false)
	hub.Event("prune", "Limpieza de /home/ubuntu/.cache/ci-go: eliminó 4 carpeta(s) de runners que ya no existen y liberó 14.2 GB.")
	hub.Event("prune", "Poda go-build: liberó 2.1 GB (de 9.0 GB a 6.9 GB, tope 8.0 GB).")
	hub.RunnerStarted("runlight-ba523b6e", "6 job(s) asignado(s), 1 runner(s) vivos, límite 3")
	hub.JobStarted(1, "runlight-ba523b6e", "asteby-hq/ops", "ops appliance (linux-amd64)")
	hub.SetUsage("runlight-ba523b6e", 2100, 3300, 163)
	hub.RunnerStarted("runlight-70bbdbaf", "6 job(s) asignado(s), 2 runner(s) vivos, límite 3")
	hub.JobStarted(2, "runlight-70bbdbaf", "asteby-hq/addons", "lint + build + test (TS + Go)")
	hub.SetUsage("runlight-70bbdbaf", 1400, 1900, 88)
	hub.RunnerStarted("runlight-75b5edaf", "6 job(s) asignado(s), 3 runner(s) vivos, límite 3")
	for i, j := range [][2]string{{"asteby-hq/addons", "contrato Storefront API v1"}, {"asteby-hq/ops", "backend (go test + vet)"}, {"asteby-hq/ops", "migraciones (postgres 18)"}} {
		hub.JobQueued(int64(10+i), j[0], j[1], []string{"self-hosted", "ci"}, time.Now().Add(-time.Duration(40+i*70)*time.Second))
	}
	for i, j := range []struct {
		repo, job, res string
		run            time.Duration
		mem            uint64
	}{
		{"asteby-hq/addons", "migraciones ya publicadas no se editan", "succeeded", 17 * time.Second, 612},
		{"asteby-hq/ops", "changed surfaces", "succeeded", 21 * time.Second, 480},
		{"asteby-hq/addons", "lint + build + test (TS + Go)", "succeeded", 3*time.Minute + 12*time.Second, 2100},
		{"asteby-hq/addons", "lint + build + test (TS + Go)", "succeeded", 2*time.Minute + 50*time.Second, 1900},
		{"asteby-hq/ops", "ops appliance (linux-amd64)", "failed", 7 * time.Minute, 3300},
		{"asteby-hq/addons", "changelog/client.json es+en", "succeeded", 29 * time.Second, 540},
	} {
		hub.JobDone(int64(100+i), "runlight-x", j.repo, j.job, j.res, 12*time.Second, j.run, j.mem)
	}

	go func() {
		for range time.Tick(2 * time.Second) {
			step(time.Now())
		}
	}()
	fmt.Println("dashboard demo on http://" + *addr)
	var handler http.Handler = dashboard.Handler(hub, opts...)
	if *mock {
		handler = withMocks(handler)
	}
	log.Fatal(http.ListenAndServe(*addr, handler))
}
