// Package agent collega configurazione, trasporto WS e CLI claude: riceve i task
// dal gateway, scompatta lo zip del ticket, invoca claude e ristreamma i risultati.
package agent

import (
	"context"
	"io"
	"log"
	"os"
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
	deltaSeq        int // numero progressivo dei chat.delta emessi in questa sessione
}

func New(cfg *config.Config) *Agent {
	lg, err := runlog.New(cfg.LogDir)
	if err != nil {
		// Il logging non deve impedire l'avvio: degradiamo a solo stdout.
		log.Printf("logging disabilitato (%v)", err)
	} else {
		log.Printf("log su %s", lg.Dir())
		// Mirroring del log standard di Go su runner.log: TUTTI i log.Printf
		// finiscono sia in stderr (console / SCM stdout-log) sia nel file
		// generale. Senza questa redirezione, runner.log conteneva solo le
		// righe scritte esplicitamente con lg.Info() -- la console aveva
		// molto piu' contesto.
		if w := lg.Writer(); w != nil {
			log.SetOutput(io.MultiWriter(os.Stderr, w))
		}
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
	log.Printf("task.start sessione=%s ticket=%d progetto=%d", in.SessionID, in.TicketID, in.ProjectID)
	a.lg.Info("sessione aperta: sessione=%s ticket=%d progetto=%d", in.SessionID, in.TicketID, in.ProjectID)

	projPath, ok := a.cfg.ProjectPath(in.ProjectID)
	if !ok {
		// Senza projectPath non possiamo creare .agent-runner/ dentro il repo:
		// usiamo il transcript legacy in <configDir>/logs/ come fallback.
		slog := a.lg.Session(in.SessionID, in.TicketID)
		slog.Log(runlog.Event{Kind: "task.start", Direction: "in", Text: in.Instructions})
		a.failSession(conn, slog, in.SessionID, "project_not_declared", "Progetto non presidiato da questo agente.")
		return
	}

	// Da qui in avanti tutto va sotto <projPath>/.agent-runner/.
	slog := a.lg.SessionAt(sessionTranscriptPath(projPath, in.SessionID), in.SessionID, in.TicketID)
	slog.Log(runlog.Event{Kind: "task.start", Direction: "in", Text: in.Instructions})

	hasFiles := in.BlobID != ""
	if hasFiles {
		if err := a.fetchTicketZip(in.BlobID, attachmentsDirFor(projPath, in.TicketID)); err != nil {
			a.failSession(conn, slog, in.SessionID, "blob_not_found", err.Error())
			return
		}
	}

	// Workdir per claude = scratch effimero della sessione (rimosso a task.done).
	workdir := sessionWorkdir(projPath, in.SessionID)
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		a.failSession(conn, slog, in.SessionID, "internal", "creazione workdir: "+err.Error())
		return
	}

	prompt := buildFirstPrompt(in.Instructions, in.TicketID, hasFiles)
	savePromptFile(projPath, in.SessionID, prompt)
	slog.Log(runlog.Event{Kind: "prompt", Direction: "out", Text: prompt})

	// Stream raw NDJSON di claude (include thinking) in streams/<sid>.ndjson.
	streamFile := openSessionStream(projPath, in.SessionID)
	if streamFile != nil {
		defer streamFile.Close()
	}

	// Pre-registriamo lo stato sessione PRIMA di chiamare claude.
	st := &sessionState{
		claudeSessionID: "", projectID: in.ProjectID,
		ticketID: in.TicketID, workdir: workdir, slog: slog,
	}
	a.mu.Lock()
	a.sessions[in.SessionID] = st
	a.mu.Unlock()

	res, err := a.runner.Run(ctx, workdir, prompt, claude.RunOptions{
		OnDelta:    a.makeDeltaCallback(conn, in.SessionID, st),
		StreamSink: streamFile,
	})
	a.logClaudeRaw(slog, res)
	if err != nil {
		a.failSession(conn, slog, in.SessionID, "claude_failed", err.Error())
		return
	}

	a.mu.Lock()
	st.claudeSessionID = res.SessionID
	a.mu.Unlock()

	slog.Log(runlog.Event{Kind: "result", Direction: "out", Text: res.Text, IsError: res.IsError,
		CostUSD: res.CostUSD, DurationMS: res.DurationMS, ClaudeSession: res.SessionID})
	log.Printf("task.start completato sessione=%s is_error=%v costo=%.4f durata_ms=%d", in.SessionID, res.IsError, res.CostUSD, res.DurationMS)
	a.lg.Info("task.start completato: sessione=%s is_error=%v costo=%.4f durata_ms=%d", in.SessionID, res.IsError, res.CostUSD, res.DurationMS)

	_ = conn.WriteJSON(protocol.TaskStartedFrame(in.SessionID, in.TicketID, res.SessionID, workdir))
	_ = conn.WriteJSON(protocol.ChatResultFrame(in.SessionID, res.Text, res.IsError, res.SessionID, res.CostUSD, res.DurationMS, version.Runner))
}

func (a *Agent) handleChatSend(ctx context.Context, conn *wsclient.Conn, in protocol.Inbound) {
	a.mu.Lock()
	st := a.sessions[in.SessionID]
	a.mu.Unlock()
	if st == nil {
		slog := a.lg.Session(in.SessionID, in.TicketID)
		a.failSession(conn, slog, in.SessionID, "unknown_session", "Sessione sconosciuta su questo agente.")
		return
	}
	st.slog.Log(runlog.Event{Kind: "chat.send", Direction: "in", Text: in.Text})
	log.Printf("chat.send sessione=%s", in.SessionID)

	projPath, _ := a.cfg.ProjectPath(st.projectID)
	savePromptFile(projPath, in.SessionID, in.Text)
	st.slog.Log(runlog.Event{Kind: "prompt", Direction: "out", Text: in.Text})

	streamFile := openSessionStream(projPath, in.SessionID)
	if streamFile != nil {
		defer streamFile.Close()
	}

	res, err := a.runner.Run(ctx, st.workdir, in.Text, claude.RunOptions{
		ResumeSession: st.claudeSessionID,
		OnDelta:       a.makeDeltaCallback(conn, in.SessionID, st),
		StreamSink:    streamFile,
	})
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
		a.lg.Info("sessione chiusa: sessione=%s ticket=%d progetto=%d", in.SessionID, st.ticketID, st.projectID)
		// Pulisce solo lo scratch work/<sid>/. Transcript, stream, prompt e
		// attachments restano in .agent-runner/ per ispezione post-mortem.
		cleanupSessionWorkdir(st.workdir)
	}
}

// logClaudeRaw registra l'output grezzo di claude (stdout + stderr), se presente.
func (a *Agent) logClaudeRaw(slog *runlog.Session, res *claude.Result) {
	if res == nil {
		return
	}
	slog.Log(runlog.Event{Kind: "claude.raw", Stdout: res.RawStdout, Stderr: res.Stderr})
}

// makeDeltaCallback ritorna un callback che il runner claude invoca per ogni
// blocco di testo dell'assistente. Il callback inoltra al gateway un chat.delta
// con un seq progressivo PER sessione, e tiene traccia del delta nel transcript
// JSONL (kind="chat.delta", direction="out").
//
// Errori di WriteJSON sono ignorati di proposito: il delta e' best-effort, il
// chat.result finale resta la fonte di verita'. Se la sessione e' caduta nel
// frattempo, l'orchestratrice non vedra' i delta ma vedra' comunque l'errore
// successivo dal task.
func (a *Agent) makeDeltaCallback(conn *wsclient.Conn, sessionID string, st *sessionState) func(text string) {
	return func(text string) {
		a.mu.Lock()
		st.deltaSeq++
		seq := st.deltaSeq
		a.mu.Unlock()
		_ = conn.WriteJSON(protocol.ChatDeltaFrame(sessionID, seq, text))
		st.slog.Log(runlog.Event{Kind: "chat.delta", Direction: "out", Text: text})
	}
}

// failSession logga e notifica un errore di sessione al gateway.
func (a *Agent) failSession(conn *wsclient.Conn, slog *runlog.Session, sessionID, code, msg string) {
	slog.Log(runlog.Event{Kind: "error", Direction: "out", Code: code, Text: msg, IsError: true})
	log.Printf("errore sessione=%s [%s]: %s", sessionID, code, msg)
	a.lg.Info("sessione chiusa con errore: sessione=%s [%s]: %s", sessionID, code, msg)
	_ = conn.WriteJSON(protocol.ErrorFrame(sessionID, code, msg))
}
