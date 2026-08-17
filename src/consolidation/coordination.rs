use std::{
    collections::HashSet,
    future::Future,
    path::PathBuf,
    sync::{Arc, Mutex},
    time::Duration,
};

use anyhow::{Result, anyhow};
use chrono::{DateTime, SecondsFormat, Utc};
use serde_json::json;
use tokio::task::JoinHandle;
use tokio_util::sync::CancellationToken;

use super::{decide, get_local_rank, get_peer_rank};
use crate::cortex::{Cortex, read_manifest};

const DEFAULT_WATCHDOG_TIMEOUT: Duration = Duration::from_secs(10 * 60);
const DEFAULT_WATCHDOG_INTERVAL: Duration = Duration::from_secs(60);

fn watchdog_timeout(value: &str) -> Duration {
    if value.trim().is_empty() {
        DEFAULT_WATCHDOG_TIMEOUT
    } else {
        crate::federation::parse_interval(value).unwrap_or(DEFAULT_WATCHDOG_TIMEOUT)
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum FailReason {
    EndpointDown,
    LlmError,
    ValidationFailed,
    PeerOutranked,
    NoWinnerAtRecheck,
    ContextCanceled,
    WatchdogExpired,
}

impl FailReason {
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::EndpointDown => "endpoint_down",
            Self::LlmError => "llm_error",
            Self::ValidationFailed => "validation_failed",
            Self::PeerOutranked => "peer_outranked",
            Self::NoWinnerAtRecheck => "no_winner_at_recheck",
            Self::ContextCanceled => "context_canceled",
            Self::WatchdogExpired => "watchdog_expired",
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum GateState {
    Skipped,
    Preempted,
    Succeeded,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct GateResult {
    pub state: GateState,
    pub window_id: String,
    pub winner: String,
    pub reason: String,
}

#[derive(Debug, Default)]
pub struct InFlightRegistry {
    windows: Mutex<HashSet<String>>,
}

impl InFlightRegistry {
    pub fn begin(&self, window_id: &str) {
        if !window_id.is_empty() {
            self.windows
                .lock()
                .expect("in-flight registry mutex poisoned")
                .insert(window_id.to_owned());
        }
    }

    pub fn end(&self, window_id: &str) {
        self.windows
            .lock()
            .expect("in-flight registry mutex poisoned")
            .remove(window_id);
    }

    pub fn is_active(&self, window_id: &str) -> bool {
        !window_id.is_empty()
            && self
                .windows
                .lock()
                .expect("in-flight registry mutex poisoned")
                .contains(window_id)
    }

    fn track(self: &Arc<Self>, window_id: &str) -> InFlightGuard {
        self.begin(window_id);
        InFlightGuard {
            registry: Arc::clone(self),
            window_id: window_id.to_owned(),
        }
    }
}

struct InFlightGuard {
    registry: Arc<InFlightRegistry>,
    window_id: String,
}

impl Drop for InFlightGuard {
    fn drop(&mut self) {
        self.registry.end(&self.window_id);
    }
}

pub struct PassGate {
    name: String,
    path: PathBuf,
    peer_names: Vec<String>,
    quiet_period: Duration,
    registry: Arc<InFlightRegistry>,
}

impl PassGate {
    pub fn new(
        name: impl Into<String>,
        path: impl Into<PathBuf>,
        peer_names: Vec<String>,
        quiet_period: Duration,
        registry: Arc<InFlightRegistry>,
    ) -> Self {
        Self {
            name: name.into(),
            path: path.into(),
            peer_names,
            quiet_period,
            registry,
        }
    }

    pub async fn run<F, PassFuture>(
        &self,
        cancellation: CancellationToken,
        trigger: &str,
        pass: F,
    ) -> Result<GateResult>
    where
        F: FnOnce(String) -> PassFuture,
        PassFuture: Future<Output = Result<()>>,
    {
        self.run_with_wait(
            trigger,
            move |duration| async move {
                if duration.is_zero() {
                    return Ok(());
                }
                tokio::select! {
                    _ = cancellation.cancelled() => Err(anyhow!("context canceled")),
                    _ = tokio::time::sleep(duration) => Ok(()),
                }
            },
            pass,
        )
        .await
    }

    pub async fn run_with_wait<F, PassFuture, W, WaitFuture>(
        &self,
        trigger: &str,
        wait: W,
        pass: F,
    ) -> Result<GateResult>
    where
        F: FnOnce(String) -> PassFuture,
        PassFuture: Future<Output = Result<()>>,
        W: FnOnce(Duration) -> WaitFuture,
        WaitFuture: Future<Output = Result<()>>,
    {
        let initial = self.decision(Utc::now())?;
        if !initial.should_run {
            return Ok(GateResult {
                state: GateState::Skipped,
                window_id: String::new(),
                winner: initial.winner,
                reason: initial.reason,
            });
        }

        let window_id = ulid::Ulid::new().to_string();
        self.emit(
            "consolidation_claim",
            &window_id,
            json!({"window_id":window_id,"cortex_id":initial.winner}),
        )?;

        if let Err(error) = wait(self.quiet_period).await {
            let _ = self.emit_fail(
                &window_id,
                &initial.winner,
                FailReason::ContextCanceled.as_str(),
            );
            return Err(error);
        }

        let recheck = self.decision(Utc::now())?;
        if !recheck.should_run || recheck.winner != initial.winner {
            let reason = if recheck.winner.is_empty() {
                FailReason::NoWinnerAtRecheck
            } else {
                FailReason::PeerOutranked
            };
            let _ = self.emit_fail(&window_id, &initial.winner, reason.as_str());
            return Ok(GateResult {
                state: GateState::Preempted,
                window_id,
                winner: recheck.winner,
                reason: reason.as_str().into(),
            });
        }

        let _in_flight = self.registry.track(&window_id);
        eprintln!("consolidation running as elected winner (trigger={trigger} window={window_id})");
        if let Err(error) = pass(window_id.clone()).await {
            let _ = self.emit_fail(&window_id, &initial.winner, &error.to_string());
            return Err(error);
        }
        if let Err(error) = self.emit(
            "consolidation_success",
            &window_id,
            json!({"window_id":window_id,"cortex_id":initial.winner}),
        ) {
            eprintln!("consolidation success event emission failed: {error:#}");
        }
        Ok(GateResult {
            state: GateState::Succeeded,
            window_id,
            winner: initial.winner,
            reason: "winner".into(),
        })
    }

    fn decision(&self, now: DateTime<Utc>) -> Result<super::ElectionOutcome> {
        let cx = Cortex::open(&self.name, &self.path)?;
        let local = get_local_rank(&cx).unwrap_or_default();
        let peers = self
            .peer_names
            .iter()
            .filter_map(|name| get_peer_rank(&cx, name).ok());
        Ok(decide(&cx.id, local, peers, self.quiet_period, now))
    }

    fn emit(&self, action: &str, window_id: &str, data: serde_json::Value) -> Result<()> {
        Cortex::open(&self.name, &self.path)?.emit_coordination_event(action, window_id, data)
    }

    fn emit_fail(&self, window_id: &str, winner: &str, reason: &str) -> Result<()> {
        self.emit(
            "consolidation_fail",
            window_id,
            json!({"window_id":window_id,"cortex_id":winner,"reason":reason}),
        )
    }
}

pub fn sweep_watchdog(
    cx: &Cortex,
    local_cortex_id: &str,
    timeout: Duration,
    remote_grace: Duration,
    now: DateTime<Utc>,
    registry: &InFlightRegistry,
) -> Result<usize> {
    let local_cutoff = now
        - chrono::Duration::from_std(timeout)
            .map_err(|_| anyhow!("watchdog timeout is too large"))?;
    let remote_cutoff = now
        - chrono::Duration::from_std(timeout.saturating_add(remote_grace))
            .map_err(|_| anyhow!("watchdog timeout plus grace is too large"))?;
    let claims = cx.unresolved_coordination_claims_before(
        &local_cutoff.to_rfc3339_opts(SecondsFormat::Secs, true),
    )?;
    let mut closed = 0;
    for claim in claims {
        if registry.is_active(&claim.window_id) {
            continue;
        }
        if !remote_grace.is_zero()
            && claim.winner_id != local_cortex_id
            && let Ok(claimed_at) = DateTime::parse_from_rfc3339(&claim.timestamp)
            && claimed_at.with_timezone(&Utc) > remote_cutoff
        {
            continue;
        }
        let data = json!({
            "window_id":claim.window_id,
            "cortex_id":claim.winner_id,
            "reason":FailReason::WatchdogExpired.as_str(),
        });
        match cx.emit_coordination_event("consolidation_fail", &claim.window_id, data) {
            Ok(()) => closed += 1,
            Err(error) => eprintln!(
                "consolidation watchdog failed to close window {}: {error:#}",
                claim.window_id
            ),
        }
    }
    Ok(closed)
}

pub struct WatchdogScheduler {
    cancellation: CancellationToken,
    worker: JoinHandle<()>,
}

impl WatchdogScheduler {
    pub fn start(
        name: String,
        path: PathBuf,
        cancellation: CancellationToken,
        registry: Arc<InFlightRegistry>,
    ) -> Result<Option<Self>> {
        let manifest = read_manifest(&path)?;
        let config = manifest.consolidation_config()?.unwrap_or_default();
        let federation = manifest.federation.unwrap_or_default();
        if !config.enabled || federation.peers.is_empty() {
            return Ok(None);
        }
        let timeout = watchdog_timeout(&config.watchdog_timeout);
        let federation_interval = crate::federation::parse_interval(&federation.interval)
            .unwrap_or(Duration::from_secs(30));
        let remote_grace = federation_interval.saturating_mul(2);
        let local_cortex_id = manifest.id;
        let worker_cancellation = cancellation.clone();
        let worker = tokio::spawn(async move {
            loop {
                if worker_cancellation.is_cancelled() {
                    break;
                }
                match Cortex::open(&name, &path).and_then(|cx| {
                    sweep_watchdog(
                        &cx,
                        &local_cortex_id,
                        timeout,
                        remote_grace,
                        Utc::now(),
                        &registry,
                    )
                }) {
                    Ok(_) => {}
                    Err(error) => eprintln!("consolidation watchdog sweep failed: {error:#}"),
                }
                tokio::select! {
                    _ = worker_cancellation.cancelled() => break,
                    _ = tokio::time::sleep(DEFAULT_WATCHDOG_INTERVAL) => {}
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
            eprintln!("consolidation watchdog worker join warning: {error}");
        }
    }
}

#[cfg(test)]
mod tests {
    use anyhow::bail;

    use super::*;
    use crate::consolidation::{RankEntry, set_local_rank, set_peer_rank};

    fn cortex(temp: &tempfile::TempDir, name: &str) -> Cortex {
        Cortex::create(name, temp.path()).unwrap();
        Cortex::open(name, temp.path().join(name)).unwrap()
    }

    fn rank(id: &str, value: u8) -> RankEntry {
        RankEntry {
            cortex_id: id.into(),
            rank: value,
            observed_at: (Utc::now() - chrono::Duration::hours(1))
                .to_rfc3339_opts(SecondsFormat::Secs, true),
        }
    }

    fn fail_reason(cx: &Cortex, window_id: &str) -> String {
        cx.history(window_id)
            .unwrap()
            .into_iter()
            .rev()
            .find(|event| event.action == "consolidation_fail")
            .and_then(|event| event.data["reason"].as_str().map(str::to_owned))
            .unwrap_or_default()
    }

    #[tokio::test]
    async fn gate_skips_without_emitting_when_a_peer_wins() {
        let temp = tempfile::tempdir().unwrap();
        let cx = cortex(&temp, "gate-skip");
        set_local_rank(&cx, &rank(&cx.id, 20)).unwrap();
        set_peer_rank(&cx, "peer-b", &rank("01PEER", 80)).unwrap();
        let gate = PassGate::new(
            "gate-skip",
            cx.dir.clone(),
            vec!["peer-b".into()],
            Duration::ZERO,
            Arc::new(InFlightRegistry::default()),
        );

        let result = gate
            .run_with_wait(
                "threshold",
                |_| async { Ok(()) },
                |_| async { bail!("pass must not run") },
            )
            .await
            .unwrap();

        assert_eq!(result.state, GateState::Skipped);
        assert_eq!(result.winner, "01PEER");
        assert!(
            cx.events_since("", 100)
                .unwrap()
                .iter()
                .all(|event| !event.action.starts_with("consolidation_"))
        );
    }

    #[tokio::test]
    async fn gate_claims_tracks_runs_and_succeeds() {
        let temp = tempfile::tempdir().unwrap();
        let cx = cortex(&temp, "gate-success");
        set_local_rank(&cx, &rank(&cx.id, 90)).unwrap();
        set_peer_rank(&cx, "peer-b", &rank("01PEER", 10)).unwrap();
        let registry = Arc::new(InFlightRegistry::default());
        let observed_registry = Arc::clone(&registry);
        let gate = PassGate::new(
            "gate-success",
            cx.dir.clone(),
            vec!["peer-b".into()],
            Duration::ZERO,
            Arc::clone(&registry),
        );

        let result = gate
            .run_with_wait(
                "cron",
                |_| async { Ok(()) },
                move |window_id| async move {
                    assert!(observed_registry.is_active(&window_id));
                    Ok(())
                },
            )
            .await
            .unwrap();

        assert_eq!(result.state, GateState::Succeeded);
        assert!(!registry.is_active(&result.window_id));
        let actions: Vec<_> = cx
            .history(&result.window_id)
            .unwrap()
            .into_iter()
            .map(|event| event.action)
            .collect();
        assert_eq!(
            actions,
            vec!["consolidation_claim", "consolidation_success"]
        );
    }

    #[tokio::test]
    async fn gate_distinguishes_no_winner_and_peer_preemption() {
        for (peer_rank, expected) in [
            (0, FailReason::NoWinnerAtRecheck),
            (99, FailReason::PeerOutranked),
        ] {
            let temp = tempfile::tempdir().unwrap();
            let name = format!("gate-preempt-{peer_rank}");
            let cx = cortex(&temp, &name);
            set_local_rank(&cx, &rank(&cx.id, 50)).unwrap();
            let path = cx.dir.clone();
            let local_id = cx.id.clone();
            let gate = PassGate::new(
                name.clone(),
                path.clone(),
                vec!["peer-b".into()],
                Duration::ZERO,
                Arc::new(InFlightRegistry::default()),
            );

            let wait_name = name.clone();
            let wait_path = path.clone();
            let result = gate
                .run_with_wait(
                    "cron",
                    move |_| async move {
                        let state = Cortex::open(wait_name, wait_path)?;
                        if peer_rank == 0 {
                            set_local_rank(&state, &rank(&local_id, 0))?;
                        } else {
                            set_peer_rank(&state, "peer-b", &rank("01PEER", peer_rank))?;
                        }
                        Ok(())
                    },
                    |_| async { bail!("preempted pass must not run") },
                )
                .await
                .unwrap();

            assert_eq!(result.state, GateState::Preempted);
            assert_eq!(result.reason, expected.as_str());
            assert_eq!(fail_reason(&cx, &result.window_id), expected.as_str());
        }
    }

    #[tokio::test]
    async fn gate_cancellation_and_pass_errors_emit_fail_and_clear_inflight() {
        let temp = tempfile::tempdir().unwrap();
        let cx = cortex(&temp, "gate-errors");
        set_local_rank(&cx, &rank(&cx.id, 90)).unwrap();
        let registry = Arc::new(InFlightRegistry::default());
        let gate = PassGate::new(
            "gate-errors",
            cx.dir.clone(),
            Vec::new(),
            Duration::ZERO,
            Arc::clone(&registry),
        );

        let pass_error = gate
            .run_with_wait(
                "threshold",
                |_| async { Ok(()) },
                |_| async { bail!("pipeline failed") },
            )
            .await
            .unwrap_err();
        assert_eq!(pass_error.to_string(), "pipeline failed");
        let failed_window = cx
            .events_since("", 100)
            .unwrap()
            .into_iter()
            .find(|event| event.action == "consolidation_claim")
            .unwrap()
            .trace_id;
        assert_eq!(fail_reason(&cx, &failed_window), "pipeline failed");
        assert!(!registry.is_active(&failed_window));

        let cancel_gate = PassGate::new(
            "gate-errors",
            cx.dir.clone(),
            Vec::new(),
            Duration::from_secs(10),
            Arc::clone(&registry),
        );
        let cancellation = CancellationToken::new();
        cancellation.cancel();
        let canceled = cancel_gate
            .run(cancellation, "cron", |_| async {
                bail!("pass must not run")
            })
            .await
            .unwrap_err();
        assert_eq!(canceled.to_string(), "context canceled");
        let last_claim = cx
            .events_since("", 100)
            .unwrap()
            .into_iter()
            .rev()
            .find(|event| event.action == "consolidation_claim")
            .unwrap()
            .trace_id;
        assert_eq!(
            fail_reason(&cx, &last_claim),
            FailReason::ContextCanceled.as_str()
        );
    }

    #[test]
    fn watchdog_closes_only_unresolved_stale_claims_once() {
        let temp = tempfile::tempdir().unwrap();
        let cx = cortex(&temp, "watchdog-stale");
        let stale_window = ulid::Ulid::new().to_string();
        cx.emit_coordination_event(
            "consolidation_claim",
            &stale_window,
            json!({"window_id":stale_window,"cortex_id":cx.id}),
        )
        .unwrap();
        let resolved_window = ulid::Ulid::new().to_string();
        cx.emit_coordination_event(
            "consolidation_claim",
            &resolved_window,
            json!({"window_id":resolved_window,"cortex_id":cx.id}),
        )
        .unwrap();
        cx.emit_coordination_event(
            "consolidation_success",
            &resolved_window,
            json!({"window_id":resolved_window,"cortex_id":cx.id}),
        )
        .unwrap();
        let future = Utc::now() + chrono::Duration::minutes(20);
        let registry = InFlightRegistry::default();

        assert_eq!(
            sweep_watchdog(
                &cx,
                &cx.id,
                Duration::from_secs(600),
                Duration::ZERO,
                future,
                &registry,
            )
            .unwrap(),
            1
        );
        assert_eq!(
            fail_reason(&cx, &stale_window),
            FailReason::WatchdogExpired.as_str()
        );
        assert_eq!(
            sweep_watchdog(
                &cx,
                &cx.id,
                Duration::from_secs(600),
                Duration::ZERO,
                future,
                &registry,
            )
            .unwrap(),
            0
        );
        assert!(fail_reason(&cx, &resolved_window).is_empty());
    }

    #[test]
    fn watchdog_skips_inflight_local_and_grace_period_remote_claims() {
        let temp = tempfile::tempdir().unwrap();
        let alpha = cortex(&temp, "watchdog-alpha");
        let beta = cortex(&temp, "watchdog-beta");
        let local_window = ulid::Ulid::new().to_string();
        beta.emit_coordination_event(
            "consolidation_claim",
            &local_window,
            json!({"window_id":local_window,"cortex_id":beta.id}),
        )
        .unwrap();
        let registry = InFlightRegistry::default();
        registry.begin(&local_window);
        let future = Utc::now() + chrono::Duration::minutes(20);
        assert_eq!(
            sweep_watchdog(
                &beta,
                &beta.id,
                Duration::from_secs(600),
                Duration::ZERO,
                future,
                &registry,
            )
            .unwrap(),
            0
        );
        beta.emit_coordination_event(
            "consolidation_success",
            &local_window,
            json!({"window_id":local_window,"cortex_id":beta.id}),
        )
        .unwrap();
        registry.end(&local_window);

        let remote_window = ulid::Ulid::new().to_string();
        alpha
            .emit_coordination_event(
                "consolidation_claim",
                &remote_window,
                json!({"window_id":remote_window,"cortex_id":alpha.id}),
            )
            .unwrap();
        let event = alpha.history(&remote_window).unwrap().remove(0);
        let claimed_at = DateTime::parse_from_rfc3339(&event.timestamp)
            .unwrap()
            .with_timezone(&Utc);
        beta.replay_event(&event).unwrap();

        assert_eq!(
            sweep_watchdog(
                &beta,
                &beta.id,
                Duration::from_secs(600),
                Duration::from_secs(300),
                claimed_at + chrono::Duration::minutes(12),
                &registry,
            )
            .unwrap(),
            0
        );
        assert_eq!(
            sweep_watchdog(
                &beta,
                &beta.id,
                Duration::from_secs(600),
                Duration::from_secs(300),
                claimed_at + chrono::Duration::minutes(20),
                &registry,
            )
            .unwrap(),
            1
        );
        assert_eq!(
            fail_reason(&beta, &remote_window),
            FailReason::WatchdogExpired.as_str()
        );
    }

    #[test]
    fn watchdog_timeout_parser_matches_supported_manifest_units() {
        assert_eq!(watchdog_timeout("10m"), Duration::from_secs(600));
        assert_eq!(watchdog_timeout("1h"), Duration::from_secs(3600));
        assert_eq!(watchdog_timeout(""), DEFAULT_WATCHDOG_TIMEOUT);
        assert_eq!(watchdog_timeout("invalid"), DEFAULT_WATCHDOG_TIMEOUT);
        assert_eq!(watchdog_timeout("0s"), DEFAULT_WATCHDOG_TIMEOUT);
    }
}
