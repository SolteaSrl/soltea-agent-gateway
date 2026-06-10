"""Test end-to-end del gateway con il TestClient di Starlette.

Simula un agente e un'orchestratrice connessi insieme e verifica registry,
risoluzione progetto, blob roundtrip e l'intero giro task.start -> chat -> done.
"""
from __future__ import annotations

from pathlib import Path

import pytest
from fastapi.testclient import TestClient

from soltea_gateway.config import Config
from soltea_gateway.server import create_app


@pytest.fixture()
def client(tmp_path: Path) -> TestClient:
    cfg = Config(
        orchestrator_token="orch-tok",
        shared_agent_token="agent-tok",
        blob_dir=tmp_path / "blobs",
        heartbeat_seconds=30,
    )
    app = create_app(cfg)
    return TestClient(app)


def _hello_agent(ws, agent_id="win-dev-01", project_id=1234, runner_version="0.4.0"):
    ws.send_json({
        "type": "hello", "role": "agent", "token": "agent-tok", "agent_id": agent_id,
        "projects": [{"project_id": project_id, "name": "Proj X", "path": "C:/dev/x"}],
        "runner_version": runner_version,
    })
    return ws.receive_json()


def _hello_orch(ws):
    ws.send_json({"type": "hello", "role": "orchestrator", "token": "orch-tok", "client_id": "claudia"})
    return ws.receive_json()


def test_healthz(client: TestClient):
    assert client.get("/healthz").json()["status"] == "ok"


def test_agent_bad_token_rejected(client: TestClient):
    with client.websocket_connect("/ws") as ws:
        ws.send_json({"type": "hello", "role": "agent", "token": "nope", "agent_id": "x", "projects": []})
        frame = ws.receive_json()
        assert frame["type"] == "error"
        assert frame["code"] == "unauthorized"


def test_blob_roundtrip(client: TestClient):
    r = client.post("/blobs", content=b"PK\x03\x04zip", headers={"Authorization": "Bearer orch-tok"})
    assert r.status_code == 200
    blob_id = r.json()["blob_id"]
    g = client.get(f"/blobs/{blob_id}", headers={"Authorization": "Bearer agent-tok"})
    assert g.status_code == 200
    assert g.content == b"PK\x03\x04zip"


def test_blob_requires_auth(client: TestClient):
    assert client.post("/blobs", content=b"x").status_code == 401


def test_resolve_and_full_task_flow(client: TestClient):
    with client.websocket_connect("/ws") as agent, client.websocket_connect("/ws") as orch:
        wa = _hello_agent(agent)
        assert wa["type"] == "welcome" and wa["registered_projects"] == [1234]
        wo = _hello_orch(orch)
        assert wo["type"] == "welcome"

        # Discovery
        orch.send_json({"type": "resolve_project", "req_id": "r1", "project_id": 1234})
        res = orch.receive_json()
        assert res["type"] == "project_resolved" and res["agent_id"] == "win-dev-01" and res["online"]

        # Upload zip ticket
        blob_id = client.post(
            "/blobs", content=b"PK\x03\x04ticket", headers={"Authorization": "Bearer orch-tok"}
        ).json()["blob_id"]

        # task.start
        orch.send_json({
            "type": "task.start", "req_id": "t1", "project_id": 1234, "ticket_id": 5678,
            "blob_id": blob_id, "instructions": "Risolvi.",
        })
        # L'agente riceve il task con il session_id assegnato
        at = agent.receive_json()
        assert at["type"] == "task.start" and at["ticket_id"] == 5678 and at["blob_id"] == blob_id
        session_id = at["session_id"]
        # L'orchestratrice riceve la conferma di apertura sessione
        so = orch.receive_json()
        assert so["type"] == "session_opened" and so["session_id"] == session_id

        # L'agente avvia claude e ristreamma
        agent.send_json({"type": "task.started", "session_id": session_id, "ticket_id": 5678,
                         "claude_session_id": "cs-1", "workdir": "C:/dev/x/.tickets/5678"})
        assert orch.receive_json()["type"] == "task.started"
        agent.send_json({"type": "chat.result", "session_id": session_id, "text": "Fatto.", "is_error": False})
        r = orch.receive_json()
        assert r["type"] == "chat.result" and r["text"] == "Fatto."

        # Secondo turno
        orch.send_json({"type": "chat.send", "session_id": session_id, "text": "Aggiungi un test."})
        assert agent.receive_json()["text"] == "Aggiungi un test."
        agent.send_json({"type": "chat.result", "session_id": session_id, "text": "Test aggiunto."})
        assert orch.receive_json()["text"] == "Test aggiunto."

        # Chiusura
        orch.send_json({"type": "task.done", "session_id": session_id, "outcome": "resolved"})
        assert agent.receive_json()["type"] == "task.done"


def test_task_start_no_agent(client: TestClient):
    with client.websocket_connect("/ws") as orch:
        _hello_orch(orch)
        orch.send_json({"type": "task.start", "req_id": "t1", "project_id": 9999,
                        "ticket_id": 1, "instructions": ""})
        err = orch.receive_json()
        assert err["type"] == "error" and err["code"] == "no_agent_for_project"


def _prov_app(tmp_path: Path):
    cfg = Config(
        orchestrator_token="orch-tok",
        agent_tokens={"runner-env-01": "env-secret"},
        agent_tokens_file=tmp_path / "agent_tokens.json",
        blob_dir=tmp_path / "blobs",
    )
    return cfg, create_app(cfg)


def test_provision_requires_orch_token(tmp_path: Path):
    _cfg, app = _prov_app(tmp_path)
    client = TestClient(app)
    assert client.post("/provision", json={"agent_id": "runner-x"}).status_code == 401
    assert client.post("/revoke", json={"agent_id": "runner-x"}).status_code == 401


def test_provision_creates_working_token(tmp_path: Path):
    _cfg, app = _prov_app(tmp_path)
    client = TestClient(app)
    r = client.post("/provision", json={"agent_id": "runner-pcmarcello-01"},
                    headers={"Authorization": "Bearer orch-tok"})
    assert r.status_code == 200
    token = r.json()["token"]
    assert r.json()["agent_id"] == "runner-pcmarcello-01" and token

    # Il token appena creato autentica davvero un agente sulla WS.
    with client.websocket_connect("/ws") as ws:
        ws.send_json({"type": "hello", "role": "agent", "token": token,
                      "agent_id": "runner-pcmarcello-01", "projects": []})
        assert ws.receive_json()["type"] == "welcome"


def test_provision_autogenerates_agent_id(tmp_path: Path):
    _cfg, app = _prov_app(tmp_path)
    client = TestClient(app)
    r = client.post("/provision", json={}, headers={"Authorization": "Bearer orch-tok"})
    assert r.status_code == 200 and r.json()["agent_id"].startswith("runner-")


def test_provision_conflict_and_rotate(tmp_path: Path):
    _cfg, app = _prov_app(tmp_path)
    client = TestClient(app)
    h = {"Authorization": "Bearer orch-tok"}
    first = client.post("/provision", json={"agent_id": "runner-dup"}, headers=h).json()["token"]
    # Stesso id senza rotate -> 409
    assert client.post("/provision", json={"agent_id": "runner-dup"}, headers=h).status_code == 409
    # Con rotate -> nuovo token diverso
    rot = client.post("/provision", json={"agent_id": "runner-dup", "rotate": True}, headers=h)
    assert rot.status_code == 200 and rot.json()["token"] != first


def test_provision_blocks_env_id(tmp_path: Path):
    _cfg, app = _prov_app(tmp_path)
    client = TestClient(app)
    h = {"Authorization": "Bearer orch-tok"}
    assert client.post("/provision", json={"agent_id": "runner-env-01"}, headers=h).status_code == 409
    assert client.post("/revoke", json={"agent_id": "runner-env-01"}, headers=h).status_code == 409


def test_provision_persists_across_restart(tmp_path: Path):
    cfg1, app1 = _prov_app(tmp_path)
    token = TestClient(app1).post(
        "/provision", json={"agent_id": "runner-keep"}, headers={"Authorization": "Bearer orch-tok"}
    ).json()["token"]
    # Nuova istanza dell'app (= restart) sullo stesso file: il token resta valido.
    _cfg2, app2 = _prov_app(tmp_path)
    client2 = TestClient(app2)
    with client2.websocket_connect("/ws") as ws:
        ws.send_json({"type": "hello", "role": "agent", "token": token,
                      "agent_id": "runner-keep", "projects": []})
        assert ws.receive_json()["type"] == "welcome"


def test_revoke_invalidates_token(tmp_path: Path):
    _cfg, app = _prov_app(tmp_path)
    client = TestClient(app)
    h = {"Authorization": "Bearer orch-tok"}
    token = client.post("/provision", json={"agent_id": "runner-tmp"}, headers=h).json()["token"]
    r = client.post("/revoke", json={"agent_id": "runner-tmp"}, headers=h)
    assert r.status_code == 200 and r.json()["revoked"] is True
    with client.websocket_connect("/ws") as ws:
        ws.send_json({"type": "hello", "role": "agent", "token": token,
                      "agent_id": "runner-tmp", "projects": []})
        frame = ws.receive_json()
        assert frame["type"] == "error" and frame["code"] == "unauthorized"


def test_runner_latest_404_when_not_configured(client: TestClient):
    """Senza GW_RUNNER_LATEST_* configurati l'endpoint risponde 404 (auto-update off)."""
    r = client.get("/runner/latest")
    assert r.status_code == 404
    assert r.json()["error"] == "not_configured"


def test_runner_latest_returns_metadata_when_configured(tmp_path: Path):
    cfg = Config(
        orchestrator_token="orch-tok",
        shared_agent_token="agent-tok",
        blob_dir=tmp_path / "blobs",
        runner_latest_version="0.6.0",
        runner_latest_url="https://github.com/SolteaSrl/x/releases/download/runner-v0.6.0/agent-runner.exe",
        runner_latest_sha256="455ec667deaf713cc5878d3301a67b10726c883747d857928923e720eb53abf0",
    )
    c = TestClient(create_app(cfg))
    r = c.get("/runner/latest")
    assert r.status_code == 200
    body = r.json()
    assert body["version"] == "0.6.0"
    assert body["asset_url"].endswith("agent-runner.exe")
    assert len(body["sha256"]) == 64


def test_runner_latest_no_auth_required(tmp_path: Path):
    """L'endpoint NON deve richiedere auth: il launcher Windows polla
    da rete non controllata e l'asset URL e' pubblico."""
    cfg = Config(
        orchestrator_token="orch-tok",
        blob_dir=tmp_path / "blobs",
        runner_latest_version="0.6.0",
        runner_latest_url="https://x/y.exe",
        runner_latest_sha256="a" * 64,
    )
    c = TestClient(create_app(cfg))
    # Senza alcun header Authorization → 200 OK.
    assert c.get("/runner/latest").status_code == 200


def test_list_agents_reports_runner_version(client: TestClient):
    with client.websocket_connect("/ws") as agent, client.websocket_connect("/ws") as orch:
        _hello_agent(agent, runner_version="0.4.0")
        _hello_orch(orch)
        orch.send_json({"type": "list_agents", "req_id": "la1"})
        res = orch.receive_json()
        assert res["type"] == "agents"
        assert res["agents"][0]["agent_id"] == "win-dev-01"
        assert res["agents"][0]["runner_version"] == "0.4.0"


def test_welcome_advertises_protocol_version(client: TestClient):
    """Sia agente sia orchestratrice ricevono la versione di protocollo del gateway."""
    from soltea_gateway.protocol import PROTOCOL_VERSION

    with client.websocket_connect("/ws") as agent:
        wa = _hello_agent(agent)
        assert wa["protocol_version"] == PROTOCOL_VERSION

    with client.websocket_connect("/ws") as orch:
        wo = _hello_orch(orch)
        assert wo["protocol_version"] == PROTOCOL_VERSION


def test_hello_without_protocol_version_is_accepted_for_backcompat(client: TestClient):
    """Runner v0.4.x non mandano protocol_version: il gateway deve accettarli."""
    with client.websocket_connect("/ws") as ws:
        # _hello_agent dichiara runner_version ma NON protocol_version.
        wa = _hello_agent(ws)
        assert wa["type"] == "welcome"


def test_runner_with_incompatible_protocol_is_rejected(client: TestClient):
    """Un runner che dichiara protocol_version=0 va rifiutato con codice chiaro."""
    with client.websocket_connect("/ws") as ws:
        ws.send_json({
            "type": "hello", "role": "agent", "token": "agent-tok", "agent_id": "win-dev-01",
            "projects": [], "runner_version": "0.0.0", "protocol_version": 0,
        })
        frame = ws.receive_json()
        assert frame["type"] == "error"
        assert frame["code"] == "protocol_incompatible"
        assert frame["gateway_protocol_version"] >= 1


def test_hello_with_garbage_protocol_version_is_rejected(client: TestClient):
    with client.websocket_connect("/ws") as ws:
        ws.send_json({
            "type": "hello", "role": "agent", "token": "agent-tok", "agent_id": "x",
            "projects": [], "protocol_version": "non-un-numero",
        })
        frame = ws.receive_json()
        assert frame["type"] == "error"
        assert frame["code"] == "bad_hello"


def test_session_opened_includes_runner_version(client: TestClient):
    """All'apertura di sessione, l'orch riceve la versione del runner senza dover
    incrociare list_agents (diagnostica + rate-limit dei round-trip)."""
    with client.websocket_connect("/ws") as agent, client.websocket_connect("/ws") as orch:
        _hello_agent(agent, runner_version="0.5.0-rc1")
        _hello_orch(orch)
        orch.send_json({"type": "task.start", "req_id": "t1", "project_id": 1234,
                        "ticket_id": 7777, "instructions": "..."})
        agent.receive_json()  # il task instradato al runner
        so = orch.receive_json()
        assert so["type"] == "session_opened"
        assert so["runner_version"] == "0.5.0-rc1"


def test_session_opened_runner_version_absent_when_unknown(client: TestClient):
    """Runner che non dichiara la versione -> session_opened ha runner_version=None
    (non stringa vuota: il client distingue "non lo so" da "stringa vuota inviata")."""
    with client.websocket_connect("/ws") as agent, client.websocket_connect("/ws") as orch:
        # hello esplicito SENZA runner_version
        agent.send_json({
            "type": "hello", "role": "agent", "token": "agent-tok", "agent_id": "win-dev-01",
            "projects": [{"project_id": 1234, "name": "Proj X", "path": "C:/dev/x"}],
        })
        assert agent.receive_json()["type"] == "welcome"
        _hello_orch(orch)
        orch.send_json({"type": "task.start", "req_id": "t1", "project_id": 1234,
                        "ticket_id": 7777, "instructions": "..."})
        agent.receive_json()
        so = orch.receive_json()
        assert so["type"] == "session_opened"
        assert so["runner_version"] is None


# ---------------------------------------------------------------- riattacco --


def _reattach_app(tmp_path: Path, grace: int = 60, buf_max: int = 1000):
    cfg = Config(
        orchestrator_token="orch-tok",
        shared_agent_token="agent-tok",
        blob_dir=tmp_path / "blobs",
        heartbeat_seconds=30,
        orphan_grace_seconds=grace,
        orphan_buffer_max_frames=buf_max,
    )
    return cfg, TestClient(create_app(cfg))


def _open_session(client: TestClient, agent, orch, ticket=7777):
    """Helper: agente registrato, orch connessa, sessione aperta, ritorna session_id."""
    _hello_agent(agent)
    _hello_orch(orch)
    orch.send_json({"type": "task.start", "req_id": "t1", "project_id": 1234,
                    "ticket_id": ticket, "instructions": "x"})
    agent.receive_json()  # task.start instradato
    so = orch.receive_json()
    assert so["type"] == "session_opened"
    return so["session_id"]


def test_orphan_session_buffers_agent_frames_then_reattach_flushes(tmp_path: Path):
    """L'orch cade dopo l'apertura sessione, l'agente continua a mandare frame,
    una nuova orch fa session_attach e riceve i frame bufferizzati in ordine."""
    _cfg, client = _reattach_app(tmp_path, grace=60)
    with client.websocket_connect("/ws") as agent:
        # Apriamo orch + sessione, poi chiudiamo l'orch (= caduta).
        with client.websocket_connect("/ws") as orch:
            session_id = _open_session(client, agent, orch)
        # Sessione orfana. Agente manda 3 frame: vanno nel buffer.
        agent.send_json({"type": "task.started", "session_id": session_id, "ticket_id": 7777,
                         "claude_session_id": "cs", "workdir": "/x"})
        agent.send_json({"type": "chat.delta", "session_id": session_id, "seq": 1, "text": "ciao "})
        agent.send_json({"type": "chat.delta", "session_id": session_id, "seq": 2, "text": "marcello"})

        # Nuova orch si riattacca.
        with client.websocket_connect("/ws") as orch2:
            orch2.send_json({"type": "hello", "role": "orchestrator", "token": "orch-tok", "client_id": "claudia-2"})
            assert orch2.receive_json()["type"] == "welcome"
            orch2.send_json({"type": "session_attach", "req_id": "a1", "session_id": session_id})
            ack = orch2.receive_json()
            assert ack["type"] == "session_attached" and ack["session_id"] == session_id
            assert ack["buffered_frames"] == 3
            # Frame consegnati nello stesso ordine.
            assert orch2.receive_json()["type"] == "task.started"
            d1 = orch2.receive_json()
            assert d1["type"] == "chat.delta" and d1["seq"] == 1 and d1["text"] == "ciao "
            d2 = orch2.receive_json()
            assert d2["type"] == "chat.delta" and d2["seq"] == 2

            # Da qui in poi i frame arrivano live, non bufferizzati.
            agent.send_json({"type": "chat.result", "session_id": session_id, "text": "fatto", "is_error": False})
            live = orch2.receive_json()
            assert live["type"] == "chat.result" and live["text"] == "fatto"


def test_attach_unknown_session_returns_error(tmp_path: Path):
    _cfg, client = _reattach_app(tmp_path)
    with client.websocket_connect("/ws") as orch:
        _hello_orch(orch)
        orch.send_json({"type": "session_attach", "req_id": "a1", "session_id": "sess-nope"})
        err = orch.receive_json()
        assert err["type"] == "error" and err["code"] == "unknown_session"


def test_attach_to_already_attached_session_is_rejected(tmp_path: Path):
    """Se la sessione e' viva e l'orch e' connessa, un secondo attach va respinto."""
    _cfg, client = _reattach_app(tmp_path)
    with client.websocket_connect("/ws") as agent, client.websocket_connect("/ws") as orch:
        session_id = _open_session(client, agent, orch)
        with client.websocket_connect("/ws") as orch2:
            orch2.send_json({"type": "hello", "role": "orchestrator", "token": "orch-tok", "client_id": "claudia-2"})
            assert orch2.receive_json()["type"] == "welcome"
            orch2.send_json({"type": "session_attach", "req_id": "a1", "session_id": session_id})
            err = orch2.receive_json()
            assert err["type"] == "error" and err["code"] == "session_already_attached"


def test_orphan_grace_zero_closes_session_immediately(tmp_path: Path):
    """Con orphan_grace_seconds=0 il riattacco e' disabilitato: l'agente riceve
    subito session_lost alla disconnessione dell'orch."""
    _cfg, client = _reattach_app(tmp_path, grace=0)
    with client.websocket_connect("/ws") as agent:
        with client.websocket_connect("/ws") as orch:
            session_id = _open_session(client, agent, orch)
        # Provo a mandare un frame: la sessione non esiste piu' (chiusa) -> niente
        # arrivera' all'orch (gia' caduta), ma anche un riattacco fallisce.
        with client.websocket_connect("/ws") as orch2:
            orch2.send_json({"type": "hello", "role": "orchestrator", "token": "orch-tok", "client_id": "claudia-2"})
            assert orch2.receive_json()["type"] == "welcome"
            orch2.send_json({"type": "session_attach", "req_id": "a1", "session_id": session_id})
            err = orch2.receive_json()
            assert err["type"] == "error" and err["code"] == "unknown_session"


def test_orphan_buffer_overflow_closes_session_and_notifies_agent(tmp_path: Path):
    """Se l'agente bombarda la sessione orfana oltre il buffer max, il gateway
    chiude la sessione e manda session_lost all'agente."""
    _cfg, client = _reattach_app(tmp_path, grace=60, buf_max=3)
    with client.websocket_connect("/ws") as agent:
        with client.websocket_connect("/ws") as orch:
            session_id = _open_session(client, agent, orch)
        # 3 frame entrano nel buffer, il 4o trigger overflow.
        for i in range(3):
            agent.send_json({"type": "chat.delta", "session_id": session_id, "seq": i, "text": "x"})
        agent.send_json({"type": "chat.delta", "session_id": session_id, "seq": 3, "text": "x"})
        notice = agent.receive_json()
        assert notice["type"] == "session_lost"
        assert notice["reason"] == "session_buffer_overflow"


def test_orphan_expire_loop_closes_old_sessions(tmp_path: Path):
    """Il loop di expire chiude le sessioni orfane oltre la finestra. Forziamo
    chiamando expire_orphans() con `now` futuro per evitare di aspettare il tick."""
    cfg, client = _reattach_app(tmp_path, grace=1)
    with client.websocket_connect("/ws") as agent:
        with client.websocket_connect("/ws") as orch:
            session_id = _open_session(client, agent, orch)
        # Sessione ora orfana. Chiamiamo expire passando un now=futuro per
        # simulare la scadenza senza dipendere dal time.monotonic() reale.
        hub = client.app.state.hub
        import time as _t
        expired = hub.expire_orphans(now=_t.monotonic() + 10)
        assert len(expired) == 1 and expired[0].session_id == session_id
        # Agente riceve la notifica via il loop background (con un piccolo grace)
        # ma in test la chiamiamo manualmente: simuliamo emettendo session_lost.
        # Verifichiamo solo che la sessione non esista piu'.
        assert hub.get(session_id) is None


def test_agent_disconnect_closes_orphan_session_too(tmp_path: Path):
    """Se l'agente cade mentre la sessione e' orfana, la sessione deve chiudersi.
    Nessuna orch e' presente per ricevere errori, ma lo stato dell'hub e' pulito."""
    _cfg, client = _reattach_app(tmp_path, grace=60)
    with client.websocket_connect("/ws") as agent:
        with client.websocket_connect("/ws") as orch:
            session_id = _open_session(client, agent, orch)
        # Ora chiudiamo anche l'agente (uscita dal with).
    hub = client.app.state.hub
    assert hub.get(session_id) is None
