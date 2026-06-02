# DESIGN — soltea-agent-gateway

> Documento di architettura. Versione iniziale (scaffold). Autrice: Claudia Soltea.

## 1. Obiettivo

Permettere a **Claudia** (orchestratrice AI) di risolvere i ticket di Project-Open
delegando il lavoro di codice a **agenti Claude Code** che girano su ambienti di
sviluppo remoti, uno o più per progetto.

Flusso a regime:

1. Claudia riceve un ticket da ]po[ e ne ricava il `project_id`.
2. Chiede al gateway **quale agente presidia quel progetto** ed è online.
3. Prepara uno **zip del ticket** (descrizione, criteri di accettazione, allegati)
   e lo carica sul gateway.
4. Apre una **sessione** verso l'agente remoto, gli passa lo zip + le istruzioni.
5. **Chatta** con l'agente (turn-based) finché il ticket è risolto.
6. Aggiorna stato/ore del ticket su ]po[.

## 2. Vincoli (decisi con Marcello, 2026-06-01)

- **Tutti dietro NAT**, gateway compreso come unico host pubblico. → Orchestratrice
  e agenti si connettono **in uscita** al gateway; nessuna porta in entrata sulle VM.
- **Registro in RAM**, niente DB. Gli agenti **auto-dichiarano** i propri progetti
  alla registrazione; il gateway possiede il registro vivo (ricostruito ad ogni
  riconnessione).
- **Gateway**: componente separato, gira sulla macchina dell'MCP ]po[
  (`projectopen.soltea.it`, Ubuntu) **dietro nginx sul 443**.
- **Agent-runner**: VM **Windows** senza Python → **singolo `.exe` autocontenuto**
  installabile come **servizio Windows**. Scelto **Go**.
- Su Windows `claude.exe` è già nel PATH (oggi avviato via Git-bash, vedi
  `StartClaude.bat` del repo `project-open-mcp`).

## 3. Topologia

```
                          Internet (443, TLS)
                                  │
                          ┌───────▼────────┐
                          │     nginx      │  projectopen.soltea.it
                          │  /mcp   → MCP ]po[ (8181)
                          │  /agents→ gateway (8182)   [WSS + HTTP blob]
                          └───────┬────────┘
                                  │ (loopback)
                        ┌─────────▼─────────┐
                        │   GATEWAY (Py)    │
                        │  registry (RAM)   │
                        │  session routing  │
                        │  blob store (fs)  │
                        └──▲─────────────▲──┘
           WSS (out)       │             │      WSS (out)
        ┌──────────────────┘             └───────────────────┐
   ┌────┴─────┐                                         ┌─────┴──────┐
   │ Claudia  │ (orchestrator)                          │ agent-runner│ (Go, VM Win)
   │          │                                         │  claude.exe │
   └──────────┘                                         │  repo proj  │
                                                        └─────────────┘
```

Una sola macchina pubblica; tutto il resto dial-out. nginx fa da terminatore TLS
e inoltra il WebSocket (upgrade) verso il gateway in loopback.

## 4. Componenti

### 4.1 Gateway (Python, FastAPI + uvicorn)

Responsabilità:

- **Registry** (`registry.py`): mappa `agent_id → {websocket, progetti, heartbeat}`
  e l'indice inverso `project_id → agent_id`. Tutto in RAM; si ripopola alle
  registrazioni. Heartbeat per marcare online/offline.
- **Hub / routing** (`hub.py`): gestisce le **sessioni** (`session_id ↔ orchestratore
  ↔ agente ↔ ticket/progetto`) e fa da relay dei frame di chat tra i due capi.
- **Blob store** (`blobs.py`): endpoint HTTP per **upload/download dello zip** del
  ticket. L'orchestratrice fa `POST /blobs` e ottiene un `blob_id`; l'agente fa
  `GET /blobs/{id}`. (Lo zip su WS è scomodo: si passa solo l'handle nel frame.)
- **Server** (`server.py`): app FastAPI con l'endpoint WS `/ws`, gli endpoint blob,
  `/agents` (debug: chi è online) e `/healthz`.

Perché FastAPI/uvicorn: stesso runtime già usato dal deploy dell'MCP, WS + HTTP
nello stesso processo, asyncio.

### 4.2 Agent-runner (Go, servizio Windows)

Responsabilità:

- **config** (`internal/config`): legge `config.json` (vedi §7) con la mappa
  `project_id → cartella`, le credenziali e i parametri di `claude`.
- **wsclient** (`internal/wsclient`): connessione WSS al gateway con **auto-reconnect**
  (backoff) e **heartbeat**; serializza gli invii.
- **claude** (`internal/claude`): lancia `claude.exe` in modalità non interattiva
  (`-p --output-format json`) nella cartella del progetto, mantenendo la
  **sessione** (`--resume <session_id>`) per la continuità della chat.
- **agent** (`internal/agent`): orchestra il tutto — riceve `task.start`/`chat.send`
  dal gateway, scarica/scompatta lo zip, invoca claude, ristreamma i risultati.
- **svc** (`internal/svc`): wrapper servizio Windows (install/uninstall/start/stop),
  così l'`.exe` si registra e gira come servizio.

Singolo binario statico, zero runtime da installare. Cross-compilato da Linux con
`GOOS=windows GOARCH=amd64`.

## 5. Flusso end-to-end (happy path)

```
Claudia                     Gateway                         agent-runner (VM)         claude.exe
   │  hello(orchestrator) ───▶│                                  │                        │
   │                          │◀── hello(agent, projects[]) ─────│ (al boot)              │
   │  resolve_project(pid) ──▶│                                  │                        │
   │  ◀── agent_id, online ───│                                  │                        │
   │  POST /blobs (ticket.zip)│                                  │                        │
   │  ◀── blob_id ────────────│                                  │                        │
   │  task.start{pid,ticket,  │                                  │                        │
   │     blob_id,instructions}│── task.start ──────────────────▶ │                        │
   │                          │                                  │ GET /blobs/{id}        │
   │                          │                                  │ unzip → workdir        │
   │                          │                                  │ claude -p (1° turno) ─▶│
   │                          │                                  │ ◀── result, sid ───────│
   │  ◀── task.started{sess,  │◀── task.started{sess, claude_sid}│                        │
   │       claude_sid} ───────│                                  │                        │
   │  ◀── chat.result(text) ──│◀── chat.result(text) ────────────│                        │
   │  chat.send("rivedi X")──▶│── chat.send ───────────────────▶ │ claude -p --resume ───▶│
   │                          │                                  │ ◀── result ────────────│
   │  ◀── chat.result ────────│◀── chat.result ──────────────────│                        │
   │  ... (fino a risoluzione)│                                  │                        │
   │  task.done ─────────────▶│── task.done ───────────────────▶ │ (cleanup workdir)      │
```

Turn-based nella v1 (un giro richiesta/risposta con `--resume`). Il full-duplex
streaming (`--input-format/--output-format stream-json`) è un'evoluzione successiva.

## 6. Sicurezza

- **TLS** terminato da nginx (acme.sh già presente sull'host).
- **Auth gateway↔agente**: token per-agente nel frame `hello` (bearer). Token noti
  al gateway via config/env. Separati dalle credenziali ]po[ pass-through dell'MCP.
- **Auth orchestratrice**: token dedicato per Claudia.
- **Allow-list progetti**: un agente può operare **solo** sui progetti che dichiara;
  il gateway rifiuta `task.start` per progetti non presidiati da quell'agente.
- **Blob**: id non indovinabili (uuid4), scaricabili solo con token valido, TTL e
  pulizia dopo `task.done`.

## 7. `config.json` dell'agent-runner (esempio)

```json
{
  "gateway_url": "wss://projectopen.soltea.it/agents/ws",
  "agent_id": "win-dev-01",
  "token": "REPLACE_WITH_AGENT_TOKEN",
  "claude_path": "claude.exe",
  "use_git_bash": false,
  "git_bash_path": "C:\\Program Files\\Git\\bin\\bash.exe",
  "default_model": "",
  "permission_mode": "acceptEdits",
  "heartbeat_seconds": 30,
  "projects": [
    { "project_id": 1234, "name": "Sito Cliente X", "path": "C:\\dev\\cliente-x" },
    { "project_id": 1240, "name": "Gestionale Y",  "path": "C:\\dev\\gestionale-y" }
  ]
}
```

## 8. Deployment

### Gateway (Ubuntu)
- Servizio **systemd** (`gateway/deploy/soltea-gateway.service`), in ascolto su
  `127.0.0.1:8182`.
- **nginx** (`gateway/deploy/nginx-soltea-agents.conf`): location `/agents/` con
  upgrade WebSocket e `proxy_read_timeout` lungo per le connessioni persistenti.
- Variabili d'ambiente per token e percorso blob (vedi `gateway/README.md`).

### Agent-runner (Windows)
- Cross-build: `GOOS=windows GOARCH=amd64 go build -o agent-runner.exe ./cmd/...`
  (vedi `agent-runner/scripts/build.sh`).
- Copia `agent-runner.exe` + `config.json` sulla VM, poi
  `agent-runner.exe install && agent-runner.exe start`.

## 9. Scelte tecniche e trade-off

| Tema | Scelta | Perché |
|---|---|---|
| Trasporto | WSS dial-out da tutti | Tutti dietro NAT; un solo host pubblico |
| Runner | Go, singolo exe | No Python su Windows; servizio nativo; statico |
| Gateway | Python FastAPI | Stesso stack dell'MCP, WS+HTTP insieme |
| Stato | RAM, no DB | Richiesto; registro ricostruito a runtime |
| Chat | Turn-based + `--resume` | Semplice e robusto; streaming come evoluzione |
| File ticket | Blob HTTP + handle | WS inadatto a binari grandi |

## 10. TODO(live) — da validare sull'ambiente reale

- Integrazione effettiva con `claude.exe` su Windows (quoting, env, Git-bash).
- Config nginx in produzione + regola iptables (mai flushare la chain: si perde SSH).
- Installazione/avvio come servizio Windows su VM reale.
- Rotazione token e gestione segreti.
- Backpressure/limiti dimensione blob; TTL e GC dei blob.
- Eventuale streaming full-duplex.
