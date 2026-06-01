// Package protocol definisce i tipi di frame scambiati col gateway.
// Vedi docs/PROTOCOL.md per la specifica completa.
package protocol

const (
	Hello       = "hello"
	Welcome     = "welcome"
	Ping        = "ping"
	Pong        = "pong"
	TaskStart   = "task.start"
	TaskStarted = "task.started"
	ChatSend    = "chat.send"
	ChatDelta   = "chat.delta"
	ChatResult  = "chat.result"
	TaskDone    = "task.done"
	Error       = "error"

	RoleAgent = "agent"
)

// Inbound e' l'envelope generico ricevuto dal gateway: si legge prima Type, poi
// si accede ai campi pertinenti.
type Inbound struct {
	Type         string `json:"type"`
	SessionID    string `json:"session_id"`
	ProjectID    int    `json:"project_id"`
	TicketID     int    `json:"ticket_id"`
	BlobID       string `json:"blob_id"`
	Instructions string `json:"instructions"`
	Text         string `json:"text"`
	TS           int64  `json:"ts"`
	Code         string `json:"code"`
	Message      string `json:"message"`
}

// --- costruttori dei frame in uscita (verso il gateway) ---

func Hello_(agentID, token string, projects []map[string]any) map[string]any {
	return map[string]any{
		"type": Hello, "role": RoleAgent, "agent_id": agentID,
		"token": token, "projects": projects,
	}
}

func PingFrame(ts int64) map[string]any { return map[string]any{"type": Ping, "ts": ts} }
func PongFrame(ts int64) map[string]any { return map[string]any{"type": Pong, "ts": ts} }

func TaskStartedFrame(sessionID string, ticketID int, claudeSessionID, workdir string) map[string]any {
	return map[string]any{
		"type": TaskStarted, "session_id": sessionID, "ticket_id": ticketID,
		"claude_session_id": claudeSessionID, "workdir": workdir,
	}
}

func ChatResultFrame(sessionID, text string, isError bool, claudeSessionID string, costUSD float64, durationMS int64) map[string]any {
	return map[string]any{
		"type": ChatResult, "session_id": sessionID, "text": text, "is_error": isError,
		"claude_session_id": claudeSessionID, "cost_usd": costUSD, "duration_ms": durationMS,
	}
}

func ErrorFrame(sessionID, code, message string) map[string]any {
	return map[string]any{"type": Error, "session_id": sessionID, "code": code, "message": message}
}
