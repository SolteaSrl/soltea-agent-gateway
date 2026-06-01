# PROTOCOL — frame su WebSocket

Tutti i messaggi sono **JSON** (un frame = un oggetto). Trasporto: WSS verso il
gateway. Sia l'orchestratrice sia gli agent-runner usano lo stesso endpoint `/ws`
e si identificano nel primo frame (`hello`).

Ogni frame ha un campo `type`. I campi non pertinenti sono omessi.

## 1. Handshake

### `hello` (client → gateway), primo frame obbligatorio
```json
{ "type": "hello", "role": "agent", "token": "...", "agent_id": "win-dev-01",
  "projects": [ { "project_id": 1234, "name": "Sito X", "path": "C:\\dev\\x" } ] }
```
```json
{ "type": "hello", "role": "orchestrator", "token": "...", "client_id": "claudia" }
```
- `role`: `"agent"` | `"orchestrator"`.
- Per `agent`: `projects[]` con `project_id` (int) e opzionali `name`, `path`.
- Auth: `token` confrontato con i token noti al gateway. Token invalido → `error` + chiusura.

### `welcome` (gateway → client), risposta a `hello`
```json
{ "type": "welcome", "session_scope": "agent", "agent_id": "win-dev-01",
  "registered_projects": [1234, 1240], "heartbeat_seconds": 30 }
```

## 2. Liveness

### `ping` / `pong`
```json
{ "type": "ping", "ts": 1780351820 }
{ "type": "pong", "ts": 1780351820 }
```
Inviati periodicamente (`heartbeat_seconds`). Il gateway marca offline gli agenti
che non danno segni di vita oltre `2 × heartbeat_seconds`.

## 3. Discovery (orchestratrice → gateway)

### `resolve_project` → `project_resolved`
```json
{ "type": "resolve_project", "req_id": "r1", "project_id": 1234 }
```
```json
{ "type": "project_resolved", "req_id": "r1", "project_id": 1234,
  "agent_id": "win-dev-01", "online": true }
```
Se nessun agente presidia il progetto: `agent_id: null, online: false`.

### `list_agents` → `agents`
```json
{ "type": "list_agents", "req_id": "r2" }
```
```json
{ "type": "agents", "req_id": "r2", "agents": [
  { "agent_id": "win-dev-01", "online": true, "projects": [1234, 1240] } ] }
```

## 4. Ciclo di lavoro su un ticket

### `task.start` (orchestratrice → gateway → agente)
```json
{ "type": "task.start", "req_id": "t1", "project_id": 1234, "ticket_id": 5678,
  "blob_id": "9f1c...", "instructions": "Risolvi il ticket secondo i criteri nello zip." }
```
Il gateway: risolve `project_id` → agente, genera un `session_id`, registra la
sessione e inoltra il frame all'agente (aggiungendo `session_id`). Se il progetto
non è presidiato/online → `error`.

### `task.started` (agente → gateway → orchestratrice)
```json
{ "type": "task.started", "session_id": "sess-abc", "ticket_id": 5678,
  "claude_session_id": "77c17ef9-...", "workdir": "C:\\dev\\x\\.tickets\\5678" }
```

### `chat.send` (orchestratrice → gateway → agente)
```json
{ "type": "chat.send", "session_id": "sess-abc", "text": "Rivedi la gestione errori in client.py" }
```

### `chat.delta` (agente → … → orchestratrice) — opzionale, streaming
```json
{ "type": "chat.delta", "session_id": "sess-abc", "text": "sto leggendo client.py..." }
```

### `chat.result` (agente → … → orchestratrice) — fine di un turno
```json
{ "type": "chat.result", "session_id": "sess-abc", "text": "Fatto. Ho aggiunto try/except e un test.",
  "is_error": false, "claude_session_id": "77c17ef9-...", "cost_usd": 0.10, "duration_ms": 24025 }
```

### `task.done` (orchestratrice → gateway → agente)
```json
{ "type": "task.done", "session_id": "sess-abc", "outcome": "resolved" }
```
L'agente fa cleanup del workdir; il gateway chiude la sessione e libera il blob.

## 5. Errori

### `error` (gateway/agente → controparte)
```json
{ "type": "error", "req_id": "t1", "session_id": "sess-abc",
  "code": "no_agent_for_project", "message": "Nessun agente online per il progetto 1234." }
```
Codici: `unauthorized`, `bad_hello`, `no_agent_for_project`, `project_not_declared`,
`unknown_session`, `blob_not_found`, `claude_failed`, `internal`.

## 6. Blob (HTTP, non WS)

- `POST /blobs` — body = zip binario, header `Authorization: Bearer <orch_token>`,
  header opzionale `X-Ticket-Id`. Risposta: `{ "blob_id": "9f1c..." }`.
- `GET /blobs/{blob_id}` — header `Authorization: Bearer <token>` (agente o orch).
  Risposta: lo zip (`application/zip`). 404 se assente/scaduto.

I blob hanno id `uuid4`, un TTL e vengono rimossi dopo `task.done`.

## 7. Note di instradamento

- Una **sessione** lega esattamente un orchestratore e un agente. I frame `chat.*`
  e `task.*` portano `session_id`; il gateway li inoltra al capo opposto.
- I frame di discovery (`resolve_project`, `list_agents`) portano `req_id` e la
  risposta lo ripete, così l'orchestratrice può correlare richieste concorrenti.
