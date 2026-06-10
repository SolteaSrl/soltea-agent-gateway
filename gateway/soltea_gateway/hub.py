"""Hub: gestisce le sessioni e instrada i frame tra orchestratrice e agente.

Una sessione lega un orchestratore e un agente attorno a un ticket. I frame
chat.*/task.* portano un session_id; l'hub li inoltra al capo opposto.

Riattacco sessione (dal v2 del protocollo): se l'orchestratrice si disconnette
mentre ha sessioni aperte, l'hub NON chiude subito ma marca la sessione come
"orfana" e bufferizza i frame in arrivo dall'agente per `orphan_grace_seconds`
secondi. Se entro la finestra arriva un `session_attach` da una nuova
connessione, riattacca la sessione, scarica il buffer alla nuova orchestratrice
e riprende il routing live. Oltre la finestra, la sessione viene chiusa e
l'agente notificato con `session_lost`.
"""
from __future__ import annotations

import time
import uuid
from dataclasses import dataclass, field

from .connection import Connection
from .registry import Registry


@dataclass
class Session:
    session_id: str
    orchestrator: Connection | None
    agent_id: str
    ticket_id: int | None
    project_id: int
    # Timestamp monotonic da cui la sessione e' orfana (orchestratrice caduta).
    # None se l'orchestratrice e' attiva.
    orphan_since: float | None = None
    # Frame dell'agente accumulati durante l'orfanaggio, riconsegnati al
    # riattacco. Bounded da Hub.orphan_buffer_max_frames.
    buffer: list[dict] = field(default_factory=list)
    # Ultimo client_id che ha tenuto questa sessione (per ricordare chi era).
    # Non e' usato per autorizzare il riattacco: chiunque sia autenticata come
    # orchestratrice e conosca il session_id puo' riattaccarsi.
    last_orchestrator_id: str = ""


class Hub:
    def __init__(
        self,
        registry: Registry,
        orphan_grace_seconds: int = 60,
        orphan_buffer_max_frames: int = 1000,
    ) -> None:
        self.registry = registry
        self.orphan_grace_seconds = orphan_grace_seconds
        self.orphan_buffer_max_frames = orphan_buffer_max_frames
        self._sessions: dict[str, Session] = {}
        # Indici inversi per pulizia su disconnessione.
        self._by_orchestrator: dict[str, set[str]] = {}
        self._by_agent: dict[str, set[str]] = {}

    # ----------------------------------------------------------- lifecycle --

    def create_session(
        self,
        orchestrator: Connection,
        agent_id: str,
        project_id: int,
        ticket_id: int | None,
    ) -> Session:
        session_id = "sess-" + uuid.uuid4().hex[:16]
        sess = Session(
            session_id, orchestrator, agent_id, ticket_id, project_id,
            last_orchestrator_id=orchestrator.peer_id,
        )
        self._sessions[session_id] = sess
        self._by_orchestrator.setdefault(orchestrator.peer_id, set()).add(session_id)
        self._by_agent.setdefault(agent_id, set()).add(session_id)
        return sess

    def get(self, session_id: str) -> Session | None:
        return self._sessions.get(session_id)

    def close_session(self, session_id: str) -> None:
        sess = self._sessions.pop(session_id, None)
        if sess is None:
            return
        owner_id = sess.last_orchestrator_id or (sess.orchestrator.peer_id if sess.orchestrator else "")
        if owner_id:
            self._by_orchestrator.get(owner_id, set()).discard(session_id)
        self._by_agent.get(sess.agent_id, set()).discard(session_id)

    def sessions_for_agent(self, agent_id: str) -> list[Session]:
        return [self._sessions[s] for s in self._by_agent.get(agent_id, set()) if s in self._sessions]

    def sessions_for_orchestrator(self, peer_id: str) -> list[Session]:
        return [self._sessions[s] for s in self._by_orchestrator.get(peer_id, set()) if s in self._sessions]

    # ----------------------------------------------------- disconnect/orphan --

    def drop_agent(self, conn: Connection) -> list[Session]:
        """L'agente cade -> tutte le sue sessioni vanno chiuse subito.

        L'orchestratrice viene notificata dal chiamante (con il frame error
        appropriato), poi le sessioni rimosse.
        """
        affected = self.sessions_for_agent(conn.peer_id)
        for sess in affected:
            self.close_session(sess.session_id)
        return affected

    def orphan_orchestrator(self, conn: Connection) -> list[Session]:
        """L'orchestratrice cade -> sessioni marcate orfane (non chiuse).

        Se `orphan_grace_seconds <= 0` il riattacco e' disabilitato e la sessione
        viene chiusa direttamente. Ritorna le sessioni interessate per il log.
        """
        affected = self.sessions_for_orchestrator(conn.peer_id)
        if self.orphan_grace_seconds <= 0:
            for sess in affected:
                self.close_session(sess.session_id)
            return affected
        now = time.monotonic()
        for sess in affected:
            sess.orchestrator = None
            sess.orphan_since = now
        return affected

    def buffer_agent_frame(self, sess: Session, frame: dict) -> bool:
        """Accoda un frame agente nel buffer di una sessione orfana.

        Ritorna False se il buffer ha sforato il cap: il chiamante deve chiudere
        la sessione (`close_session`) e notificare l'agente con `session_lost`.
        """
        if len(sess.buffer) >= self.orphan_buffer_max_frames:
            return False
        sess.buffer.append(frame)
        return True

    def attach_orchestrator(
        self, conn: Connection, session_id: str
    ) -> tuple[Session | None, str | None, list[dict]]:
        """Tenta il riattacco a una sessione orfana.

        Ritorna (sess, err_code, buffered):
          - (None, "unknown_session", []): session_id sconosciuto/gia' chiuso.
          - (None, "session_already_attached", []): esiste ma ha gia' un'orch.
          - (sess, None, [...frame...]): riattacco riuscito, frame da consegnare.

        Su successo, l'indice _by_orchestrator viene aggiornato al nuovo peer_id
        (cosi' un eventuale futuro disconnect colpisce solo questa nuova conn).
        """
        sess = self._sessions.get(session_id)
        if sess is None:
            return None, "unknown_session", []
        if sess.orchestrator is not None:
            return None, "session_already_attached", []
        if sess.last_orchestrator_id:
            self._by_orchestrator.get(sess.last_orchestrator_id, set()).discard(session_id)
        self._by_orchestrator.setdefault(conn.peer_id, set()).add(session_id)
        sess.orchestrator = conn
        sess.last_orchestrator_id = conn.peer_id
        sess.orphan_since = None
        buffered, sess.buffer = sess.buffer, []
        return sess, None, buffered

    def expire_orphans(self, now: float | None = None) -> list[Session]:
        """Trova e rimuove le sessioni orfane oltre la finestra di grace.

        Ritorna l'elenco delle sessioni scadute (per notificare gli agenti).
        Il chiamante e' responsabile di mandare `session_lost` agli agenti.
        """
        if self.orphan_grace_seconds <= 0:
            return []
        cutoff = (now if now is not None else time.monotonic()) - self.orphan_grace_seconds
        expired = [
            s for s in self._sessions.values()
            if s.orphan_since is not None and s.orphan_since <= cutoff
        ]
        for sess in expired:
            self.close_session(sess.session_id)
        return expired
