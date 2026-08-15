use std::{env, path::PathBuf, time::Duration};

use anyhow::{Context, Result, bail};
use serde::{Deserialize, Serialize};
use tokio_util::sync::CancellationToken;

use crate::cortex::{Cortex, EmbedBackfillOptions, Manifest};

const CODEC_VERSION: u8 = 1;
const DEFAULT_BATCH_SIZE: usize = 64;

#[derive(Clone)]
pub struct HttpEmbedder {
    endpoint: String,
    api_key: String,
    client: reqwest::Client,
}

pub struct Maintainer {
    task: tokio::task::JoinHandle<()>,
}

impl Maintainer {
    pub fn start(
        name: String,
        path: PathBuf,
        manifest: &Manifest,
        cancellation: CancellationToken,
    ) -> Option<Self> {
        let search = manifest
            .search
            .as_ref()
            .filter(|search| search.semantic_enabled)?;
        let model = search.embedding_model.clone();
        let endpoint = manifest.resolved_embedding_endpoint().ok()?;
        let api_key_env = manifest.resolved_embedding_api_key_env().ok()?;
        let client = match HttpEmbedder::new(&endpoint, &api_key_env) {
            Ok(client) => client,
            Err(_) => {
                eprintln!("[embed] client construction failed; auto-embed disabled");
                return None;
            }
        };
        let max_chars = search.effective_max_chars();
        let interval = Duration::from_secs(if search.embed_interval_seconds == 0 {
            300
        } else {
            search.embed_interval_seconds
        });
        let task = tokio::spawn(async move {
            let mut ticker = tokio::time::interval(interval);
            loop {
                tokio::select! {
                    _ = cancellation.cancelled() => break,
                    _ = ticker.tick() => {
                        let result = async {
                            let mut cortex = Cortex::open(&name, &path)?;
                            cortex.embed_backfill(
                                &client,
                                &model,
                                &EmbedBackfillOptions {
                                    max_chars,
                                    ..Default::default()
                                },
                            ).await
                        };
                        tokio::select! {
                            _ = cancellation.cancelled() => break,
                            result = result => match result {
                                Ok(result) if result.embedded > 0 => {
                                    eprintln!("[embed] embedded {} trace(s)", result.embedded);
                                }
                                Ok(_) => {}
                                Err(_) => eprintln!("[embed] backfill pass failed"),
                            }
                        }
                    }
                }
            }
        });
        Some(Self { task })
    }

    pub async fn stop(self) {
        let _ = self.task.await;
    }
}

#[derive(Serialize)]
struct EmbeddingRequest<'a> {
    model: &'a str,
    input: &'a [String],
}

#[derive(Deserialize)]
struct EmbeddingResponse {
    #[serde(default)]
    data: Vec<EmbeddingData>,
    error: Option<ProviderError>,
}

#[derive(Deserialize)]
struct EmbeddingData {
    #[serde(default)]
    index: usize,
    embedding: Vec<f32>,
}

#[derive(Deserialize)]
struct ProviderError {
    #[serde(default, rename = "message")]
    _message: String,
}

impl HttpEmbedder {
    pub fn new(endpoint: &str, api_key_env: &str) -> Result<Self> {
        if endpoint.is_empty() {
            bail!("embedding endpoint is empty");
        }
        let endpoint = endpoint.trim_end_matches('/').to_owned();
        reqwest::Url::parse(&endpoint).context("invalid embedding endpoint")?;
        Ok(Self {
            endpoint,
            api_key: env::var(api_key_env).unwrap_or_default(),
            client: reqwest::Client::builder()
                .timeout(Duration::from_secs(5 * 60))
                .build()?,
        })
    }

    pub async fn embed(&self, model: &str, inputs: &[String]) -> Result<Vec<Vec<f32>>> {
        if model.is_empty() {
            bail!("embedding model is empty");
        }
        if inputs.is_empty() {
            return Ok(Vec::new());
        }
        let mut output = Vec::with_capacity(inputs.len());
        for batch in inputs.chunks(DEFAULT_BATCH_SIZE) {
            output.extend(self.embed_batch(model, batch).await?);
        }
        Ok(output)
    }

    async fn embed_batch(&self, model: &str, inputs: &[String]) -> Result<Vec<Vec<f32>>> {
        let mut request = self
            .client
            .post(format!("{}/embeddings", self.endpoint))
            .json(&EmbeddingRequest {
                model,
                input: inputs,
            });
        if !self.api_key.is_empty() {
            request = request.bearer_auth(&self.api_key);
        }
        let response = request.send().await.context("posting embeddings request")?;
        let status = response.status();
        let bytes = response
            .bytes()
            .await
            .context("reading embeddings response")?;
        let parsed = serde_json::from_slice::<EmbeddingResponse>(&bytes);
        if !status.is_success() {
            if let Ok(parsed) = parsed
                && parsed.error.is_some()
            {
                bail!("embeddings endpoint returned {status}");
            }
            bail!("embeddings endpoint returned {status}");
        }
        let parsed = parsed.context("parsing embeddings response")?;
        if parsed.error.is_some() {
            bail!("embeddings endpoint returned an error");
        }
        if parsed.data.len() != inputs.len() {
            bail!(
                "embeddings response count {} != input count {}",
                parsed.data.len(),
                inputs.len()
            );
        }

        let by_index = indices_are_permutation(&parsed.data, inputs.len());
        let mut vectors = vec![Vec::new(); inputs.len()];
        for (position, item) in parsed.data.into_iter().enumerate() {
            let slot = if by_index { item.index } else { position };
            if item.embedding.is_empty() {
                bail!("embeddings response has an empty vector at slot {slot}");
            }
            vectors[slot] = item.embedding;
        }
        Ok(vectors)
    }
}

fn indices_are_permutation(data: &[EmbeddingData], count: usize) -> bool {
    if data.len() != count {
        return false;
    }
    let mut seen = vec![false; count];
    for item in data {
        if item.index >= count || seen[item.index] {
            return false;
        }
        seen[item.index] = true;
    }
    true
}

pub fn text(title: &str, body: &str, max_chars: usize) -> String {
    let title = title.trim();
    let body = body.trim();
    let combined = match (title.is_empty(), body.is_empty()) {
        (false, false) => format!("{title}\n\n{body}"),
        (false, true) => title.to_owned(),
        (true, false) => body.to_owned(),
        (true, true) => String::new(),
    };
    if max_chars == 0 {
        combined
    } else {
        combined.chars().take(max_chars).collect()
    }
}

pub fn encode(vector: &[f32]) -> Vec<u8> {
    let mut out = Vec::with_capacity(1 + vector.len() * 4);
    out.push(CODEC_VERSION);
    for value in vector {
        out.extend_from_slice(&value.to_le_bytes());
    }
    out
}

pub fn decode(blob: &[u8]) -> Result<Vec<f32>> {
    if blob.first() != Some(&CODEC_VERSION) || !(blob.len() - 1).is_multiple_of(4) {
        bail!("invalid embedding blob");
    }
    Ok(blob[1..]
        .chunks_exact(4)
        .map(|bytes| f32::from_le_bytes(bytes.try_into().unwrap()))
        .collect())
}

pub fn normalize(vector: &mut [f32]) {
    let sum = vector
        .iter()
        .map(|value| f64::from(*value) * f64::from(*value))
        .sum::<f64>();
    if sum > 0.0 {
        let inverse = (1.0 / sum.sqrt()) as f32;
        for value in vector {
            *value *= inverse;
        }
    }
}

pub fn cosine(left: &[f32], right: &[f32]) -> Option<f64> {
    (left.len() == right.len()).then(|| {
        left.iter()
            .zip(right)
            .map(|(a, b)| f64::from(*a) * f64::from(*b))
            .sum()
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn codec_round_trip() {
        let v = vec![1.0, -2.5, f32::INFINITY];
        assert_eq!(decode(&encode(&v)).unwrap(), v);
    }

    #[test]
    fn text_is_trimmed_and_unicode_safe() {
        assert_eq!(text(" title ", " body ", 0), "title\n\nbody");
        assert_eq!(text("éclair", "", 3), "écl");
    }

    #[test]
    fn normalization_matches_unit_length() {
        let mut vector = vec![3.0, 4.0];
        normalize(&mut vector);
        assert_eq!(vector, vec![0.6, 0.8]);
    }
}
