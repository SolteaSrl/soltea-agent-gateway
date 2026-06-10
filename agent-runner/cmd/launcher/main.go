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
//   - Loop infinito finche' non riceve SIGINT/SIGTERM o lo Stop del servizio.
//
// Verbi (come il worker):
//
//	install | uninstall | start | stop | restart  -> gestisce il servizio Windows
//	run | debug                                    -> avvio in foreground (debug)
//	(vuoto)                                        -> avviato dal SCM (servizio)
//
// I flag (-worker / -gateway / -poll / -canary / -worker-args) vanno passati
// PRIMA del verbo e in install vengono persistiti come Arguments del servizio
// (saranno rilanciati identici al boot del servizio).
//
// E' un binario *minimale*: si aggiorna RARAMENTE, perche' e' lui che
// aggiorna il worker. Nome del servizio: "SolteaAgentLauncher" (simmetrico
// a "SolteaAgentRunner" del worker, da disinstallare).
//
// Esempi:
//
//	agent-launcher.exe -gateway https://projectopen.soltea.it/agents \
//	    -poll 60m -canary 60s install
//	agent-launcher.exe start
//	agent-launcher.exe stop
//	agent-launcher.exe uninstall
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/kardianos/service"

	"github.com/marcelloobertisolte-lab/soltea-agent-gateway/agent-runner/internal/selfupdate"
)

// ServiceName e' il nome del servizio Windows del launcher (intenzionalmente
// diverso da "SolteaAgentRunner" del worker per non avere collisioni durante
// la transizione e per renderlo riconoscibile in services.msc).
const ServiceName = "SolteaAgentLauncher"
const ServiceDisplayName = "Soltea Agent Launcher"
const ServiceDescription = "Supervisor dell'agent-runner Soltea: polla il gateway per gli aggiornamenti del worker, gestisce canary/rollback e lo spawna come sottoprocesso."

// ExitCodeUpdateReady (75 = EX_TEMPFAIL su BSD) e' il codice riservato per il
// worker per dire "ho qualcosa da aggiornare, rilanciami". Il launcher lo
// riconosce come exit "intenzionale" -> NESSUN rollback, applica pending.
const ExitCodeUpdateReady = 75

// Options raccoglie i parametri operativi del launcher (sia per foreground
// che per il servizio Windows). Vengono parsati dai flag.
type Options struct {
	Worker     string
	WorkerArgs string
	Gateway    string
	Poll       time.Duration
	Canary     time.Duration
	Once       bool
}

// program implementa service.Interface per kardianos/service.
type program struct {
	opt    Options
	cancel context.CancelFunc
}

func (p *program) Start(s service.Service) error {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	go runLoop(ctx, p.opt)
	return nil
}

func (p *program) Stop(s service.Service) error {
	if p.cancel != nil {
		p.cancel()
	}
	return nil
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[launcher] ")

	// Dirotta il log standard SIA su stderr SIA su <exeDir>/launcher.log:
	// se il launcher gira come servizio Windows non c'e' una console che
	// vede stderr, quindi senza questo redirect i log del launcher
	// (poll/scarica/canary/rollback) andrebbero persi. Nil-safe: se il file
	// non e' apribile (permessi/RO) si resta a stderr.
	if w := openLauncherLog(); w != nil {
		log.SetOutput(io.MultiWriter(os.Stderr, w))
	}

	defaultWorker := filepath.Join(exeDir(), workerExeName())
	worker := flag.String("worker", defaultWorker, "percorso del binario worker")
	workerArgs := flag.String("worker-args", "", "args extra da passare al worker (spazi-separati)")
	gateway := flag.String("gateway", "", "base HTTP del gateway, es. https://.../agents (vuoto = auto-update off)")
	poll := flag.Duration("poll", time.Hour, "intervallo di poll su /runner/latest")
	canary := flag.Duration("canary", 60*time.Second, "se il worker exit con err entro questo tempo -> rollback")
	once := flag.Bool("once", false, "lancia il worker UNA volta e exit (per test)")
	flag.Parse()
	verb := flag.Arg(0)

	opt := Options{
		Worker: *worker, WorkerArgs: *workerArgs, Gateway: *gateway,
		Poll: *poll, Canary: *canary, Once: *once,
	}

	prg := &program{opt: opt}
	svcConfig := buildServiceConfig(opt)
	s, err := service.New(prg, svcConfig)
	if err != nil {
		log.Fatalf("init servizio: %v", err)
	}

	switch verb {
	case "":
		// Avviato dal SCM (servizio) o eseguibile lanciato senza verbi. La
		// libreria kardianos rileva il contesto: se siamo nel servizio,
		// `s.Run()` blocca chiamando Start/Stop sul SCM; se siamo in console,
		// si comporta come "foreground" e blocca finche' Ctrl+C non chiude.
		if err := s.Run(); err != nil {
			log.Fatalf("run: %v", err)
		}
	case "run", "debug":
		// Foreground esplicito: stesso loop, ma stampiamo subito i parametri
		// e gestiamo Ctrl+C noi (utile per il debug locale).
		runForeground(opt)
	case "install":
		if err := service.Control(s, "install"); err != nil {
			log.Fatalf("install: %v", err)
		}
		log.Printf("'install' eseguito. Argomenti registrati nel servizio: %v", svcConfig.Arguments)
	case "uninstall", "start", "stop", "restart":
		if err := service.Control(s, verb); err != nil {
			log.Fatalf("%s: %v", verb, err)
		}
		log.Printf("'%s' eseguito.", verb)
	default:
		fmt.Fprintf(os.Stderr, "verbo sconosciuto: %q\n", verb)
		fmt.Fprintln(os.Stderr, "usa: install | uninstall | start | stop | restart | run")
		os.Exit(2)
	}
}

// buildServiceConfig costruisce il service.Config con gli args persistenti.
// All'install, gli args vengono salvati nel binPath del servizio Windows: il
// servizio rilancia il launcher con ESATTAMENTE questi parametri.
func buildServiceConfig(opt Options) *service.Config {
	args := []string{}
	if opt.Worker != "" {
		args = append(args, "-worker", opt.Worker)
	}
	if opt.WorkerArgs != "" {
		args = append(args, "-worker-args", opt.WorkerArgs)
	}
	if opt.Gateway != "" {
		args = append(args, "-gateway", opt.Gateway)
	}
	if opt.Poll != 0 {
		args = append(args, "-poll", opt.Poll.String())
	}
	if opt.Canary != 0 {
		args = append(args, "-canary", opt.Canary.String())
	}
	return &service.Config{
		Name:        ServiceName,
		DisplayName: ServiceDisplayName,
		Description: ServiceDescription,
		Arguments:   args,
	}
}

// runForeground replica il blocco "run loop + ctrl+c" che fa il SCM, per i
// verbi "run"/"debug" usati in console.
func runForeground(opt Options) {
	if _, err := os.Stat(opt.Worker); err != nil {
		log.Fatalf("worker non trovato: %s (%v)", opt.Worker, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-sigs
		log.Printf("ricevuto %s: shutting down", s)
		cancel()
	}()
	runLoop(ctx, opt)
}

// runLoop e' il cuore del launcher: poll + spawn worker + canary/rollback.
// Usato sia dal program.Start (in goroutine) sia da runForeground (bloccante).
//
// `updateReady` e' il canale su cui updatePollLoop notifica "ho appena scaricato
// un nuovo .new pronto da applicare". Il loop principale, quando lo vede,
// killa il worker corrente cosi' al prossimo giro applyPendingIfAny puo'
// rotear i file. Senza questo, il worker (che gira come server e non esce
// mai da solo) terrebbe il vecchio binario in eterno e il poll continuerebbe
// a riscaricare il .new ad ogni tick.
func runLoop(ctx context.Context, opt Options) {
	if _, err := os.Stat(opt.Worker); err != nil {
		log.Printf("worker non trovato: %s (%v) -- esco", opt.Worker, err)
		return
	}
	// Buffer 1: il poll loop notifica al massimo "ho un update", se il main loop
	// non lo ha ancora consumato il secondo invio viene scartato.
	updateReady := make(chan struct{}, 1)
	if opt.Gateway != "" {
		log.Printf("auto-update: poll %s ogni %s", opt.Gateway, opt.Poll)
		go updatePollLoop(ctx, opt.Gateway, opt.Worker, opt.Poll, updateReady)
	} else {
		log.Printf("auto-update: disabilitato (no -gateway)")
	}

	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for ctx.Err() == nil {
		applyPendingIfAny(opt.Worker)

		code, dur, killedForUpdate := runWorker(ctx, opt.Worker, opt.WorkerArgs, updateReady)
		log.Printf("worker exit: code=%d durata=%s killedForUpdate=%v", code, dur, killedForUpdate)

		switch {
		case ctx.Err() != nil:
			return
		case killedForUpdate:
			// Kill volontario per applicare update: niente canary/rollback,
			// niente backoff, applyPending al prossimo giro.
			backoff = time.Second
			log.Printf("worker terminato per applicare update -> apply al prossimo ciclo")
		case code == ExitCodeUpdateReady:
			backoff = time.Second
			log.Printf("worker richiede update -> applico al prossimo ciclo")
		case code != 0 && dur < opt.Canary && rollback(opt.Worker):
			log.Printf("CANARY FAILED (exit %d in %s < %s) -> ROLLBACK eseguito", code, dur, opt.Canary)
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

		if opt.Once {
			return
		}
	}
}

// runWorker spawna il worker e attende uno fra: exit naturale, ctx.Done,
// segnale di update scaricato. Ritorna (exitCode, durata, killedForUpdate).
//
// Se `updateReady` riceve un segnale prima dell'exit naturale, il worker
// viene Killato (TerminateProcess su Windows) per permettere a
// applyPendingIfAny di ruotare i file al prossimo giro.
func runWorker(ctx context.Context, worker, extraArgs string, updateReady <-chan struct{}) (int, time.Duration, bool) {
	args := splitArgs(extraArgs)
	start := time.Now()
	cmd := exec.CommandContext(ctx, worker, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		log.Printf("avvio worker fallito: %v", err)
		return -1, time.Since(start), false
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	killedForUpdate := false
	select {
	case <-updateReady:
		log.Printf("rilevato update pronto -> kill del worker per applicarlo")
		killedForUpdate = true
		_ = cmd.Process.Kill()
		<-done
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-done
		return -1, time.Since(start), false
	case err := <-done:
		if err == nil {
			return 0, time.Since(start), false
		}
		var exitErr *exec.ExitError
		if asExit(err, &exitErr) {
			return exitErr.ExitCode(), time.Since(start), false
		}
		log.Printf("worker terminato con errore non-exit: %v", err)
		return -1, time.Since(start), false
	}
	return 0, time.Since(start), killedForUpdate
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
	// Aggiorna la cache della versione installata in modo che il prossimo poll
	// NON consideri l'asset come "nuovo".
	if err := writeAtomic(worker+".version", []byte(mk.Version+"\n"), 0o644); err != nil {
		log.Printf("aggiornamento cache .version: %v", err)
	}
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
//
// Quando un download va a buon fine, notifica `updateReady` (non bloccante:
// se il main loop non lo ha ancora consumato, il segnale precedente vince e
// questo viene scartato). Il main loop killera' il worker per applicare.
//
// Skip-download intelligente: se .update-pending esiste gia' per la versione
// "latest", evitiamo di ri-scaricare. Necessario perche' il worker non esce
// mai da solo, e senza questo check il poll riscarica il .new ad ogni tick
// (overhead di rete + churn del filesystem) finche' il main loop non killa
// il worker.
func updatePollLoop(ctx context.Context, gatewayBase, worker string, every time.Duration, updateReady chan<- struct{}) {
	client := &selfupdate.Client{GatewayBase: gatewayBase}
	doCheck := func() {
		current := currentWorkerVersion(worker)
		m, err := client.Fetch(ctx)
		if err != nil {
			log.Printf("update check: errore %v", err)
			return
		}
		if m == nil {
			return // 404 lato gateway: auto-update off
		}
		if !m.IsNewerThan(current) {
			log.Printf("update check: versione corrente %q == latest %q, niente da fare", current, m.Version)
			return
		}
		// Evita di riscaricare se il .new e il marker pending per la STESSA
		// versione esistono gia': aspettiamo solo che il main loop killi il
		// worker e applichi.
		if mk, err := selfupdate.ReadPending(worker); err == nil && mk.Version == m.Version {
			log.Printf("update gia' scaricato: %q pronto in attesa di apply, notifico main loop", m.Version)
			notifyUpdateReady(updateReady)
			return
		}
		log.Printf("update disponibile: %q -> %q", current, m.Version)
		if err := client.Download(ctx, worker, *m); err != nil {
			log.Printf("download update: errore %v", err)
			return
		}
		log.Printf("update SCARICATO: v%s pronto in %s.new", m.Version, filepath.Base(worker))
		notifyUpdateReady(updateReady)
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

// notifyUpdateReady manda un segnale non bloccante al main loop. Buffer=1:
// se non e' ancora stato consumato, il successivo viene scartato (l'effetto
// e' lo stesso, l'update e' pronto e basta).
func notifyUpdateReady(ch chan<- struct{}) {
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

// currentWorkerVersion ritorna la versione attualmente installata del worker.
// Risoluzione, in ordine:
//  1. cache su file `<worker>.version` (scritto da applyPendingIfAny dopo un
//     update e popolato al primo giro come fallback)
//  2. exec `worker -print-version` (sicuro: il flag non avvia la sessione, esce
//     subito)
//  3. "" se entrambi falliscono (vecchio comportamento -> scarica sempre)
//
// Cache: dopo (2) scriviamo il risultato su `<worker>.version` cosi' i giri
// successivi non riavviano il worker per leggere la versione.
func currentWorkerVersion(worker string) string {
	cachePath := worker + ".version"
	if data, err := os.ReadFile(cachePath); err == nil {
		if v := string(bytesTrim(data)); v != "" {
			return v
		}
	}
	// Exec di sondaggio: timeout 10s e' piu' che abbastanza per un flag che
	// stampa una const.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, worker, "-print-version")
	out, err := cmd.Output()
	if err != nil {
		log.Printf("currentWorkerVersion: impossibile interrogare %s (-print-version): %v", filepath.Base(worker), err)
		return ""
	}
	v := string(bytesTrim(out))
	if v == "" {
		return ""
	}
	if err := writeAtomic(cachePath, []byte(v+"\n"), 0o644); err != nil {
		log.Printf("cache .version: %v", err)
	}
	return v
}

// writeAtomic scrive via temp + rename. Errori solo se il rename fallisce.
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
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

// openLauncherLog apre (in append) il file launcher.log accanto al binario
// del launcher. Ritorna nil se l'apertura fallisce: il caller usa stderr solo.
func openLauncherLog() *os.File {
	path := filepath.Join(exeDir(), "launcher.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[launcher] avviso: launcher.log non apribile (%v); log solo in console\n", err)
		return nil
	}
	return f
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
