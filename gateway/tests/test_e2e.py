"""Test E2E che usano MockRunner per simulare un agent-runner Go in-process.

I test in `test_gateway.py` verificano singole capability del gateway via WS
manuale; questi qui esercitano *insieme* le feature introdotte negli ultimi PR
(`chat.delta` streaming + riattacco sessione + `runner_version` in
`session_opened` + `protocol_version` handshake) per validare il flusso
completo orchestratrice <-> gateway <-> runner.

Niente VM, niente claude.exe: serve a far girare i casi non banali in CI.
"""
from __future__ import annotations

from pathlib import Path

from fastapi.testclient import TestClient

from soltea_gateway.config import Config
from soltea_gateway.server import create_app

from runner_mock import MockOrchestrator, MockRunner, DEFAULT_PROJECT_ID  # noqa: E402


def _make_client(tmp_path: Path, grace: int = 60, buf_max: int = 1000) -> TestClient:
    cfg = Config(
        orchestrator_token="orch-tok",
        shared_agent_token="agent-tok",
        blob_dir=tmp_path / "blobs",
        heartbeat_seconds=30,
        orphan_grace_seconds=grace,
        orphan_buffer_max_frames=buf_max,
    )
    return TestClient(create_app(cfg))


def test_e2e_streaming_full_task_flow(tmp_path: Path):
    """Path completo: hello, task.start, task.started, N delta, chat.result, task.done.
    Verifica che i delta arrivino in ordine all'orch e che `runner_version` sia
    presente in session_opened (PR1) e in chat.result."""
    client = _make_client(tmp_path)
    with client.websocket_connect("/ws") as agent_ws, client.websocket_connect("/ws") as orch_ws:
        runner = MockRunner(agent_ws, runner_version="0.5.0-rc1")
        orch = MockOrchestrator(orch_ws)

        wa = runner.hello()
        assert wa["type"] == "welcome" and wa["protocol_version"] == 1
        wo = orch.hello()
        assert wo["type"] == "welcome"

        orch.start_task(project_id=DEFAULT_PROJECT_ID, ticket_id=42, instructions="Risolvi.")
        task = runner.wait_for_task_start()
        session_id = task["session_id"]
        assert task["ticket_id"] == 42

        # L'orch riceve session_opened con runner_version popolato (PR1).
        so = orch.expect("session_opened")
        assert so["session_id"] == session_id
        assert so["runner_version"] == "0.5.0-rc1"

        # Streaming: task.started + 3 delta + result.
        runner.send_task_started(session_id, ticket_id=42)
        assert orch.expect("task.started")["session_id"] == session_id

        for i, chunk in enumerate(["Sto leggendo... ", "trovato il bug... ", "fix applicato."], start=1):
            runner.send_delta(session_id, i, chunk)
            d = orch.expect("chat.delta")
            assert d["seq"] == i and d["text"] == chunk

        runner.send_result(session_id, "Ciao, ho applicato la fix.",
                           cost_usd=0.42, duration_ms=12_345)
        res = orch.expect("chat.result")
        assert res["text"] == "Ciao, ho applicato la fix."
        assert res["runner_version"] == "0.5.0-rc1"
        assert res["cost_usd"] == 0.42

        orch.task_done(session_id)
        assert runner.wait_for_task_done()["session_id"] == session_id


def test_e2e_reattach_during_streaming_flushes_in_order(tmp_path: Path):
    """L'orch cade dopo 2 delta, il runner ne emette altri 3 verso una sessione
    orfana, una nuova orch fa session_attach e riceve i 3 + il chat.result finale,
    *in ordine*. Esercita PR2 (reattachment) + PR3 (delta) insieme."""
    client = _make_client(tmp_path, grace=60)
    with client.websocket_connect("/ws") as agent_ws:
        runner = MockRunner(agent_ws)
        assert runner.hello()["type"] == "welcome"

        # Prima orch: apre il task, riceve 2 delta, poi cade.
        with client.websocket_connect("/ws") as orch1_ws:
            o1 = MockOrchestrator(orch1_ws, client_id="claudia-1")
            assert o1.hello()["type"] == "welcome"
            o1.start_task(project_id=DEFAULT_PROJECT_ID, ticket_id=7, instructions="x")
            task = runner.wait_for_task_start()
            session_id = task["session_id"]
            assert o1.expect("session_opened")["session_id"] == session_id
            runner.send_task_started(session_id, ticket_id=7)
            assert o1.expect("task.started")
            runner.send_delta(session_id, 1, "delta-1 ")
            runner.send_delta(session_id, 2, "delta-2 ")
            assert o1.expect("chat.delta")["seq"] == 1
            assert o1.expect("chat.delta")["seq"] == 2
        # ^^ orch1 caduta: la sessione e' ora orfana.

        # Il runner continua a emettere durante l'orfanaggio: i frame vanno nel buffer.
        runner.send_delta(session_id, 3, "delta-3 ")
        runner.send_delta(session_id, 4, "delta-4 ")
        runner.send_delta(session_id, 5, "delta-5.")
        runner.send_result(session_id, "totale: delta-1 delta-2 delta-3 delta-4 delta-5.",
                           cost_usd=0.5, duration_ms=9_000)

        # Nuova orch si riattacca: deve ricevere session_attached + 4 frame buf + ack iniziale.
        with client.websocket_connect("/ws") as orch2_ws:
            o2 = MockOrchestrator(orch2_ws, client_id="claudia-2")
            assert o2.hello()["type"] == "welcome"
            o2.attach(session_id)
            ack = o2.expect("session_attached")
            assert ack["buffered_frames"] == 4  # 3 delta + 1 chat.result
            d3 = o2.expect("chat.delta"); assert d3["seq"] == 3
            d4 = o2.expect("chat.delta"); assert d4["seq"] == 4
            d5 = o2.expect("chat.delta"); assert d5["seq"] == 5
            res = o2.expect("chat.result")
            assert "delta-5" in res["text"]

            o2.task_done(session_id)
            assert runner.wait_for_task_done()["session_id"] == session_id


def test_e2e_chained_chat_send_keeps_delta_seq_continuous(tmp_path: Path):
    """Il `seq` dei delta e' progressivo per sessione, non per turno: dopo
    un `chat.send` la numerazione *continua* invece di ripartire da 1.
    (Lato gateway non c'e' enforcement: e' una convenzione del runner.
    Qui verifichiamo solo che il routing non perturba il valore.)"""
    client = _make_client(tmp_path)
    with client.websocket_connect("/ws") as agent_ws, client.websocket_connect("/ws") as orch_ws:
        runner = MockRunner(agent_ws)
        orch = MockOrchestrator(orch_ws)
        runner.hello(); orch.hello()

        orch.start_task(project_id=DEFAULT_PROJECT_ID, ticket_id=1, instructions="ok")
        task = runner.wait_for_task_start()
        sid = task["session_id"]
        assert orch.expect("session_opened")["session_id"] == sid
        runner.send_task_started(sid, ticket_id=1)
        assert orch.expect("task.started")

        # Primo turno: 2 delta + result.
        runner.send_delta(sid, 1, "uno "); runner.send_delta(sid, 2, "due")
        assert orch.expect("chat.delta")["seq"] == 1
        assert orch.expect("chat.delta")["seq"] == 2
        runner.send_result(sid, "uno due")
        assert orch.expect("chat.result")["text"] == "uno due"

        # Secondo turno: chat.send -> 3 delta numerati 3,4,5 -> result.
        orch.chat_send(sid, "continua")
        assert runner.wait_for_chat_send()["text"] == "continua"
        runner.send_delta(sid, 3, "tre "); runner.send_delta(sid, 4, "quattro ")
        runner.send_delta(sid, 5, "cinque")
        assert orch.expect("chat.delta")["seq"] == 3
        assert orch.expect("chat.delta")["seq"] == 4
        assert orch.expect("chat.delta")["seq"] == 5
        runner.send_result(sid, "uno due tre quattro cinque")
        final = orch.expect("chat.result")
        assert final["text"] == "uno due tre quattro cinque"
        orch.task_done(sid)


def test_e2e_runner_with_old_protocol_version_rejected_via_mock(tmp_path: Path):
    """Verifica via mock che un runner con protocol_version=0 (= troppo vecchio
    per il gateway corrente) viene respinto con un errore esplicito."""
    client = _make_client(tmp_path)
    with client.websocket_connect("/ws") as ws:
        ws.send_json({
            "type": "hello", "role": "agent", "token": "agent-tok",
            "agent_id": "mock-old", "projects": [], "runner_version": "0.0.0",
            "protocol_version": 0,
        })
        err = ws.receive_json()
        assert err["type"] == "error"
        assert err["code"] == "protocol_incompatible"
        assert err["gateway_protocol_version"] >= 1
