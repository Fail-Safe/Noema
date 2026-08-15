use std::{path::PathBuf, sync::Arc, time::Duration};

use anyhow::Result;
use tokio::task::JoinHandle;
use tokio_util::sync::CancellationToken;

use super::{InFlightRegistry, PassGate};
use crate::cortex::{Cortex, PromotionCandidate, read_manifest};

const DEFAULT_WINDOW: Duration = Duration::from_secs(24 * 60 * 60);
const DEFAULT_POLL_INTERVAL: Duration = Duration::from_secs(60);
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

#[derive(Debug, Default)]
pub struct ThresholdState {
    armed: bool,
}

impl ThresholdState {
    pub fn should_fire(&mut self, count: i64, threshold: i64) -> bool {
        if threshold <= 0 {
            return false;
        }
        if self.armed {
            if count < ((threshold as f64) * 0.8) as i64 {
                self.armed = false;
            }
            return false;
        }
        if count > threshold {
            self.armed = true;
            return true;
        }
        false
    }
}

pub struct ThresholdScheduler {
    cancellation: CancellationToken,
    worker: JoinHandle<()>,
}

impl ThresholdScheduler {
    pub fn start(
        name: String,
        path: PathBuf,
        cancellation: CancellationToken,
        registry: Arc<InFlightRegistry>,
    ) -> Result<Option<Self>> {
        let manifest = read_manifest(&path)?;
        let config = manifest.consolidation_config()?.unwrap_or_default();
        if !config.enabled || config.threshold_short <= 0 {
            return Ok(None);
        }
        let federation = manifest.federation.unwrap_or_default();
        let peers: Vec<_> = federation
            .peers
            .iter()
            .map(|peer| peer.name.clone())
            .collect();
        let quiet_period = crate::federation::parse_interval(&federation.interval)
            .unwrap_or(Duration::from_secs(30))
            .saturating_mul(2);
        let heuristic = HeuristicConfig {
            window: if config.window_hours > 0 {
                Duration::from_secs((config.window_hours as u64).saturating_mul(60 * 60))
            } else {
                DEFAULT_WINDOW
            },
            ..HeuristicConfig::default()
        };
        let worker_cancellation = cancellation.clone();
        let worker = tokio::spawn(async move {
            let mut state = ThresholdState::default();
            let gate = (!peers.is_empty())
                .then(|| PassGate::new(name.clone(), path.clone(), peers, quiet_period, registry));
            loop {
                if worker_cancellation.is_cancelled() {
                    break;
                }
                let count = Cortex::open(&name, &path).and_then(|cx| cx.short_tier_count());
                match count {
                    Ok(count) if state.should_fire(count, config.threshold_short) => {
                        let pass_name = name.clone();
                        let pass_path = path.clone();
                        let pass_config = heuristic.clone();
                        let pass_cancellation = worker_cancellation.clone();
                        let outcome = if let Some(gate) = &gate {
                            gate.run(
                                worker_cancellation.clone(),
                                "threshold",
                                move |_| async move {
                                    let cx = Cortex::open(pass_name, pass_path)?;
                                    run_heuristic_pass(&cx, &pass_config, &pass_cancellation)?;
                                    Ok(())
                                },
                            )
                            .await
                            .map(|_| ())
                        } else {
                            Cortex::open(pass_name, pass_path).and_then(|cx| {
                                run_heuristic_pass(&cx, &pass_config, &pass_cancellation)
                                    .map(|_| ())
                            })
                        };
                        if let Err(error) = outcome {
                            eprintln!("consolidation threshold pass failed: {error:#}");
                        }
                    }
                    Ok(_) => {}
                    Err(error) => eprintln!("consolidation threshold probe failed: {error:#}"),
                }
                tokio::select! {
                    _ = worker_cancellation.cancelled() => break,
                    _ = tokio::time::sleep(DEFAULT_POLL_INTERVAL) => {}
                }
            }
        });
        Ok(Some(Self {
            cancellation,
            worker,
        }))
    }

    pub async fn stop(self) {
        self.cancellation.cancel();
        if let Err(error) = self.worker.await {
            eprintln!("consolidation threshold worker join warning: {error}");
        }
    }
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
    fn threshold_is_strict_and_rearms_below_eighty_percent() {
        let mut state = ThresholdState::default();
        assert!(!state.should_fire(10, 10));
        assert!(state.should_fire(11, 10));
        assert!(!state.should_fire(12, 10));
        assert!(!state.should_fire(8, 10));
        assert!(!state.should_fire(7, 10));
        assert!(state.should_fire(11, 10));
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
