//! Memory-tier consolidation eligibility, election, and scoring.

mod cadence;
mod coordination;
mod distillation;
mod graduation;
mod heuristic;

pub use cadence::{CadenceScheduler, CadenceState};
pub use coordination::{
    FailReason, GateResult, GateState, InFlightRegistry, PassGate, WatchdogScheduler,
    sweep_watchdog,
};
pub use distillation::{DistillationConfig, DistillationResult, run_distillation_pass};
pub use graduation::{GraduationPassConfig, run_graduation_pass, should_graduate};
pub use heuristic::{HeuristicConfig, PassResult, run_heuristic_pass, score_candidate};

use std::{path::PathBuf, time::Duration};

use anyhow::Result;
use chrono::{DateTime, Utc};
use rand::TryRngCore;
use serde::{Deserialize, Serialize};
use tokio::task::JoinHandle;
use tokio_util::sync::CancellationToken;

use crate::cortex::{ConsolidationConfig, Cortex, PeerEntry, read_manifest};

pub const RANK_INELIGIBLE: u8 = 0;
pub const RANK_MIN: u8 = 1;
pub const RANK_MAX: u8 = 99;
pub const LOCAL_RANK_KEY: &str = "consolidation:rank";
const DEFAULT_CHECK_INTERVAL: Duration = Duration::from_secs(15 * 60);

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct RankEntry {
    pub cortex_id: String,
    pub rank: u8,
    pub observed_at: String,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ElectionOutcome {
    pub should_run: bool,
    pub winner: String,
    pub reason: String,
}

pub struct EligibilityScheduler {
    cancellation: CancellationToken,
    worker: JoinHandle<()>,
}

impl EligibilityScheduler {
    pub fn start(
        name: String,
        path: PathBuf,
        cancellation: CancellationToken,
    ) -> Result<Option<Self>> {
        let manifest = read_manifest(&path)?;
        if !manifest
            .consolidation_config()?
            .is_some_and(|config| config.enabled)
        {
            return Ok(None);
        }
        let worker_cancellation = cancellation.clone();
        let worker = tokio::spawn(async move {
            loop {
                if worker_cancellation.is_cancelled() {
                    break;
                }
                if let Err(error) =
                    refresh_eligibility_with_cancellation(&name, &path, &worker_cancellation).await
                {
                    eprintln!("consolidation eligibility refresh failed: {error:#}");
                }
                tokio::select! {
                    _ = worker_cancellation.cancelled() => break,
                    _ = tokio::time::sleep(DEFAULT_CHECK_INTERVAL) => {}
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
            eprintln!("consolidation eligibility worker join warning: {error}");
        }
    }
}

pub fn peer_rank_key(name: &str) -> String {
    format!("peer:{name}:consolidation_rank")
}

pub fn get_local_rank(cx: &Cortex) -> Result<RankEntry> {
    get_rank(cx, LOCAL_RANK_KEY)
}

pub fn set_local_rank(cx: &Cortex, entry: &RankEntry) -> Result<()> {
    cx.set_federation_state(LOCAL_RANK_KEY, &serde_json::to_string(entry)?)
}

pub fn get_peer_rank(cx: &Cortex, name: &str) -> Result<RankEntry> {
    get_rank(cx, &peer_rank_key(name))
}

pub fn set_peer_rank(cx: &Cortex, name: &str, entry: &RankEntry) -> Result<()> {
    cx.set_federation_state(&peer_rank_key(name), &serde_json::to_string(entry)?)
}

fn get_rank(cx: &Cortex, key: &str) -> Result<RankEntry> {
    let value = cx.federation_state(key)?;
    if value.is_empty() {
        return Ok(RankEntry::default());
    }
    Ok(serde_json::from_str(&value).unwrap_or_default())
}

pub fn generate_rank() -> Result<u8> {
    let mut rng = rand::rngs::OsRng;
    loop {
        let mut sample = [0_u8; 1];
        rng.try_fill_bytes(&mut sample)?;
        if sample[0] < 198 {
            return Ok((sample[0] % RANK_MAX) + RANK_MIN);
        }
    }
}

pub fn eligibility_entry(
    config: &ConsolidationConfig,
    federation_mode: &str,
    endpoint_reachable: bool,
    cortex_id: &str,
    observed_at: &str,
    eligible_rank: u8,
) -> RankEntry {
    let rank = if !config.enabled
        || !config.llm_enabled
        || !config.has_trigger()
        || federation_mode == "subscribe"
        || !endpoint_reachable
    {
        RANK_INELIGIBLE
    } else {
        eligible_rank
    };
    RankEntry {
        cortex_id: cortex_id.to_owned(),
        rank,
        observed_at: observed_at.to_owned(),
    }
}

pub async fn refresh_eligibility(name: &str, path: &std::path::Path) -> Result<RankEntry> {
    refresh_eligibility_with_cancellation(name, path, &CancellationToken::new()).await
}

async fn refresh_eligibility_with_cancellation(
    name: &str,
    path: &std::path::Path,
    cancellation: &CancellationToken,
) -> Result<RankEntry> {
    let manifest = read_manifest(path)?;
    let config = manifest.consolidation_config()?.unwrap_or_default();
    let mode = manifest
        .federation
        .as_ref()
        .map(|federation| federation.mode.as_str())
        .filter(|mode| !mode.is_empty())
        .unwrap_or("sync");
    let should_probe =
        config.enabled && config.llm_enabled && config.has_trigger() && mode != "subscribe";
    let reachable = should_probe && probe_endpoint(&config.local_llm_endpoint, cancellation).await;
    let rank = if reachable {
        generate_rank()?
    } else {
        RANK_INELIGIBLE
    };
    let cx = Cortex::open(name, path)?;
    let entry = eligibility_entry(
        &config,
        mode,
        reachable,
        &cx.id,
        &Utc::now().to_rfc3339_opts(chrono::SecondsFormat::Secs, true),
        rank,
    );
    set_local_rank(&cx, &entry)?;
    Ok(entry)
}

async fn probe_endpoint(endpoint: &str, cancellation: &CancellationToken) -> bool {
    if endpoint.is_empty() {
        return false;
    }
    let Ok(client) = reqwest::Client::builder()
        .timeout(Duration::from_secs(5))
        .redirect(reqwest::redirect::Policy::none())
        .build()
    else {
        return false;
    };
    let url = format!("{}/models", endpoint.trim_end_matches('/'));
    tokio::select! {
        _ = cancellation.cancelled() => false,
        response = client.get(url).send() => {
            response.is_ok_and(|response| response.status().is_success())
        }
    }
}

pub fn elect_winner(entries: &[RankEntry], quiet_period: Duration, now: DateTime<Utc>) -> String {
    let Ok(minimum_age) = chrono::Duration::from_std(quiet_period) else {
        return String::new();
    };
    entries
        .iter()
        .filter(|entry| entry.rank > RANK_INELIGIBLE)
        .filter_map(|entry| {
            DateTime::parse_from_rfc3339(&entry.observed_at)
                .ok()
                .map(|observed| (entry, observed.with_timezone(&Utc)))
        })
        .filter(|(_, observed)| now.signed_duration_since(*observed) >= minimum_age)
        .max_by(|(left, _), (right, _)| {
            left.rank
                .cmp(&right.rank)
                .then_with(|| left.cortex_id.cmp(&right.cortex_id))
        })
        .map(|(entry, _)| entry.cortex_id.clone())
        .unwrap_or_default()
}

pub fn decide(
    local_cortex_id: &str,
    local: RankEntry,
    peers: impl IntoIterator<Item = RankEntry>,
    quiet_period: Duration,
    now: DateTime<Utc>,
) -> ElectionOutcome {
    let mut entries = vec![local];
    entries.extend(peers);
    let winner = elect_winner(&entries, quiet_period, now);
    match winner.as_str() {
        "" => ElectionOutcome {
            should_run: false,
            winner,
            reason: "no eligible peer".into(),
        },
        winner if winner != local_cortex_id => ElectionOutcome {
            should_run: false,
            winner: winner.to_owned(),
            reason: format!("peer {winner} won"),
        },
        _ => ElectionOutcome {
            should_run: true,
            winner,
            reason: "winner".into(),
        },
    }
}

pub fn configured_peer_ranks(cx: &Cortex, peers: &[PeerEntry]) -> Vec<RankEntry> {
    peers
        .iter()
        .filter_map(|peer| get_peer_rank(cx, &peer.name).ok())
        .collect()
}

#[cfg(test)]
mod tests {
    use chrono::TimeZone;

    use super::*;

    fn entry(id: &str, rank: u8, observed_at: &str) -> RankEntry {
        RankEntry {
            cortex_id: id.into(),
            rank,
            observed_at: observed_at.into(),
        }
    }

    #[test]
    fn generated_ranks_stay_inside_the_supported_range() {
        for _ in 0..1000 {
            assert!((RANK_MIN..=RANK_MAX).contains(&generate_rank().unwrap()));
        }
    }

    #[test]
    fn eligibility_matches_public_gates() {
        let mut config = ConsolidationConfig {
            enabled: true,
            cron: "03:00".into(),
            llm_enabled: true,
            local_llm_endpoint: "http://127.0.0.1:1".into(),
            ..Default::default()
        };
        let eligible =
            eligibility_entry(&config, "sync", true, "01LOCAL", "2026-08-15T12:00:00Z", 42);
        assert_eq!(eligible.rank, 42);

        config.llm_enabled = false;
        assert_eq!(
            eligibility_entry(&config, "sync", true, "01LOCAL", "now", 42).rank,
            RANK_INELIGIBLE
        );
        config.llm_enabled = true;
        assert_eq!(
            eligibility_entry(&config, "subscribe", true, "01LOCAL", "now", 42).rank,
            RANK_INELIGIBLE
        );
        assert_eq!(
            eligibility_entry(&config, "sync", false, "01LOCAL", "now", 42).rank,
            RANK_INELIGIBLE
        );
        config.cron.clear();
        assert_eq!(
            eligibility_entry(&config, "sync", true, "01LOCAL", "now", 42).rank,
            RANK_INELIGIBLE
        );
    }

    #[test]
    fn election_filters_fresh_and_breaks_ties_by_lexical_id() {
        let now = Utc.with_ymd_and_hms(2026, 8, 15, 12, 0, 0).unwrap();
        let entries = vec![
            entry("01FRESH", 99, "2026-08-15T11:59:55Z"),
            entry("01AAA", 50, "2026-08-15T11:00:00Z"),
            entry("01ZZZ", 50, "2026-08-15T11:00:00Z"),
            entry("01BAD", 98, "not-a-timestamp"),
        ];
        assert_eq!(
            elect_winner(&entries, Duration::from_secs(60), now),
            "01ZZZ"
        );
    }

    #[test]
    fn decision_distinguishes_no_winner_peer_and_local_wins() {
        let now = Utc.with_ymd_and_hms(2026, 8, 15, 12, 0, 0).unwrap();
        let old = "2026-08-15T11:00:00Z";
        let none = decide("01LOCAL", entry("01LOCAL", 0, old), [], Duration::ZERO, now);
        assert_eq!(none.reason, "no eligible peer");

        let peer = decide(
            "01LOCAL",
            entry("01LOCAL", 10, old),
            [entry("01PEER", 20, old)],
            Duration::ZERO,
            now,
        );
        assert_eq!(peer.winner, "01PEER");
        assert!(!peer.should_run);

        let local = decide(
            "01LOCAL",
            entry("01LOCAL", 30, old),
            [entry("01PEER", 20, old)],
            Duration::ZERO,
            now,
        );
        assert!(local.should_run);
    }
}
