use std::{fs, path::Path, time::Duration};

use anyhow::{Context, Result, bail};
use chrono::{DateTime, SecondsFormat, Utc};
use tokio::task::JoinHandle;
use tokio_util::sync::CancellationToken;
use x509_parser::{pem::parse_x509_pem, prelude::FromDer};

const DAY_SECONDS: i64 = 24 * 60 * 60;
const NEAR_EXPIRY_SECONDS: i64 = 7 * DAY_SECONDS;
pub const CERT_MONITOR_INTERVAL: Duration = Duration::from_secs(60 * 60);

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ExpiryStatus {
    Ok,
    NearExpiry,
    Expired,
    NotYetValid,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct LeafValidity {
    pub not_before: i64,
    pub not_after: i64,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Classification {
    pub status: ExpiryStatus,
    pub not_after: i64,
    pub days_remaining: i64,
}

pub fn load_leaf(path: &Path) -> Result<LeafValidity> {
    if path.as_os_str().is_empty() {
        bail!("tlsutil: empty cert path")
    }
    let data = fs::read(path).with_context(|| format!("tlsutil: reading {}", path.display()))?;
    let mut remaining = data.as_slice();
    while !remaining.is_empty() {
        let (rest, pem) = parse_x509_pem(remaining).map_err(|error| {
            anyhow::anyhow!("tlsutil: parsing PEM from {}: {error}", path.display())
        })?;
        remaining = rest;
        if pem.label != "CERTIFICATE" {
            continue;
        }
        let (_, certificate) = x509_parser::certificate::X509Certificate::from_der(&pem.contents)
            .map_err(|error| {
            anyhow::anyhow!(
                "tlsutil: parsing certificate from {}: {error}",
                path.display()
            )
        })?;
        return Ok(LeafValidity {
            not_before: certificate.validity().not_before.timestamp(),
            not_after: certificate.validity().not_after.timestamp(),
        });
    }
    bail!("tlsutil: no CERTIFICATE block in {}", path.display())
}

pub fn classify(validity: LeafValidity, now: DateTime<Utc>) -> Classification {
    let now = now.timestamp();
    let mut result = Classification {
        status: ExpiryStatus::Ok,
        not_after: validity.not_after,
        days_remaining: 0,
    };
    if now < validity.not_before {
        result.status = ExpiryStatus::NotYetValid;
    } else if now >= validity.not_after {
        result.status = ExpiryStatus::Expired;
        result.days_remaining = -((now - validity.not_after) / DAY_SECONDS);
    } else {
        result.days_remaining = (validity.not_after - now) / DAY_SECONDS;
        if validity.not_after - now <= NEAR_EXPIRY_SECONDS {
            result.status = ExpiryStatus::NearExpiry;
        }
    }
    result
}

pub fn gate_startup(
    cert_path: &Path,
    allow_expired: bool,
    now: DateTime<Utc>,
) -> Result<Option<String>> {
    let validity = load_leaf(cert_path).map_err(|error| {
        anyhow::anyhow!("refusing to start: cannot read TLS certificate: {error:#}")
    })?;
    let classification = classify(validity, now);
    let path = cert_path.display();
    let not_after = format_timestamp(classification.not_after);
    match classification.status {
        ExpiryStatus::Expired if !allow_expired => bail!(
            "refusing to start: TLS certificate at {path} expired {} day(s) ago (NotAfter={not_after}).\n  Clients reject expired certificates and the server would appear unreachable.\n  Rotate the certificate and restart, or pass --insecure-allow-expired briefly to rotate it in place",
            -classification.days_remaining
        ),
        ExpiryStatus::Expired => Ok(Some(format!(
            "[serve] WARN --insecure-allow-expired: TLS cert at {path} expired {} day(s) ago (NotAfter={not_after})",
            -classification.days_remaining
        ))),
        ExpiryStatus::NotYetValid if !allow_expired => bail!(
            "refusing to start: TLS certificate at {path} NotBefore is in the future (NotAfter={not_after}).\n  This is usually clock skew or the wrong certificate path.\n  Pass --insecure-allow-expired to bypass this check"
        ),
        ExpiryStatus::NotYetValid => Ok(Some(format!(
            "[serve] WARN --insecure-allow-expired: TLS cert at {path} not yet valid (NotAfter={not_after})"
        ))),
        ExpiryStatus::NearExpiry => Ok(Some(format!(
            "[serve] WARN TLS cert at {path} expires in {} day(s) (NotAfter={not_after}) - rotate soon",
            classification.days_remaining
        ))),
        ExpiryStatus::Ok => Ok(None),
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum WarningBand {
    Fresh,
    Days90,
    Days30,
    Days7,
    Expired,
    Unknown,
}

impl WarningBand {
    fn label(self) -> &'static str {
        match self {
            Self::Fresh => "fresh",
            Self::Days90 => "≤90d",
            Self::Days30 => "≤30d",
            Self::Days7 => "≤7d",
            Self::Expired => "expired",
            Self::Unknown => "unknown",
        }
    }
}

#[derive(Default)]
struct MonitorState {
    last_band: Option<WarningBand>,
}

impl MonitorState {
    fn observe(&mut self, cert_path: &Path, observation: Result<Classification>) -> Option<String> {
        let (band, message) = match observation {
            Ok(classification) => monitor_message(cert_path, classification),
            Err(error) => (
                WarningBand::Unknown,
                format!("cannot read {}: {error:#}", cert_path.display()),
            ),
        };
        if self.last_band == Some(band) {
            return None;
        }
        self.last_band = Some(band);
        Some(format!("[cert-monitor] {}: {message}", band.label()))
    }
}

fn monitor_message(path: &Path, classification: Classification) -> (WarningBand, String) {
    let path = path.display();
    let not_after = format_timestamp(classification.not_after);
    match classification.status {
        ExpiryStatus::Expired => (
            WarningBand::Expired,
            format!(
                "{path} expired {} day(s) ago (NotAfter={not_after}) - rotate the cert and restart `noema serve`",
                -classification.days_remaining
            ),
        ),
        ExpiryStatus::NotYetValid => (
            WarningBand::Expired,
            format!(
                "{path} NotBefore is in the future (NotAfter={not_after}) - clock skew or wrong cert"
            ),
        ),
        ExpiryStatus::NearExpiry => (
            WarningBand::Days7,
            format!(
                "{path} expires in {} day(s) (NotAfter={not_after})",
                classification.days_remaining
            ),
        ),
        ExpiryStatus::Ok if classification.days_remaining <= 30 => (
            WarningBand::Days30,
            format!(
                "{path} expires in {} day(s) (NotAfter={not_after})",
                classification.days_remaining
            ),
        ),
        ExpiryStatus::Ok if classification.days_remaining <= 90 => (
            WarningBand::Days90,
            format!(
                "{path} expires in {} day(s) (NotAfter={not_after})",
                classification.days_remaining
            ),
        ),
        ExpiryStatus::Ok => (
            WarningBand::Fresh,
            format!(
                "{path} ok, {} days until NotAfter={not_after}",
                classification.days_remaining
            ),
        ),
    }
}

pub struct CertMonitor {
    cancellation: CancellationToken,
    worker: JoinHandle<()>,
}

impl CertMonitor {
    pub fn start(cert_path: &Path) -> Self {
        let cert_path = cert_path.to_path_buf();
        let cancellation = CancellationToken::new();
        let worker_cancellation = cancellation.clone();
        let worker = tokio::spawn(async move {
            let mut state = MonitorState::default();
            check_once(&cert_path, Utc::now(), &mut state);
            let mut interval = tokio::time::interval(CERT_MONITOR_INTERVAL);
            interval.tick().await;
            loop {
                tokio::select! {
                    _ = worker_cancellation.cancelled() => break,
                    _ = interval.tick() => check_once(&cert_path, Utc::now(), &mut state),
                }
            }
        });
        Self {
            cancellation,
            worker,
        }
    }

    pub async fn stop(self) {
        self.cancellation.cancel();
        let _ = self.worker.await;
    }
}

fn check_once(cert_path: &Path, now: DateTime<Utc>, state: &mut MonitorState) {
    let observation = load_leaf(cert_path).map(|validity| classify(validity, now));
    if let Some(message) = state.observe(cert_path, observation) {
        eprintln!("{message}");
    }
}

fn format_timestamp(timestamp: i64) -> String {
    DateTime::<Utc>::from_timestamp(timestamp, 0)
        .map(|value| value.to_rfc3339_opts(SecondsFormat::Secs, true))
        .unwrap_or_else(|| timestamp.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn at(timestamp: i64) -> DateTime<Utc> {
        DateTime::from_timestamp(timestamp, 0).unwrap()
    }

    #[test]
    fn classifies_validity_boundaries_like_the_go_oracle() {
        let now = 2_000_000_000;
        assert_eq!(
            classify(
                LeafValidity {
                    not_before: now - DAY_SECONDS,
                    not_after: now + 8 * DAY_SECONDS,
                },
                at(now),
            )
            .status,
            ExpiryStatus::Ok
        );
        assert_eq!(
            classify(
                LeafValidity {
                    not_before: now - DAY_SECONDS,
                    not_after: now + 7 * DAY_SECONDS,
                },
                at(now),
            )
            .status,
            ExpiryStatus::NearExpiry
        );
        assert_eq!(
            classify(
                LeafValidity {
                    not_before: now - DAY_SECONDS,
                    not_after: now,
                },
                at(now),
            )
            .status,
            ExpiryStatus::Expired
        );
        assert_eq!(
            classify(
                LeafValidity {
                    not_before: now + 1,
                    not_after: now + 30 * DAY_SECONDS,
                },
                at(now),
            )
            .status,
            ExpiryStatus::NotYetValid
        );
    }

    #[test]
    fn monitor_emits_only_band_transitions() {
        let path = Path::new("server.crt");
        let mut state = MonitorState::default();
        let observation = |days| {
            Ok(Classification {
                status: if days <= 7 {
                    ExpiryStatus::NearExpiry
                } else {
                    ExpiryStatus::Ok
                },
                not_after: 2_000_000_000,
                days_remaining: days,
            })
        };
        assert!(state.observe(path, observation(100)).is_some());
        assert!(state.observe(path, observation(99)).is_none());
        assert!(state.observe(path, observation(90)).is_some());
        assert!(state.observe(path, observation(31)).is_none());
        assert!(state.observe(path, observation(30)).is_some());
        assert!(state.observe(path, observation(7)).is_some());
        assert!(state.observe(path, observation(6)).is_none());
        assert!(
            state
                .observe(
                    path,
                    Ok(Classification {
                        status: ExpiryStatus::Expired,
                        not_after: 2_000_000_000,
                        days_remaining: -1,
                    })
                )
                .is_some()
        );
        assert!(
            state
                .observe(path, Err(anyhow::anyhow!("unreadable")))
                .is_some()
        );
        assert!(
            state
                .observe(path, Err(anyhow::anyhow!("still unreadable")))
                .is_none()
        );
    }

    #[test]
    fn malformed_pem_does_not_expose_certificate_contents() {
        let temp = tempfile::tempdir().unwrap();
        let path = temp.path().join("server.crt");
        fs::write(&path, "not a certificate\nprivate marker").unwrap();
        let error = load_leaf(&path).unwrap_err().to_string();
        assert!(error.contains("server.crt"));
        assert!(!error.contains("private marker"));
    }
}
