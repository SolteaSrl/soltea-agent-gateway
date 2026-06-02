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
	"github.com/marcelloobertisolte-lab/soltea-agent-gateway/agent-runner/internal/runlog"
	"github.com/marcelloobertisolte-lab/soltea-agent-gateway/agent-runner/internal/version"
	"github.com/marcelloobertisolte-lab/soltea-agent-gateway/agent-runner/internal/wsclient"
)

type Agent struct {
	cfg    *config.Config
	runner *claude.Runner
	lg     *runlog.Logger

	mu       sync.Mutex
	sessions map[string]*sessionState // session_id -> stato
}

type sessionState struct {
	claudeSessionID string
	projectID       int
	ticketID        int
	workdir         string
	slog            *runlog.Session
}

func New(cfg *config.Config) *Agent {
	lg, err := runlog.New(cfg.LogDir)
	if err != nil {
		// Il logging non deve impedire l'avvio: degradiamo a solo stdout.
		log.Printf("logging disabilitato (%v)", err)
	} else {
		log.Printf("log su %s", lg.Dir())
	}
	log.Printf("agent-runner v%s", version.Runner)
	lg.Info("agent-runner v%s avviato", version.Runner)
	return &Agent{
		cfg: cfg,
		lg:  lg,
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

	if err := conn.WriteJSON(protocol.Hello_(a.cfg.AgentID, a.cfg.Token, version.Runner, a.cfg.ProjectsForHello())); err != nil {
		return err
	}
	log.Printf("connesso al gateway %s come %s (runner v%s)", a.cfg.GatewayURL, a.cfg.AgentID, version.Runner)
	a.lg.Info("connesso al gateway %s come %s (runner v%s)", a.cfg.GatewayURL, a.cfg.AgentID, version.Runner)

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
		a.lg.Info("registrato sul gateway")
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
		a.lg.Info("errore dal gateway [%s]: %s", in.Code, in.Message)
	default:
		log.Printf("frame ignoto: %s", in.Type)
	}
}

func (a *Agent) handleTaskStart(ctx context.Context, conn *wsclient.Conn, in protocol.Inbound) {
	slog := a.lg.Session(in.SessionID, in.TicketID)
	slog.Log(runlog.Event{Kind: "task.start", Direction: "in", Text: in.Instructions})
	log.Printf("task.start sessione=%s ticket=%d progetto=%d", in.SessionID, in.TicketID, in.ProjectID)

	projPath, ok := a.cfg.ProjectPath(in.ProjectID)
	if !ok {
		a.failSession(conn, slog, in.SessionID, "project_not_declared", "Progetto non presidiato da questo agente.")
		return
	}

	// Scarica e scompatta lo zip del ticket in <repo>/.tickets/<id>/.
	workdir := ticketDir(projPath, in.TicketID)
	hasFiles := in.BlobID != ""
	if hasFiles {
		if err := a.fetchTicketZip(in.BlobID, workdir); err != nil {
			a.failSession(conn, slog, in.SessionID, "blob_not_found", err.Error())
			return
		}
	}

	prompt := buildFirstPrompt(in.Instructions, in.TicketID, hasFiles)
	slog.Log(runlog.Event{Kind: "prompt", Direction: "out", Text: prompt})
	res, err := a.runner.Run(ctx, projPath, prompt, "")
	a.logClaudeRaw(slog, res)
	if err != nil {
		a.failSession(conn, slog, in.SessionID, "claude_failed", err.Error())
		return
	}

	a.mu.Lock()
	a.sessions[in.SessionID] = &sessionState{
		claudeSessionID: res.SessionID, projectID: in.ProjectID,
		ticketID: in.TicketID, workdir: workdir, slog: slog,
	}
	a.mu.Unlock()

	slog.Log(runlog.Event{Kind: "result", Direction: "out", Text: res.Text, IsError: res.IsError,
		CostUSD: res.CostUSD, DurationMS: res.DurationMS, ClaudeSession: res.SessionID})
	log.Printf("task.start completato sessione=%s is_error=%v costo=%.4f durata_ms=%d", in.SessionID, res.IsError, res.CostUSD, res.DurationMS)

	_ = conn.WriteJSON(protocol.TaskStartedFrame(in.SessionID, in.TicketID, res.SessionID, workdir))
	_ = conn.WriteJSON(protocol.ChatResultFrame(in.SessionID, res.Text, res.IsError, res.SessionID, res.CostUSD, res.DurationMS, version.Runner))
}

func (a *Agent) handleChatSend(ctx context.Context, conn *wsclient.Conn, in protocol.Inbound) {
	a.mu.Lock()
	st := a.sessions[in.SessionID]
	a.mu.Unlock()
	if st == nil {
		// Sessione ignota: logghiamo comunque su un transcript dedicato.
		slog := a.lg.Session(in.SessionID, in.TicketID)
		a.failSession(conn, slog, in.SessionID, "unknown_session", "Sessione sconosciuta su questo agente.")
		return
	}
	st.slog.Log(runlog.Event{Kind: "chat.send", Direction: "in", Text: in.Text})
	log.Printf("chat.send sessione=%s", in.SessionID)

	projPath, _ := a.cfg.ProjectPath(st.projectID)
	st.slog.Log(runlog.Event{Kind: "prompt", Direction: "out", Text: in.Text})
	res, err := a.runner.Run(ctx, projPath, in.Text, st.claudeSessionID)
	a.logClaudeRaw(st.slog, res)
	if err != nil {
		a.failSession(conn, st.slog, in.SessionID, "claude_failed", err.Error())
		return
	}
	if res.SessionID != "" {
		a.mu.Lock()
		st.claudeSessionID = res.SessionID
		a.mu.Unlock()
	}
	st.slog.Log(runlog.Event{Kind: "result", Direction: "out", Text: res.Text, IsError: res.IsError,
		CostUSD: res.CostUSD, DurationMS: res.DurationMS, ClaudeSession: res.SessionID})
	_ = conn.WriteJSON(protocol.ChatResultFrame(in.SessionID, res.Text, res.IsError, res.SessionID, res.CostUSD, res.DurationMS, version.Runner))
}

func (a *Agent) handleTaskDone(in protocol.Inbound) {
	a.mu.Lock()
	st := a.sessions[in.SessionID]
	delete(a.sessions, in.SessionID)
	a.mu.Unlock()
	if st != nil {
		st.slog.Log(runlog.Event{Kind: "task.done", Direction: "in"})
		log.Printf("task.done sessione=%s", in.SessionID)
		cleanupTicketDir(st.workdir)
	}
}

// logClaudeRaw registra l'output grezzo di claude (stdout + stderr), se presente.
func (a *Agent) logClaudeRaw(slog *runlog.Session, res *claude.Result) {
	if res == nil {
		return
	}
	slog.Log(runlog.Event{Kind: "claude.raw", Stdout: res.RawStdout, Stderr: res.Stderr})
}

// failSession logga e notifica un errore di sessione al gateway.
func (a *Agent) failSession(conn *wsclient.Conn, slog *runlog.Session, sessionID, code, msg string) {
	slog.Log(runlog.Event{Kind: "error", Direction: "out", Code: code, Text: msg, IsError: true})
	log.Printf("errore sessione=%s [%s]: %s", sessionID, code, msg)
	_ = conn.WriteJSON(protocol.ErrorFrame(sessionID, code, msg))
}
