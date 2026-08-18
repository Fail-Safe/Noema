use std::time::Duration;

use anyhow::Result;
use tokio_util::sync::CancellationToken;

use super::PassResult;
use crate::cortex::{Cortex, PromotionCandidate};

const DEFAULT_MIN_AGE: Duration = Duration::from_secs(14 * 24 * 60 * 60);

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct GraduationPassConfig {
    pub min_age: Duration,
    pub min_read_count: i64,
    pub allow_modified: bool,
}

impl Default for GraduationPassConfig {
    fn default() -> Self {
        Self {
            min_age: DEFAULT_MIN_AGE,
            min_read_count: 3,
            allow_modified: false,
        }
    }
}

impl GraduationPassConfig {
    fn resolved(&self) -> Self {
        Self {
            min_age: if self.min_age.is_zero() {
                DEFAULT_MIN_AGE
            } else {
                self.min_age
            },
            min_read_count: if self.min_read_count <= 0 {
                3
            } else {
                self.min_read_count
            },
            allow_modified: self.allow_modified,
        }
    }
}

pub fn should_graduate(candidate: &PromotionCandidate, config: &GraduationPassConfig) -> bool {
    candidate.trace_type != "preference"
        && candidate.read_count + candidate.search_hit_count >= config.min_read_count
        && (config.allow_modified || candidate.modify_count == 0)
        && candidate.tier_votes >= 0
}

pub fn run_graduation_pass(
    cx: &Cortex,
    config: &GraduationPassConfig,
    cancellation: &CancellationToken,
) -> Result<PassResult> {
    let config = config.resolved();
    let candidates = cx.graduation_candidates(config.min_age)?;
    let mut result = PassResult {
        considered: candidates.len(),
        ..PassResult::default()
    };
    for candidate in candidates {
        if cancellation.is_cancelled() {
            anyhow::bail!("context canceled");
        }
        if !should_graduate(&candidate, &config) {
            result.skipped += 1;
            continue;
        }
        match cx.promote(&candidate.id, "long") {
            Ok(()) => result.promoted += 1,
            Err(error) => {
                result.skipped += 1;
                eprintln!(
                    "consolidation graduate failed id={}: {error:#}",
                    candidate.id
                );
            }
        }
    }
    eprintln!(
        "consolidation graduation pass complete considered={} graduated={} skipped={}",
        result.considered, result.promoted, result.skipped
    );
    Ok(result)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::trace::Trace;

    #[test]
    fn criteria_gate_every_condition() {
        let config = GraduationPassConfig::default();
        let qualifies = PromotionCandidate {
            trace_type: "fact".into(),
            read_count: 1,
            search_hit_count: 2,
            ..PromotionCandidate::default()
        };
        assert!(should_graduate(&qualifies, &config));
        assert!(!should_graduate(
            &PromotionCandidate {
                trace_type: "preference".into(),
                read_count: 99,
                ..PromotionCandidate::default()
            },
            &config
        ));
        assert!(!should_graduate(
            &PromotionCandidate {
                trace_type: "fact".into(),
                read_count: 2,
                ..PromotionCandidate::default()
            },
            &config
        ));
        assert!(!should_graduate(
            &PromotionCandidate {
                trace_type: "fact".into(),
                read_count: 3,
                modify_count: 1,
                ..PromotionCandidate::default()
            },
            &config
        ));
        assert!(!should_graduate(
            &PromotionCandidate {
                trace_type: "fact".into(),
                read_count: 3,
                tier_votes: -1,
                ..PromotionCandidate::default()
            },
            &config
        ));
        assert!(should_graduate(
            &PromotionCandidate {
                trace_type: "fact".into(),
                read_count: 3,
                modify_count: 1,
                ..PromotionCandidate::default()
            },
            &GraduationPassConfig {
                allow_modified: true,
                ..GraduationPassConfig::default()
            }
        ));
    }

    #[test]
    fn pass_promotes_old_qualified_mid_once() {
        let temp = tempfile::tempdir().unwrap();
        Cortex::create("graduation", temp.path()).unwrap();
        let cx = Cortex::open("graduation", temp.path().join("graduation")).unwrap();
        let mut durable = Trace::new("durable", "fact", "", vec![], "body");
        cx.add(&mut durable).unwrap();
        cx.promote(&durable.frontmatter.id, "mid").unwrap();
        for _ in 0..3 {
            cx.bump_read(&durable.frontmatter.id).unwrap();
        }

        let cancellation = CancellationToken::new();
        let config = GraduationPassConfig {
            min_age: Duration::from_nanos(1),
            ..GraduationPassConfig::default()
        };
        let first = run_graduation_pass(&cx, &config, &cancellation).unwrap();
        assert_eq!(first.promoted, 1);
        assert_eq!(cx.get(&durable.frontmatter.id).unwrap().tier, "long");
        let second = run_graduation_pass(&cx, &config, &cancellation).unwrap();
        assert_eq!(second.promoted, 0);
        assert_eq!(
            cx.history(&durable.frontmatter.id)
                .unwrap()
                .into_iter()
                .filter(|event| event.action == "promote")
                .count(),
            2
        );
    }
}
