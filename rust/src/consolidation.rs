//! Memory-tier consolidation scheduling and scoring.

use serde::Serialize;

use crate::cortex::Row;

#[derive(Debug, Clone, Serialize)]
pub struct CandidateScore {
    pub id: String,
    pub score: f64,
}

pub fn rank(rows: &[Row]) -> Vec<CandidateScore> {
    let mut ranked: Vec<_> = rows
        .iter()
        .map(|row| CandidateScore {
            id: row.id.clone(),
            score: 0.0,
        })
        .collect();
    ranked.sort_by(|a, b| a.id.cmp(&b.id));
    ranked
}
