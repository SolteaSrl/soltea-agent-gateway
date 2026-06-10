"""App FastAPI del gateway: endpoint WS /ws, blob HTTP, discovery, health.

Sia l'orchestratrice sia gli agent-runner si connettono a /ws e si identificano nel
primo frame `hello`. L'hub instrada i frame chat.*/task.* fra i due capi di ogni
sessione; il registry risolve "quale agente presidia un progetto".
"""
from __future__ import annotations

import asyncio
import contextlib
import logging
import secrets
from typing import Any

from fastapi import FastAPI, Header, Request, Response, WebSocket, WebSocketDisconnect
from fastapi.responses import JSONResponse

from . import protocol as P
from .blobs import BlobStore
from .config import Config
from .connection import Connection
from .hub import Hub
from .registry import Registry
from .tokenstore import AgentTokenStore

log = logging.getLogger("soltea_gateway")


def _bearer(authorization: str | None) -> str:
    if not authorization:
        return ""
    parts = authorization.split(None, 1)
    if len(parts) == 2 and parts[0].lower() == "bearer":
        return parts[1].strip()
    return authorization.strip()


def create_app(config: Config | None = None) -> FastAPI:
    cfg = config or Config.from_env()
    registry = Registry()
    hub = Hub(
        registry,
        orphan_grace_seconds=cfg.orphan_grace_seconds,
        orphan_buffer_max_frames=cfg.orphan_buffer_max_frames,
    )
    blobs = BlobStore(cfg.blob_dir, cfg.blob_ttl_seconds)

    # Token statici (da env) vs token creati a runtime (persistiti su file).
    # Gli id definiti via env sono "di sistema": non si provisionano/revocano via API.
    env_agent_ids = set(cfg.agent_tokens)
    token_store = AgentTokenStore(cfg.agent_tokens_file)
    for aid, tok in token_store.tokens().items():
        if aid not in env_agent_ids:  # l'env vince sui conflitti
            cfg.agent_tokens[aid] = tok

    @contextlib.asynccontextmanager
    async def lifespan(app: FastAPI):
        gc_task = asyncio.create_task(_blob_gc_loop(blobs, cfg.blob_ttl_seconds))
        orphan_task = asyncio.create_task(_orphan_expire_loop(hub))
        log.info("Gateway avviato su %s:%s", cfg.host, cfg.port)
        try:
            yield
        finally:
            for t in (gc_task, orphan_task):
                t.cancel()
                with contextlib.suppress(asyncio.CancelledError):
                    await t

    app = FastAPI(title="soltea-agent-gateway", version="0.1.0", lifespan=lifespan)
    # Riferimenti utili ai test.
    app.state.config = cfg
    app.state.registry = registry
    app.state.hub = hub
    app.state.blobs = blobs

    # ---------------------------------------------------------------- HTTP --

    @app.get("/healthz")
    async def healthz() -> dict[str, Any]:
        return {"status": "ok", "agents": len(registry.snapshot())}

    @app.get("/agents")
    async def list_agents_http(authorization: str | None = Header(default=None)) -> Response:
        if not cfg.check_orchestrator_token(_bearer(authorization)):
            return JSONResponse({"error": "unauthorized"}, status_code=401)
        return JSONResponse({"agents": registry.snapshot()})

    @app.get("/runner/latest")
    async def runner_latest() -> Response:
        """Versione runner consigliata + URL e SHA256 dell'asset.

        No auth: l'asset URL e' tipicamente una release GitHub pubblica e
        il launcher Windows polla da qualunque rete. Niente segreti qui.
        Risponde 404 se l'admin non ha configurato GW_RUNNER_LATEST_*.
        """
        if not cfg.runner_latest_version or not cfg.runner_latest_url:
            return JSONResponse({"error": "not_configured"}, status_code=404)
        return JSONResponse({
            "version": cfg.runner_latest_version,
            "asset_url": cfg.runner_latest_url,
            "sha256": cfg.runner_latest_sha256,
        })

    @app.post("/provision")
    async def provision_agent(
        request: Request,
        authorization: str | None = Header(default=None),
    ) -> Response:
        """Crea (o ruota) la coppia agent_id/token. Solo orchestratrice.

        Body JSON: {"agent_id": "runner-...", "rotate": false}.
        Se agent_id manca, ne genera uno. Il token e' restituito UNA volta sola.
        """
        if not cfg.check_orchestrator_token(_bearer(authorization)):
            return JSONResponse({"error": "unauthorized"}, status_code=401)
        try:
            body = await request.json() if await request.body() else {}
        except ValueError:
            return JSONResponse({"error": "bad_json"}, status_code=400)
        agent_id = (body.get("agent_id") or "").strip() or f"runner-{secrets.token_hex(4)}"
        rotate = bool(body.get("rotate", False))
        if agent_id in env_agent_ids:
            return JSONResponse(
                {"error": "managed_by_env", "agent_id": agent_id,
                 "message": "agent_id definito staticamente in GW_AGENT_TOKENS; gestiscilo lì."},
                status_code=409,
            )
        if token_store.has(agent_id) and not rotate:
            return JSONResponse(
                {"error": "already_exists", "agent_id": agent_id,
                 "message": "agent_id già provisionato; usa rotate=true per rigenerare il token."},
                status_code=409,
            )
        token = token_store.provision(agent_id, rotate=True)
        cfg.agent_tokens[agent_id] = token
        log.info("Provision agente: %s (rotate=%s)", agent_id, rotate)
        return JSONResponse({"agent_id": agent_id, "token": token, "rotated": rotate})

    @app.post("/revoke")
    async def revoke_agent(
        request: Request,
        authorization: str | None = Header(default=None),
    ) -> Response:
        """Revoca un agent_id provisionato a runtime. Solo orchestratrice."""
        if not cfg.check_orchestrator_token(_bearer(authorization)):
            return JSONResponse({"error": "unauthorized"}, status_code=401)
        try:
            body = await request.json() if await request.body() else {}
        except ValueError:
            return JSONResponse({"error": "bad_json"}, status_code=400)
        agent_id = (body.get("agent_id") or "").strip()
        if not agent_id:
            return JSONResponse({"error": "missing_agent_id"}, status_code=400)
        if agent_id in env_agent_ids:
            return JSONResponse(
                {"error": "managed_by_env", "agent_id": agent_id,
                 "message": "agent_id definito staticamente in GW_AGENT_TOKENS; gestiscilo lì."},
                status_code=409,
            )
        removed = token_store.revoke(agent_id)
        cfg.agent_tokens.pop(agent_id, None)
        log.info("Revoke agente: %s (esisteva=%s)", agent_id, removed)
        return JSONResponse({"agent_id": agent_id, "revoked": removed})

    @app.post("/blobs")
    async def upload_blob(
        request: Request,
        authorization: str | None = Header(default=None),
    ) -> Response:
        if not cfg.check_orchestrator_token(_bearer(authorization)):
            return JSONResponse({"error": "unauthorized"}, status_code=401)
        data = await request.body()
        if len(data) > cfg.blob_max_bytes:
            return JSONResponse({"error": "blob_too_large"}, status_code=413)
        blob_id = blobs.put(data)
        return JSONResponse({"blob_id": blob_id})

    @app.get("/blobs/{blob_id}")
    async def download_blob(
        blob_id: str,
        authorization: str | None = Header(default=None),
    ) -> Response:
        token = _bearer(authorization)
        # Accettano sia l'orchestratrice sia un agente (token condiviso/registrato).
        ok = cfg.check_orchestrator_token(token) or _any_agent_token(cfg, token)
        if not ok:
            return JSONResponse({"error": "unauthorized"}, status_code=401)
        data = blobs.get(blob_id)
        if data is None:
            return JSONResponse({"error": P.ERR_BLOB_NOT_FOUND}, status_code=404)
        return Response(content=data, media_type="application/zip")

    # ----------------------------------------------------------- WebSocket --

    @app.websocket("/ws")
    async def ws_endpoint(ws: WebSocket) -> None:
        await ws.accept()
        conn: Connection | None = None
        try:
            hello = await ws.receive_json()
            conn = await _handle_hello(ws, hello, cfg, registry)
            if conn is None:
                await ws.close()
                return
            await _serve(conn, cfg, registry, hub, blobs)
        except WebSocketDisconnect:
            pass
        except Exception:  # pragma: no cover - difensivo
            log.exception("Errore non gestito nella WS")
        finally:
            if conn is not None:
                await _cleanup(conn, registry, hub)

    return app


def _any_agent_token(cfg: Config, token: str) -> bool:
    if cfg.shared_agent_token and token == cfg.shared_agent_token:
        return True
    return token in set(cfg.agent_tokens.values()) if token else False


async def _handle_hello(
    ws: WebSocket, hello: dict, cfg: Config, registry: Registry
) -> Connection | None:
    if hello.get("type") != P.HELLO:
        await ws.send_json(P.error(P.ERR_BAD_HELLO, "Primo frame deve essere 'hello'."))
        return None
    role = hello.get("role")
    token = hello.get("token", "")

    # Versione protocollo: il runner la dichiara, l'orchestratrice e' interna
    # quindi non la richiediamo. Se assente assumiamo 1 (compat con v0.4.x).
    peer_proto = hello.get("protocol_version", P.PROTOCOL_VERSION)
    try:
        peer_proto = int(peer_proto)
    except (TypeError, ValueError):
        await ws.send_json(P.error(P.ERR_BAD_HELLO, "protocol_version deve essere intero."))
        return None
    if role == P.ROLE_AGENT and peer_proto < P.MIN_RUNNER_PROTOCOL_VERSION:
        await ws.send_json(
            P.error(
                P.ERR_PROTOCOL_INCOMPATIBLE,
                f"Runner protocol_version={peer_proto} < minimo richiesto "
                f"{P.MIN_RUNNER_PROTOCOL_VERSION}. Aggiorna il runner.",
                gateway_protocol_version=P.PROTOCOL_VERSION,
            )
        )
        return None

    if role == P.ROLE_AGENT:
        agent_id = hello.get("agent_id")
        if not agent_id:
            await ws.send_json(P.error(P.ERR_BAD_HELLO, "agent_id mancante."))
            return None
        if not cfg.check_agent_token(agent_id, token):
            await ws.send_json(P.error(P.ERR_UNAUTHORIZED, "Token agente non valido."))
            return None
        conn = Connection(ws, P.ROLE_AGENT, agent_id)
        projects = hello.get("projects", []) or []
        runner_version = hello.get("runner_version", "")
        entry = registry.register(agent_id, conn, projects, runner_version)
        await conn.send(
            {
                "type": P.WELCOME,
                "session_scope": "agent",
                "agent_id": agent_id,
                "registered_projects": sorted(entry.projects.keys()),
                "heartbeat_seconds": cfg.heartbeat_seconds,
                "protocol_version": P.PROTOCOL_VERSION,
            }
        )
        log.info(
            "Agente registrato: %s progetti=%s runner=%s proto=%d",
            agent_id, sorted(entry.projects.keys()), entry.runner_version or "?", peer_proto,
        )
        return conn

    if role == P.ROLE_ORCHESTRATOR:
        if not cfg.check_orchestrator_token(token):
            await ws.send_json(P.error(P.ERR_UNAUTHORIZED, "Token orchestratrice non valido."))
            return None
        client_id = hello.get("client_id", "orchestrator")
        conn = Connection(ws, P.ROLE_ORCHESTRATOR, client_id)
        await conn.send(
            {"type": P.WELCOME, "session_scope": "orchestrator", "client_id": client_id,
             "heartbeat_seconds": cfg.heartbeat_seconds,
             "protocol_version": P.PROTOCOL_VERSION}
        )
        log.info("Orchestratrice connessa: %s", client_id)
        return conn

    await ws.send_json(P.error(P.ERR_BAD_HELLO, f"role sconosciuto: {role!r}"))
    return None


async def _serve(
    conn: Connection, cfg: Config, registry: Registry, hub: Hub, blobs: BlobStore
) -> None:
    while True:
        frame = await conn.ws.receive_json()
        conn.touch()
        ftype = frame.get("type")

        if ftype == P.PING:
            await conn.send({"type": P.PONG, "ts": frame.get("ts")})
            continue
        if ftype == P.PONG:
            continue

        if conn.role == P.ROLE_ORCHESTRATOR:
            await _handle_orchestrator_frame(frame, conn, cfg, registry, hub)
        else:
            await _handle_agent_frame(frame, conn, hub, blobs)


async def _handle_orchestrator_frame(
    frame: dict, conn: Connection, cfg: Config, registry: Registry, hub: Hub
) -> None:
    ftype = frame.get("type")

    if ftype == P.RESOLVE_PROJECT:
        pid = int(frame["project_id"])
        entry = registry.agent_for_project(pid)
        await conn.send(
            {
                "type": P.PROJECT_RESOLVED,
                "req_id": frame.get("req_id"),
                "project_id": pid,
                "agent_id": entry.agent_id if entry else None,
                "online": entry is not None,
            }
        )
        return

    if ftype == P.LIST_AGENTS:
        await conn.send({"type": P.AGENTS, "req_id": frame.get("req_id"), "agents": registry.snapshot()})
        return

    if ftype == P.TASK_START:
        pid = int(frame["project_id"])
        entry = registry.agent_for_project(pid)
        if entry is None:
            await conn.send(P.error(P.ERR_NO_AGENT, f"Nessun agente online per il progetto {pid}.",
                                    req_id=frame.get("req_id"), project_id=pid))
            return
        sess = hub.create_session(conn, entry.agent_id, pid, frame.get("ticket_id"))
        # Inoltra all'agente, aggiungendo il session_id assegnato.
        await entry.conn.send(
            {
                "type": P.TASK_START,
                "session_id": sess.session_id,
                "project_id": pid,
                "ticket_id": frame.get("ticket_id"),
                "blob_id": frame.get("blob_id"),
                "instructions": frame.get("instructions", ""),
            }
        )
        # Confermiamo all'orchestratrice il session_id + versione del runner
        # che ha preso in carico la sessione (utile per diagnostica e per il
        # CLI orchestratrice senza dover incrociare list_agents).
        await conn.send({"type": "session_opened", "req_id": frame.get("req_id"),
                         "session_id": sess.session_id, "agent_id": entry.agent_id,
                         "runner_version": entry.runner_version or None})
        return

    if ftype in (P.CHAT_SEND, P.TASK_DONE):
        sid = frame.get("session_id")
        sess = hub.get(sid) if sid else None
        if sess is None or sess.orchestrator is not conn:
            await conn.send(P.error(P.ERR_UNKNOWN_SESSION, f"Sessione sconosciuta: {sid}", session_id=sid))
            return
        agent = registry.get(sess.agent_id)
        if agent is None:
            await conn.send(P.error(P.ERR_NO_AGENT, "Agente non piu' online.", session_id=sid))
            hub.close_session(sid)
            return
        await agent.conn.send(frame)
        if ftype == P.TASK_DONE:
            hub.close_session(sid)
        return

    if ftype == P.SESSION_ATTACH:
        sid = frame.get("session_id", "")
        sess, err, buffered = hub.attach_orchestrator(conn, sid)
        if err is not None:
            code = {
                "unknown_session": P.ERR_UNKNOWN_SESSION,
                "session_already_attached": P.ERR_SESSION_ALREADY_ATTACHED,
            }[err]
            await conn.send(P.error(code, f"Riattacco fallito: {err}",
                                    req_id=frame.get("req_id"), session_id=sid))
            return
        assert sess is not None
        # Conferma del riattacco + flush dei frame bufferizzati durante l'orfanaggio.
        await conn.send({
            "type": P.SESSION_ATTACHED,
            "req_id": frame.get("req_id"),
            "session_id": sess.session_id,
            "agent_id": sess.agent_id,
            "buffered_frames": len(buffered),
        })
        for buf_frame in buffered:
            await conn.send(buf_frame)
        log.info("Riattacco sessione %s da %s (%d frame in coda)", sid, conn.peer_id, len(buffered))
        return

    await conn.send(P.error(P.ERR_INTERNAL, f"Frame non gestito dall'orchestratrice: {ftype!r}"))


async def _handle_agent_frame(frame: dict, conn: Connection, hub: Hub, blobs: BlobStore) -> None:
    ftype = frame.get("type")
    # Tutti i frame dell'agente verso l'orchestratrice portano session_id.
    if ftype in (P.TASK_STARTED, P.CHAT_DELTA, P.CHAT_RESULT, P.ERROR):
        sid = frame.get("session_id")
        sess = hub.get(sid) if sid else None
        if sess is None:
            log.debug("Frame agente per sessione ignota %s", sid)
            return
        if sess.orchestrator is None:
            # Sessione orfana: bufferizza per il riattacco. Se il buffer scoppia,
            # chiudiamo la sessione e notifichiamo l'agente.
            if not hub.buffer_agent_frame(sess, frame):
                hub.close_session(sess.session_id)
                with contextlib.suppress(Exception):
                    await conn.send({
                        "type": P.SESSION_LOST, "session_id": sess.session_id,
                        "reason": P.ERR_SESSION_BUFFER_OVERFLOW,
                    })
                log.warning("Sessione %s chiusa per buffer overflow (>%d frame).",
                            sess.session_id, hub.orphan_buffer_max_frames)
            return
        await sess.orchestrator.send(frame)
        return
    log.debug("Frame agente non instradabile: %s", ftype)


async def _cleanup(conn: Connection, registry: Registry, hub: Hub) -> None:
    if conn.role == P.ROLE_AGENT:
        affected = hub.drop_agent(conn)
        registry.unregister(conn.peer_id)
        for sess in affected:
            # Sessione chiusa per caduta agente: l'orch (se presente) viene
            # avvisata. Se la sessione era gia' orfana, l'orch non c'e' e nessuno
            # ricevera' l'errore.
            if sess.orchestrator is not None:
                with contextlib.suppress(Exception):
                    await sess.orchestrator.send(
                        P.error(P.ERR_NO_AGENT, "Agente disconnesso.", session_id=sess.session_id)
                    )
        log.info("Agente disconnesso: %s (sessioni chiuse: %d)", conn, len(affected))
        return

    # Orchestratrice: marca le sessioni come orfane (saranno chiuse al timeout).
    orphaned = hub.orphan_orchestrator(conn)
    if hub.orphan_grace_seconds > 0 and orphaned:
        log.info("Orchestratrice disconnessa: %s (sessioni orfane: %d, grace=%ds)",
                 conn, len(orphaned), hub.orphan_grace_seconds)
    else:
        log.info("Orchestratrice disconnessa: %s (sessioni chiuse: %d)", conn, len(orphaned))


async def _blob_gc_loop(blobs: BlobStore, ttl: int) -> None:
    interval = max(60, ttl // 4)
    while True:
        await asyncio.sleep(interval)
        with contextlib.suppress(Exception):
            removed = blobs.gc()
            if removed:
                log.info("Blob GC: rimossi %d", removed)


async def _orphan_expire_loop(hub: Hub) -> None:
    """Loop periodico che chiude le sessioni orfane oltre la finestra di grace.

    Per ogni sessione scaduta notifica l'agente con `session_lost` (best-effort:
    se l'agente e' anche lui giu' nel frattempo il send fallisce silenziosamente).
    """
    if hub.orphan_grace_seconds <= 0:
        return
    # Tick al massimo ogni 5 secondi, mai meno di un quarto della finestra.
    interval = max(1, min(5, hub.orphan_grace_seconds // 4 or 1))
    while True:
        await asyncio.sleep(interval)
        with contextlib.suppress(Exception):
            expired = hub.expire_orphans()
            if not expired:
                continue
            for sess in expired:
                agent_entry = hub.registry.get(sess.agent_id)
                if agent_entry is None:
                    continue
                with contextlib.suppress(Exception):
                    await agent_entry.conn.send({
                        "type": P.SESSION_LOST, "session_id": sess.session_id,
                        "reason": P.ERR_SESSION_ORPHAN_EXPIRED,
                    })
            log.info("Sessioni orfane scadute: %d", len(expired))
