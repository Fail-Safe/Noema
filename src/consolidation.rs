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
use rand::TryRng;
use serde::{Deserialize, Serialize};
use tokio::task::JoinHandle;
use tokio_util::sync::CancellationToken;

use crate::cortex::{ConsolidationConfig, Cortex, PeerEntry, read_manifest};

pub const RANK_INELIGIBLE: u8 = 0;
pub const RANK_MIN: u8 = 1;
pub const RANK_MAX: u8 = 99;
pub const LOCAL_RANK_KEY: &str = "consolidation:rank";
/// How often [`EligibilityScheduler`] rewrites the local consolidation rank.
const RANK_REFRESH_INTERVAL: Duration = Duration::from_secs(15 * 60);
/// Missed refreshes a rank may accumulate before the election ignores it.
///
/// A live peer rewrites its rank every [`RANK_REFRESH_INTERVAL`]. Keeping two
/// intervals lets one refresh fail or one sync cycle slip; a peer that stays
/// silent longer is treated as offline. The quiet period is added separately,
/// because a rank cannot win until it is at least that old and the following
/// refresh has to clear the same wait.
const RANK_FRESHNESS_REFRESHES: u32 = 2;

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
    /// Publish this cortex's consolidation rank while `noema serve` is running.
    ///
    /// The worker starts whenever a consolidation block is configured, including
    /// `enabled: false` and an empty schedule. An ineligible node advertises
    /// rank 0 immediately, so a restart cannot keep serving the rank it held
    /// while consolidation was still enabled. With no consolidation block there
    /// is nothing to advertise and the worker stays off.
    pub fn start(
        name: String,
        path: PathBuf,
        cancellation: CancellationToken,
    ) -> Result<Option<Self>> {
        let manifest = read_manifest(&path)?;
        let Some(config) = manifest.consolidation_config()? else {
            return Ok(None);
        };
        let mode = manifest_federation_mode(&manifest);
        if ineligible_without_probe(&config, &mode) {
            store_local_rank(&name, &path, &config, &mode, false, RANK_INELIGIBLE)?;
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
                    _ = tokio::time::sleep(RANK_REFRESH_INTERVAL) => {}
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
    let mut rng = rand::rngs::SysRng;
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

fn manifest_federation_mode(manifest: &crate::cortex::Manifest) -> String {
    manifest
        .federation
        .as_ref()
        .map(|federation| federation.mode.clone())
        .filter(|mode| !mode.is_empty())
        .unwrap_or_else(|| "sync".to_owned())
}

fn ineligible_without_probe(config: &ConsolidationConfig, mode: &str) -> bool {
    !config.enabled || !config.llm_enabled || !config.has_trigger() || mode == "subscribe"
}

fn store_local_rank(
    name: &str,
    path: &std::path::Path,
    config: &ConsolidationConfig,
    mode: &str,
    endpoint_reachable: bool,
    eligible_rank: u8,
) -> Result<RankEntry> {
    let cx = Cortex::open(name, path)?;
    let entry = eligibility_entry(
        config,
        mode,
        endpoint_reachable,
        &cx.id,
        &Utc::now().to_rfc3339_opts(chrono::SecondsFormat::Secs, true),
        eligible_rank,
    );
    set_local_rank(&cx, &entry)?;
    Ok(entry)
}

async fn refresh_eligibility_with_cancellation(
    name: &str,
    path: &std::path::Path,
    cancellation: &CancellationToken,
) -> Result<RankEntry> {
    let manifest = read_manifest(path)?;
    let config = manifest.consolidation_config()?.unwrap_or_default();
    let mode = manifest_federation_mode(&manifest);
    let reachable = !ineligible_without_probe(&config, &mode)
        && probe_endpoint(&config.local_llm_endpoint, cancellation).await;
    let rank = if reachable {
        generate_rank()?
    } else {
        RANK_INELIGIBLE
    };
    store_local_rank(name, path, &config, &mode, reachable, rank)
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

/// Oldest rank that can still win for this quiet period.
///
/// The window is the quiet period plus two [`RANK_REFRESH_INTERVAL`]s. Peers
/// that have not refreshed within that window are ignored, which is what stops
/// a sleeping or disconnected peer from winning on its last advertised rank.
fn rank_max_age(quiet_period: Duration) -> Duration {
    quiet_period.saturating_add(RANK_REFRESH_INTERVAL.saturating_mul(RANK_FRESHNESS_REFRESHES))
}

/// Choose the eligible cortex with the highest rank.
///
/// A rank must be at least `quiet_period` old, so a newly advertised value has
/// time to reach the ring, and no older than the quiet period plus two refresh
/// intervals. Older ranks are ignored. Rank 0 never wins. Equal ranks break
/// toward the greater cortex id.
pub fn elect_winner(entries: &[RankEntry], quiet_period: Duration, now: DateTime<Utc>) -> String {
    let Ok(minimum_age) = chrono::Duration::from_std(quiet_period) else {
        return String::new();
    };
    let Ok(maximum_age) = chrono::Duration::from_std(rank_max_age(quiet_period)) else {
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
        .filter(|(_, observed)| {
            let age = now.signed_duration_since(*observed);
            age >= minimum_age && age <= maximum_age
        })
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

        config.enabled = false;
        assert_eq!(
            eligibility_entry(&config, "sync", true, "01LOCAL", "now", 42).rank,
            RANK_INELIGIBLE
        );
        config.enabled = true;
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

    fn observed(now: DateTime<Utc>, age: Duration) -> String {
        (now - chrono::Duration::from_std(age).unwrap())
            .to_rfc3339_opts(chrono::SecondsFormat::Secs, true)
    }

    #[test]
    fn election_filters_fresh_and_breaks_ties_by_lexical_id() {
        let now = Utc.with_ymd_and_hms(2026, 8, 15, 12, 0, 0).unwrap();
        let quiet = Duration::from_secs(60);
        let settled = observed(now, Duration::from_secs(10 * 60));
        let entries = vec![
            entry("01FRESH", 99, &observed(now, Duration::from_secs(5))),
            entry("01AAA", 50, &settled),
            entry("01ZZZ", 50, &settled),
            entry("01BAD", 98, "not-a-timestamp"),
        ];
        assert_eq!(elect_winner(&entries, quiet, now), "01ZZZ");
    }

    #[test]
    fn election_ignores_stale_ranks_and_keeps_a_normal_contest() {
        let now = Utc.with_ymd_and_hms(2026, 8, 15, 12, 0, 0).unwrap();
        let quiet = Duration::from_secs(60);
        let live = observed(now, Duration::from_secs(10 * 60));
        let at_limit = observed(now, rank_max_age(quiet));
        let stale = observed(now, rank_max_age(quiet) + Duration::from_secs(1));
        let entries = vec![
            entry("01ASLEEP", 99, &stale),
            entry("01LIMIT", 70, &at_limit),
            entry("01LIVE", 40, &live),
        ];
        assert_eq!(elect_winner(&entries, quiet, now), "01LIMIT");

        let blocked = decide(
            "01LOCAL",
            entry("01LOCAL", 40, &live),
            [entry("01ASLEEP", 99, &stale), entry("01OTHER", 30, &live)],
            quiet,
            now,
        );
        assert!(blocked.should_run);
        assert_eq!(blocked.winner, "01LOCAL");
        assert_eq!(blocked.reason, "winner");

        let peer_wins = decide(
            "01LOCAL",
            entry("01LOCAL", 10, &live),
            [entry("01ASLEEP", 99, &stale), entry("01LIVE", 55, &live)],
            quiet,
            now,
        );
        assert!(!peer_wins.should_run);
        assert_eq!(peer_wins.winner, "01LIVE");
        assert_eq!(peer_wins.reason, "peer 01LIVE won");

        let only_stale = decide(
            "01LOCAL",
            entry("01LOCAL", 90, &stale),
            [entry("01ASLEEP", 99, &stale)],
            quiet,
            now,
        );
        assert!(!only_stale.should_run);
        assert_eq!(only_stale.winner, "");
        assert_eq!(only_stale.reason, "no eligible peer");
    }

    #[test]
    fn decision_distinguishes_no_winner_peer_and_local_wins() {
        let now = Utc.with_ymd_and_hms(2026, 8, 15, 12, 0, 0).unwrap();
        let old = observed(now, Duration::from_secs(10 * 60));
        let none = decide(
            "01LOCAL",
            entry("01LOCAL", 0, &old),
            [],
            Duration::ZERO,
            now,
        );
        assert_eq!(none.reason, "no eligible peer");

        let peer = decide(
            "01LOCAL",
            entry("01LOCAL", 10, &old),
            [entry("01PEER", 20, &old)],
            Duration::ZERO,
            now,
        );
        assert_eq!(peer.winner, "01PEER");
        assert!(!peer.should_run);

        let local = decide(
            "01LOCAL",
            entry("01LOCAL", 30, &old),
            [entry("01PEER", 20, &old)],
            Duration::ZERO,
            now,
        );
        assert!(local.should_run);
    }

    fn write_consolidation(dir: &std::path::Path, source: &str) {
        let mut manifest = read_manifest(dir).unwrap();
        manifest.consolidation = Some(serde_yaml::from_str(source).unwrap());
        crate::cortex::write_manifest(dir, &manifest).unwrap();
    }

    #[tokio::test]
    async fn disabled_consolidation_advertises_rank_zero_before_serving() {
        let temp = tempfile::tempdir().unwrap();
        let name = "disabled-rank";
        let manifest = Cortex::create(name, temp.path()).unwrap();
        let dir = temp.path().join(name);
        write_consolidation(
            &dir,
            "enabled: false\ncron: \"03:00\"\nllm_enabled: true\nlocal_llm_endpoint: http://127.0.0.1:9/v1\n",
        );
        let scheduler =
            EligibilityScheduler::start(name.to_owned(), dir.clone(), CancellationToken::new())
                .unwrap()
                .expect("disabled consolidation still publishes a rank");
        let rank = get_local_rank(&Cortex::open(name, &dir).unwrap()).unwrap();
        scheduler.stop().await;

        assert_eq!(rank.rank, RANK_INELIGIBLE);
        assert_eq!(rank.cortex_id, manifest.id);
        let outcome = decide(
            &manifest.id,
            rank,
            [entry("01PEER", 10, "2026-08-15T11:50:00Z")],
            Duration::from_secs(60),
            Utc.with_ymd_and_hms(2026, 8, 15, 12, 0, 0).unwrap(),
        );
        assert!(!outcome.should_run);
        assert_eq!(outcome.winner, "01PEER");
    }

    #[tokio::test]
    async fn empty_schedule_advertises_rank_zero() {
        let temp = tempfile::tempdir().unwrap();
        let name = "empty-cron-rank";
        let manifest = Cortex::create(name, temp.path()).unwrap();
        let dir = temp.path().join(name);
        write_consolidation(
            &dir,
            "enabled: true\nllm_enabled: true\nlocal_llm_endpoint: http://127.0.0.1:9/v1\n",
        );
        let entry = refresh_eligibility(name, &dir).await.unwrap();
        assert_eq!(entry.rank, RANK_INELIGIBLE);
        assert_eq!(entry.cortex_id, manifest.id);
        assert_eq!(
            get_local_rank(&Cortex::open(name, &dir).unwrap())
                .unwrap()
                .rank,
            RANK_INELIGIBLE
        );
    }
}
