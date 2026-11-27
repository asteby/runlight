package settings

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/dannyjp49/runlight/internal/alerts"
)

func testDefaults() Defaults {
	return Defaults{MaxRunners: 4, MinRunners: 0, MemPerRunnerMB: 1024, MemReserveMB: 256, DiskMinFreeMB: 2048,
		IdleTTLSec: 30, JanitorIntervalMin: 10, PSICPU: 60, PSIMem: 8, PSIIO: 50}
}

func schema() []Field {
	f := StaticFields(testDefaults())
	f = append(f, CacheFields(map[string]float64{"go-mod": 4, "go-build": 8})...)
	return append(f, AlertFields(alerts.RuleCatalog())...)
}

func crossRules(v map[string]any) FieldErrors {
	errs := FieldErrors{}
	if v["scaling.minRunners"].(int) > v["scaling.maxRunners"].(int) {
		errs["scaling.minRunners"] = "No puede ser mayor que el máximo de runners."
	}
	return errs
}

func open(t *testing.T) (*Manager, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "state", "settings.json")
	m, err := Open(p, schema(), WithValidator(crossRules))
	if err != nil {
		t.Fatal(err)
	}
	return m, p
}

func TestDefaultsAreTypedAndEffective(t *testing.T) {
	m, _ := open(t)
	v := m.Values()
	if v["scaling.maxRunners"] != 4 || v["pressure.cpuHigh"] != 60.0 || v["cache.go-build.capGb"] != 8.0 {
		t.Fatalf("values = %v", v)
	}
	if b, ok := v["alert.disk_low.enabled"].(bool); !ok || !b {
		t.Fatalf("alert enabled default = %v", v["alert.disk_low.enabled"])
	}
	if _, ok := v["alert.daemon_restart.threshold"]; ok {
		t.Fatal("a rule without a threshold must not get a threshold field")
	}
}

func TestCacheFieldsStableOrder(t *testing.T) {
	f := CacheFields(map[string]float64{"zeta": 1, "alfa": 2, "mid": 3})
	if len(f) != 3 || f[0].Key != "cache.alfa.capGb" || f[1].Key != "cache.mid.capGb" || f[2].Key != "cache.zeta.capGb" {
		t.Fatalf("order = %v", f)
	}
}

func TestUpdateAppliesPersistsAndReloads(t *testing.T) {
	m, p := open(t)
	res, err := m.Update("danny", map[string]any{"scaling.maxRunners": float64(6), "pressure.cpuHigh": 70.5, "scaling.minRunners": 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Applied) != 2 || len(res.Unchanged) != 1 || res.Unchanged[0] != "scaling.minRunners" {
		t.Fatalf("result = %+v", res)
	}
	if res.Values["scaling.maxRunners"] != 6 {
		t.Fatalf("int must be typed int: %#v", res.Values["scaling.maxRunners"])
	}
	m2, err := Open(p, schema(), WithValidator(crossRules))
	if err != nil {
		t.Fatal(err)
	}
	if v := m2.Values(); v["scaling.maxRunners"] != 6 || v["pressure.cpuHigh"] != 70.5 {
		t.Fatalf("reloaded = %v", v)
	}
	if a := m2.Audit(); len(a) != 1 || a[0].Actor != "danny" || len(a[0].Changes) != 2 {
		t.Fatalf("audit = %+v", a)
	}
}

func TestOverridesFileHoldsOnlyTheDifference(t *testing.T) {
	m, p := open(t)
	if _, err := m.Update("a", map[string]any{"scaling.maxRunners": 6}); err != nil {
		t.Fatal(err)
	}
	var f overridesFile
	raw, _ := os.ReadFile(p)
	if err := json.Unmarshal(raw, &f); err != nil || len(f.Overrides) != 1 {
		t.Fatalf("file = %s", raw)
	}
	// Going back to the default removes the override.
	if _, err := m.Update("a", map[string]any{"scaling.maxRunners": 4}); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(p)
	f = overridesFile{}
	_ = json.Unmarshal(raw, &f)
	if len(f.Overrides) != 0 {
		t.Fatalf("override should be gone: %s", raw)
	}
}

func TestInvalidChangeAppliesNothing(t *testing.T) {
	m, p := open(t)
	_, err := m.Update("a", map[string]any{"scaling.maxRunners": 8, "pressure.cpuHigh": 500.0})
	fe, ok := err.(FieldErrors)
	if !ok || fe["pressure.cpuHigh"] == "" || len(fe) != 1 {
		t.Fatalf("err = %v", err)
	}
	if m.Values()["scaling.maxRunners"] != 4 {
		t.Fatal("a valid field must not be applied when another is invalid")
	}
	if _, e := os.Stat(p); e == nil {
		t.Fatal("nothing should have been written")
	}
	if len(m.Audit()) != 0 {
		t.Fatal("rejected updates are not audited")
	}
}

func TestFieldValidationMessages(t *testing.T) {
	m, _ := open(t)
	cases := map[string]any{
		"nope.key":               1,
		"scaling.maxRunners":     2.5,
		"pressure.cpuHigh":       math.NaN(),
		"pressure.memHigh":       math.Inf(1),
		"scaling.idleTtlSec":     "30",
		"alert.disk_low.enabled": 1,
		"scaling.memReserveMb":   100,
		"cache.go-mod.capGb":     0.1,
		"scaling.minRunners":     nil,
	}
	for k, v := range cases {
		_, err := m.Update("a", map[string]any{k: v})
		fe, ok := err.(FieldErrors)
		if !ok || fe[k] == "" {
			t.Errorf("%s=%v: err = %v", k, v, err)
			continue
		}
		if !strings.ContainsAny(fe[k], "áéíóúñ") && !strings.Contains(fe[k], "Debe") && !strings.Contains(fe[k], "desconocido") {
			t.Errorf("%s: message not in Spanish: %q", k, fe[k])
		}
	}
	if len(m.Audit()) != 0 {
		t.Fatal("nothing should have been applied")
	}
}

func TestCrossValidatorBlocksAndMergesWithCurrent(t *testing.T) {
	m, _ := open(t)
	if _, err := m.Update("a", map[string]any{"scaling.minRunners": 5}); err == nil {
		t.Fatal("min 5 > max 4 must be rejected")
	}
	if _, err := m.Update("a", map[string]any{"scaling.minRunners": 5, "scaling.maxRunners": 8}); err != nil {
		t.Fatalf("both together are fine: %v", err)
	}
	if _, err := m.Update("a", map[string]any{"scaling.maxRunners": 3}); err == nil {
		t.Fatal("lowering max below the stored min must be rejected")
	}
}

func TestOnChangeRunsAfterPersistAndOutsideLock(t *testing.T) {
	m, p := open(t)
	var got []string
	var seen any
	m.OnChange(func(changed []string, values map[string]any) {
		got = changed
		seen = m.Values()["scaling.maxRunners"] // would deadlock if the lock were held
		if _, err := os.Stat(p); err != nil {
			t.Errorf("must be persisted before OnChange: %v", err)
		}
		_ = values
	})
	m.OnChange(func([]string, map[string]any) { panic("boom") })
	n := 0
	m.OnChange(func([]string, map[string]any) { n++ })
	if _, err := m.Update("a", map[string]any{"scaling.maxRunners": 5}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "scaling.maxRunners" || seen != 5 || n != 1 {
		t.Fatalf("got=%v seen=%v n=%d", got, seen, n)
	}
	// No-op and rejected updates do not fire it.
	_, _ = m.Update("a", map[string]any{"scaling.maxRunners": 5})
	_, _ = m.Update("a", map[string]any{"scaling.maxRunners": 99})
	if n != 1 {
		t.Fatalf("callbacks fired %d times", n)
	}
}

func TestAuditNewestFirstAndCapped(t *testing.T) {
	m, _ := open(t)
	for i := 0; i < 230; i++ {
		v := 1 + i%2 // alternate 1,2 so every update changes something
		if _, err := m.Update("a"+string(rune('A'+i%3)), map[string]any{"scaling.maxRunners": v + 1}); err != nil {
			t.Fatal(err)
		}
	}
	a := m.Audit()
	if len(a) != 200 {
		t.Fatalf("audit len = %d", len(a))
	}
	if a[0].At.Before(a[1].At) {
		t.Fatal("newest must come first")
	}
	a[0].Changes[0].To = "tampered"
	if m.Audit()[0].Changes[0].To == "tampered" {
		t.Fatal("Audit must return a copy")
	}
}

func TestCorruptFilesAreSetAsideAndUnknownKeysDropped(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p+".audit.json", []byte("[[["), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := Open(p, schema())
	if err != nil {
		t.Fatal(err)
	}
	if m.Values()["scaling.maxRunners"] != 4 || len(m.Audit()) != 0 {
		t.Fatal("must start clean")
	}
	for _, f := range []string{p + ".corrupt-1", p + ".audit.json.corrupt-1"} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("missing quarantined file: %v", err)
		}
	}

	good := `{"version":1,"overrides":{"scaling.maxRunners":7,"gone.key":1,"pressure.cpuHigh":9999,"scaling.minRunners":"x"}}`
	if err := os.WriteFile(p, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err = Open(p, schema())
	if err != nil {
		t.Fatal(err)
	}
	v := m.Values()
	if v["scaling.maxRunners"] != 7 || v["pressure.cpuHigh"] != 60.0 || v["scaling.minRunners"] != 0 {
		t.Fatalf("values = %v", v)
	}
}

func TestStoredOverridesRejectedByCrossRulesAreDropped(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.json")
	_ = os.WriteFile(p, []byte(`{"version":1,"overrides":{"scaling.minRunners":8}}`), 0o600)
	m, err := Open(p, schema(), WithValidator(crossRules))
	if err != nil {
		t.Fatal(err)
	}
	if m.Values()["scaling.minRunners"] != 0 {
		t.Fatal("inconsistent stored override must be dropped")
	}
}

func TestOpenRejectsBadSchema(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.json")
	if _, err := Open(p, []Field{{Key: "a", Kind: KindInt, Default: 1}, {Key: "a", Kind: KindInt, Default: 1}}); err == nil {
		t.Fatal("duplicate key")
	}
	if _, err := Open(p, []Field{{Key: "a", Kind: KindInt, Default: "x"}}); err == nil {
		t.Fatal("bad default type")
	}
	if _, err := Open("", nil); err == nil {
		t.Fatal("empty path")
	}
}

func TestFilePermissionsAndNoTempLeftovers(t *testing.T) {
	m, p := open(t)
	if _, err := m.Update("a", map[string]any{"scaling.maxRunners": 5}); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		for _, f := range []string{p, p + ".audit.json"} {
			if fi, err := os.Stat(f); err != nil || fi.Mode().Perm() != 0o600 {
				t.Fatalf("%s mode = %v err=%v", f, fi, err)
			}
		}
		if fi, _ := os.Stat(filepath.Dir(p)); fi.Mode().Perm() != 0o700 {
			t.Fatalf("dir mode = %v", fi.Mode().Perm())
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(p))
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestConcurrentUpdatesAndReads(t *testing.T) {
	m, _ := open(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_, _ = m.Update("a", map[string]any{"scaling.maxRunners": 1 + (i+j)%8})
				_ = m.Values()
				_ = m.Audit()
			}
		}()
	}
	wg.Wait()
}

func TestAlertFieldsFollowTheCatalog(t *testing.T) {
	rules := alerts.RuleCatalog()
	fields := AlertFields(rules)
	by := map[string]Field{}
	for _, f := range fields {
		by[f.Key] = f
	}
	for _, r := range rules {
		e, ok := by["alert."+string(r.ID)+".enabled"]
		if !ok || e.Kind != KindBool || e.Default != r.Config.Enabled {
			t.Errorf("%s: enabled field = %+v", r.ID, e)
		}
		if r.Max > r.Min {
			th, ok := by["alert."+string(r.ID)+".threshold"]
			if !ok || th.Min != r.Min || th.Max != r.Max || th.Default != r.Config.Threshold {
				t.Errorf("%s: threshold field = %+v", r.ID, th)
			}
		}
	}
}
