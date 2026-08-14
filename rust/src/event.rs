use std::collections::BTreeMap;

use serde::{Deserialize, Serialize};
use serde_json::Value;

use crate::trace::now_rfc3339;

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
            id: ulid::Ulid::new().to_string(),
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
