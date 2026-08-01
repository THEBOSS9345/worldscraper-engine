//! Diagnostic: query the live Tantivy index from the command line.
//!
//! Useful for confirming the index is being populated without going through the
//! UI, and for debugging ranking. Opening the index read-only from a second
//! process is safe — Tantivy allows many readers alongside the single writer.
//!
//!   cargo run --example query -- "rust async"

use std::path::PathBuf;

use tantivy::collector::TopDocs;
use tantivy::query::QueryParser;
use tantivy::schema::Value;
use tantivy::{Index, TantivyDocument};

fn main() -> tantivy::Result<()> {
    let query_str: String = std::env::args().skip(1).collect::<Vec<_>>().join(" ");
    let query_str = if query_str.trim().is_empty() {
        "search".to_string()
    } else {
        query_str
    };

    let dir: PathBuf = dirs::config_dir()
        .expect("no config dir")
        .join("WorldScraper")
        .join("index");

    println!("index: {}", dir.display());
    let index = Index::open_in_dir(&dir)?;
    let reader = index.reader()?;
    let searcher = reader.searcher();
    println!("documents indexed: {}", searcher.num_docs());
    println!("segments: {}", searcher.segment_readers().len());

    let schema = index.schema();
    let title = schema.get_field("title").expect("title field");
    let description = schema.get_field("description").expect("description field");
    let body = schema.get_field("body").expect("body field");
    let url = schema.get_field("url").expect("url field");
    let category = schema.get_field("category").expect("category field");

    let mut parser = QueryParser::for_index(&index, vec![title, description, body]);
    parser.set_field_boost(title, 4.0);
    parser.set_conjunction_by_default();

    let query = parser.parse_query(&query_str)?;
    let hits = searcher.search(&query, &TopDocs::with_limit(10).order_by_score())?;

    println!("\nquery {query_str:?} -> {} hits shown\n", hits.len());
    for (score, addr) in hits {
        let doc: TantivyDocument = searcher.doc(addr)?;
        let get = |f| {
            doc.get_first(f)
                .and_then(|v| v.as_str())
                .unwrap_or("")
                .to_string()
        };
        let t = get(title);
        println!("  [{score:.3}] {}", if t.is_empty() { "(untitled)" } else { &t });
        println!("          {}", get(url));
        println!("          category={}  {}", get(category), truncate(&get(description), 90));
    }

    Ok(())
}

fn truncate(s: &str, n: usize) -> String {
    if s.chars().count() <= n {
        return s.to_string();
    }
    s.chars().take(n).collect::<String>() + "…"
}
