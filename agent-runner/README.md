# Agent-runner (Go)

Demone che gira su una VM Windows di sviluppo, si registra al gateway dichiarando
i progetti che presidia, riceve i task, scompatta lo zip del ticket, invoca
`claude.exe` nella cartella del progetto e ristreamma i risultati al gateway.

Singolo `.exe` autocontenuto, installabile come **servizio Windows**. Nessun
runtime da installare (no Python).

## Build (cross-compile da Linux/macOS)
```bash
cd agent-runner
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o dist/agent-runner.exe .
# oppure:
./scripts/build.sh
```

## Configurazione
Copia `config.example.json` in `config.json` (accanto all'exe) e compila:

| Campo | Significato |
|---|---|
| `gateway_url` | `wss://projectopen.soltea.it/agents/ws` |
| `agent_id` | identificativo univoco della VM (es. `win-dev-01`) |
| `token` | token assegnato a questo agente dal gateway |
| `claude_path` | `claude.exe` (se nel PATH) o percorso assoluto |
| `use_git_bash` | `true` per lanciare claude via Git-bash |
| `permission_mode` | `acceptEdits` / `bypassPermissions` / ... |
| `log_dir` | cartella dei log; vuoto = `logs/` accanto al `config.json` |
| `projects[]` | `project_id` → `path` della cartella del repo |

## Installazione come servizio (sulla VM Windows)
```bat
REM da prompt amministratore, nella cartella dell'exe
agent-runner.exe -config C:\soltea\config.json install
agent-runner.exe -config C:\soltea\config.json start
REM stop / uninstall:
agent-runner.exe stop
agent-runner.exe uninstall
```
Il percorso del config viene salvato negli argomenti del servizio, quindi il SCM
lo ritrova anche partendo da `C:\Windows\System32`.

## Debug in primo piano
```bat
agent-runner.exe -config C:\soltea\config.json run
```

## Come invoca claude
Per ogni turno esegue, nella cartella del progetto:
```
claude -p "<prompt>" --output-format json --permission-mode <mode> [--model <m>] [--resume <session>]
```
Il primo turno apre la sessione; i turni successivi usano `--resume` con il
`session_id` restituito da claude, così la chat mantiene il contesto.

## Logging
Tutto finisce su file nella cartella `log_dir` (così resta anche girando come
servizio, dove non c'è console):

- `runner.log` — eventi generali (connessione, registrazione, errori di trasporto).
- `ticket-<id>-<session>.jsonl` — il **transcript completo** della sessione: un
  evento JSON per riga. `kind` può essere `task.start`, `prompt` (cosa mandiamo a
  claude), `claude.raw` (stdout **e** stderr grezzi di claude), `result`,
  `chat.send`, `error`, `task.done`.

Lettura comoda di un transcript:
```bash
# tutto leggibile
cat logs/ticket-69010-*.jsonl | jq .
# solo i testi della chat
cat logs/ticket-69010-*.jsonl | jq -r 'select(.kind=="result" or .kind=="prompt") | .text'
# l'output grezzo di claude quando qualcosa va storto
cat logs/ticket-69010-*.jsonl | jq -r 'select(.kind=="claude.raw") | .stderr'
```

> `TODO(live)`: validare quoting/ambiente di `claude.exe` su Windows reale e
> l'installazione come servizio sulla VM.
