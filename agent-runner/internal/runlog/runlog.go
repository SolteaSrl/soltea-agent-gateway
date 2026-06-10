// Package runlog fornisce il logging persistente del runner.
//
// Scrive due cose, sotto una cartella configurabile (default: accanto al
// config.json):
//   - runner.log      — eventi generali del processo (connessioni, registrazione,
//     errori di trasporto), in append.
//   - ticket-<id>-<session>.jsonl — il transcript completo di ogni sessione: un
//     evento JSON per riga (task.start, prompt inviato a claude, output GREZZO di
//     claude — stdout e stderr —, risultato, chat.send, errori, task.done).
//
// Il formato JSONL e' volutamente grezzo e completo: si legge con `jq` e cattura
// "tutta la chat e tutto cio' che claude produce", anche quando il runner gira
// come servizio Windows e non c'e' una console da guardare.
package runlog

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Logger e' thread-safe: piu' sessioni possono scrivere in parallelo.
type Logger struct {
	dir     string
	mu      sync.Mutex
	general *os.File
}

// New crea (se serve) la cartella dei log e apre il log generale in append.
func New(dir string) (*Logger, error) {
	if dir == "" {
		dir = "logs"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creazione cartella log %q: %w", dir, err)
	}
	gf, err := os.OpenFile(filepath.Join(dir, "runner.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("apertura runner.log: %w", err)
	}
	return &Logger{dir: dir, general: gf}, nil
}

// Dir e' la cartella dove finiscono i log (utile per stamparla all'avvio).
func (l *Logger) Dir() string {
	if l == nil {
		return ""
	}
	return l.dir
}

// Close chiude il log generale. Tutti i metodi restano sicuri su Logger nil.
func (l *Logger) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.general != nil {
		_ = l.general.Close()
		l.general = nil
	}
}

// Writer ritorna l'`*os.File` del log generale come io.Writer, per essere
// usato con `log.SetOutput(io.MultiWriter(os.Stderr, lg.Writer()))`: cosi'
// TUTTI i log.Printf standard del runner finiscono ANCHE in runner.log,
// non solo le righe esplicite passate per Info(). Ritorna nil se il logger
// e' nil o non ha un file aperto -- il chiamante deve controllarlo per non
// passare nil al MultiWriter.
func (l *Logger) Writer() io.Writer {
	if l == nil || l.general == nil {
		return nil
	}
	return &lockedWriter{l: l}
}

// lockedWriter serializza le Write sul file generale con lo stesso mutex usato
// da Info(): cosi' MultiWriter(stderr, file) non corrompe il transcript se due
// goroutine loggano insieme.
type lockedWriter struct{ l *Logger }

func (w *lockedWriter) Write(p []byte) (int, error) {
	if w.l == nil {
		return len(p), nil
	}
	w.l.mu.Lock()
	defer w.l.mu.Unlock()
	if w.l.general == nil {
		return len(p), nil
	}
	return w.l.general.Write(p)
}

// Info appende una riga al log generale (oltre a quanto gia' va su stdout).
func (l *Logger) Info(format string, args ...any) {
	if l == nil {
		return
	}
	line := fmt.Sprintf("%s %s\n", now(), fmt.Sprintf(format, args...))
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.general != nil {
		_, _ = l.general.WriteString(line)
	}
}

// Event e' un singolo evento del transcript di sessione.
type Event struct {
	Time          string  `json:"time"`
	Session       string  `json:"session_id"`
	Ticket        int     `json:"ticket_id,omitempty"`
	Kind          string  `json:"kind"`                // task.start | prompt | claude.raw | result | chat.send | error | task.done
	Direction     string  `json:"direction,omitempty"` // in (verso il runner) | out (verso il gateway)
	Text          string  `json:"text,omitempty"`
	Stdout        string  `json:"stdout,omitempty"`
	Stderr        string  `json:"stderr,omitempty"`
	IsError       bool    `json:"is_error,omitempty"`
	Code          string  `json:"code,omitempty"`
	CostUSD       float64 `json:"cost_usd,omitempty"`
	DurationMS    int64   `json:"duration_ms,omitempty"`
	ClaudeSession string  `json:"claude_session_id,omitempty"`
}

// Session lega gli eventi a un singolo session_id/ticket. Il transcript
// finisce nella cartella globale del Logger (<configDir>/logs/), nel formato
// legacy `ticket-<id>-<session>.jsonl`. Usato solo come fallback quando il
// progetto non e' ancora noto (es. errore prima di risolvere il workdir).
func (l *Logger) Session(sessionID string, ticketID int) *Session {
	if l == nil {
		return nil
	}
	name := fmt.Sprintf("ticket-%d-%s.jsonl", ticketID, sanitize(sessionID))
	return &Session{l: l, id: sessionID, ticket: ticketID, path: filepath.Join(l.dir, name)}
}

// SessionAt apre un transcript di sessione a un percorso esplicito (es.
// <projectPath>/.agent-runner/sessions/<sid>.jsonl). La cartella padre viene
// creata se serve. Usato dall'agent per tenere il transcript dentro
// .agent-runner/ del progetto, non in <configDir>/logs/.
func (l *Logger) SessionAt(path string, sessionID string, ticketID int) *Session {
	if l == nil {
		return nil
	}
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	return &Session{l: l, id: sessionID, ticket: ticketID, path: path}
}

// Session e' il writer del transcript di una sessione.
type Session struct {
	l      *Logger
	id     string
	ticket int
	path   string
}

// File e' il percorso del transcript di questa sessione.
func (s *Session) File() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Log appende un evento al transcript della sessione (una riga JSON).
func (s *Session) Log(ev Event) {
	if s == nil || s.l == nil || s.path == "" {
		return
	}
	ev.Time = now()
	ev.Session = s.id
	if ev.Ticket == 0 {
		ev.Ticket = s.ticket
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	s.l.mu.Lock()
	defer s.l.mu.Unlock()
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(data, '\n'))
}

func now() string { return time.Now().Format("2006-01-02T15:04:05.000Z07:00") }

// sanitize rende un id sicuro come nome file.
func sanitize(s string) string {
	if s == "" {
		return "nosession"
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, s)
}
