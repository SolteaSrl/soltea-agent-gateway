// Package svc avvolge l'agente in un servizio (Windows service / systemd / launchd)
// tramite kardianos/service, cosi' lo stesso .exe puo' install/start/stop.
package svc

import (
	"context"
	"log"

	"github.com/kardianos/service"

	"github.com/marcelloobertisolte-lab/soltea-agent-gateway/agent-runner/internal/agent"
	"github.com/marcelloobertisolte-lab/soltea-agent-gateway/agent-runner/internal/config"
)

// program implementa service.Interface.
type program struct {
	cfg    *config.Config
	agent  *agent.Agent
	cancel context.CancelFunc
}

func (p *program) Start(s service.Service) error {
	// Non bloccare: lancia il lavoro in background.
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	go p.agent.Run(ctx)
	return nil
}

func (p *program) Stop(s service.Service) error {
	if p.cancel != nil {
		p.cancel()
	}
	return nil
}

// Config descrive il servizio.
func Config() *service.Config {
	return &service.Config{
		Name:        "SolteaAgentRunner",
		DisplayName: "Soltea Agent Runner",
		Description: "Agent-runner che collega le VM di sviluppo al gateway di orchestrazione Soltea.",
	}
}

// New costruisce il servizio attorno all'agente.
func New(cfg *config.Config, svcArgs []string) (service.Service, *program, error) {
	prg := &program{cfg: cfg, agent: agent.New(cfg)}
	sc := Config()
	sc.Arguments = svcArgs
	s, err := service.New(prg, sc)
	if err != nil {
		return nil, nil, err
	}
	return s, prg, nil
}

// Control esegue un verbo del servizio (install/uninstall/start/stop/restart).
func Control(s service.Service, action string) error {
	return service.Control(s, action)
}

// RunInteractive avvia l'agente in primo piano (per debug, fuori dal servizio).
func RunInteractive(cfg *config.Config) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log.Printf("avvio agent-runner in modalita' interattiva")
	agent.New(cfg).Run(ctx)
}
