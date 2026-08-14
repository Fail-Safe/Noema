use std::collections::{BTreeMap, BTreeSet};

use serde::{Deserialize, Serialize};

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
