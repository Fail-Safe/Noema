use std::collections::{BTreeMap, BTreeSet};

use anyhow::{Context, Result, bail};
use chrono::Utc;
use rmcp::{
    ServiceExt,
    model::{CallToolRequestParams, ClientInfo},
    transport::StreamableHttpClientTransport,
};
use serde::{Deserialize, Serialize};
use serde_json::json;

use crate::{
    cortex::{Cortex, MANIFEST_VERSION, PeerEntry},
    event::Event,
    eventsig,
};

pub type VectorClock = BTreeMap<String, u64>;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub enum Relation {
    Before,
    Equal,
    After,
    Concurrent,
}

pub fn compare(left: &VectorClock, right: &VectorClock) -> Relation {
    let keys: BTreeSet<_> = left.keys().chain(right.keys()).collect();
    let mut less = false;
    let mut greater = false;
    for key in keys {
        let a = left.get(key).copied().unwrap_or_default();
        let b = right.get(key).copied().unwrap_or_default();
        less |= a < b;
        greater |= a > b;
    }
    match (less, greater) {
        (false, false) => Relation::Equal,
        (true, false) => Relation::Before,
        (false, true) => Relation::After,
        (true, true) => Relation::Concurrent,
    }
}

pub fn merge(left: &mut VectorClock, right: &VectorClock) {
    for (key, value) in right {
        left.entry(key.clone())
            .and_modify(|current| *current = (*current).max(*value))
            .or_insert(*value);
    }
}

#[derive(Debug, Clone, Default, Serialize)]
pub struct SyncReport {
    pub peer: String,
    pub batches: usize,
    pub events: usize,
    pub cursor: String,
}

#[derive(Debug, Deserialize)]
struct PeerIdentity {
    id: String,
    #[allow(dead_code)]
    name: String,
    #[serde(alias = "manifest_version")]
    version: u32,
    #[serde(default, alias = "public_key")]
    pubkey: String,
}

pub async fn sync_peer(cx: &Cortex, peer: &PeerEntry) -> Result<SyncReport> {
    if peer.mode == "paused" {
        bail!("peer {:?} is paused", peer.name);
    }
    if !peer.ca.is_empty() {
        bail!("custom peer CA files are not yet supported by the Rust experiment");
    }

    let endpoint = format!("{}/mcp", peer.endpoint.trim_end_matches('/'));
    let transport = StreamableHttpClientTransport::from_uri(endpoint.clone());
    let mut client = ClientInfo::default()
        .serve(transport)
        .await
        .with_context(|| format!("connecting to peer {:?} at {endpoint}", peer.name))?;

    let identity_result = client
        .call_tool(
            CallToolRequestParams::new("cortex_identity").with_arguments(serde_json::Map::new()),
        )
        .await
        .with_context(|| format!("calling cortex_identity on peer {:?}", peer.name))?;
    if identity_result.is_error == Some(true) {
        bail!("peer {:?} refused cortex_identity", peer.name);
    }
    let identity: PeerIdentity = identity_result
        .into_typed()
        .with_context(|| format!("parsing cortex_identity from peer {:?}", peer.name))?;
    verify_and_pin_identity(cx, peer, &identity)?;

    let cursor_key = format!("peer:{}:last_event", peer.name);
    let mut cursor = cx.federation_state(&cursor_key)?;
    let mut report = SyncReport {
        peer: peer.name.clone(),
        cursor: cursor.clone(),
        ..SyncReport::default()
    };

    loop {
        let mut arguments = serde_json::Map::new();
        arguments.insert("limit".into(), json!(100));
        if !cursor.is_empty() {
            arguments.insert("since".into(), json!(cursor));
        }
        let result = client
            .call_tool(CallToolRequestParams::new("sync_events").with_arguments(arguments))
            .await
            .with_context(|| format!("calling sync_events on peer {:?}", peer.name))?;
        if result.is_error == Some(true) {
            bail!("peer {:?} refused sync_events", peer.name);
        }
        let events: Vec<Event> = result
            .into_typed()
            .with_context(|| format!("parsing sync_events from peer {:?}", peer.name))?;
        if events.len() > 100 {
            bail!(
                "peer {:?} returned {} events for a 100-event request",
                peer.name,
                events.len()
            );
        }
        if serde_json::to_vec(&events)?.len() > 100 * 1024 * 1024 {
            bail!("peer {:?} returned an oversized event batch", peer.name);
        }
        if events.is_empty() {
            break;
        }

        report.batches += 1;
        let batch_len = events.len();
        for event in events {
            if !cursor.is_empty() && event.id <= cursor {
                bail!(
                    "peer {:?} returned non-advancing event {} after cursor {}",
                    peer.name,
                    event.id,
                    cursor
                );
            }
            cx.replay_event(&event).with_context(|| {
                format!(
                    "replaying event {} ({}) from peer {:?}; cursor remains at {}",
                    event.id, event.action, peer.name, cursor
                )
            })?;
            cursor = event.id;
            cx.set_federation_state(&cursor_key, &cursor)?;
            report.events += 1;
            report.cursor.clone_from(&cursor);
        }
        if batch_len < 100 {
            break;
        }
    }

    cx.set_federation_state(
        &format!("peer:{}:last_seen", peer.name),
        &Utc::now().to_rfc3339(),
    )?;
    client.close().await.context("closing peer MCP client")?;
    Ok(report)
}

fn verify_and_pin_identity(cx: &Cortex, peer: &PeerEntry, identity: &PeerIdentity) -> Result<()> {
    if identity.version < MANIFEST_VERSION {
        bail!(
            "peer {:?} manifest version {} is below required version {}",
            peer.name,
            identity.version,
            MANIFEST_VERSION
        );
    }
    if identity.id.is_empty() {
        bail!("peer {:?} reported no stable cortex ID", peer.name);
    }
    if identity.id == cx.id {
        bail!("peer {:?} resolves to this cortex", peer.name);
    }

    let identity_key = format!("peer:{}:cortex_id", peer.name);
    let pinned_id = cx.federation_state(&identity_key)?;
    if !pinned_id.is_empty() && pinned_id != identity.id {
        bail!(
            "peer {:?} identity mismatch: pinned {}, advertised {}; reset the peer only if this replacement is intentional",
            peer.name,
            pinned_id,
            identity.id
        );
    }

    let key_key = format!("cortexkey:{}", identity.id);
    let pinned_key = cx.federation_state(&key_key)?;
    if !peer.pubkey.is_empty() {
        if identity.pubkey.is_empty() || !public_keys_equal(&peer.pubkey, &identity.pubkey) {
            bail!(
                "peer {:?} does not match its configured public-key pin",
                peer.name
            );
        }
        cx.set_federation_state(&key_key, &peer.pubkey)?;
    } else if !identity.pubkey.is_empty() {
        if !pinned_key.is_empty() && !public_keys_equal(&pinned_key, &identity.pubkey) {
            bail!("peer {:?} changed its pinned signing key", peer.name);
        }
        if pinned_key.is_empty() {
            cx.set_federation_state(&key_key, &identity.pubkey)?;
        }
    }
    if pinned_id.is_empty() {
        cx.set_federation_state(&identity_key, &identity.id)?;
    }
    Ok(())
}

fn public_keys_equal(left: &str, right: &str) -> bool {
    match (eventsig::parse_public(left), eventsig::parse_public(right)) {
        (Ok(left), Ok(right)) => left == right,
        _ => false,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn detects_concurrency() {
        let a = BTreeMap::from([("a".into(), 2), ("b".into(), 1)]);
        let b = BTreeMap::from([("a".into(), 1), ("b".into(), 2)]);
        assert_eq!(compare(&a, &b), Relation::Concurrent);
    }
}
