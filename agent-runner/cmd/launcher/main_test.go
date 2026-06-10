package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestApplyPendingIfAny_RotatesWorkerAndConsumesMarker(t *testing.T) {
	tmp := t.TempDir()
	worker := filepath.Join(tmp, "agent-runner")
	if err := os.WriteFile(worker, []byte("OLD"), 0o755); err != nil {
		t.Fatalf("write worker: %v", err)
	}
	if err := os.WriteFile(worker+".new", []byte("NEW"), 0o755); err != nil {
		t.Fatalf("write .new: %v", err)
	}
	mk, _ := json.Marshal(map[string]string{"version": "0.7.0", "sha256": "abc", "ts": "2026-06-10T00:00:00Z"})
	if err := os.WriteFile(worker+".update-pending", mk, 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	applyPendingIfAny(worker)

	if b, _ := os.ReadFile(worker); string(b) != "NEW" {
		t.Errorf("worker non sostituito: %q", b)
	}
	if b, _ := os.ReadFile(worker + ".old"); string(b) != "OLD" {
		t.Errorf(".old non e' la versione vecchia: %q", b)
	}
	if _, err := os.Stat(worker + ".update-pending"); !os.IsNotExist(err) {
		t.Errorf("marker non consumato: %v", err)
	}
	if _, err := os.Stat(worker + ".new"); !os.IsNotExist(err) {
		t.Errorf(".new non rimosso (e' il rename del rotate)")
	}
}

func TestApplyPendingIfAny_NoOpWithoutMarker(t *testing.T) {
	tmp := t.TempDir()
	worker := filepath.Join(tmp, "agent-runner")
	_ = os.WriteFile(worker, []byte("OLD"), 0o755)
	applyPendingIfAny(worker) // niente marker -> no-op silenzioso
	if b, _ := os.ReadFile(worker); string(b) != "OLD" {
		t.Errorf("worker modificato senza marker: %q", b)
	}
}

func TestApplyPendingIfAny_RemovesStaleMarkerIfNewMissing(t *testing.T) {
	tmp := t.TempDir()
	worker := filepath.Join(tmp, "agent-runner")
	_ = os.WriteFile(worker, []byte("OLD"), 0o755)
	mk, _ := json.Marshal(map[string]string{"version": "0.7.0", "sha256": "abc", "ts": "x"})
	_ = os.WriteFile(worker+".update-pending", mk, 0o644)
	// .new mancante
	applyPendingIfAny(worker)
	if _, err := os.Stat(worker + ".update-pending"); !os.IsNotExist(err) {
		t.Errorf("marker stale non rimosso")
	}
	if b, _ := os.ReadFile(worker); string(b) != "OLD" {
		t.Errorf("worker toccato: %q", b)
	}
}

func TestRollback_RestoresOldIfPresent(t *testing.T) {
	tmp := t.TempDir()
	worker := filepath.Join(tmp, "agent-runner")
	_ = os.WriteFile(worker, []byte("BROKEN"), 0o755)
	_ = os.WriteFile(worker+".old", []byte("OLD-WORKING"), 0o755)
	if !rollback(worker) {
		t.Fatal("rollback ha restituito false")
	}
	if b, _ := os.ReadFile(worker); string(b) != "OLD-WORKING" {
		t.Errorf("worker dopo rollback=%q", b)
	}
	if b, _ := os.ReadFile(worker + ".broken"); string(b) != "BROKEN" {
		t.Errorf(".broken=%q", b)
	}
	if _, err := os.Stat(worker + ".old"); !os.IsNotExist(err) {
		t.Errorf(".old non consumato")
	}
}

func TestRollback_NoOpIfNoOld(t *testing.T) {
	tmp := t.TempDir()
	worker := filepath.Join(tmp, "agent-runner")
	_ = os.WriteFile(worker, []byte("X"), 0o755)
	if rollback(worker) {
		t.Error("rollback ha restituito true senza .old")
	}
	if b, _ := os.ReadFile(worker); string(b) != "X" {
		t.Errorf("worker toccato: %q", b)
	}
}

func TestSplitArgs_BasicSpaceSeparated(t *testing.T) {
	got := splitArgs("a b   c")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%q want=%q", i, got[i], want[i])
		}
	}
	if len(splitArgs("")) != 0 {
		t.Error("vuoto -> array vuoto")
	}
}

func TestCurrentWorkerVersion_ReadsCacheFile(t *testing.T) {
	tmp := t.TempDir()
	worker := filepath.Join(tmp, "agent-runner")
	if err := os.WriteFile(worker, []byte("anything"), 0o755); err != nil {
		t.Fatalf("write worker: %v", err)
	}
	if err := os.WriteFile(worker+".version", []byte("0.6.0\n"), 0o644); err != nil {
		t.Fatalf("write .version: %v", err)
	}
	if v := currentWorkerVersion(worker); v != "0.6.0" {
		t.Errorf("currentWorkerVersion=%q want=0.6.0", v)
	}
}

func TestCurrentWorkerVersion_FallsBackToExecAndCaches(t *testing.T) {
	tmp := t.TempDir()
	worker := filepath.Join(tmp, "fake-worker.sh")
	// Fake worker che stampa una versione fissa quando invocato con -print-version.
	script := "#!/bin/sh\ncase \"$1\" in -print-version) echo 0.7.0 ;; *) echo other ;; esac\n"
	if err := os.WriteFile(worker, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake worker: %v", err)
	}
	v := currentWorkerVersion(worker)
	if v != "0.7.0" {
		t.Errorf("currentWorkerVersion=%q want=0.7.0", v)
	}
	// La cache deve essere stata scritta.
	data, err := os.ReadFile(worker + ".version")
	if err != nil {
		t.Fatalf("cache .version non scritta: %v", err)
	}
	if string(bytesTrim(data)) != "0.7.0" {
		t.Errorf(".version cache=%q want=0.7.0", data)
	}
}

func TestCurrentWorkerVersion_ExecFailureReturnsEmpty(t *testing.T) {
	tmp := t.TempDir()
	worker := filepath.Join(tmp, "nonexistent")
	if v := currentWorkerVersion(worker); v != "" {
		t.Errorf("currentWorkerVersion(nonexistent)=%q want=empty", v)
	}
	if _, err := os.Stat(worker + ".version"); !os.IsNotExist(err) {
		t.Errorf("cache scritta nonostante exec failure")
	}
}

func TestApplyPendingIfAny_UpdatesVersionCache(t *testing.T) {
	tmp := t.TempDir()
	worker := filepath.Join(tmp, "agent-runner")
	_ = os.WriteFile(worker, []byte("OLD"), 0o755)
	_ = os.WriteFile(worker+".new", []byte("NEW"), 0o755)
	mk := `{"version":"0.6.1","sha256":"deadbeef","ts":"2026-06-10T00:00:00Z"}`
	_ = os.WriteFile(worker+".update-pending", []byte(mk), 0o644)
	// Stato pre-apply: cache "vecchia" 0.6.0.
	_ = os.WriteFile(worker+".version", []byte("0.6.0\n"), 0o644)

	applyPendingIfAny(worker)

	data, err := os.ReadFile(worker + ".version")
	if err != nil {
		t.Fatalf("cache .version mancante: %v", err)
	}
	if got := string(bytesTrim(data)); got != "0.6.1" {
		t.Errorf("cache .version=%q want=0.6.1 (deve essere aggiornata dopo apply)", got)
	}
}

func TestWriteAtomic_TmpRenameSucceeds(t *testing.T) {
	tmp := t.TempDir()
	dst := filepath.Join(tmp, "out.txt")
	if err := writeAtomic(dst, []byte("hello"), 0o644); err != nil {
		t.Fatalf("writeAtomic: %v", err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "hello" {
		t.Errorf("contenuto=%q", b)
	}
	// Nessun .tmp residuo.
	if _, err := os.Stat(dst + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp residuo: %v", err)
	}
}

func TestBuildServiceConfig_PersistsAllRelevantArgs(t *testing.T) {
	opt := Options{
		Worker:     `C:\Devel\soltea-agent\agent-runner.exe`,
		WorkerArgs: "",
		Gateway:    "https://projectopen.soltea.it/agents",
		Poll:       time.Hour,
		Canary:     60 * time.Second,
	}
	cfg := buildServiceConfig(opt)
	if cfg.Name != ServiceName {
		t.Errorf("Name=%q want=%q", cfg.Name, ServiceName)
	}
	if cfg.DisplayName == "" || cfg.Description == "" {
		t.Errorf("Display/Description vuoti: %+v", cfg)
	}
	// Gli args persistiti devono contenere worker+gateway+poll+canary, NON
	// worker-args (vuoto).
	args := cfg.Arguments
	mustContain := []string{"-worker", opt.Worker, "-gateway", opt.Gateway, "-poll", "1h0m0s", "-canary", "1m0s"}
	for _, want := range mustContain {
		if !contains(args, want) {
			t.Errorf("Arguments=%v non contiene %q", args, want)
		}
	}
	if contains(args, "-worker-args") {
		t.Errorf("Arguments include -worker-args anche se vuoto: %v", args)
	}
}

func TestNotifyUpdateReady_NonBlockingWithFullBuffer(t *testing.T) {
	ch := make(chan struct{}, 1)
	notifyUpdateReady(ch)  // primo invio ok
	notifyUpdateReady(ch)  // secondo invio NON deve bloccare
	notifyUpdateReady(nil) // nil channel: no panic
	if len(ch) != 1 {
		t.Errorf("len(ch)=%d want=1", len(ch))
	}
}

func TestRunWorker_UpdateReadyKillsWorkerAndReturnsKilledFlag(t *testing.T) {
	if testing.Short() {
		t.Skip("test richiede sleep reale")
	}
	tmp := t.TempDir()
	// Fake worker che vive 30s se nessuno lo killa.
	worker := filepath.Join(tmp, "fake-sleeper.sh")
	if err := os.WriteFile(worker, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatalf("write worker: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	updateReady := make(chan struct{}, 1)
	// Notifica l'update dopo 200ms.
	go func() {
		time.Sleep(200 * time.Millisecond)
		notifyUpdateReady(updateReady)
	}()
	t0 := time.Now()
	code, dur, killed := runWorker(ctx, worker, "", updateReady)
	elapsed := time.Since(t0)
	if !killed {
		t.Errorf("killedForUpdate=false want=true")
	}
	if elapsed > 2*time.Second {
		t.Errorf("runWorker non e' tornato subito: %s", elapsed)
	}
	if dur > 2*time.Second {
		t.Errorf("dur=%s troppo alta", dur)
	}
	_ = code // su Linux sleep killato -> code potrebbe essere 0 con killed=true
}

func TestRunWorker_NaturalExitDoesNotMarkKilled(t *testing.T) {
	tmp := t.TempDir()
	// Fake worker che esce subito con code=0.
	worker := filepath.Join(tmp, "fake-quick.sh")
	if err := os.WriteFile(worker, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	updateReady := make(chan struct{}, 1) // mai notificato
	code, _, killed := runWorker(ctx, worker, "", updateReady)
	if killed {
		t.Errorf("killedForUpdate=true ma worker e' uscito da solo")
	}
	if code != 0 {
		t.Errorf("code=%d want=0", code)
	}
}

func TestRunWorker_ExitCodeNonZeroPropagated(t *testing.T) {
	tmp := t.TempDir()
	worker := filepath.Join(tmp, "fake-fail.sh")
	_ = os.WriteFile(worker, []byte("#!/bin/sh\nexit 17\n"), 0o755)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	updateReady := make(chan struct{}, 1)
	code, _, killed := runWorker(ctx, worker, "", updateReady)
	if killed {
		t.Error("killed=true ma worker e' uscito da solo")
	}
	if code != 17 {
		t.Errorf("code=%d want=17", code)
	}
}

func TestBuildServiceConfig_OmitsEmptyOptions(t *testing.T) {
	opt := Options{Worker: "/x/agent-runner"} // niente gateway/poll/canary/worker-args
	cfg := buildServiceConfig(opt)
	for _, banned := range []string{"-gateway", "-worker-args"} {
		if contains(cfg.Arguments, banned) {
			t.Errorf("Arguments=%v include %q quando opzione e' vuota", cfg.Arguments, banned)
		}
	}
	if !contains(cfg.Arguments, "-worker") {
		t.Errorf("Arguments=%v dovrebbe contenere -worker", cfg.Arguments)
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func TestBytesTrim_StripsWhitespaceBothEnds(t *testing.T) {
	cases := map[string]string{
		"  abc  ":     "abc",
		"\n0.6.0\r\n": "0.6.0",
		"x":           "x",
		"":            "",
	}
	for in, want := range cases {
		got := string(bytesTrim([]byte(in)))
		if got != want {
			t.Errorf("bytesTrim(%q)=%q want=%q", in, got, want)
		}
	}
}
