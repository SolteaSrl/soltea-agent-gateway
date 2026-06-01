# soltea-agent-gateway

Gateway di orchestrazione che mette in comunicazione **Claudia** (l'orchestratrice
AI di Soltea) con una flotta di **agenti Claude Code** che girano su ambienti di
sviluppo remoti (VM Windows), uno per progetto.

Tutti i nodi stanno dietro NAT: il gateway è l'**unico punto pubblico** e fa da
centralino. Sia l'orchestratrice sia gli agent-runner aprono una connessione
**WebSocket in uscita** verso il gateway, che instrada i messaggi e fa da tramite
per i file (lo zip del ticket).

```
 Claudia ──WSS──▶  GATEWAY (pubblico, dietro nginx/443)  ◀──WSS── agent-runner (VM Windows) ─▶ claude.exe ─▶ repo progetto
                   • registry agente↔progetto (in RAM)
                   • routing delle sessioni
                   • blob store per gli zip dei ticket
```

## Componenti

| Componente | Linguaggio | Dove gira | Cartella |
|---|---|---|---|
| Gateway | Python (FastAPI + uvicorn) | Ubuntu, accanto all'MCP ]po[, dietro nginx sul 443 | [`gateway/`](gateway/) |
| Agent-runner | Go (singolo `.exe`, servizio Windows) | VM Windows di sviluppo | [`agent-runner/`](agent-runner/) |

## Documentazione

- [`DESIGN.md`](DESIGN.md) — architettura, flusso end-to-end, deployment.
- [`docs/PROTOCOL.md`](docs/PROTOCOL.md) — protocollo di rete (frame JSON su WSS).
- [`gateway/README.md`](gateway/README.md) — avvio e configurazione del gateway.
- [`agent-runner/README.md`](agent-runner/README.md) — build, install come servizio Windows.

## Stato

Scaffold iniziale: impianto, protocollo e happy-path implementati. I punti che
richiedono un ambiente live (integrazione reale con `claude.exe` su Windows,
installazione come servizio, TLS/nginx in produzione) sono marcati `TODO(live)`.
