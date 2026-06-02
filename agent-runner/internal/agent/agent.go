// Package agent collega configurazione, trasporto WS e CLI claude: riceve i task
// dal gateway, scompatta lo zip del ticket, invoca claude e ristreamma i risultati.
package agent

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/marcelloobertisolte-lab/soltea-agent-gateway/agent-runner/internal/claude"
	"github.com/marcelloobertisolte-lab/soltea-agent-gateway/agent-runner/internal/config"
	"github.com/marcelloobertisolte-lab/soltea-agent-gateway/agent-runner/internal/protocol"
	"github.com/marcelloobertisolte-lab/soltea-agent-gateway/agent-runner/internal/wsclient"
)

type Agent struct {
	cfg    *config.Config
	runner *claude.Runner

	mu       sync.Mutex
	sessions map[string]*sessionState // session_id -> stato
}

type sessionState struct {
	claudeSessionID string
	projectID       int
	ticketID        int
	workdir         string
}

func New(cfg *config.Config) *Agent {
	return &Agent{
		cfg: cfg,
		runner: claude.New(claude.Options{
			ClaudePath:     cfg.ClaudePath,
			UseGitBash:     cfg.UseGitBash,
			GitBashPath:    cfg.GitBashPath,
			Model:          cfg.DefaultModel,
			PermissionMode: cfg.PermissionMode,
		}),
		sessions: map[string]*sessionState{},
	}
}

// Run mantiene la connessione al gateway con auto-reconnect finche' il contesto vive.
func (a *Agent) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := a.connectAndServe(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("connessione caduta: %v (retry tra %s)", err, backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (a *Agent) connectAndServe(ctx context.Context) error {
	conn, err := wsclient.Dial(a.cfg.GatewayURL)
	if err != nil {
		return err
	}
	defer conn.Close()

	if err := conn.WriteJSON(protocol.Hello_(a.cfg.AgentID, a.cfg.Token, a.cfg.ProjectsForHello())); err != nil {
		return err
	}
	log.Printf("connesso al gateway %s come %s", a.cfg.GatewayURL, a.cfg.AgentID)

	hbCtx, stopHB := context.WithCancel(ctx)
	defer stopHB()
	go a.heartbeat(hbCtx, conn)

	for {
		var in protocol.Inbound
		if err := conn.ReadJSON(&in); err != nil {
			return err
		}
		a.dispatch(ctx, conn, in)
	}
}

func (a *Agent) heartbeat(ctx context.Context, conn *wsclient.Conn) {
	t := time.NewTicker(time.Duration(a.cfg.HeartbeatSeconds) * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := conn.WriteJSON(protocol.PingFrame(time.Now().Unix())); err != nil {
				return
			}
		}
	}
}

func (a *Agent) dispatch(ctx context.Context, conn *wsclient.Conn, in protocol.Inbound) {
	switch in.Type {
	case protocol.Welcome:
		log.Printf("registrato sul gateway")
	case protocol.Ping:
		_ = conn.WriteJSON(protocol.PongFrame(in.TS))
	case protocol.Pong:
		// nulla
	case protocol.TaskStart:
		go a.handleTaskStart(ctx, conn, in)
	case protocol.ChatSend:
		go a.handleChatSend(ctx, conn, in)
	case protocol.TaskDone:
		a.handleTaskDone(in)
	case protocol.Error:
		log.Printf("errore dal gateway [%s]: %s", in.Code, in.Message)
	default:
		log.Printf("frame ignoto: %s", in.Type)
	}
}

func (a *Agent) handleTaskStart(ctx context.Context, conn *wsclient.Conn, in protocol.Inbound) {
	projPath, ok := a.cfg.ProjectPath(in.ProjectID)
	if !ok {
		_ = conn.WriteJSON(protocol.ErrorFrame(in.SessionID, "project_not_declared",
			"Progetto non presidiato da questo agente."))
		return
	}

	// Scarica e scompatta lo zip del ticket in <repo>/.tickets/<id>/.
	workdir := ticketDir(projPath, in.TicketID)
	if in.BlobID != "" {
		if err := a.fetchTicketZip(in.BlobID, workdir); err != nil {
			_ = conn.WriteJSON(protocol.ErrorFrame(in.SessionID, "blob_not_found", err.Error()))
			return
		}
	}

	prompt := buildFirstPrompt(in.Instructions, in.TicketID)
	res, err := a.runner.Run(ctx, projPath, prompt, "")
	if err != nil {
		_ = conn.WriteJSON(protocol.ErrorFrame(in.SessionID, "claude_failed", err.Error()))
		return
	}

	a.mu.Lock()
	a.sessions[in.SessionID] = &sessionState{
		claudeSessionID: res.SessionID, projectID: in.ProjectID,
		ticketID: in.TicketID, workdir: workdir,
	}
	a.mu.Unlock()

	_ = conn.WriteJSON(protocol.TaskStartedFrame(in.SessionID, in.TicketID, res.SessionID, workdir))
	_ = conn.WriteJSON(protocol.ChatResultFrame(in.SessionID, res.Text, res.IsError, res.SessionID, res.CostUSD, res.DurationMS))
}

func (a *Agent) handleChatSend(ctx context.Context, conn *wsclient.Conn, in protocol.Inbound) {
	a.mu.Lock()
	st := a.sessions[in.SessionID]
	a.mu.Unlock()
	if st == nil {
		_ = conn.WriteJSON(protocol.ErrorFrame(in.SessionID, "unknown_session", "Sessione sconosciuta su questo agente."))
		return
	}
	projPath, _ := a.cfg.ProjectPath(st.projectID)
	res, err := a.runner.Run(ctx, projPath, in.Text, st.claudeSessionID)
	if err != nil {
		_ = conn.WriteJSON(protocol.ErrorFrame(in.SessionID, "claude_failed", err.Error()))
		return
	}
	if res.SessionID != "" {
		a.mu.Lock()
		st.claudeSessionID = res.SessionID
		a.mu.Unlock()
	}
	_ = conn.WriteJSON(protocol.ChatResultFrame(in.SessionID, res.Text, res.IsError, res.SessionID, res.CostUSD, res.DurationMS))
}

func (a *Agent) handleTaskDone(in protocol.Inbound) {
	a.mu.Lock()
	st := a.sessions[in.SessionID]
	delete(a.sessions, in.SessionID)
	a.mu.Unlock()
	if st != nil {
		cleanupTicketDir(st.workdir)
	}
}
