// Command agent-launcher e' il supervisor del worker (agent-runner.exe).
//
// Responsabilita':
//   - Polla il gateway su /runner/latest e scarica le nuove versioni del
//     worker (verifica SHA256). Vedi internal/selfupdate.
//   - Applica gli update PRIMA di avviare il worker:
//     1. se esiste <worker>.update-pending e <worker>.new, ruota:
//     worker -> worker.old, worker.new -> worker, marker rimosso.
//   - Spawna il worker, attende l'exit, gestisce:
//   - canary: se il worker exit con codice != 0 entro CanarySeconds e
//     <worker>.old esiste -> ROLLBACK (rinomina .old -> worker), backoff
//     e riavvio col vecchio.
//   - exit normale > canary: backoff incrementale (max 30s) e restart.
//   - Loop infinito finche' non riceve SIGINT/SIGTERM.
//
// E' un binario *minimale* (~stdlib only): si aggiorna RARAMENTE, perche'
// e' lui che aggiorna il worker. In Windows lo si registra come servizio al
// posto di agent-runner.exe; il worker NON si registra piu' come servizio.
//
// Uso tipico:
//
//	agent-launcher.exe                    (config.json e worker accanto)
//	agent-launcher.exe -worker C:\Devel\soltea-agent\agent-runner.exe \
//	    -gateway https://projectopen.soltea.it/agents \
//	    -poll 60m -canary 60s
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/marcelloobertisolte-lab/soltea-agent-gateway/agent-runner/internal/selfupdate"
)

// ExitCodeUpdateReady (75 = EX_TEMPFAIL su BSD) e' il codice riservato per il
// worker per dire "ho qualcosa da aggiornare, rilanciami". Il launcher lo
// riconosce come exit "intenzionale" -> NESSUN rollback, applica pending.
const ExitCodeUpdateReady = 75

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[launcher] ")

	defaultWorker := filepath.Join(exeDir(), workerExeName())
	worker := flag.String("worker", defaultWorker, "percorso del binario worker")
	workerArgs := flag.String("worker-args", "", "args extra da passare al worker (spazi-separati)")
	gateway := flag.String("gateway", "", "base HTTP del gateway, es. https://.../agents (vuoto = auto-update off)")
	poll := flag.Duration("poll", time.Hour, "intervallo di poll su /runner/latest")
	canary := flag.Duration("canary", 60*time.Second, "se il worker exit con err entro questo tempo -> rollback")
	once := flag.Bool("once", false, "lancia il worker UNA volta e exit (per test)")
	flag.Parse()

	if _, err := os.Stat(*worker); err != nil {
		log.Fatalf("worker non trovato: %s (%v)", *worker, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Cattura SIGINT/SIGTERM per spegnimento ordinato.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-sigs
		log.Printf("ricevuto %s: shutting down", s)
		cancel()
	}()

	// Poll dello update in background. Indipendente dal worker (gira anche
	// mentre il worker e' attivo: non lo disturba, scrive solo .new + marker).
	if *gateway != "" {
		log.Printf("auto-update: poll %s ogni %s", *gateway, *poll)
		go updatePollLoop(ctx, *gateway, *worker, *poll)
	} else {
		log.Printf("auto-update: disabilitato (no -gateway)")
	}

	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for ctx.Err() == nil {
		applyPendingIfAny(*worker)

		code, dur := runWorker(ctx, *worker, *workerArgs)
		log.Printf("worker exit: code=%d durata=%s", code, dur)

		switch {
		case ctx.Err() != nil:
			return
		case code == ExitCodeUpdateReady:
			// Update richiesto: applica al prossimo giro, niente backoff.
			backoff = time.Second
			log.Printf("worker richiede update -> applico al prossimo ciclo")
		case code != 0 && dur < *canary && rollback(*worker):
			log.Printf("CANARY FAILED (exit %d in %s < %s) -> ROLLBACK eseguito", code, dur, *canary)
			backoff = time.Second
		case code != 0:
			log.Printf("worker exit con errore -> restart fra %s", backoff)
			sleepCtx(ctx, backoff)
			if backoff < maxBackoff {
				backoff *= 2
			}
		default:
			backoff = time.Second
		}

		if *once {
			return
		}
	}
}

// runWorker spawna il worker, attende l'exit, ritorna (exitCode, durata).
// Su SIGINT/SIGTERM del padre, il worker viene segnalato a sua volta.
func runWorker(ctx context.Context, worker, extraArgs string) (int, time.Duration) {
	args := splitArgs(extraArgs)
	start := time.Now()
	cmd := exec.CommandContext(ctx, worker, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if asExit(err, &exitErr) {
			return exitErr.ExitCode(), time.Since(start)
		}
		// errore non-exit (impossibile avviare): pretendiamo code -1 e bailout breve.
		log.Printf("avvio worker fallito: %v", err)
		return -1, time.Since(start)
	}
	return 0, time.Since(start)
}

// asExit fa un type assert dato che errors.As non e' importato (stdlib-only-ish).
func asExit(err error, target **exec.ExitError) bool {
	if e, ok := err.(*exec.ExitError); ok {
		*target = e
		return true
	}
	return false
}

func splitArgs(s string) []string {
	out := []string{}
	cur := []rune{}
	for _, r := range s {
		if r == ' ' || r == '\t' {
			if len(cur) > 0 {
				out = append(out, string(cur))
				cur = cur[:0]
			}
			continue
		}
		cur = append(cur, r)
	}
	if len(cur) > 0 {
		out = append(out, string(cur))
	}
	return out
}

// applyPendingIfAny: se esistono <worker>.update-pending e <worker>.new, ruota
// worker -> .old, .new -> worker. Best-effort: log e proseguo se qualche rename
// fallisce (il worker vecchio resta vivo, il prossimo poll riprova).
//
// Atomico per quanto possibile su Windows: se il rename worker->.old fallisce
// (es. file lock), abortisci senza toccare .new -> retry al ciclo successivo.
func applyPendingIfAny(worker string) {
	pending := worker + ".update-pending"
	newPath := worker + ".new"
	if _, err := os.Stat(pending); err != nil {
		return
	}
	if _, err := os.Stat(newPath); err != nil {
		log.Printf("marker pending ma .new mancante: rimuovo marker stale (%v)", err)
		_ = os.Remove(pending)
		return
	}
	mk, err := selfupdate.ReadPending(worker)
	if err != nil {
		log.Printf("marker pending illeggibile (%v) -> ignoro", err)
		return
	}
	oldPath := worker + ".old"
	_ = os.Remove(oldPath) // .old vecchio: liberiamo lo slot
	if err := os.Rename(worker, oldPath); err != nil {
		log.Printf("rotazione worker->old fallita (%v) -> retry al prossimo ciclo", err)
		return
	}
	if err := os.Rename(newPath, worker); err != nil {
		log.Printf("rotazione new->worker fallita (%v) -> ripristino", err)
		_ = os.Rename(oldPath, worker)
		return
	}
	_ = os.Remove(pending)
	log.Printf("UPDATE APPLICATO: %s -> v%s (sha=%s)", filepath.Base(worker), mk.Version, shorten(mk.SHA256))
}

// rollback ripristina <worker>.old se esiste. True se ha fatto qualcosa.
func rollback(worker string) bool {
	oldPath := worker + ".old"
	if _, err := os.Stat(oldPath); err != nil {
		return false
	}
	// Sposta il worker corrente in .broken per analisi, poi .old -> worker.
	broken := worker + ".broken"
	_ = os.Remove(broken)
	if err := os.Rename(worker, broken); err != nil {
		log.Printf("rollback: rename worker->broken fallito (%v)", err)
		return false
	}
	if err := os.Rename(oldPath, worker); err != nil {
		log.Printf("rollback: rename old->worker fallito (%v) -- ripristino broken", err)
		_ = os.Rename(broken, worker)
		return false
	}
	return true
}

// updatePollLoop polla /runner/latest e scarica i nuovi .new. Tick "subito +
// ogni poll": al boot del launcher controlliamo subito, cosi' un update gia'
// pronto sul gateway entra in vigore al primo avvio.
func updatePollLoop(ctx context.Context, gatewayBase, worker string, every time.Duration) {
	client := &selfupdate.Client{GatewayBase: gatewayBase}
	doCheck := func() {
		// La versione "corrente" del worker la deduciamo dal contenuto del marker
		// applicato (se c'e'). Altrimenti "" -> scarica sempre (il primo giro).
		current := readAppliedVersion(worker)
		m, err := client.Fetch(ctx)
		if err != nil {
			log.Printf("update check: errore %v", err)
			return
		}
		if m == nil {
			return // 404 lato gateway: auto-update off
		}
		if !m.IsNewerThan(current) {
			return
		}
		log.Printf("update disponibile: %s -> %s", current, m.Version)
		if err := client.Download(ctx, worker, *m); err != nil {
			log.Printf("download update: errore %v", err)
			return
		}
		log.Printf("update SCARICATO: v%s pronto in %s.new (verra' applicato al prossimo restart del worker)", m.Version, filepath.Base(worker))
	}
	doCheck()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			doCheck()
		}
	}
}

// readAppliedVersion legge il marker <worker>.update-applied (scritto dopo
// applyPendingIfAny in una versione futura) o il marker pending se non c'e'.
// Per ora -> stringa vuota se non sa la versione, cosi' Fetch torna sempre il
// manifest e IsNewerThan compara con "". Funziona bene: la prima volta scarica,
// poi confrontera' con la versione che il launcher annota dopo l'apply.
func readAppliedVersion(worker string) string {
	if mk, err := selfupdate.ReadPending(worker); err == nil {
		return mk.Version
	}
	data, err := os.ReadFile(worker + ".version")
	if err != nil {
		return ""
	}
	return string(bytesTrim(data))
}

func bytesTrim(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ' || b[len(b)-1] == '\t') {
		b = b[:len(b)-1]
	}
	for len(b) > 0 && (b[0] == '\n' || b[0] == '\r' || b[0] == ' ' || b[0] == '\t') {
		b = b[1:]
	}
	return b
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

func workerExeName() string {
	if isWindows() {
		return "agent-runner.exe"
	}
	return "agent-runner"
}

func isWindows() bool { return os.PathSeparator == '\\' }

func shorten(s string) string {
	if len(s) > 12 {
		return s[:12] + "…"
	}
	return s
}

// Variabile usata solo per evitare warning "imported and not used"
// nel caso il package fmt non sia gia' tirato in da log.
var _ = fmt.Sprint
