use std::{env, fmt, path::PathBuf, time::Duration};

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
                                &EmbedBackfillOptions::default(),
                            ).await
                        };
                        tokio::select! {
                            _ = cancellation.cancelled() => break,
                            result = result => match result {
                                Ok(result) if result.embedded > 0 || !result.failures.is_empty() => {
                                    eprintln!("[embed] embedded {} trace(s), {} truncated, {} failed, {} deferred",
                                        result.embedded, result.truncated, result.failures.len(), result.deferred);
                                    for failure in result.failures {
                                        eprintln!("[embed] trace {}: {}; retry after {}", failure.trace_id, failure.reason, failure.retry_after);
                                    }
                                }
                                Ok(_) => {}
                                Err(error) => eprintln!("[embed] backfill pass failed: {error}"),
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
    #[serde(default)]
    message: String,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum SizeLimit {
    Context,
    PhysicalBatch,
}

#[derive(Debug, Clone)]
pub struct InputSizeError {
    pub kind: SizeLimit,
    pub tokens: Option<usize>,
    pub limit: Option<usize>,
}

impl fmt::Display for InputSizeError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self.kind {
            SizeLimit::Context => "embedding input exceeds the server context limit",
            SizeLimit::PhysicalBatch => {
                "embedding input exceeds the server physical batch size (n_ubatch / --ubatch-size)"
            }
        })?;
        if let Some(tokens) = self.tokens {
            write!(f, "; input_tokens={tokens}")?;
        }
        if let Some(limit) = self.limit {
            write!(f, "; limit={limit}")?;
        }
        Ok(())
    }
}

impl std::error::Error for InputSizeError {}

#[derive(Debug, thiserror::Error)]
#[error(
    "embedding server has no compatible /tokenize endpoint; configure search.tokenizer_path or raise the server's supported input limits"
)]
pub struct TokenizerUnavailable;

#[derive(Debug, Clone)]
pub struct PreparedInput {
    pub text: String,
    pub tokens: Option<usize>,
    pub truncated: bool,
}

// Only known size diagnostics and their numeric fields may leave a provider
// response. Provider messages can contain the input itself or credentials.
fn input_size_error(status: reqwest::StatusCode, message: &str) -> Option<InputSizeError> {
    let message = message.to_ascii_lowercase();
    let kind = if matches!(status.as_u16(), 400 | 413 | 422)
        && (message.contains("larger than the max context size")
            || message.contains("exceeds the available context size")
            || message.contains("maximum context length"))
    {
        SizeLimit::Context
    } else if matches!(status.as_u16(), 400 | 500)
        && message.contains("increase the physical batch size")
    {
        SizeLimit::PhysicalBatch
    } else {
        return None;
    };
    fn number(message: &str, pattern: &str) -> Option<usize> {
        regex::Regex::new(pattern).ok()?.captures(message)?[1]
            .parse()
            .ok()
    }
    Some(InputSizeError {
        kind,
        tokens: number(&message, r"input \((\d+) tokens\)"),
        limit: number(
            &message,
            r"(?:max context size|available context size|maximum context length)(?: is)?[ :\(]+(\d+)",
        ),
    })
}

impl HttpEmbedder {
    pub fn new(endpoint: &str, api_key_env: &str) -> Result<Self> {
        Self::with_api_key(endpoint, env::var(api_key_env).unwrap_or_default())
    }

    fn with_api_key(endpoint: &str, api_key: String) -> Result<Self> {
        if endpoint.is_empty() {
            bail!("embedding endpoint is empty");
        }
        let endpoint = endpoint.trim_end_matches('/').to_owned();
        let parsed = reqwest::Url::parse(&endpoint).context("invalid embedding endpoint")?;
        // Reqwest turns URL userinfo into Basic auth even without an API key.
        let credentials =
            !api_key.is_empty() || !parsed.username().is_empty() || parsed.password().is_some();
        if credentials && parsed.scheme() != "https" {
            bail!("embedding credentials require HTTPS; refusing cleartext HTTP or other schemes");
        }
        Ok(Self {
            endpoint,
            api_key,
            client: reqwest::Client::builder()
                .https_only(credentials)
                .redirect(reqwest::redirect::Policy::none())
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

    pub fn preparation_key(
        &self,
        max_chars: usize,
        max_tokens: usize,
        tokenizer_path: &str,
    ) -> String {
        preparation_key(&self.endpoint, max_chars, max_tokens, tokenizer_path)
    }

    fn tokenizer_url(&self, path: &str) -> Result<reqwest::Url> {
        let mut url = reqwest::Url::parse(&self.endpoint)?;
        if path.is_empty() {
            let base = url.path().trim_end_matches('/').trim_end_matches("/v1");
            url.set_path(&format!("{base}/tokenize"));
        } else {
            if !path.starts_with('/') || path.starts_with("//") || path.contains(['?', '#']) {
                bail!("search.tokenizer_path must be an absolute path on the embedding server");
            }
            url.set_path(path);
        }
        url.set_query(None);
        url.set_fragment(None);
        Ok(url)
    }

    pub async fn token_count(&self, input: &str, tokenizer_path: &str) -> Result<usize> {
        let mut request = self
            .client
            .post(self.tokenizer_url(tokenizer_path)?)
            .json(&serde_json::json!({"content":input,"add_special":true,"parse_special":true}));
        if !self.api_key.is_empty() {
            request = request.bearer_auth(&self.api_key);
        }
        let response = request
            .send()
            .await
            .map_err(reqwest::Error::without_url)
            .context("posting tokenization request")?;
        let status = response.status();
        if matches!(status.as_u16(), 404 | 405 | 501) {
            return Err(TokenizerUnavailable.into());
        }
        if !status.is_success() {
            bail!("tokenization endpoint returned {status}");
        }
        #[derive(Deserialize)]
        struct Tokens {
            tokens: Vec<u32>,
        }
        let parsed: Tokens = response
            .json()
            .await
            .map_err(|_| anyhow::anyhow!("tokenization endpoint returned an invalid response"))?;
        Ok(parsed.tokens.len())
    }

    pub async fn fit_input(
        &self,
        input: &mut PreparedInput,
        budget: usize,
        tokenizer_path: &str,
    ) -> Result<()> {
        if budget == 0 {
            bail!("embedding token budget must be positive");
        }
        let count = match input.tokens {
            Some(count) => count,
            None => self.token_count(&input.text, tokenizer_path).await?,
        };
        input.tokens = Some(count);
        if count <= budget {
            return Ok(());
        }
        let boundaries: Vec<usize> = input
            .text
            .char_indices()
            .map(|(offset, _)| offset)
            .chain(std::iter::once(input.text.len()))
            .collect();
        let (mut low, mut high) = (0, boundaries.len() - 1);
        let mut best = None;
        while low + 1 < high {
            let mid = low + (high - low) / 2;
            let tokens = self
                .token_count(&input.text[..boundaries[mid]], tokenizer_path)
                .await?;
            if tokens <= budget {
                best = Some((boundaries[mid], tokens));
                low = mid;
            } else {
                high = mid;
            }
        }
        let Some((end, tokens)) = best else {
            return Err(InputSizeError {
                kind: SizeLimit::Context,
                tokens: Some(count),
                limit: Some(budget),
            }
            .into());
        };
        input.text.truncate(end);
        input.tokens = Some(tokens);
        input.truncated = true;
        Ok(())
    }

    pub async fn recover_input(
        &self,
        model: &str,
        input: &mut PreparedInput,
        mut error: InputSizeError,
        tokenizer_path: &str,
    ) -> Result<Vec<f32>> {
        // Bound recovery even if a provider reports inconsistent limits.
        for _ in 0..8 {
            let count = match input.tokens {
                Some(count) => count,
                None => self.token_count(&input.text, tokenizer_path).await?,
            };
            input.tokens = Some(count);
            let budget = match error.limit {
                Some(limit) if limit < count => limit,
                _ => count / 2,
            };
            if budget == 0 {
                break;
            }
            self.fit_input(input, budget, tokenizer_path).await?;
            match self.embed(model, std::slice::from_ref(&input.text)).await {
                Ok(mut vectors) => return Ok(vectors.remove(0)),
                Err(next) => match next.downcast_ref::<InputSizeError>() {
                    Some(size) => error = size.clone(),
                    None => return Err(next),
                },
            }
        }
        Err(error.into())
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
        let response = request
            .send()
            .await
            .map_err(reqwest::Error::without_url)
            .context("posting embeddings request")?;
        let status = response.status();
        let bytes = response
            .bytes()
            .await
            .map_err(reqwest::Error::without_url)
            .context("reading embeddings response")?;
        let parsed = serde_json::from_slice::<EmbeddingResponse>(&bytes);
        if !status.is_success() {
            if let Ok(parsed) = parsed
                && let Some(error) = parsed.error
                && let Some(error) = input_size_error(status, &error.message)
            {
                return Err(anyhow::Error::new(error)
                    .context(format!("embeddings endpoint returned {status}")));
            }
            bail!("embeddings endpoint returned {status}");
        }
        let parsed = parsed
            .map_err(|_| anyhow::anyhow!("embedding endpoint returned an invalid response"))?;
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

pub fn preparation_key(
    endpoint: &str,
    max_chars: usize,
    max_tokens: usize,
    tokenizer_path: &str,
) -> String {
    crate::trace::content_hash(
        &serde_json::json!([
            "token-budget-v1",
            endpoint.trim_end_matches('/'),
            max_chars,
            max_tokens,
            tokenizer_path
        ])
        .to_string(),
    )
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
        .as_chunks::<4>()
        .0
        .iter()
        .map(|bytes| f32::from_le_bytes(*bytes))
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

    #[test]
    fn credentials_require_https_including_loopback() {
        for endpoint in [
            "http://embeddings.example/v1",
            "http://127.0.0.1:9000/v1",
            "http://127.1:9000/v1",
            "http://[::1]:9000/v1",
            "http://localhost:9000/v1",
            "http://localhost.:9000/v1",
            "ftp://embeddings.example/v1",
        ] {
            let error = HttpEmbedder::with_api_key(endpoint, "synthetic-key".into())
                .err()
                .expect("credentials must require HTTPS");
            assert!(!error.to_string().contains(endpoint));
            assert!(!error.to_string().contains("synthetic-key"));
        }
    }

    #[test]
    fn url_credentials_require_https_without_an_api_key() {
        for endpoint in [
            "http://synthetic-user:synthetic-password@embeddings.example/v1",
            "http://synthetic-user@embeddings.example/v1",
            "http://:synthetic-password@embeddings.example/v1",
            "http://synthetic-user:synthetic-password@localhost:9000/v1",
        ] {
            let error = HttpEmbedder::new(endpoint, "").err().unwrap();
            let message = error.to_string();
            assert!(!message.contains("synthetic-user"));
            assert!(!message.contains("synthetic-password"));
            assert!(!message.contains(endpoint));
        }
    }

    #[tokio::test]
    async fn credentialed_client_blocks_both_posts_if_endpoint_downgrades() {
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        listener.set_nonblocking(true).unwrap();
        for (endpoint, key, userinfo) in [
            ("https://embeddings.example/v1", "synthetic-key", ""),
            (
                "https://synthetic-user:synthetic-password@embeddings.example/v1",
                "",
                "synthetic-user:synthetic-password@",
            ),
        ] {
            let mut client = HttpEmbedder::with_api_key(endpoint, key.into()).unwrap();
            // Exercise the transport invariant independently of constructor rejection.
            client.endpoint = format!("http://{userinfo}{}/v1", listener.local_addr().unwrap());
            assert!(
                client
                    .embed("synthetic-model", &["synthetic-input".into()])
                    .await
                    .is_err()
            );
            for path in ["", "/custom/tokenize"] {
                assert!(client.token_count("synthetic-input", path).await.is_err());
            }
            assert!(
                matches!(listener.accept(), Err(error) if error.kind() == std::io::ErrorKind::WouldBlock)
            );
        }
    }

    #[tokio::test]
    async fn credential_free_http_still_embeds_and_tokenizes() {
        use axum::{Json, Router, http::HeaderMap, routing::post};
        async fn response(headers: HeaderMap) -> Json<serde_json::Value> {
            assert!(!headers.contains_key("authorization"));
            Json(serde_json::json!({"tokens":[1,2],"data":[{"index":0,"embedding":[1.0]}]}))
        }
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let endpoint = format!("http://{}/v1", listener.local_addr().unwrap());
        let app = Router::new()
            .route("/v1/embeddings", post(response))
            .route("/tokenize", post(response))
            .route("/custom/tokenize", post(response));
        let server = tokio::spawn(async move {
            axum::serve(listener, app).await.unwrap();
        });
        let client = HttpEmbedder::new(&endpoint, "").unwrap();
        assert_eq!(
            client
                .embed("synthetic-model", &["synthetic-input".into()])
                .await
                .unwrap(),
            vec![vec![1.0]]
        );
        for path in ["", "/custom/tokenize"] {
            assert_eq!(
                client.token_count("synthetic-input", path).await.unwrap(),
                2
            );
        }
        server.abort();
    }

    #[test]
    fn size_diagnostics_only_expose_known_numeric_fields() {
        let error = input_size_error(reqwest::StatusCode::BAD_REQUEST,
            "input (8533 tokens) is larger than the max context size (2048): private-input secret=abc"
        ).unwrap();
        assert_eq!(error.kind, SizeLimit::Context);
        assert_eq!((error.tokens, error.limit), (Some(8533), Some(2048)));
        assert!(!error.to_string().contains("private-input"));
        assert!(!error.to_string().contains("abc"));
        assert!(
            input_size_error(
                reqwest::StatusCode::UNAUTHORIZED,
                "input (8533 tokens) is larger than the max context size (2048)"
            )
            .is_none()
        );
        assert!(
            input_size_error(
                reqwest::StatusCode::INTERNAL_SERVER_ERROR,
                "backend unavailable: private-input"
            )
            .is_none()
        );
    }

    #[test]
    fn tokenizer_paths_keep_input_and_credentials_on_the_embedding_origin() {
        let client = HttpEmbedder::new("https://embeddings.example/proxy/v1", "").unwrap();
        assert_eq!(
            client.tokenizer_url("").unwrap().as_str(),
            "https://embeddings.example/proxy/tokenize"
        );
        assert_eq!(
            client.tokenizer_url("/custom/tokenize").unwrap().as_str(),
            "https://embeddings.example/custom/tokenize"
        );
        for path in [
            "https://other.example/tokenize",
            "//other.example/tokenize",
            "/tokenize?key=secret",
            "/tokenize#fragment",
        ] {
            assert!(client.tokenizer_url(path).is_err());
        }
    }
}
