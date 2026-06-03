# Gateway (Python)

Centralino di orchestrazione: WebSocket `/ws` per orchestratrice e agenti, blob
store HTTP per gli zip dei ticket, registry in RAM e routing delle sessioni.

## Requisiti
- Python ≥ 3.10
- `pip` o [uv](https://docs.astral.sh/uv/)

## Avvio in locale (dev)
```bash
cd gateway
python -m venv .venv && . .venv/bin/activate
pip install -e .
# Senza token configurati, in dev accetta qualsiasi orchestratrice; per gli
# agenti usa un token condiviso:
export GW_SHARED_AGENT_TOKEN=dev-token
export GW_BLOB_DIR=/tmp/soltea-blobs
python -m soltea_gateway
# -> http://127.0.0.1:8182/healthz
```

## Configurazione
Tutte le variabili in [`.env.example`](.env.example). Le principali:

| Variabile | Significato |
|---|---|
| `GW_HOST` / `GW_PORT` | bind (default `127.0.0.1:8182`, dietro nginx) |
| `GW_ORCH_TOKEN` | token dell'orchestratrice (Claudia) |
| `GW_AGENT_TOKENS` | `agent_id:token,...` per gli agenti (statici, gestiti via env) |
| `GW_SHARED_AGENT_TOKEN` | token unico condiviso (solo dev) |
| `GW_AGENT_TOKENS_FILE` | file JSON dei token creati a runtime via `/provision` |
| `GW_BLOB_DIR` | cartella dei blob (zip ticket) |
| `GW_BLOB_TTL_SECONDS` | TTL dei blob |

## Endpoint
- `GET  /healthz` — stato + numero agenti online.
- `GET  /agents` — elenco agenti (richiede token orchestratrice).
- `POST /provision` — crea/ruota `agent_id`+token (token orchestratrice) → `{agent_id, token}`.
  Body `{"agent_id": "...", "rotate": false}`; senza `agent_id` ne genera uno. Il token
  viene restituito **una sola volta** e persistito in `GW_AGENT_TOKENS_FILE`. Gli id
  definiti in `GW_AGENT_TOKENS` (statici) non si possono provisionare/revocare via API → 409.
- `POST /revoke` — revoca un `agent_id` provisionato a runtime (token orchestratrice) → `{revoked}`.
- `POST /blobs` — upload zip ticket (token orchestratrice) → `{blob_id}`.
- `GET  /blobs/{id}` — download zip (token orchestratrice o agente).
- `WS   /ws` — canale di orchestrazione (vedi [`../docs/PROTOCOL.md`](../docs/PROTOCOL.md)).

## Produzione
1. `deploy/soltea-gateway.service` → `/etc/systemd/system/`, segreti in
   `/etc/soltea-gateway/gateway.env` (chmod 600).
2. `deploy/nginx-soltea-agents.conf` → location `/agents/` nel server 443
   esistente (ricordarsi la `map $http_upgrade $connection_upgrade` in `http{}`).
3. `systemctl enable --now soltea-gateway` e reload nginx.

## Test
```bash
pip install -e '.[dev]'
pytest
```
