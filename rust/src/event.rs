use std::{
    collections::BTreeMap,
    sync::{LazyLock, Mutex},
};

use serde::{Deserialize, Serialize};
use serde_json::Value;

use crate::trace::now_rfc3339;

static EVENT_IDS: LazyLock<Mutex<ulid::Generator>> =
    LazyLock::new(|| Mutex::new(ulid::Generator::new()));

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct Event {
    pub id: String,
    pub action: String,
    pub trace_id: String,
    pub cortex_id: String,
    pub origin: String,
    pub timestamp: String,
    #[serde(default = "empty_object", skip_serializing_if = "is_empty_object")]
    pub data: Value,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    pub vclock: BTreeMap<String, u64>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub signature: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub pubkey: String,
}

impl Event {
    pub fn new(
        action: impl Into<String>,
        trace_id: impl Into<String>,
        cortex_id: impl Into<String>,
        origin: impl Into<String>,
        data: Value,
        vclock: BTreeMap<String, u64>,
    ) -> Self {
        Self {
            id: EVENT_IDS
                .lock()
                .expect("event ID generator mutex poisoned")
                .generate()
                .unwrap_or_else(|_| ulid::Ulid::new())
                .to_string(),
            action: action.into(),
            trace_id: trace_id.into(),
            cortex_id: cortex_id.into(),
            origin: origin.into(),
            timestamp: now_rfc3339(),
            data,
            vclock,
            signature: String::new(),
            pubkey: String::new(),
        }
    }

    pub fn normalized_data(&self) -> Vec<u8> {
        if self.data.is_null() {
            b"{}".to_vec()
        } else {
            serde_json::to_vec(&self.data).unwrap_or_else(|_| b"{}".to_vec())
        }
    }
}

fn empty_object() -> Value {
    serde_json::json!({})
}

fn is_empty_object(value: &Value) -> bool {
    value.as_object().is_some_and(serde_json::Map::is_empty)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn event_ids_are_lexically_monotonic() {
        let first = Event::new(
            "create",
            "20260814-first",
            "c",
            "c",
            serde_json::json!({}),
            BTreeMap::new(),
        );
        let second = Event::new(
            "update",
            "20260814-first",
            "c",
            "c",
            serde_json::json!({}),
            BTreeMap::new(),
        );
        assert!(first.id < second.id);
    }
}
