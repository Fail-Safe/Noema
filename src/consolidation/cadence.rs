use std::{path::PathBuf, sync::Arc, time::Duration};

use anyhow::Result;
use chrono::{DateTime, Datelike, Local, Timelike, Utc};
use tokio::task::JoinHandle;
use tokio_util::sync::CancellationToken;

use super::{
    DistillationConfig, GraduationPassConfig, HeuristicConfig, InFlightRegistry, PassGate,
    run_distillation_pass, run_graduation_pass, run_heuristic_pass,
};
use crate::cortex::{Cortex, read_manifest};

const DEFAULT_POLL_INTERVAL: Duration = Duration::from_secs(60);
const CRON_RETRY_WINDOW: chrono::Duration = chrono::Duration::minutes(5);
const CRON_MAX_RETRIES: u8 = 3;

#[derive(Debug, Clone, PartialEq, Eq)]
struct LocalClock {
    day: String,
    minute_of_day: u32,
}

impl LocalClock {
    fn now() -> (Self, DateTime<Utc>) {
        let local = Local::now();
        (
            Self {
                day: format!(
                    "{:04}-{:02}-{:02}",
                    local.year(),
                    local.month(),
                    local.day()
                ),
                minute_of_day: local.hour() * 60 + local.minute(),
            },
            local.with_timezone(&Utc),
        )
    }

    #[cfg(test)]
    fn at(day: &str, hour: u32, minute: u32) -> Self {
        Self {
            day: day.into(),
            minute_of_day: hour * 60 + minute,
        }
    }
}

fn parse_cron_hhmm(value: &str) -> Option<u32> {
    let bytes = value.as_bytes();
    if bytes.len() != 5
        || bytes[2] != b':'
        || !bytes[0..2].iter().all(u8::is_ascii_digit)
        || !bytes[3..5].iter().all(u8::is_ascii_digit)
    {
        return None;
    }
    let hour: u32 = value[0..2].parse().ok()?;
    let minute: u32 = value[3..5].parse().ok()?;
    (hour < 24 && minute < 60).then_some(hour * 60 + minute)
}

#[derive(Debug, Clone)]
struct CronRetry {
    fire_time: DateTime<Utc>,
    retries_left: u8,
    target_day: String,
}

#[derive(Debug, Default)]
pub struct CadenceState {
    last_run: Option<DateTime<Utc>>,
    threshold_armed: bool,
    last_cron_day: String,
    cron_retry: Option<CronRetry>,
}

impl CadenceState {
    fn cron_should_fire(&self, cron: &str, clock: &LocalClock) -> bool {
        let Some(scheduled) = parse_cron_hhmm(cron) else {
            return false;
        };
        self.last_cron_day != clock.day
            && self
                .cron_retry
                .as_ref()
                .is_none_or(|retry| retry.target_day != clock.day)
            && clock.minute_of_day >= scheduled
    }

    pub fn threshold_should_fire(&mut self, count: i64, threshold: i64) -> bool {
        if threshold <= 0 {
            return false;
        }
        if self.threshold_armed {
            if count < ((threshold as f64) * 0.8) as i64 {
                self.threshold_armed = false;
            }
            return false;
        }
        if count > threshold {
            self.threshold_armed = true;
            return true;
        }
        false
    }

    fn idle_should_fire(
        &self,
        last_mutation: Option<DateTime<Utc>>,
        idle_minutes: i64,
        now: DateTime<Utc>,
    ) -> bool {
        if idle_minutes <= 0 {
            return false;
        }
        let Some(last_mutation) = last_mutation else {
            return false;
        };
        let cooldown = chrono::Duration::minutes(idle_minutes);
        self.last_run
            .is_none_or(|last_run| now.signed_duration_since(last_run) >= cooldown)
            && now.signed_duration_since(last_mutation) >= cooldown
    }

    fn mark_fired(
        &mut self,
        trigger: &str,
        now: DateTime<Utc>,
        clock: &LocalClock,
        federated: bool,
    ) {
        self.last_run = Some(now);
        if trigger != "cron" {
            return;
        }
        if federated {
            self.cron_retry = Some(CronRetry {
                fire_time: now,
                retries_left: CRON_MAX_RETRIES,
                target_day: clock.day.clone(),
            });
        } else {
            self.last_cron_day.clone_from(&clock.day);
        }
    }

    fn retry_cutoff(&self, now: DateTime<Utc>) -> Option<DateTime<Utc>> {
        self.cron_retry
            .as_ref()
            .filter(|retry| now.signed_duration_since(retry.fire_time) >= CRON_RETRY_WINDOW)
            .map(|retry| retry.fire_time)
    }

    fn resolve_retry(&mut self, success: bool, now: DateTime<Utc>) -> bool {
        let Some(retry) = &mut self.cron_retry else {
            return false;
        };
        if success || retry.retries_left == 0 {
            self.last_cron_day.clone_from(&retry.target_day);
            self.cron_retry = None;
            return false;
        }
        retry.retries_left -= 1;
        retry.fire_time = now;
        self.last_run = Some(now);
        true
    }
}

#[derive(Clone)]
struct PipelineConfig {
    distillation: Option<DistillationConfig>,
    heuristic: HeuristicConfig,
    graduation: Option<GraduationPassConfig>,
}

async fn run_pipeline(
    name: &str,
    path: &std::path::Path,
    config: &PipelineConfig,
    cancellation: &CancellationToken,
) -> Result<()> {
    if let Some(distillation) = &config.distillation {
        let cx = Cortex::open(name, path)?;
        match run_distillation_pass(cx, distillation, cancellation).await {
            Ok(result) => eprintln!(
                "consolidation auto-distillation complete considered={} attempted={} distilled={} rejected={} fallback={} skipped={}",
                result.considered,
                result.attempted,
                result.distilled,
                result.rejected,
                result.fallback_promotions,
                result.skipped
            ),
            Err(error) if cancellation.is_cancelled() => return Err(error),
            Err(error) => eprintln!(
                "consolidation auto-distillation skipped; continuing maintenance: {error:#}"
            ),
        }
    }
    let cx = Cortex::open(name, path)?;
    let heuristic = run_heuristic_pass(&cx, &config.heuristic, cancellation).map(|_| ());
    let graduation = config
        .graduation
        .as_ref()
        .map(|graduation| run_graduation_pass(&cx, graduation, cancellation).map(|_| ()))
        .unwrap_or(Ok(()));
    heuristic.and(graduation)
}

async fn run_scheduled_pass(
    name: String,
    path: PathBuf,
    trigger: &'static str,
    gate: Option<&PassGate>,
    pipeline: PipelineConfig,
    cancellation: CancellationToken,
) -> Result<()> {
    eprintln!("consolidation pass firing (trigger={trigger})");
    if let Some(gate) = gate {
        gate.run(cancellation.clone(), trigger, move |_| async move {
            run_pipeline(&name, &path, &pipeline, &cancellation).await
        })
        .await
        .map(|_| ())
    } else {
        run_pipeline(&name, &path, &pipeline, &cancellation).await
    }
}

pub struct CadenceScheduler {
    cancellation: CancellationToken,
    worker: JoinHandle<()>,
}

impl CadenceScheduler {
    pub fn start(
        name: String,
        path: PathBuf,
        cancellation: CancellationToken,
        registry: Arc<InFlightRegistry>,
    ) -> Result<Option<Self>> {
        let manifest = read_manifest(&path)?;
        let config = manifest.consolidation_config()?.unwrap_or_default();
        if !config.enabled || !config.has_trigger() {
            return Ok(None);
        }
        let federation = manifest.federation.unwrap_or_default();
        let peers: Vec<_> = federation
            .peers
            .iter()
            .map(|peer| peer.name.clone())
            .collect();
        let federated = !peers.is_empty();
        let quiet_period = crate::federation::parse_interval(&federation.interval)
            .unwrap_or(Duration::from_secs(30))
            .saturating_mul(2);
        let graduation = config.graduation.clone().unwrap_or_default();
        let heuristic = HeuristicConfig {
            window: if config.window_hours > 0 {
                Duration::from_secs((config.window_hours as u64).saturating_mul(60 * 60))
            } else {
                Duration::from_secs(24 * 60 * 60)
            },
            ..HeuristicConfig::default()
        };
        let pipeline = PipelineConfig {
            distillation: config
                .auto_distillation_enabled
                .then(|| DistillationConfig {
                    window: heuristic.window,
                    model_tier: config.effective_model_tier().into(),
                    model_name: config.model_name.clone(),
                    endpoint: config.local_llm_endpoint.clone(),
                    api_key_env: config.api_key_env.clone(),
                    max_retries: 1,
                    dry_run: false,
                    heuristic: heuristic.clone(),
                }),
            heuristic,
            graduation: graduation
                .effective_enabled()
                .then(|| GraduationPassConfig {
                    min_age: graduation.effective_min_age(),
                    min_read_count: graduation.effective_min_read_count(),
                    allow_modified: !graduation.effective_require_unmodified(),
                }),
        };
        let worker_cancellation = cancellation.clone();
        let worker = tokio::spawn(async move {
            let gate = federated
                .then(|| PassGate::new(name.clone(), path.clone(), peers, quiet_period, registry));
            let mut state = CadenceState::default();
            loop {
                if worker_cancellation.is_cancelled() {
                    break;
                }
                let (clock, now) = LocalClock::now();

                if let Some(cutoff) = state.retry_cutoff(now) {
                    match Cortex::open(&name, &path)
                        .and_then(|cx| cx.has_consolidation_success_after(cutoff))
                    {
                        Ok(success) if state.resolve_retry(success, now) => {
                            if let Err(error) = run_scheduled_pass(
                                name.clone(),
                                path.clone(),
                                "cron",
                                gate.as_ref(),
                                pipeline.clone(),
                                worker_cancellation.clone(),
                            )
                            .await
                            {
                                eprintln!("consolidation cron retry pass failed: {error:#}");
                            }
                        }
                        Ok(_) => {}
                        Err(error) => {
                            eprintln!("consolidation cron retry check failed: {error:#}")
                        }
                    }
                }

                let trigger = if state.cron_should_fire(&config.cron, &clock) {
                    Some("cron")
                } else {
                    match Cortex::open(&name, &path) {
                        Ok(cx) => {
                            let threshold = if config.threshold_short > 0 {
                                match cx.short_tier_count() {
                                    Ok(count) => {
                                        state.threshold_should_fire(count, config.threshold_short)
                                    }
                                    Err(error) => {
                                        eprintln!(
                                            "consolidation threshold probe failed: {error:#}"
                                        );
                                        false
                                    }
                                }
                            } else {
                                false
                            };
                            if threshold {
                                Some("threshold")
                            } else if config.idle_minutes > 0 {
                                match cx.last_mutation_time() {
                                    Ok(last)
                                        if state.idle_should_fire(
                                            last,
                                            config.idle_minutes,
                                            now,
                                        ) =>
                                    {
                                        Some("idle")
                                    }
                                    Ok(_) => None,
                                    Err(error) => {
                                        eprintln!("consolidation idle probe failed: {error:#}");
                                        None
                                    }
                                }
                            } else {
                                None
                            }
                        }
                        Err(error) => {
                            eprintln!("consolidation cadence probe failed: {error:#}");
                            None
                        }
                    }
                };

                if let Some(trigger) = trigger {
                    state.mark_fired(trigger, now, &clock, federated);
                    if let Err(error) = run_scheduled_pass(
                        name.clone(),
                        path.clone(),
                        trigger,
                        gate.as_ref(),
                        pipeline.clone(),
                        worker_cancellation.clone(),
                    )
                    .await
                    {
                        eprintln!("consolidation pass failed (trigger={trigger}): {error:#}");
                    }
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
            eprintln!("consolidation cadence worker join warning: {error}");
        }
    }
}

#[cfg(test)]
mod tests {
    use chrono::TimeZone;

    use super::*;
    use crate::cortex::ConsolidationConfig;

    fn utc(day: u32, hour: u32, minute: u32) -> DateTime<Utc> {
        Utc.with_ymd_and_hms(2026, 4, day, hour, minute, 0).unwrap()
    }

    #[test]
    fn cron_parser_is_strict() {
        for value in ["00:00", "03:00", "23:59", "09:30"] {
            assert!(parse_cron_hhmm(value).is_some(), "{value}");
        }
        for value in ["", "3:00", "25:00", "03:60", "0300", "three"] {
            assert!(parse_cron_hhmm(value).is_none(), "{value}");
        }
    }

    #[test]
    fn cron_fires_once_per_day_and_marks_single_node_immediately() {
        let mut state = CadenceState::default();
        let before = LocalClock::at("2026-04-19", 2, 59);
        let due = LocalClock::at("2026-04-19", 3, 0);
        assert!(!state.cron_should_fire("03:00", &before));
        assert!(state.cron_should_fire("03:00", &due));
        state.mark_fired("cron", utc(19, 3, 0), &due, false);
        assert!(!state.cron_should_fire("03:00", &LocalClock::at("2026-04-19", 4, 0)));
        assert!(state.cron_should_fire("03:00", &LocalClock::at("2026-04-20", 3, 0)));
    }

    #[test]
    fn threshold_is_strict_and_rearms_below_eighty_percent() {
        let mut state = CadenceState::default();
        assert!(!state.threshold_should_fire(10, 10));
        assert!(state.threshold_should_fire(11, 10));
        assert!(!state.threshold_should_fire(12, 10));
        assert!(!state.threshold_should_fire(8, 10));
        assert!(!state.threshold_should_fire(7, 10));
        assert!(state.threshold_should_fire(11, 10));
    }

    #[test]
    fn idle_requires_history_and_obeys_any_pass_cooldown() {
        let mut state = CadenceState::default();
        assert!(!state.idle_should_fire(None, 30, utc(19, 12, 0)));
        assert!(!state.idle_should_fire(Some(utc(19, 12, 0)), 30, utc(19, 12, 29)));
        assert!(state.idle_should_fire(Some(utc(19, 12, 0)), 30, utc(19, 12, 30)));
        state.mark_fired(
            "threshold",
            utc(19, 12, 30),
            &LocalClock::at("2026-04-19", 12, 30),
            false,
        );
        assert!(!state.idle_should_fire(Some(utc(19, 12, 0)), 30, utc(19, 12, 59)));
        assert!(state.idle_should_fire(Some(utc(19, 12, 0)), 30, utc(19, 13, 0)));
    }

    #[test]
    fn cron_retry_succeeds_refires_and_exhausts_consistently() {
        let clock = LocalClock::at("2026-04-19", 3, 0);
        let mut success = CadenceState::default();
        success.mark_fired("cron", utc(19, 3, 0), &clock, true);
        assert_eq!(success.retry_cutoff(utc(19, 3, 4)), None);
        assert!(success.retry_cutoff(utc(19, 3, 5)).is_some());
        assert!(!success.resolve_retry(true, utc(19, 3, 5)));
        assert!(!success.cron_should_fire("03:00", &LocalClock::at("2026-04-19", 4, 0)));

        let mut exhausted = CadenceState::default();
        exhausted.mark_fired("cron", utc(19, 3, 0), &clock, true);
        for minute in [5, 10, 15] {
            assert!(exhausted.resolve_retry(false, utc(19, 3, minute)));
        }
        assert!(!exhausted.resolve_retry(false, utc(19, 3, 20)));
        assert!(exhausted.cron_retry.is_none());
        assert!(!exhausted.cron_should_fire("03:00", &LocalClock::at("2026-04-19", 4, 0)));
    }

    #[test]
    fn configured_trigger_priority_is_stable() {
        let state = CadenceState::default();
        assert!(state.cron_should_fire("03:00", &LocalClock::at("2026-04-19", 3, 0)));
    }

    #[test]
    fn manifest_defaults_enable_strict_graduation() {
        let config = ConsolidationConfig::default();
        let graduation = config.graduation.unwrap_or_default();
        assert!(graduation.effective_enabled());
        assert_eq!(
            graduation.effective_min_age(),
            Duration::from_secs(14 * 24 * 60 * 60)
        );
        assert_eq!(graduation.effective_min_read_count(), 3);
        assert!(graduation.effective_require_unmodified());
    }
}
