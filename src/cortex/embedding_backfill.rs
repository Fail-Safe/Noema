use super::*;
use crate::embedding::{InputSizeError, PreparedInput, TokenizerUnavailable};

struct Candidate {
    id: String,
    source_hash: String,
    title: String,
    input: PreparedInput,
}

impl Cortex {
    fn embedding_preparation_key(&self) -> Result<String> {
        let search = self.manifest.search.clone().unwrap_or_default();
        Ok(embedding::preparation_key(
            &self.manifest.resolved_embedding_endpoint()?,
            search.effective_max_chars(),
            search.max_tokens,
            &search.tokenizer_path,
        ))
    }

    fn deferred_embeddings(&self, model: &str, policy: &str) -> Result<usize> {
        Ok(self.connection.query_row(
            "SELECT COUNT(*) FROM embedding_failures f JOIN traces t ON t.id=f.trace_id
             WHERE t.trashed_at IS NULL AND f.embedding_model=?1
             AND f.source_hash=t.content_hash AND f.source_title=t.title
             AND f.preparation_key=?2 AND f.retry_after>?3",
            params![model, policy, trace::now_rfc3339()],
            |row| Ok(row.get::<_, i64>(0)? as usize),
        )?)
    }

    pub fn embedding_status(&self, model: &str) -> Result<EmbeddingStatus> {
        let policy = self.embedding_preparation_key()?;
        let embeddable: usize = self.connection.query_row(
            "SELECT COUNT(*) FROM traces WHERE trashed_at IS NULL",
            [],
            |row| Ok(row.get::<_, i64>(0)? as usize),
        )?;
        let with_row: usize = self.connection.query_row(
            "SELECT COUNT(*) FROM traces t JOIN trace_embeddings te ON te.trace_id=t.id WHERE t.trashed_at IS NULL",
            [], |row| Ok(row.get::<_, i64>(0)? as usize),
        )?;
        let (embedded, truncated): (usize, usize) = self.connection.query_row(
            "SELECT COUNT(*),COALESCE(SUM(te.truncated),0) FROM traces t
             JOIN trace_embeddings te ON te.trace_id=t.id
             WHERE t.trashed_at IS NULL AND te.embedding_model=?1 AND te.source_hash=t.content_hash
             AND te.source_title=t.title AND te.preparation_key=?2",
            params![model, policy],
            |row| {
                Ok((
                    row.get::<_, i64>(0)? as usize,
                    row.get::<_, i64>(1)? as usize,
                ))
            },
        )?;
        Ok(EmbeddingStatus {
            model: model.to_owned(),
            embeddable,
            embedded,
            truncated,
            stale: with_row - embedded,
            missing: embeddable - with_row,
            deferred: self.deferred_embeddings(model, &policy)?,
        })
    }

    pub async fn embed_backfill(
        &mut self,
        embedder: &HttpEmbedder,
        model: &str,
        options: &EmbedBackfillOptions,
    ) -> Result<EmbedBackfillResult> {
        if model.is_empty() {
            bail!("embedding model is empty");
        }
        let batch_size = if options.batch_size == 0 {
            64
        } else {
            options.batch_size.min(64)
        };
        let search = self.manifest.search.clone().unwrap_or_default();
        let max_chars = if options.max_chars == 0 {
            search.effective_max_chars()
        } else {
            options.max_chars
        };
        let policy = embedder.preparation_key(max_chars, search.max_tokens, &search.tokenizer_path);
        let mut sql = String::from(
            "SELECT t.id,COALESCE(t.content_hash,'') FROM traces t
             LEFT JOIN trace_embeddings te ON te.trace_id=t.id WHERE t.trashed_at IS NULL",
        );
        let mut values = Vec::new();
        if !options.force {
            sql.push_str(
                " AND (te.trace_id IS NULL OR te.embedding_model!=? OR te.source_hash!=t.content_hash
                  OR te.source_title!=t.title OR te.preparation_key!=?
                  OR t.content_hash IS NULL OR t.content_hash='')
                  AND NOT EXISTS (SELECT 1 FROM embedding_failures f WHERE f.trace_id=t.id
                    AND f.embedding_model=? AND f.source_hash=t.content_hash AND f.source_title=t.title
                    AND f.preparation_key=? AND f.retry_after>?)",
            );
            values.extend([
                Value::Text(model.to_owned()),
                Value::Text(policy.clone()),
                Value::Text(model.to_owned()),
                Value::Text(policy.clone()),
                Value::Text(trace::now_rfc3339()),
            ]);
        }
        sql.push_str(" ORDER BY t.created_at,t.id");
        if options.limit > 0 {
            sql.push_str(" LIMIT ?");
            values.push(Value::Integer(options.limit as i64));
        }
        let candidates = {
            let mut statement = self.connection.prepare(&sql)?;
            statement
                .query_map(params_from_iter(values), |row| {
                    Ok((row.get::<_, String>(0)?, row.get::<_, String>(1)?))
                })?
                .collect::<rusqlite::Result<Vec<_>>>()?
        };
        let mut result = EmbedBackfillResult {
            considered: candidates.len(),
            deferred: if options.force {
                0
            } else {
                self.deferred_embeddings(model, &policy)?
            },
            ..Default::default()
        };
        for batch in candidates.chunks(batch_size) {
            let mut items = Vec::with_capacity(batch.len());
            for (id, indexed_hash) in batch {
                let Ok((row, parsed)) = self.get_trace(id) else {
                    continue;
                };
                let source_hash = if indexed_hash.is_empty() {
                    let hash = trace::content_hash(&parsed.body);
                    self.connection.execute(
                        "UPDATE traces SET content_hash=?1 WHERE id=?2 AND (content_hash IS NULL OR content_hash='')",
                        params![hash, id],
                    )?;
                    hash
                } else {
                    indexed_hash.clone()
                };
                let full_text = embedding::text(&row.title, &parsed.body, 0);
                let text: String = full_text.chars().take(max_chars).collect();
                let mut item = Candidate {
                    id: id.clone(),
                    source_hash,
                    title: row.title,
                    input: PreparedInput {
                        truncated: text.len() < full_text.len(),
                        text,
                        tokens: None,
                    },
                };
                if search.max_tokens > 0
                    && let Err(error) = embedder
                        .fit_input(&mut item.input, search.max_tokens, &search.tokenizer_path)
                        .await
                {
                    if error.is::<InputSizeError>() {
                        self.defer_embedding(
                            &item,
                            model,
                            &policy,
                            &error.to_string(),
                            &mut result,
                        )?;
                        continue;
                    }
                    return Err(error.context(format!("preparing embedding for trace {}", item.id)));
                }
                items.push(item);
            }
            if items.is_empty() {
                continue;
            }
            // Persist each successful split immediately. A later service failure
            // must not discard embeddings that have already been computed.
            let mut pending: Vec<_> = std::iter::once(0..items.len()).collect();
            while let Some(range) = pending.pop() {
                let inputs: Vec<_> = items[range.clone()]
                    .iter()
                    .map(|item| item.input.text.clone())
                    .collect();
                match embedder.embed(model, &inputs).await {
                    Ok(vectors) => {
                        self.store_embeddings(&items[range], vectors, model, &policy, &mut result)?;
                    }
                    Err(error) => {
                        let Some(size) = error.downcast_ref::<InputSizeError>().cloned() else {
                            return Err(error.context(format!(
                                "embedding batch starting at trace {}; {} embeddings saved in this pass",
                                items[range.start].id, result.embedded,
                            )));
                        };
                        if range.len() > 1 {
                            let mid = range.start + range.len() / 2;
                            pending.push(mid..range.end);
                            pending.push(range.start..mid);
                            continue;
                        }
                        let item = &mut items[range.start];
                        match embedder.recover_input(model, &mut item.input, size.clone(), &search.tokenizer_path).await {
                            Ok(vector) => self.store_embeddings(std::slice::from_ref(item), vec![vector], model, &policy, &mut result)?,
                            Err(error) if error.is::<InputSizeError>() || error.is::<TokenizerUnavailable>() => {
                                self.defer_embedding(item, model, &policy, &format!("{size}; {error}"), &mut result)?;
                            }
                            Err(error) => return Err(error.context(format!(
                                "recovering embedding for trace {}; {} embeddings saved in this pass", item.id, result.embedded,
                            ))),
                        }
                    }
                }
            }
        }
        Ok(result)
    }

    fn defer_embedding(
        &self,
        item: &Candidate,
        model: &str,
        policy: &str,
        reason: &str,
        result: &mut EmbedBackfillResult,
    ) -> Result<()> {
        let retry_after =
            (Utc::now() + Duration::hours(1)).to_rfc3339_opts(chrono::SecondsFormat::Secs, true);
        self.connection.execute(
            "INSERT INTO embedding_failures(trace_id,embedding_model,source_hash,source_title,preparation_key,reason,retry_after)
             VALUES (?1,?2,?3,?4,?5,?6,?7) ON CONFLICT(trace_id) DO UPDATE SET
             embedding_model=excluded.embedding_model,source_hash=excluded.source_hash,source_title=excluded.source_title,
             preparation_key=excluded.preparation_key,reason=excluded.reason,retry_after=excluded.retry_after",
            params![item.id, model, item.source_hash, item.title, policy, reason, retry_after],
        )?;
        result.failures.push(EmbedFailure {
            trace_id: item.id.clone(),
            reason: reason.to_owned(),
            retry_after,
        });
        Ok(())
    }

    fn store_embeddings(
        &self,
        items: &[Candidate],
        vectors: Vec<Vec<f32>>,
        model: &str,
        policy: &str,
        result: &mut EmbedBackfillResult,
    ) -> Result<()> {
        let tx = self.connection.unchecked_transaction()?;
        let mut embedded = 0;
        let mut truncated = 0;
        for (item, mut vector) in items.iter().zip(vectors) {
            embedding::normalize(&mut vector);
            let changed = tx.execute(
                "INSERT INTO trace_embeddings(trace_id,embedding_model,dim,embedding,source_hash,updated_at,
                    preparation_key,source_title,input_hash,input_tokens,truncated)
                 SELECT ?1,?2,?3,?4,?5,?6,?7,?8,?9,?10,?11 FROM traces
                 WHERE id=?1 AND content_hash=?5 AND title=?8 AND trashed_at IS NULL
                 ON CONFLICT(trace_id) DO UPDATE SET embedding_model=excluded.embedding_model,
                 dim=excluded.dim,embedding=excluded.embedding,source_hash=excluded.source_hash,updated_at=excluded.updated_at,
                 preparation_key=excluded.preparation_key,source_title=excluded.source_title,input_hash=excluded.input_hash,
                 input_tokens=excluded.input_tokens,truncated=excluded.truncated",
                params![item.id, model, vector.len() as i64, embedding::encode(&vector), item.source_hash, trace::now_rfc3339(),
                    policy, item.title, trace::content_hash(&item.input.text), item.input.tokens.map(|tokens| tokens as i64), item.input.truncated],
            )?;
            if changed > 0 {
                tx.execute(
                    "DELETE FROM embedding_failures WHERE trace_id=?1",
                    [&item.id],
                )?;
                embedded += 1;
                truncated += usize::from(item.input.truncated);
            }
        }
        tx.commit()?;
        result.embedded += embedded;
        result.truncated += truncated;
        Ok(())
    }
}
