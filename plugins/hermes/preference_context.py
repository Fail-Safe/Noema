"""Conditional preference reuse for clients that own model-context delivery."""

from __future__ import annotations

from copy import deepcopy
from dataclasses import dataclass
import threading


@dataclass(frozen=True)
class PreferenceScope:
    cortex_id: str
    server_endpoint: str
    max_body_chars: int = 4000


@dataclass(frozen=True)
class RecallTicket:
    generation: int
    request_id: int
    scope: PreferenceScope
    include_preferences: bool
    sent_revision: str | None


@dataclass(frozen=True)
class RecallDelivery:
    output: dict
    ticket: RecallTicket


class PreferenceContext:
    """A response is reusable only after explicit delivery to the current context."""

    def __init__(self, observer=None):
        self._lock = threading.RLock()
        self._observer = observer
        self._generation = 0
        self._request_id = 0
        self._latest_preference_request = 0
        self._scope = None
        self._revision = None
        self._preferences = None
        self._delivered = False

    @property
    def preferences(self):
        with self._lock:
            return deepcopy(self._preferences)

    def _event(self, revision, reason):
        if self._observer:
            self._observer({"event": "recall_context", "call": self._request_id,
                            "revision": revision, "reason": reason})

    def invalidate(self, reason="context_reset"):
        with self._lock:
            allowed = {"context_reset", "compaction", "new_session", "scope_changed",
                       "preference_write", "response_error"}
            reason = reason if reason in allowed else "context_reset"
            self._generation += 1
            self._revision = None
            self._preferences = None
            self._delivered = False
            self._event("cleared", reason)

    def prepare(self, arguments, scope, reuse=True):
        with self._lock:
            if self._scope != scope:
                self.invalidate("scope_changed")
                self._scope = scope
            request = deepcopy(arguments)
            request.pop("known_preferences_revision", None)
            request["max_body_chars"] = scope.max_body_chars
            include = request.get("include_preferences", True) is not False
            self._request_id += 1
            if include:
                self._latest_preference_request = self._request_id
            revision = self._revision if include and reuse and self._delivered else None
            if revision is not None:
                request["known_preferences_revision"] = revision
            return request, RecallTicket(self._generation, self._request_id, scope, include, revision)

    def _current(self, ticket):
        return (ticket.generation == self._generation and ticket.scope == self._scope
                and (not ticket.include_preferences
                     or ticket.request_id == self._latest_preference_request))

    def apply(self, ticket, output):
        with self._lock:
            if not self._current(ticket):
                self._event("stale", "context_or_request_changed")
                return None
            output = deepcopy(output)
            if not ticket.include_preferences:
                return RecallDelivery(output, ticket)
            revision = output.get("preferences_revision")
            unchanged = output.get("preferences_unchanged", False)
            preferences = output.get("preferences")
            if (not isinstance(preferences, list)
                    or (revision is not None and not isinstance(revision, str))
                    or not isinstance(unchanged, bool)):
                self.invalidate("response_error")
                raise ValueError("Invalid preference response")
            if unchanged:
                if (not self._delivered or ticket.sent_revision is None
                        or revision != ticket.sent_revision or revision != self._revision
                        or preferences):
                    self.invalidate("response_error")
                    raise ValueError("Invalid conditional preference response")
                self._event("hit", "unchanged")
            else:
                self._preferences = deepcopy(preferences)
                self._revision = revision or None
                self._delivered = False
                self._event("miss", "refreshed" if revision else "older_server")
            return RecallDelivery(output, ticket)

    def mark_delivered(self, delivery):
        """Call only after this response's view is available in the model context."""
        with self._lock:
            if not self._current(delivery.ticket):
                return False
            if delivery.ticket.include_preferences:
                self._delivered = True
            return True


class RecallContextClient:
    """Wrap a callable returning decoded recall_context output; do not auto-ack."""

    def __init__(self, call, observer=None):
        self._call = call
        self.context = PreferenceContext(observer)

    def recall(self, arguments, scope, reuse=True):
        request, ticket = self.context.prepare(arguments, scope, reuse)
        try:
            output = self._call(request)
        except Exception:
            # A stale failure must not invalidate a newer context's view.
            with self.context._lock:
                if self.context._current(ticket):
                    self.context.invalidate("response_error")
            raise
        return self.context.apply(ticket, output)
