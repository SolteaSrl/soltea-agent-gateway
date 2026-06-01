// Command agent-runner collega una VM di sviluppo Windows al gateway di
// orchestrazione Soltea. Lo stesso .exe gira come servizio Windows o in primo
// piano (debug), e gestisce i verbi install/uninstall/start/stop/restart.
//
// Uso:
//
//	agent-runner.exe -config C:\path\config.json install
//	agent-runner.exe -config C:\path\config.json start
//	agent-runner.exe -config C:\path\config.json run     # primo piano (debug)
//	agent-runner.exe                                      # avviato dal SCM (servizio)
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/marcelloobertisolte-lab/soltea-agent-gateway/agent-runner/internal/config"
	"github.com/marcelloobertisolte-lab/soltea-agent-gateway/agent-runner/internal/svc"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[agent-runner] ")

	cfgPath := flag.String("config", defaultConfigPath(), "percorso del config.json")
	flag.Parse()
	verb := flag.Arg(0)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	// Percorso assoluto: il servizio gira con working dir = system32.
	absCfg, _ := filepath.Abs(*cfgPath)
	svcArgs := []string{"-config", absCfg}

	s, _, err := svc.New(cfg, svcArgs)
	if err != nil {
		log.Fatalf("servizio: %v", err)
	}

	switch verb {
	case "":
		// Avviato dal gestore servizi (o in foreground): blocca fino allo Stop.
		if err := s.Run(); err != nil {
			log.Fatalf("run: %v", err)
		}
	case "run", "debug":
		svc.RunInteractive(cfg)
	case "install", "uninstall", "start", "stop", "restart":
		if err := svc.Control(s, verb); err != nil {
			log.Fatalf("%s: %v", verb, err)
		}
		log.Printf("'%s' eseguito.", verb)
	default:
		fmt.Fprintf(os.Stderr, "verbo sconosciuto: %q\n", verb)
		fmt.Fprintln(os.Stderr, "usa: install | uninstall | start | stop | restart | run")
		os.Exit(2)
	}
}

func defaultConfigPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "config.json"
	}
	return filepath.Join(filepath.Dir(exe), "config.json")
}
