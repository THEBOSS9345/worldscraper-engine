//! Tantivy search index and the loop that drains the crawler's document spool.
//!
//! The handoff is deliberately pull-based and acknowledged only after a
//! successful commit: the engine keeps documents until this side confirms they
//! are durable, so a crash on either process replays rather than loses pages.

use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use anyhow::{Context, Result};
use serde::{Deserialize, Serialize};
use tantivy::collector::{Count, TopDocs};
use tantivy::query::{BooleanQuery, Occur, Query, QueryParser, TermQuery};
use tantivy::schema::{
    Field, IndexRecordOption, Schema, Value, FAST, INDEXED, STORED, STRING, TEXT,
};
use tantivy::{doc, Index, IndexReader, IndexWriter, Order, ReloadPolicy, TantivyDocument, Term};

/// How much heap the index writer may use before flushing a segment.
const WRITER_HEAP: usize = 256 * 1024 * 1024;
/// Documents buffered before forcing a commit.
const COMMIT_DOCS: u64 = 2_000;
/// Longest a document may sit uncommitted, so search stays near-live.
const COMMIT_INTERVAL: Duration = Duration::from_secs(10);
/// Stored preview length. The full body is indexed but not stored; this is the
/// window the UI draws contextual snippets from. Big enough that a query term
/// usually falls inside it, small enough that the index stays compact at scale.
const EXCERPT_CHARS: usize = 1_200;

/// Field handles for the schema.
#[derive(Clone, Copy)]
pub struct Fields {
    pub url: Field,
    pub host: Field,
    pub site: Field,
    pub title: Field,
    pub description: Field,
    pub body: Field,
    pub excerpt: Field,
    pub lang: Field,
    pub category: Field,
    pub status: Field,
    pub depth: Field,
    pub bytes: Field,
    pub fetched_at: Field,
}

fn build_schema() -> (Schema, Fields) {
    let mut b = Schema::builder();

    let fields = Fields {
        // STRING is indexed raw (not tokenized) so a URL can be deleted by term
        // when a page is recrawled.
        url: b.add_text_field("url", STRING | STORED),
        host: b.add_text_field("host", STRING | STORED),
        site: b.add_text_field("site", STRING | STORED),
        title: b.add_text_field("title", TEXT | STORED),
        description: b.add_text_field("description", TEXT | STORED),
        // Indexed but not stored: matching uses the full text, display uses the
        // excerpt below.
        body: b.add_text_field("body", TEXT),
        excerpt: b.add_text_field("excerpt", STORED),
        lang: b.add_text_field("lang", STRING | STORED),
        category: b.add_text_field("category", STRING | STORED),
        status: b.add_i64_field("status", STORED),
        depth: b.add_u64_field("depth", STORED),
        bytes: b.add_u64_field("bytes", STORED),
        fetched_at: b.add_i64_field("fetched_at", STORED | INDEXED | FAST),
    };

    (b.build(), fields)
}

/// A document as delivered by the engine's spool endpoint.
#[derive(Debug, Deserialize)]
pub struct SpoolDoc {
    pub seq: u64,
    pub url: String,
    #[serde(default)]
    pub host: String,
    #[serde(default)]
    pub site: String,
    #[serde(default)]
    pub title: String,
    #[serde(default)]
    pub description: String,
    #[serde(default)]
    pub body: String,
    #[serde(default)]
    pub lang: String,
    #[serde(default)]
    pub category: String,
    #[serde(default)]
    pub status: i64,
    #[serde(default)]
    pub bytes: i64,
    #[serde(default)]
    pub depth: i64,
    #[serde(default)]
    pub fetched_at: i64,
}

/// One search result row.
#[derive(Debug, Serialize)]
pub struct Hit {
    pub url: String,
    pub host: String,
    pub site: String,
    pub title: String,
    pub description: String,
    pub excerpt: String,
    pub lang: String,
    pub category: String,
    pub status: i64,
    pub depth: u64,
    pub bytes: u64,
    #[serde(rename = "fetchedAt")]
    pub fetched_at: i64,
    pub score: f32,
}

/// The payload returned to the UI.
#[derive(Debug, Serialize)]
pub struct SearchResponse {
    pub hits: Vec<Hit>,
    pub total: usize,
    #[serde(rename = "tookMs")]
    pub took_ms: u64,
    pub query: String,
    pub offset: usize,
    /// True when a strict all-terms search found nothing and the engine fell
    /// back to matching any term, so the UI can say so.
    pub relaxed: bool,
}

/// Search parameters coming from the UI.
#[derive(Debug, Default, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct SearchParams {
    pub query: String,
    #[serde(default)]
    pub limit: Option<usize>,
    #[serde(default)]
    pub offset: Option<usize>,
    #[serde(default)]
    pub category: Option<String>,
    #[serde(default)]
    pub host: Option<String>,
    #[serde(default)]
    pub lang: Option<String>,
    /// "relevance" (default) or "recent".
    #[serde(default)]
    pub sort: Option<String>,
}

/// Live counters describing the index.
#[derive(Debug, Serialize, Clone)]
#[serde(rename_all = "camelCase")]
pub struct IndexStats {
    pub docs: u64,
    pub segments: usize,
    pub disk_bytes: u64,
    pub cursor: u64,
    pub indexed_total: u64,
    pub pending_commit: u64,
    pub last_commit_unix: i64,
    pub indexing: bool,
}

/// The search index plus its background ingest loop.
pub struct SearchIndex {
    index: Index,
    reader: IndexReader,
    writer: Mutex<IndexWriter>,
    fields: Fields,
    dir: PathBuf,

    cursor: AtomicU64,
    indexed_total: AtomicU64,
    uncommitted: AtomicU64,
    last_commit: AtomicU64,
    indexing: AtomicBool,
}

impl SearchIndex {
    /// Opens, or creates, the index directory.
    pub fn open(dir: &Path) -> Result<Arc<Self>> {
        std::fs::create_dir_all(dir)
            .with_context(|| format!("create index dir {}", dir.display()))?;

        let (schema, fields) = build_schema();
        let index = match Index::open_in_dir(dir) {
            Ok(idx) => idx,
            Err(_) => Index::create_in_dir(dir, schema.clone())
                .with_context(|| format!("create index in {}", dir.display()))?,
        };

        let writer: IndexWriter = index
            .writer(WRITER_HEAP)
            .context("create index writer")?;

        let reader = index
            .reader_builder()
            .reload_policy(ReloadPolicy::OnCommitWithDelay)
            .try_into()
            .context("create index reader")?;

        // The ingest cursor is persisted next to the index so a restart resumes
        // exactly where the last commit finished.
        let cursor = read_cursor(dir);

        Ok(Arc::new(Self {
            index,
            reader,
            writer: Mutex::new(writer),
            fields,
            dir: dir.to_path_buf(),
            cursor: AtomicU64::new(cursor),
            indexed_total: AtomicU64::new(0),
            uncommitted: AtomicU64::new(0),
            last_commit: AtomicU64::new(now_unix() as u64),
            indexing: AtomicBool::new(false),
        }))
    }

    /// The highest spool sequence this index has durably committed.
    pub fn cursor(&self) -> u64 {
        self.cursor.load(Ordering::Relaxed)
    }

    /// Adds a batch of documents. Returns the highest sequence staged.
    pub fn add_batch(&self, docs: &[SpoolDoc]) -> Result<u64> {
        if docs.is_empty() {
            return Ok(self.cursor());
        }
        let f = self.fields;
        let mut highest = 0u64;

        let writer = self.writer.lock().map_err(|_| {
            anyhow::anyhow!("index writer lock poisoned")
        })?;

        for d in docs {
            highest = highest.max(d.seq);

            // Recrawls must replace, not duplicate.
            writer.delete_term(Term::from_field_text(f.url, &d.url));

            let excerpt: String = d.body.chars().take(EXCERPT_CHARS).collect();

            writer.add_document(doc!(
                f.url         => d.url.clone(),
                f.host        => d.host.clone(),
                f.site        => d.site.clone(),
                f.title       => d.title.clone(),
                f.description => d.description.clone(),
                f.body        => d.body.clone(),
                f.excerpt     => excerpt,
                f.lang        => d.lang.clone(),
                f.category    => d.category.clone(),
                f.status      => d.status,
                f.depth       => d.depth.max(0) as u64,
                f.bytes       => d.bytes.max(0) as u64,
                f.fetched_at  => d.fetched_at,
            ))?;
        }

        self.uncommitted
            .fetch_add(docs.len() as u64, Ordering::Relaxed);
        Ok(highest)
    }

    /// Commits staged documents and advances the durable cursor.
    pub fn commit(&self, up_to_seq: u64) -> Result<()> {
        let staged = self.uncommitted.load(Ordering::Relaxed);
        if staged == 0 && up_to_seq <= self.cursor() {
            return Ok(());
        }

        {
            let mut writer = self
                .writer
                .lock()
                .map_err(|_| anyhow::anyhow!("index writer lock poisoned"))?;
            writer.commit().context("commit index")?;
        }

        self.uncommitted.store(0, Ordering::Relaxed);
        self.indexed_total.fetch_add(staged, Ordering::Relaxed);
        self.last_commit
            .store(now_unix() as u64, Ordering::Relaxed);

        if up_to_seq > self.cursor() {
            self.cursor.store(up_to_seq, Ordering::Relaxed);
            write_cursor(&self.dir, up_to_seq);
        }
        Ok(())
    }

    /// Live index statistics for the dashboard.
    pub fn stats(&self) -> IndexStats {
        let searcher = self.reader.searcher();
        IndexStats {
            docs: searcher.num_docs(),
            segments: searcher.segment_readers().len(),
            disk_bytes: dir_size(&self.dir),
            cursor: self.cursor(),
            indexed_total: self.indexed_total.load(Ordering::Relaxed),
            pending_commit: self.uncommitted.load(Ordering::Relaxed),
            last_commit_unix: self.last_commit.load(Ordering::Relaxed) as i64,
            indexing: self.indexing.load(Ordering::Relaxed),
        }
    }

    fn set_indexing(&self, v: bool) {
        self.indexing.store(v, Ordering::Relaxed);
    }

    /// Runs a query.
    pub fn search(&self, p: &SearchParams) -> Result<SearchResponse> {
        let started = Instant::now();
        let limit = p.limit.unwrap_or(25).clamp(1, 200);
        let offset = p.offset.unwrap_or(0).min(10_000);
        let raw = p.query.trim();

        let searcher = self.reader.searcher();
        let f = self.fields;

        let filters = self.filter_clauses(p);
        let recent = p.sort.as_deref() == Some("recent");

        // Strict first: every term must appear. Only if that finds nothing do
        // we relax to "any term", which stops a long query from dead-ending.
        let mut relaxed = false;
        let mut query = self.build_query(raw, &filters, true)?;
        let mut total = searcher.search(&*query, &Count)?;

        if total == 0 && raw.split_whitespace().count() > 1 {
            let loose = self.build_query(raw, &filters, false)?;
            let loose_total = searcher.search(&*loose, &Count)?;
            if loose_total > 0 {
                relaxed = true;
                query = loose;
                total = loose_total;
            }
        }

        let addrs: Vec<(f32, tantivy::DocAddress)> = if recent {
            let collector = TopDocs::with_limit(limit)
                .and_offset(offset)
                .order_by_fast_field::<i64>("fetched_at", Order::Desc);
            searcher
                .search(&*query, &collector)?
                .into_iter()
                // Ordering by a fast field yields the sort key, absent for
                // documents indexed before the field existed.
                .map(|(ts, a)| (ts.unwrap_or_default() as f32, a))
                .collect()
        } else {
            // Over-fetch, then re-rank. BM25 alone happily puts a deep tag page
            // above a site's front page; the blend below prefers shallow, tidy
            // URLs without letting them outrank genuine relevance.
            let window = (offset + limit).saturating_mul(3).clamp(limit, 600);
            let collector = TopDocs::with_limit(window).order_by_score();
            let mut scored = searcher.search(&*query, &collector)?;

            let best = scored.first().map(|(s, _)| *s).unwrap_or(1.0).max(0.001);
            let mut ranked: Vec<(f32, f32, tantivy::DocAddress)> = Vec::with_capacity(scored.len());
            for (score, addr) in scored.drain(..) {
                let d: TantivyDocument = searcher.doc(addr)?;
                let quality = quality_bonus(&d, f);
                ranked.push((score / best * quality, score, addr));
            }
            ranked.sort_by(|a, b| b.0.partial_cmp(&a.0).unwrap_or(std::cmp::Ordering::Equal));

            ranked
                .into_iter()
                .skip(offset)
                .take(limit)
                .map(|(_, raw_score, addr)| (raw_score, addr))
                .collect()
        };

        let mut hits = Vec::with_capacity(addrs.len());
        for (score, addr) in addrs {
            let d: TantivyDocument = searcher.doc(addr)?;
            hits.push(Hit {
                url: text_of(&d, f.url),
                host: text_of(&d, f.host),
                site: text_of(&d, f.site),
                title: text_of(&d, f.title),
                description: text_of(&d, f.description),
                excerpt: text_of(&d, f.excerpt),
                lang: text_of(&d, f.lang),
                category: text_of(&d, f.category),
                status: i64_of(&d, f.status),
                depth: u64_of(&d, f.depth),
                bytes: u64_of(&d, f.bytes),
                fetched_at: i64_of(&d, f.fetched_at),
                score,
            });
        }

        Ok(SearchResponse {
            hits,
            total,
            took_ms: started.elapsed().as_millis() as u64,
            query: raw.to_string(),
            offset,
            relaxed,
        })
    }

    /// Term-level filters from the UI's chips.
    fn filter_clauses(&self, p: &SearchParams) -> Vec<(Occur, Box<dyn Query>)> {
        let f = self.fields;
        let mut out: Vec<(Occur, Box<dyn Query>)> = Vec::new();
        for (field, value) in [
            (f.category, p.category.as_deref()),
            (f.host, p.host.as_deref()),
            (f.lang, p.lang.as_deref()),
        ] {
            if let Some(v) = value.map(str::trim).filter(|v| !v.is_empty()) {
                out.push((
                    Occur::Must,
                    Box::new(TermQuery::new(
                        Term::from_field_text(field, v),
                        IndexRecordOption::Basic,
                    )),
                ));
            }
        }
        out
    }

    /// Combines the user's text with the active filters.
    fn build_query(
        &self,
        raw: &str,
        filters: &[(Occur, Box<dyn Query>)],
        conjunctive: bool,
    ) -> Result<Box<dyn Query>> {
        let f = self.fields;
        let mut clauses: Vec<(Occur, Box<dyn Query>)> = Vec::new();

        if !raw.is_empty() {
            let mut parser =
                QueryParser::for_index(&self.index, vec![f.title, f.description, f.body, f.host]);
            // A title match means far more than a passing mention in the body.
            parser.set_field_boost(f.title, 5.0);
            parser.set_field_boost(f.description, 2.0);
            parser.set_field_boost(f.host, 2.0);
            if conjunctive {
                parser.set_conjunction_by_default();
            }

            let parsed = match parser.parse_query(raw) {
                Ok(q) => q,
                // Unbalanced quotes and stray operators are normal in a search
                // box; retry sanitized instead of surfacing a parse error.
                Err(_) => parser
                    .parse_query(&sanitize_query(raw))
                    .context("parse query")?,
            };
            clauses.push((Occur::Must, parsed));
        }

        for (occur, q) in filters {
            clauses.push((*occur, q.box_clone()));
        }

        Ok(if clauses.is_empty() {
            Box::new(tantivy::query::AllQuery)
        } else {
            Box::new(BooleanQuery::new(clauses))
        })
    }
}

/// A small multiplier favouring pages that look like destinations rather than
/// deep archive pages. Bounded so it re-orders near-ties without overriding
/// relevance.
fn quality_bonus(d: &TantivyDocument, f: Fields) -> f32 {
    let mut bonus = 1.0_f32;

    // Shallower pages are usually the ones a person wanted.
    let depth = u64_of(d, f.depth) as f32;
    bonus *= 1.0 + 0.18 / (1.0 + depth);

    // Long, parameter-laden URLs are usually generated pages.
    let url = d.get_first(f.url).and_then(|v| v.as_str()).unwrap_or("");
    let segments = url.matches('/').count().saturating_sub(2) as f32;
    bonus *= 1.0 + 0.10 / (1.0 + segments);
    if url.contains('?') {
        bonus *= 0.94;
    }

    // A page with no title is nearly useless in a result list.
    if d.get_first(f.title).and_then(|v| v.as_str()).unwrap_or("").is_empty() {
        bonus *= 0.75;
    }
    if d.get_first(f.description).and_then(|v| v.as_str()).unwrap_or("").is_empty() {
        bonus *= 0.95;
    }

    bonus
}

/// Drains the engine spool into the index forever.
///
/// Runs as a background task; every error path backs off and retries rather
/// than terminating, because this loop must survive the engine restarting
/// underneath it.
pub async fn ingest_loop(index: Arc<SearchIndex>, endpoint: Arc<crate::engine::Endpoint>) {
    let client = match reqwest::Client::builder()
        .timeout(Duration::from_secs(30))
        .build()
    {
        Ok(c) => c,
        Err(e) => {
            log::error!("indexer: cannot build http client: {e}");
            return;
        }
    };

    let mut idle_backoff = Duration::from_millis(400);
    let mut last_commit = Instant::now();
    // Highest spool sequence staged but not yet durably committed. It must not
    // live in the index's cursor, which only advances on a real commit.
    let mut pending_seq = index.cursor();

    loop {
        let Some((port, token)) = endpoint.get() else {
            // Engine not up yet.
            tokio::time::sleep(Duration::from_millis(750)).await;
            continue;
        };

        let after = index.cursor();
        let url = format!("http://127.0.0.1:{port}/api/spool?after={after}&n=1000");

        let fetched = client
            .get(&url)
            .header("X-WS-Token", token.as_str())
            .send()
            .await;

        let batch: SpoolBatch = match fetched {
            Ok(resp) if resp.status().is_success() => match resp.json().await {
                Ok(b) => b,
                Err(e) => {
                    log::warn!("indexer: bad spool payload: {e}");
                    tokio::time::sleep(Duration::from_secs(2)).await;
                    continue;
                }
            },
            Ok(resp) => {
                log::warn!("indexer: spool returned {}", resp.status());
                tokio::time::sleep(Duration::from_secs(2)).await;
                continue;
            }
            Err(e) => {
                log::debug!("indexer: spool unreachable: {e}");
                tokio::time::sleep(Duration::from_secs(2)).await;
                continue;
            }
        };

        let due_by_time = last_commit.elapsed() >= COMMIT_INTERVAL;

        if batch.docs.is_empty() {
            // Nothing new: commit anything still staged, then idle.
            if index.stats().pending_commit > 0 && due_by_time {
                commit_and_ack(&index, &client, port, &token, pending_seq).await;
                pending_seq = index.cursor();
                last_commit = Instant::now();
            }
            index.set_indexing(false);
            tokio::time::sleep(idle_backoff).await;
            idle_backoff = (idle_backoff * 2).min(Duration::from_secs(5));
            continue;
        }

        idle_backoff = Duration::from_millis(400);
        index.set_indexing(true);

        let count = batch.docs.len();
        let highest = match index.add_batch(&batch.docs) {
            Ok(h) => h,
            Err(e) => {
                log::error!("indexer: add batch failed: {e}");
                tokio::time::sleep(Duration::from_secs(2)).await;
                continue;
            }
        };
        pending_seq = pending_seq.max(highest);

        let staged = index.stats().pending_commit;
        if staged >= COMMIT_DOCS || due_by_time {
            commit_and_ack(&index, &client, port, &token, pending_seq).await;
            pending_seq = index.cursor();
            last_commit = Instant::now();
        }

        log::debug!("indexer: staged {count} documents up to seq {highest}");
    }
}

/// Commits the index and only then tells the engine it may drop the documents.
async fn commit_and_ack(
    index: &Arc<SearchIndex>,
    client: &reqwest::Client,
    port: u16,
    token: &str,
    seq: u64,
) {
    if let Err(e) = index.commit(seq) {
        log::error!("indexer: commit failed, documents will be replayed: {e}");
        return;
    }
    let url = format!("http://127.0.0.1:{port}/api/spool/ack");
    if let Err(e) = client
        .post(&url)
        .header("X-WS-Token", token)
        .json(&serde_json::json!({ "seq": seq }))
        .send()
        .await
    {
        // Not fatal: the engine will simply hand the same documents back and
        // the URL-term delete makes re-indexing idempotent.
        log::warn!("indexer: ack failed: {e}");
    }
}

#[derive(Deserialize)]
struct SpoolBatch {
    #[serde(default)]
    docs: Vec<SpoolDoc>,
    #[serde(default)]
    #[allow(dead_code)]
    last: u64,
}

// ------------------------------------------------------------------ helpers --

fn text_of(d: &TantivyDocument, f: Field) -> String {
    d.get_first(f)
        .and_then(|v| v.as_str())
        .unwrap_or_default()
        .to_string()
}

fn i64_of(d: &TantivyDocument, f: Field) -> i64 {
    d.get_first(f).and_then(|v| v.as_i64()).unwrap_or_default()
}

fn u64_of(d: &TantivyDocument, f: Field) -> u64 {
    d.get_first(f).and_then(|v| v.as_u64()).unwrap_or_default()
}

/// Strips characters the query parser treats as syntax, for the retry path.
fn sanitize_query(q: &str) -> String {
    q.chars()
        .map(|c| match c {
            '"' | '(' | ')' | '[' | ']' | '{' | '}' | '^' | '~' | ':' | '+' | '-' | '*' | '!'
            | '\\' | '/' => ' ',
            other => other,
        })
        .collect::<String>()
        .split_whitespace()
        .collect::<Vec<_>>()
        .join(" ")
}

fn cursor_path(dir: &Path) -> PathBuf {
    dir.join("ingest-cursor")
}

fn read_cursor(dir: &Path) -> u64 {
    std::fs::read_to_string(cursor_path(dir))
        .ok()
        .and_then(|s| s.trim().parse().ok())
        .unwrap_or(0)
}

fn write_cursor(dir: &Path, seq: u64) {
    let path = cursor_path(dir);
    let tmp = path.with_extension("tmp");
    if std::fs::write(&tmp, seq.to_string()).is_ok() {
        let _ = std::fs::rename(&tmp, &path);
    }
}

fn dir_size(dir: &Path) -> u64 {
    let mut total = 0;
    let Ok(entries) = std::fs::read_dir(dir) else {
        return 0;
    };
    for entry in entries.flatten() {
        match entry.metadata() {
            Ok(m) if m.is_file() => total += m.len(),
            Ok(m) if m.is_dir() => total += dir_size(&entry.path()),
            _ => {}
        }
    }
    total
}

fn now_unix() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}
