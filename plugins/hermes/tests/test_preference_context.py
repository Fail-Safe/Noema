"""Context ownership, invalidation, and concurrency boundaries for preference reuse."""

import threading

import pytest

from plugins.hermes.preference_context import PreferenceContext, PreferenceScope, RecallContextClient


SCOPE = PreferenceScope("cortex-a", "stdio:server-a")


def full(revision="r1", preferences=None):
    return {"preferences_revision": revision, "preferences_unchanged": False,
            "preferences": [{"id": "policy", "body": "private-canary"}] if preferences is None else preferences,
            "results": [{"query": "task", "matches": []}]}


def load(context, output=None):
    _, ticket = context.prepare({}, SCOPE)
    delivery = context.apply(ticket, full() if output is None else output)
    assert context.mark_delivered(delivery)
    return delivery


def test_response_in_ram_is_not_context_delivery():
    context = PreferenceContext()
    _, ticket = context.prepare({}, SCOPE)
    context.apply(ticket, full())
    request, _ = context.prepare({}, SCOPE)
    assert "known_preferences_revision" not in request


def test_delivered_view_reuses_revision_and_preserves_task_results():
    context = PreferenceContext()
    load(context)
    request, ticket = context.prepare({"queries": ["task"], "known_preferences_revision": "caller-forged"}, SCOPE)
    assert request["known_preferences_revision"] == "r1"
    output = full(preferences=[])
    output["preferences_unchanged"] = True
    delivery = context.apply(ticket, output)
    assert delivery.output["results"] == output["results"]
    assert delivery.output["preferences"] == []
    assert context.preferences[0]["body"] == "private-canary"


def test_replacement_empty_set_and_empty_set_reuse():
    context = PreferenceContext()
    load(context)
    _, ticket = context.prepare({}, SCOPE)
    delivery = context.apply(ticket, full("r2", []))
    assert context.preferences == []
    assert context.mark_delivered(delivery)
    request, _ = context.prepare({}, SCOPE)
    assert request["known_preferences_revision"] == "r2"


@pytest.mark.parametrize("reason", ["new_session", "compaction", "context_reset", "preference_write"])
def test_explicit_lifecycle_invalidation_requires_full_load(reason):
    context = PreferenceContext()
    load(context)
    context.invalidate(reason)
    request, _ = context.prepare({}, SCOPE)
    assert "known_preferences_revision" not in request
    assert context.preferences is None


@pytest.mark.parametrize("scope", [PreferenceScope("cortex-b", "stdio:server-a"),
                                  PreferenceScope("cortex-a", "https://server-b"),
                                  PreferenceScope("cortex-a", "stdio:server-a", 12000)])
def test_scope_changes_invalidate(scope):
    context = PreferenceContext()
    load(context)
    request, _ = context.prepare({}, scope)
    assert "known_preferences_revision" not in request


def test_older_server_discards_token_but_retains_delivered_full_view():
    context = PreferenceContext()
    load(context)
    _, ticket = context.prepare({}, SCOPE)
    output = full()
    output.pop("preferences_revision")
    output.pop("preferences_unchanged")
    delivery = context.apply(ticket, output)
    assert context.mark_delivered(delivery)
    request, _ = context.prepare({}, SCOPE)
    assert "known_preferences_revision" not in request
    assert context.preferences == output["preferences"]


def test_preference_free_query_leaves_cache_alone():
    context = PreferenceContext()
    load(context)
    request, ticket = context.prepare({"include_preferences": False}, SCOPE)
    assert "known_preferences_revision" not in request
    context.apply(ticket, full(None, []))
    request, _ = context.prepare({}, SCOPE)
    assert request["known_preferences_revision"] == "r1"


def test_late_response_and_late_delivery_cannot_restore_compacted_view():
    context = PreferenceContext()
    _, ticket = context.prepare({}, SCOPE)
    context.invalidate("compaction")
    assert context.apply(ticket, full()) is None
    _, ticket = context.prepare({}, SCOPE)
    delivery = context.apply(ticket, full())
    context.invalidate("new_session")
    assert not context.mark_delivered(delivery)


def test_out_of_order_response_cannot_overwrite_newer_request():
    context = PreferenceContext()
    _, old = context.prepare({}, SCOPE)
    _, new = context.prepare({}, SCOPE)
    delivery = context.apply(new, full("new"))
    assert context.mark_delivered(delivery)
    assert context.apply(old, full("old")) is None
    request, _ = context.prepare({}, SCOPE)
    assert request["known_preferences_revision"] == "new"


def test_conditional_response_without_matching_delivered_view_is_rejected():
    context = PreferenceContext()
    _, ticket = context.prepare({}, SCOPE)
    output = full(preferences=[])
    output["preferences_unchanged"] = True
    with pytest.raises(ValueError, match="Invalid conditional"):
        context.apply(ticket, output)
    assert context.preferences is None


def test_returned_payload_mutation_does_not_change_cached_view():
    context = PreferenceContext()
    delivery = load(context)
    delivery.output["preferences"][0]["body"] = "edited"
    view = context.preferences
    view[0]["body"] = "also-edited"
    assert context.preferences[0]["body"] == "private-canary"


def test_observer_records_outcomes_without_content_identifiers_or_tokens():
    events = []
    context = PreferenceContext(events.append)
    load(context)
    request, ticket = context.prepare({}, SCOPE)
    context.apply(ticket, dict(full(preferences=[]), preferences_unchanged=True))
    context.invalidate("compaction")
    assert any(e["revision"] == "hit" for e in events)
    assert events[-1]["reason"] == "compaction"
    assert all(set(e) == {"event", "call", "revision", "reason"} for e in events)
    assert "private-canary" not in str(events) and "cortex-a" not in str(events)
    assert request["known_preferences_revision"] == "r1"


def test_wrapper_drops_response_when_context_resets_during_network_call():
    started = threading.Event()
    released = threading.Event()
    outcomes = []

    def call(_request):
        started.set()
        assert released.wait(5)
        return full()

    client = RecallContextClient(call)
    worker = threading.Thread(target=lambda: outcomes.append(client.recall({}, SCOPE)))
    worker.start()
    assert started.wait(5)
    client.context.invalidate("compaction")
    released.set()
    worker.join(5)
    assert not worker.is_alive() and outcomes == [None]
    assert client.context.preferences is None
