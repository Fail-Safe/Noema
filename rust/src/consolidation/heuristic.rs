use std::time::Duration;

use anyhow::Result;
use tokio_util::sync::CancellationToken;

use crate::cortex::{Cortex, PromotionCandidate};

const DEFAULT_WINDOW: Duration = Duration::from_secs(24 * 60 * 60);
const DEFAULT_PROMOTION_THRESHOLD: i64 = 5;
const MIN_INBOUND_REFERENCES_FOR_CREDIT: i64 = 2;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct HeuristicConfig {
    pub window: Duration,
    pub promotion_threshold: i64,
    pub weight_reads: i64,
    pub weight_modifies: i64,
    pub weight_lineage: i64,
    pub weight_votes: i64,
}

impl Default for HeuristicConfig {
    fn default() -> Self {
        Self {
            window: DEFAULT_WINDOW,
            promotion_threshold: DEFAULT_PROMOTION_THRESHOLD,
            weight_reads: 1,
            weight_modifies: 2,
            weight_lineage: 3,
            weight_votes: 5,
        }
    }
}

#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct PassResult {
    pub considered: usize,
    pub promoted: usize,
    pub skipped: usize,
}

pub fn score_candidate(candidate: &PromotionCandidate, config: &HeuristicConfig) -> i64 {
    let lineage_credit = if candidate.derived_from_count >= MIN_INBOUND_REFERENCES_FOR_CREDIT {
        candidate.derived_from_count * config.weight_lineage
    } else {
        0
    };
    let search_hit_credit = if candidate.source_count == 1
        && candidate.modify_count == 0
        && candidate.tier_votes == 0
    {
        0
    } else {
        candidate.search_hit_count
    };
    (candidate.read_count + search_hit_credit) * config.weight_reads
        + candidate.modify_count * config.weight_modifies
        + lineage_credit
        + candidate.tier_votes * config.weight_votes
}

pub fn run_heuristic_pass(
    cx: &Cortex,
    config: &HeuristicConfig,
    cancellation: &CancellationToken,
) -> Result<PassResult> {
    let candidates = cx.promotion_candidates("short", config.window)?;
    let mut result = PassResult {
        considered: candidates.len(),
        ..PassResult::default()
    };
    for candidate in candidates {
        if cancellation.is_cancelled() {
            anyhow::bail!("context canceled");
        }
        if score_candidate(&candidate, config) < config.promotion_threshold {
            result.skipped += 1;
            continue;
        }
        match cx.promote(&candidate.id, "mid") {
            Ok(()) => result.promoted += 1,
            Err(error) => {
                result.skipped += 1;
                eprintln!(
                    "consolidation promote failed id={}: {error:#}",
                    candidate.id
                );
            }
        }
    }
    eprintln!(
        "consolidation heuristic pass complete considered={} promoted={} skipped={}",
        result.considered, result.promoted, result.skipped
    );
    Ok(result)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::trace::Trace;

    fn candidate() -> PromotionCandidate {
        PromotionCandidate::default()
    }

    #[test]
    fn scoring_matches_go_signal_weights_and_gates() {
        let config = HeuristicConfig::default();
        let cases = [
            (
                PromotionCandidate {
                    read_count: 5,
                    ..candidate()
                },
                5,
            ),
            (
                PromotionCandidate {
                    search_hit_count: 5,
                    ..candidate()
                },
                5,
            ),
            (
                PromotionCandidate {
                    modify_count: 1,
                    ..candidate()
                },
                2,
            ),
            (
                PromotionCandidate {
                    derived_from_count: 1,
                    ..candidate()
                },
                0,
            ),
            (
                PromotionCandidate {
                    derived_from_count: 2,
                    ..candidate()
                },
                6,
            ),
            (
                PromotionCandidate {
                    tier_votes: 1,
                    ..candidate()
                },
                5,
            ),
            (
                PromotionCandidate {
                    search_hit_count: 10,
                    source_count: 1,
                    ..candidate()
                },
                0,
            ),
            (
                PromotionCandidate {
                    search_hit_count: 3,
                    modify_count: 1,
                    source_count: 1,
                    ..candidate()
                },
                5,
            ),
            (
                PromotionCandidate {
                    tier_votes: -1,
                    ..candidate()
                },
                -5,
            ),
        ];
        for (candidate, expected) in cases {
            assert_eq!(score_candidate(&candidate, &config), expected);
        }
    }

    #[test]
    fn pass_promotes_only_qualified_candidates_and_is_idempotent() {
        let temp = tempfile::tempdir().unwrap();
        Cortex::create("heuristic", temp.path()).unwrap();
        let cx = Cortex::open("heuristic", temp.path().join("heuristic")).unwrap();
        let mut hot = Trace::new("hot", "fact", "", vec![], "body");
        let mut cold = Trace::new("cold", "fact", "", vec![], "body");
        cx.add(&mut hot).unwrap();
        cx.add(&mut cold).unwrap();
        for _ in 0..5 {
            cx.bump_read(&hot.frontmatter.id).unwrap();
        }

        let cancellation = CancellationToken::new();
        let first = run_heuristic_pass(&cx, &HeuristicConfig::default(), &cancellation).unwrap();
        assert_eq!(first.promoted, 1);
        assert_eq!(cx.get(&hot.frontmatter.id).unwrap().tier, "mid");
        assert_eq!(cx.get(&cold.frontmatter.id).unwrap().tier, "short");
        let second = run_heuristic_pass(&cx, &HeuristicConfig::default(), &cancellation).unwrap();
        assert_eq!(second.promoted, 0);
        assert_eq!(
            cx.history(&hot.frontmatter.id)
                .unwrap()
                .into_iter()
                .filter(|event| event.action == "promote")
                .count(),
            1
        );
    }
}
