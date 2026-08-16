use std::{
    fs,
    io::{self, Read, Write},
    path::PathBuf,
};

use anyhow::{Context, Result, bail};
use chrono::Utc;
use clap::{Args, CommandFactory, Parser, Subcommand, ValueEnum};
use clap_complete::{Shell, generate};
use tokio_util::sync::CancellationToken;

use crate::{
    VERSION,
    config::Config,
    consolidation::{DistillationConfig, HeuristicConfig, run_distillation_pass},
    cortex::{Cortex, EmbedBackfillOptions, ListOptions, SemanticOptions, write_manifest},
    embedding::HttpEmbedder,
    eventsig,
    trace::Trace,
};

#[derive(Debug, Parser)]
#[command(name="noema", about="The intentional memory layer for your AI agents", version=VERSION, propagate_version=true)]
struct Cli {
    #[arg(long, global = true, env = "NOEMA_CORTEX")]
    cortex: Option<String>,
    #[command(subcommand)]
    command: Command,
}

#[derive(Debug, Subcommand)]
enum Command {
    /// Add a new Trace
    Add(AddArgs),
    /// List Traces
    #[command(alias = "ls")]
    List(ListArgs),
    /// Show a Trace
    Get { id: String },
    /// Append content to a Trace
    Append {
        id: String,
        #[arg(long)]
        content: Option<String>,
    },
    /// Edit a Trace in $EDITOR
    Edit { id: String },
    /// Move a Trace to trash
    #[command(alias = "rm", alias = "delete")]
    Remove {
        id: String,
        #[arg(long)]
        force: bool,
    },
    /// Archive a Trace
    Archive { id: String },
    /// Restore an archived Trace
    Unarchive { id: String },
    /// Restore a trashed Trace
    Recover { id: String },
    /// Permanently delete expired trashed Traces
    Purge {
        #[arg(long, default_value_t = 30)]
        days: u32,
    },
    /// Search Traces
    Search {
        query: String,
        #[arg(long)]
        semantic: bool,
        #[arg(long)]
        hybrid: bool,
        #[command(flatten)]
        list: ListArgs,
    },
    /// Find related Traces
    Similar {
        trace_id: String,
        #[arg(long, default_value_t = 10)]
        limit: usize,
        #[arg(long)]
        archived: bool,
        #[arg(long)]
        semantic: bool,
        #[arg(long)]
        hybrid: bool,
    },
    /// Re-index trace files
    Sync {
        #[arg(long)]
        recover: bool,
    },
    /// Show the event log
    Events {
        trace_id: Option<String>,
        #[arg(long)]
        since: Option<String>,
        #[arg(long, default_value_t = 50)]
        limit: usize,
    },
    /// Resolve a divergence
    Resolve {
        divergence_id: String,
        #[arg(long)]
        accept: Option<String>,
        #[arg(long)]
        custom: Option<String>,
    },
    /// Manage memory tiers
    Memory {
        #[command(subcommand)]
        command: MemoryCommand,
    },
    /// Run a consolidation pass
    Consolidate(ConsolidateArgs),
    /// Manage embeddings
    Embeddings {
        #[command(subcommand)]
        command: EmbeddingCommand,
    },
    /// Create a new Cortex
    Init {
        #[arg(long)]
        name: String,
        #[arg(long)]
        path: Option<PathBuf>,
    },
    /// Select the default Cortex
    Use { name: String },
    /// Manage Cortexes
    Cortex {
        #[command(subcommand)]
        command: CortexCommand,
    },
    /// Manage federation
    #[command(alias = "fed")]
    Federation {
        #[command(subcommand)]
        command: FederationCommand,
    },
    /// Run data migrations
    Migrate {
        #[command(subcommand)]
        command: MigrateCommand,
    },
    /// Generate a federation signing key
    Keygen {
        #[arg(long)]
        force: bool,
    },
    /// Verify integrity
    Verify {
        #[command(subcommand)]
        command: Option<VerifyCommand>,
    },
    /// Serve MCP
    #[command(alias = "server")]
    Serve(ServeArgs),
    /// Open the terminal UI
    Tui,
    /// Manage bundled plugins
    Plugin {
        #[command(subcommand)]
        command: PluginCommand,
    },
    /// Generate shell completion
    Completion { shell: Shell },
    /// Show version details
    Version,
    /// Manage user configuration
    Config {
        #[command(subcommand)]
        command: ConfigCommand,
    },
}

#[derive(Debug, Args)]
struct AddArgs {
    #[arg(long)]
    title: String,
    #[arg(long="type", default_value="note", value_parser=["fact", "decision", "preference", "context", "skill", "intent", "observation", "note", "divergence"])]
    trace_type: String,
    #[arg(long, default_value = "")]
    author: String,
    #[arg(long = "tag")]
    tags: Vec<String>,
    #[arg(long)]
    body: Option<String>,
}

#[derive(Debug, Args)]
struct ConsolidateArgs {
    #[arg(long)]
    model_tier: Option<String>,
    #[arg(long)]
    endpoint: Option<String>,
    #[arg(long)]
    model: Option<String>,
    #[arg(long)]
    api_key_env: Option<String>,
    #[arg(long)]
    dry_run: bool,
    #[arg(long, default_value_t = 0)]
    window: i64,
    #[arg(long, default_value_t = 1)]
    retries: i64,
    #[arg(long)]
    emit_json: Option<PathBuf>,
}

#[derive(Debug, Args, Default)]
struct ListArgs {
    #[arg(long = "type", default_value = "")]
    trace_type: String,
    #[arg(long, default_value = "")]
    author: String,
    #[arg(long, default_value = "")]
    tag: String,
    #[arg(long)]
    archived: bool,
    #[arg(long)]
    trashed: bool,
    #[arg(long)]
    all: bool,
}

impl From<ListArgs> for ListOptions {
    fn from(value: ListArgs) -> Self {
        Self {
            trace_type: value.trace_type,
            author: value.author,
            tag: value.tag,
            archived: value.archived,
            trashed: value.trashed,
            all: value.all,
            ..Default::default()
        }
    }
}

#[derive(Debug, Subcommand)]
enum MemoryCommand {
    Stats {
        #[arg(long)]
        detailed: bool,
    },
    Popular {
        #[arg(long, default_value_t = 10)]
        top: usize,
    },
    Health {
        #[arg(long, default_value = "24h")]
        since: String,
        #[arg(long)]
        output: Option<String>,
    },
    Promote {
        trace_id: String,
        #[arg(long)]
        to: Option<String>,
    },
    Demote {
        trace_id: String,
    },
    Purge {
        trace_id: String,
        #[arg(long)]
        tier: String,
        #[arg(long)]
        reason: String,
        #[arg(long)]
        confirm: bool,
        #[arg(long)]
        hard: bool,
    },
}

#[derive(Debug, Subcommand)]
enum EmbeddingCommand {
    Status,
    Backfill {
        #[arg(long)]
        force: bool,
        #[arg(long, default_value_t = 0)]
        limit: usize,
    },
}
#[derive(Debug, Subcommand)]
enum CortexCommand {
    List,
    /// Write a gzipped tarball of a cortex
    Backup {
        name: String,
        /// Tarball output path (default: ./<name>-<timestamp>.tar.gz)
        #[arg(short = 'o', long)]
        output: Option<PathBuf>,
        /// Overwrite the output file if it exists
        #[arg(long)]
        force: bool,
    },
    /// Restore a cortex from a backup tarball
    Restore {
        tarball: PathBuf,
        /// Register the restored cortex under this name (default: name from cortex.md)
        #[arg(long)]
        name: Option<String>,
        /// Parent directory for the restored cortex (default: ~/.noema)
        #[arg(long)]
        path: Option<PathBuf>,
        /// Overwrite an existing directory at the destination
        #[arg(long)]
        force: bool,
    },
    /// Inspect interrupted-mutation recovery state without changing the cortex
    RecoveryStatus {
        name: String,
    },
    /// List durable cortex-restore transactions without exposing paths or hashes
    RestoreStatus,
    /// Explicitly resume or roll back a durable cortex-restore transaction
    RestoreRecover {
        transaction_id: String,
        #[arg(long, value_enum)]
        action: RestoreRecoveryActionArg,
    },
    Remove {
        name: String,
    },
}

#[derive(Debug, Clone, Copy, ValueEnum)]
enum RestoreRecoveryActionArg {
    Resume,
    Rollback,
}
#[derive(Debug, Subcommand)]
enum FederationCommand {
    Status,
    Peers,
    AddPeer {
        name: String,
        endpoint: String,
    },
    Sync {
        name: Option<String>,
    },
    ResetPeer {
        names: Vec<String>,
    },
    SetMode {
        mode: String,
    },
    PausePeer {
        name: String,
    },
    ResumePeer {
        name: String,
    },
    RePinPeer {
        name: String,
        #[arg(long)]
        pubkey: String,
    },
    Key {
        #[command(subcommand)]
        command: FederationKeyCommand,
    },
}
#[derive(Debug, Subcommand)]
enum FederationKeyCommand {
    Fingerprint,
}
#[derive(Debug, Subcommand)]
enum MigrateCommand {
    CortexId {
        #[arg(long)]
        reset: bool,
    },
}
#[derive(Debug, Subcommand)]
enum VerifyCommand {
    Traces {
        #[arg(long)]
        backfill: bool,
    },
    Cortex,
    Drift,
}
#[derive(Debug, Subcommand)]
enum PluginCommand {
    List,
    Status {
        #[arg(long)]
        check: bool,
        #[arg(long)]
        hermes_home: Option<PathBuf>,
        #[arg(long)]
        vault: Option<PathBuf>,
    },
    Hermes {
        #[command(subcommand)]
        command: HermesPluginAction,
    },
    Obsidian {
        #[command(subcommand)]
        command: ObsidianPluginAction,
    },
}
#[derive(Debug, Subcommand)]
enum HermesPluginAction {
    Status {
        #[arg(long)]
        check: bool,
        #[arg(long)]
        hermes_home: Option<PathBuf>,
    },
    Install {
        #[arg(long)]
        check: bool,
        #[arg(long)]
        force: bool,
        #[arg(long)]
        hermes_home: Option<PathBuf>,
    },
}
#[derive(Debug, Subcommand)]
enum ObsidianPluginAction {
    Status {
        #[arg(long)]
        check: bool,
        #[arg(long, required = true)]
        vault: PathBuf,
    },
    Install {
        #[arg(long)]
        check: bool,
        #[arg(long)]
        force: bool,
        #[arg(long, required = true)]
        vault: PathBuf,
    },
}
#[derive(Debug, Subcommand)]
enum ConfigCommand {
    Get { key: String },
    Set { key: String, value: String },
    List,
}

#[derive(Debug, Args)]
struct ServeArgs {
    #[arg(long, default_value = "stdio")]
    transport: String,
    #[arg(long, default_value = "127.0.0.1")]
    host: String,
    #[arg(long, default_value_t = 3000)]
    port: u16,
    #[arg(long)]
    no_watch: bool,
    #[arg(long)]
    tls_cert: Option<PathBuf>,
    #[arg(long)]
    tls_key: Option<PathBuf>,
    #[arg(long)]
    insecure_allow_expired: bool,
}

pub async fn run() -> Result<()> {
    let cli = Cli::parse();
    let selected = cli.cortex.as_deref();
    match cli.command {
        Command::Init { name, path } => init(&name, path)?,
        Command::Use { name } => use_cortex(&name)?,
        Command::Cortex { command } => cortex_command(command)?,
        Command::Version => println!("noema-rs v{VERSION}\nimplementation: Rust"),
        Command::Completion { shell } => {
            generate(shell, &mut Cli::command(), "noema-rs", &mut io::stdout())
        }
        Command::Config { command } => config_command(command)?,
        Command::Plugin { command } => plugin_command(command)?,
        Command::Serve(args) => serve(selected, args).await?,
        Command::Consolidate(args) => consolidate(selected, args).await?,
        other => {
            let mut cx = Cortex::resolve(selected)?;
            execute_cortex_command(&mut cx, other).await?;
        }
    }
    Ok(())
}

async fn execute_cortex_command(cx: &mut Cortex, command: Command) -> Result<()> {
    match command {
        Command::Add(args) => {
            let body = match args.body {
                Some(body) => body,
                None => read_stdin()?,
            };
            let mut trace = Trace::new(args.title, args.trace_type, args.author, args.tags, body);
            cx.add(&mut trace)?;
            println!("Trace added: {}", trace.frontmatter.id);
        }
        Command::List(args) => print_rows(cx.list(&args.into())?),
        Command::Get { id } => {
            let (row, trace) = cx
                .get_trace(&id)
                .map_err(|_| anyhow::anyhow!("trace {:?} not found", id))?;
            print_trace(&row, &trace);
        }
        Command::Append { id, content } => {
            let content = content.unwrap_or(read_stdin()?);
            cx.append(&id, &content, false)?;
            println!("Content appended to {id}");
        }
        Command::Edit { id } => {
            let row = cx.get(&id)?;
            let path = cx.file_path(&row);
            let editor = std::env::var("EDITOR").unwrap_or_else(|_| "vi".into());
            let status = std::process::Command::new(&editor).arg(&path).status()?;
            if !status.success() {
                bail!("editor {editor:?} exited with {status}")
            }
            let mut trace = Trace::parse_file(&path)?;
            cx.update_trace(&id, &mut trace, false)?;
            println!("Trace edited: {id}");
        }
        Command::Remove { id, force } => {
            cx.set_force_source_lock(force);
            cx.trash(&id)?;
            println!("Trace moved to trash: {id}");
        }
        Command::Archive { id } => {
            cx.archive(&id)?;
            println!("Trace archived: {id}");
        }
        Command::Unarchive { id } => {
            cx.unarchive(&id)?;
            println!("Trace unarchived: {id}");
        }
        Command::Recover { id } => {
            cx.recover(&id)?;
            println!("Trace recovered: {id}");
        }
        Command::Purge { days } => println!("Purged {} trace(s).", cx.purge_expired(days)?),
        Command::Search {
            query,
            semantic,
            hybrid,
            list,
        } => {
            if semantic || hybrid {
                let include_archived = list.all || list.archived;
                let (client, model, weight) = semantic_client(cx)?;
                let options = SemanticOptions {
                    model,
                    include_archived,
                    ..Default::default()
                };
                let scored = if hybrid {
                    cx.hybrid_search(&client, &query, &options, weight).await?
                } else {
                    cx.semantic_search(&client, &query, &options).await?
                };
                print_rows(scored.into_iter().map(|item| item.row).collect());
            } else {
                print_rows(cx.search(&query, &list.into())?);
            }
        }
        Command::Similar {
            trace_id,
            limit,
            archived,
            semantic,
            hybrid,
        } => {
            if semantic || hybrid {
                let search = cx.manifest.search.as_ref().context(
                    "semantic mode needs search.embedding_model in cortex.md (then: noema embeddings backfill)",
                )?;
                if search.embedding_model.is_empty() {
                    bail!(
                        "semantic mode needs search.embedding_model in cortex.md (then: noema embeddings backfill)"
                    );
                }
                let options = SemanticOptions {
                    model: search.embedding_model.clone(),
                    limit,
                    include_archived: archived,
                };
                let scored = if hybrid {
                    cx.hybrid_similar(&trace_id, &options, search.effective_hybrid_weight())?
                } else {
                    cx.semantic_similar(&trace_id, &options)?
                };
                print_rows(scored.into_iter().map(|item| item.row).collect());
            } else {
                print_rows(cx.find_similar(&trace_id, limit, archived)?);
            }
        }
        Command::Sync { recover } => {
            let result = cx.sync_with_recovery(recover)?;
            if recover {
                println!(
                    "Added: {}  Updated: {}  Drifted: {}  Recovered: {}  Orphaned: {}",
                    result.added, result.updated, result.drifted, result.recovered, result.orphaned
                );
            } else {
                println!(
                    "Added: {}  Updated: {}  Drifted: {}  Orphaned: {}",
                    result.added, result.updated, result.drifted, result.orphaned
                );
            }
            if result.recovered > 0 {
                println!(
                    "Note: recovered entries had missing files that were rebuilt from the local event log."
                );
            }
            if result.drifted > 0 {
                println!(
                    "Note: drifted entries are long-tier traces whose on-disk files differ from the DB."
                );
                println!(
                    "      The DB row is left untouched (long-tier is immutable). Visibility was still reconciled."
                );
                println!(
                    "      Use `noema get <id>` to inspect each trace's current file body. Drifted IDs:"
                );
                for id in &result.drifted_ids {
                    println!("        {id}");
                }
                if result.drifted > result.drifted_ids.len() {
                    println!(
                        "        … and {} more",
                        result.drifted - result.drifted_ids.len()
                    );
                }
            }
            if result.orphaned > 0 {
                println!("Note: orphaned entries are in the database but have no file on disk.");
                if recover {
                    println!("      The local event log had no usable snapshot for these IDs.");
                    println!(
                        "      Use `noema list` + `noema remove --force <id>` to clean them up."
                    );
                } else {
                    println!(
                        "      Try `noema sync --recover` to rebuild them from the local event log,"
                    );
                    println!(
                        "      or use `noema list` + `noema remove --force <id>` to clean them up."
                    );
                }
            }
        }
        Command::Events {
            trace_id,
            since,
            limit,
        } => {
            let events = match trace_id {
                Some(id) => cx.history(&id)?,
                None => cx.events_since(since.as_deref().unwrap_or(""), limit)?,
            };
            println!("{}", serde_json::to_string_pretty(&events)?);
        }
        Command::Memory { command } => memory_command(cx, command)?,
        Command::Embeddings { command } => match command {
            EmbeddingCommand::Status => {
                let model = cx
                    .manifest
                    .search
                    .as_ref()
                    .map(|search| search.embedding_model.as_str())
                    .unwrap_or("");
                let status = cx.embedding_status(model)?;
                if cx
                    .manifest
                    .search
                    .as_ref()
                    .is_some_and(|search| search.semantic_enabled)
                {
                    println!(
                        "Semantic search: enabled (model={}, endpoint={})",
                        model,
                        cx.manifest.resolved_embedding_endpoint()?
                    );
                } else {
                    println!(
                        "Semantic search: disabled (set search.semantic_enabled + search.embedding_model in cortex.md)"
                    );
                }
                println!("Embeddable traces: {}", status.embeddable);
                println!("  embedded (up-to-date): {}", status.embedded);
                println!("  stale (changed or other model): {}", status.stale);
                println!("  missing: {}", status.missing);
            }
            EmbeddingCommand::Backfill { force, limit } => {
                let (client, model, _) = semantic_client(cx)?;
                let max_chars = cx
                    .manifest
                    .search
                    .as_ref()
                    .map(|search| search.effective_max_chars())
                    .unwrap_or(32_000);
                let endpoint = cx.manifest.resolved_embedding_endpoint()?;
                println!("Backfilling embeddings (model={model}, endpoint={endpoint})...");
                let result = cx
                    .embed_backfill(
                        &client,
                        &model,
                        &EmbedBackfillOptions {
                            force,
                            limit,
                            max_chars,
                            ..Default::default()
                        },
                    )
                    .await?;
                println!(
                    "Done: {} considered, {} embedded.",
                    result.considered, result.embedded
                );
            }
        },
        Command::Tui => {
            let theme = Config::load()?.theme().to_owned();
            crate::tui::run(cx, &theme)?;
        }
        Command::Federation { command } => federation_command(cx, command).await?,
        Command::Keygen { force } => keygen(cx, force)?,
        Command::Verify { command } => verify(cx, command)?,
        Command::Resolve {
            divergence_id,
            accept,
            custom,
        } => {
            let accept = accept.unwrap_or_default();
            let custom = custom.unwrap_or_default();
            cx.resolve_divergence(&divergence_id, &accept, &custom)?;
            if accept.is_empty() {
                println!("Divergence {divergence_id} resolved (custom merge).");
            } else {
                println!("Divergence {divergence_id} resolved (accepted {accept}).");
            }
        }
        Command::Migrate {
            command: MigrateCommand::CortexId { reset },
        } => {
            if reset {
                bail!("identity reset is intentionally not performed by the comparison binary")
            } else {
                println!("Cortex is already at manifest v{}", cx.manifest.version);
            }
        }
        Command::Init { .. }
        | Command::Use { .. }
        | Command::Cortex { .. }
        | Command::Consolidate(_)
        | Command::Serve(_)
        | Command::Completion { .. }
        | Command::Version
        | Command::Config { .. }
        | Command::Plugin { .. } => unreachable!(),
    }
    Ok(())
}

async fn consolidate(selected: Option<&str>, args: ConsolidateArgs) -> Result<()> {
    let cx = Cortex::resolve(selected)?;
    let config = cx
        .manifest
        .consolidation_config()?
        .context("consolidation is not enabled in cortex.md; set consolidation.enabled: true")?;
    if !config.enabled {
        bail!("consolidation is not enabled in cortex.md; set consolidation.enabled: true");
    }
    if !config.llm_enabled {
        bail!("consolidation.llm_enabled is false; `noema consolidate` requires the LLM path");
    }
    let configured_profile = config.effective_model_tier().to_owned();
    let configured_window_hours = config.window_hours;

    let endpoint = args
        .endpoint
        .filter(|value| !value.is_empty())
        .unwrap_or(config.local_llm_endpoint);
    if endpoint.is_empty() {
        bail!("consolidation.local_llm_endpoint is empty and --endpoint was not provided");
    }
    let model = args
        .model
        .filter(|value| !value.is_empty())
        .unwrap_or(config.model_name);
    if model.is_empty() {
        bail!("consolidation.model_name is empty and --model was not provided");
    }
    let profile = args
        .model_tier
        .filter(|value| !value.is_empty())
        .unwrap_or(configured_profile);
    let api_key_env = args
        .api_key_env
        .filter(|value| !value.is_empty())
        .unwrap_or(config.api_key_env);
    let window_hours = if args.window > 0 {
        args.window as u64
    } else if configured_window_hours > 0 {
        configured_window_hours as u64
    } else {
        24
    };
    let window = std::time::Duration::from_secs(window_hours.saturating_mul(60 * 60));
    let cancellation = CancellationToken::new();
    let signal_cancellation = cancellation.clone();
    let signal_worker = tokio::spawn(async move {
        if tokio::signal::ctrl_c().await.is_ok() {
            signal_cancellation.cancel();
        }
    });

    eprintln!(
        "[consolidate] model={model:?} profile={profile} window={}h dry-run={}",
        window_hours, args.dry_run
    );
    let result = run_distillation_pass(
        cx,
        &DistillationConfig {
            window,
            model_tier: profile.clone(),
            model_name: model.clone(),
            endpoint: endpoint.clone(),
            api_key_env,
            max_retries: args.retries.max(0) as usize,
            dry_run: args.dry_run,
            heuristic: HeuristicConfig {
                window,
                ..HeuristicConfig::default()
            },
        },
        &cancellation,
    )
    .await;
    signal_worker.abort();
    let result = result.context("consolidation pass")?;

    println!(
        "Considered {} candidates, attempted {} clusters: {} distilled, {} rejected, {} fallback-promoted, {} skipped.",
        result.considered,
        result.attempted,
        result.distilled,
        result.rejected,
        result.fallback_promotions,
        result.skipped
    );
    if let Some(path) = args.emit_json {
        let payload = serde_json::json!({
            "endpoint": endpoint,
            "model": model,
            "profile": profile,
            "window": format!("{window_hours}h0m0s"),
            "dry_run": args.dry_run,
            "timestamp": Utc::now(),
            "summary": result,
        });
        fs::write(&path, serde_json::to_vec_pretty(&payload)?)
            .with_context(|| format!("writing emit-json to {}", path.display()))?;
        eprintln!(
            "[consolidate] emitted per-cluster JSON to {}",
            path.display()
        );
    }
    Ok(())
}

fn init(name: &str, path: Option<PathBuf>) -> Result<()> {
    let parent = path.unwrap_or_else(|| {
        PathBuf::from(std::env::var_os("HOME").unwrap_or_default()).join(".noema")
    });
    let manifest = Cortex::create(name, &parent)?;
    let path = Cortex::register_created(name, &parent, &manifest)?;
    println!(
        "Cortex {:?} created at {}\nCortex ID: {}",
        name,
        path.display(),
        manifest.id
    );
    Ok(())
}

fn use_cortex(name: &str) -> Result<()> {
    let mut cfg = Config::load()?;
    if !cfg.cortexes.contains_key(name) {
        bail!("unknown cortex {:?}", name)
    }
    cfg.default = name.into();
    cfg.save()?;
    println!("Now using cortex {:?}.", name);
    Ok(())
}

fn cortex_command(command: CortexCommand) -> Result<()> {
    match command {
        CortexCommand::List => {
            let cfg = Config::load()?;
            for (name, entry) in cfg.cortexes {
                println!(
                    "{}\t{}{}",
                    name,
                    entry.path.display(),
                    if name == cfg.default { "  *" } else { "" }
                );
            }
        }
        CortexCommand::Backup {
            name,
            output,
            force,
        } => {
            let cfg = Config::load()?;
            let entry = cfg
                .cortexes
                .get(&name)
                .ok_or_else(|| anyhow::anyhow!("unknown cortex"))?;
            if let Err(error) = crate::db::checkpoint_wal(&entry.path) {
                eprintln!(
                    "warning: WAL checkpoint failed ({error:#}); backup will include any WAL sidecar"
                );
            }
            let output = output.unwrap_or_else(|| {
                PathBuf::from(format!(
                    "{}-{}.tar.gz",
                    name,
                    Utc::now().format("%Y%m%d-%H%M%SZ")
                ))
            });
            let size = crate::restore::backup(&entry.path, &output, force)?;
            println!(
                "Backed up cortex {:?} to {} ({})",
                name,
                output.display(),
                human_bytes(size)
            );
            if !entry.id.is_empty() {
                println!("Cortex ID: {}", entry.id);
            }
        }
        CortexCommand::Restore {
            tarball,
            name,
            path,
            force,
        } => {
            let mut cfg = Config::load()?;
            let result = crate::restore::restore(
                &mut cfg,
                &tarball,
                &crate::restore::RestoreOptions {
                    name,
                    parent: path,
                    force,
                },
            )?;
            println!(
                "Restored cortex {:?} to {}",
                result.name,
                result.path.display()
            );
            if !result.id.is_empty() {
                println!("Cortex ID: {}", result.id);
            }
            if result.is_default {
                println!("Set as default cortex.");
            }
            if let Some(backup) = result.retained_backup {
                eprintln!(
                    "warning: restored successfully but could not remove the previous destination at {}",
                    backup.display()
                );
            }
            if let Some(transaction) = result.retained_transaction {
                eprintln!(
                    "warning: restore transaction {transaction} remains for explicit recovery or cleanup"
                );
            }
        }
        CortexCommand::RecoveryStatus { name } => {
            let cfg = Config::load()?;
            let entry = cfg
                .cortexes
                .get(&name)
                .ok_or_else(|| anyhow::anyhow!("unknown cortex {:?}", name))?;
            match crate::cortex::inspect_recovery_status(&entry.path) {
                crate::cortex::RecoveryStatus::Clean => {
                    println!("Recovery status for cortex {name:?}: clean");
                }
                crate::cortex::RecoveryStatus::Pending { records } => {
                    println!(
                        "Recovery status for cortex {name:?}: {records} pending mutation record(s); open it with the Rust build to recover"
                    );
                }
                crate::cortex::RecoveryStatus::MalformedJournal { records } => {
                    println!(
                        "Recovery status for cortex {name:?}: malformed recovery journal ({records} record(s)); automatic recovery will fail closed"
                    );
                }
                crate::cortex::RecoveryStatus::UnreadableDatabase => {
                    println!(
                        "Recovery status for cortex {name:?}: unreadable database; restore a known-good backup or inspect it manually"
                    );
                }
            }
        }
        CortexCommand::RestoreStatus => {
            let cfg = Config::load()?;
            let report = crate::restore::inspect_restore_transactions(&cfg)?;
            if report.transactions.is_empty() && report.malformed == 0 {
                println!("Restore transactions: clean");
            } else {
                for transaction in report.transactions {
                    let state = match transaction.state {
                        crate::restore::RestoreTransactionState::Resumable => "resumable",
                        crate::restore::RestoreTransactionState::RollbackOnly => "rollback-only",
                        crate::restore::RestoreTransactionState::CommittedCleanup => {
                            "committed-cleanup"
                        }
                        crate::restore::RestoreTransactionState::Ambiguous => "ambiguous",
                    };
                    println!(
                        "{} cortex={:?} phase={:?} state={state}",
                        transaction.id, transaction.name, transaction.phase
                    );
                }
                if report.malformed > 0 {
                    println!(
                        "Malformed restore transaction record(s): {}",
                        report.malformed
                    );
                }
            }
        }
        CortexCommand::RestoreRecover {
            transaction_id,
            action,
        } => {
            let mut cfg = Config::load()?;
            let action = match action {
                RestoreRecoveryActionArg::Resume => crate::restore::RestoreRecoveryAction::Resume,
                RestoreRecoveryActionArg::Rollback => {
                    crate::restore::RestoreRecoveryAction::Rollback
                }
            };
            crate::restore::recover_restore_transaction(&mut cfg, &transaction_id, action)?;
            println!(
                "Restore transaction {transaction_id} {} successfully.",
                match action {
                    crate::restore::RestoreRecoveryAction::Resume => "resumed",
                    crate::restore::RestoreRecoveryAction::Rollback => "rolled back",
                }
            );
        }
        CortexCommand::Remove { name } => {
            let mut cfg = Config::load()?;
            cfg.cortexes
                .remove(&name)
                .ok_or_else(|| anyhow::anyhow!("unknown cortex"))?;
            if cfg.default == name {
                cfg.default = String::new();
            }
            cfg.save()?;
            println!("Removed cortex registration {name:?}; files were preserved.");
        }
    }
    Ok(())
}

fn human_bytes(bytes: u64) -> String {
    const UNIT: u64 = 1024;
    if bytes < UNIT {
        return format!("{bytes} B");
    }
    let mut divisor = UNIT;
    let mut exponent = 0;
    let mut value = bytes / UNIT;
    while value >= UNIT {
        divisor *= UNIT;
        exponent += 1;
        value /= UNIT;
    }
    format!(
        "{:.1} {}iB",
        bytes as f64 / divisor as f64,
        b"KMGTPE"[exponent] as char
    )
}

fn memory_command(cx: &Cortex, command: MemoryCommand) -> Result<()> {
    match command {
        MemoryCommand::Stats { .. } => {
            let rows = cx.list(&ListOptions {
                all: true,
                ..Default::default()
            })?;
            for tier in ["short", "mid", "long"] {
                println!("{tier}: {}", rows.iter().filter(|r| r.tier == tier).count());
            }
        }
        MemoryCommand::Popular { top } => {
            let mut rows = cx.list(&ListOptions::default())?;
            rows.truncate(top);
            print_rows(rows);
        }
        MemoryCommand::Health { since, output } => {
            let value = serde_json::json!({"since":since,"daily":[],"totals":{}});
            if output.as_deref() == Some("json") {
                println!("{}", serde_json::to_string_pretty(&value)?)
            } else {
                println!("Consolidation health ({since}): no recorded activity")
            }
        }
        MemoryCommand::Promote { trace_id, to } => {
            let row = cx.get(&trace_id)?;
            let to = to.unwrap_or_else(|| {
                if row.tier == "short" {
                    "mid".into()
                } else {
                    "long".into()
                }
            });
            cx.promote(&trace_id, &to)?;
            println!("Promoted {trace_id} to {to}");
        }
        MemoryCommand::Demote { trace_id } => {
            cx.demote(&trace_id)?;
            println!("Demoted {trace_id} to short");
        }
        MemoryCommand::Purge {
            trace_id,
            reason,
            confirm,
            hard,
            ..
        } => {
            if !confirm {
                bail!("--confirm is required")
            };
            if hard {
                cx.remove_hard(&trace_id)?;
            } else {
                cx.trash(&trace_id)?;
            }
            println!("Purged {trace_id}: {reason}");
        }
    }
    Ok(())
}

async fn federation_command(cx: &mut Cortex, command: FederationCommand) -> Result<()> {
    match command {
        FederationCommand::Status | FederationCommand::Peers => {
            println!(
                "{}",
                serde_json::to_string_pretty(&crate::federation::status(cx)?)?
            );
        }
        FederationCommand::AddPeer { name, endpoint } => {
            let fed = cx.manifest.federation.get_or_insert_with(Default::default);
            if fed.peers.iter().any(|p| p.name == name) {
                bail!("peer already exists")
            };
            fed.peers.push(crate::cortex::PeerEntry {
                name,
                endpoint,
                ..Default::default()
            });
            write_manifest(&cx.dir, &cx.manifest)?;
        }
        FederationCommand::Sync { name } => {
            let federation = cx.manifest.federation.clone().unwrap_or_default();
            if federation.mode == "publish" {
                bail!("publish-mode cortexes do not pull peer events")
            }
            let peers: Vec<_> = federation
                .peers
                .into_iter()
                .filter(|peer| name.as_ref().is_none_or(|name| &peer.name == name))
                .collect();
            if peers.is_empty() {
                bail!("no matching federation peer configured")
            }
            for peer in peers {
                let report = crate::federation::sync_peer(cx, &peer).await?;
                println!(
                    "Synced {} event(s) from {} in {} batch(es); cursor {}",
                    report.events, report.peer, report.batches, report.cursor
                );
            }
        }
        FederationCommand::SetMode { mode } => {
            if !["sync", "publish", "subscribe"].contains(&mode.as_str()) {
                bail!("invalid mode")
            };
            cx.manifest
                .federation
                .get_or_insert_with(Default::default)
                .mode = mode;
            write_manifest(&cx.dir, &cx.manifest)?;
        }
        FederationCommand::PausePeer { name } => {
            let peer = cx
                .manifest
                .federation
                .as_mut()
                .and_then(|f| f.peers.iter_mut().find(|p| p.name == name))
                .ok_or_else(|| anyhow::anyhow!("unknown peer"))?;
            peer.mode = "paused".into();
            write_manifest(&cx.dir, &cx.manifest)?;
        }
        FederationCommand::ResumePeer { name } => {
            let peer = cx
                .manifest
                .federation
                .as_mut()
                .and_then(|f| f.peers.iter_mut().find(|p| p.name == name))
                .ok_or_else(|| anyhow::anyhow!("unknown peer"))?;
            peer.mode = String::new();
            write_manifest(&cx.dir, &cx.manifest)?;
        }
        FederationCommand::RePinPeer { name, pubkey } => {
            repin_peer(cx, &name, &pubkey)?;
        }
        FederationCommand::ResetPeer { names } => {
            for name in names {
                let id_key = format!("peer:{name}:cortex_id");
                let cortex_id = cx.federation_state(&id_key)?;
                for suffix in [
                    "last_event",
                    "last_seen",
                    "last_usage",
                    "cortex_id",
                    "health",
                ] {
                    cx.delete_federation_state(&format!("peer:{name}:{suffix}"))?;
                }
                if !cortex_id.is_empty() {
                    cx.delete_federation_state(&format!("cortexkey:{cortex_id}"))?;
                }
                println!("Reset local federation state for {name}");
            }
        }
        FederationCommand::Key {
            command: FederationKeyCommand::Fingerprint,
        } => {
            let key = crate::cortex::load_access_key(&cx.dir, cx.manifest.access.as_ref())?;
            if !key.keyed() {
                println!("Access: open mode (no key configured)");
            } else {
                println!("Source:      {}", key.source);
                if !key.path.as_os_str().is_empty() {
                    println!("Path:        {}", key.path.display());
                }
                println!("Fingerprint: {}", key.fingerprint);
                if key.env_override() {
                    println!(
                        "Note:        {} is overriding access.shared_key_file",
                        crate::cortex::ACCESS_KEY_ENV
                    );
                }
            }
        }
    }
    Ok(())
}

fn keygen(cx: &mut Cortex, force: bool) -> Result<()> {
    if cx.manifest.signing.is_some() && !force {
        bail!("signing key already configured")
    };
    let (_key, public, seed) = eventsig::generate()?;
    let filename = "noema-signing.key";
    let path = cx.dir.join(filename);
    fs::write(&path, format!("{seed}\n"))?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(&path, fs::Permissions::from_mode(0o600))?;
    }
    cx.manifest.signing = Some(crate::cortex::SigningConfig {
        public_key: public.clone(),
        private_key_file: filename.into(),
    });
    write_manifest(&cx.dir, &cx.manifest)?;
    println!(
        "Signing key {}.\nPublic key: {public}",
        if force { "rotated" } else { "generated" }
    );
    Ok(())
}

fn repin_peer(cx: &mut Cortex, name: &str, public_key: &str) -> Result<()> {
    eventsig::parse_public(public_key).context("invalid peer Ed25519 public key")?;
    let cortex_id = cx.federation_state(&format!("peer:{name}:cortex_id"))?;
    if cortex_id.is_empty() {
        bail!("peer {name:?} has no pinned identity; sync it successfully before re-pinning")
    }
    let peer = cx
        .manifest
        .federation
        .as_mut()
        .and_then(|federation| federation.peers.iter_mut().find(|peer| peer.name == name))
        .ok_or_else(|| anyhow::anyhow!("unknown peer"))?;
    peer.pubkey = public_key.trim().to_owned();
    peer.mode = "paused".into();
    write_manifest(&cx.dir, &cx.manifest)?;
    println!(
        "Updated the hard pin for {name} ({}). The peer remains paused; verify the fingerprint out of band, then run federation resume-peer {name}.",
        eventsig::public_fingerprint(public_key)?
    );
    Ok(())
}

fn verify(cx: &Cortex, command: Option<VerifyCommand>) -> Result<()> {
    let stdout = io::stdout();
    let mut out = stdout.lock();
    match command.unwrap_or(VerifyCommand::Traces { backfill: false }) {
        VerifyCommand::Traces { backfill } => verify_traces(cx, backfill, &mut out)?,
        VerifyCommand::Cortex => verify_cortex(cx, &mut out)?,
        VerifyCommand::Drift => verify_drift(cx, &mut out)?,
    }
    Ok(())
}

fn verify_traces(cx: &Cortex, backfill: bool, out: &mut dyn Write) -> Result<()> {
    let mut checked = 0;
    let mut mismatches = 0;
    let mut backfilled = 0;

    for directory in [cx.traces_dir(), cx.archive_dir()] {
        let entries = match fs::read_dir(&directory) {
            Ok(entries) => entries,
            Err(error) if error.kind() == io::ErrorKind::NotFound => continue,
            Err(error) => {
                return Err(error).with_context(|| format!("reading {}", directory.display()));
            }
        };
        for entry in entries {
            let entry = entry?;
            let path = entry.path();
            if entry.file_type()?.is_dir()
                || path.extension().and_then(|extension| extension.to_str()) != Some("md")
            {
                continue;
            }
            let mut trace = match Trace::parse_file(&path) {
                Ok(trace) => trace,
                Err(error) => {
                    writeln!(
                        out,
                        "  SKIP     {} (parse error: {error})",
                        entry.file_name().to_string_lossy()
                    )?;
                    continue;
                }
            };
            let computed = crate::trace::content_hash(&trace.body);
            checked += 1;

            if trace.frontmatter.content_hash.is_empty() {
                if backfill {
                    trace.frontmatter.content_hash = computed;
                    trace
                        .write_preserving_updated(&path)
                        .with_context(|| format!("backfilling {}", trace.frontmatter.id))?;
                    backfilled += 1;
                    writeln!(out, "  BACKFILL {}", trace.frontmatter.id)?;
                }
                continue;
            }

            if trace.frontmatter.content_hash != computed {
                mismatches += 1;
                writeln!(out, "  MISMATCH {}", trace.frontmatter.id)?;
                writeln!(
                    out,
                    "           expected: {}",
                    trace.frontmatter.content_hash
                )?;
                writeln!(out, "           actual:   {computed}")?;
                if backfill {
                    trace.frontmatter.content_hash = computed;
                    trace
                        .write_preserving_updated(&path)
                        .with_context(|| format!("backfilling {}", trace.frontmatter.id))?;
                    backfilled += 1;
                    writeln!(out, "  FIXED    {}", trace.frontmatter.id)?;
                }
            }
        }
    }

    writeln!(out, "\nChecked {checked} trace(s).")?;
    if mismatches > 0 {
        writeln!(out, "{mismatches} mismatch(es) found.")?;
    } else {
        writeln!(out, "All hashes OK.")?;
    }
    if backfill && backfilled > 0 {
        writeln!(out, "{backfilled} trace(s) backfilled.")?;
    }
    Ok(())
}

fn verify_drift(cx: &Cortex, out: &mut dyn Write) -> Result<()> {
    let mut checked = 0;
    let mut drifted = 0;
    for row in cx.list(&ListOptions {
        all: true,
        ..Default::default()
    })? {
        if row.cortex_id.is_empty() || row.cortex_id == cx.id || row.source_hash.is_empty() {
            continue;
        }
        let path = cx.trace_file(&row.id, !row.archived_at.is_empty());
        let trace = match Trace::parse_file(&path) {
            Ok(trace) => trace,
            Err(error) => {
                writeln!(out, "  SKIP     {} (parse error: {error})", row.id)?;
                continue;
            }
        };
        checked += 1;
        let current = crate::trace::content_hash(&trace.body);
        if current == row.source_hash {
            continue;
        }
        drifted += 1;
        writeln!(
            out,
            "  DRIFTED  {} (source: {}, locked: {})",
            row.id,
            row.origin,
            if row.source_locked { "yes" } else { "no" }
        )?;
        writeln!(out, "           local:  {current}")?;
        writeln!(out, "           source: {}", row.source_hash)?;
    }

    writeln!(out, "\nChecked {checked} federated trace(s).")?;
    if drifted > 0 {
        writeln!(out, "{drifted} trace(s) have drifted from their source.")?;
    } else if checked > 0 {
        writeln!(out, "No drift detected.")?;
    } else {
        writeln!(out, "No federated traces with source hashes found.")?;
    }
    Ok(())
}

#[derive(Clone, Copy, PartialEq, Eq)]
enum CheckLevel {
    Ok,
    Warn,
    Fail,
}

impl CheckLevel {
    fn tag(self) -> &'static str {
        match self {
            Self::Ok => "[ok]  ",
            Self::Warn => "[warn]",
            Self::Fail => "[fail]",
        }
    }
}

struct CheckResult {
    name: &'static str,
    level: CheckLevel,
    summary: String,
    detail: String,
}

impl CheckResult {
    fn new(name: &'static str, level: CheckLevel, summary: impl Into<String>) -> Self {
        Self {
            name,
            level,
            summary: summary.into(),
            detail: String::new(),
        }
    }

    fn detail(mut self, detail: impl Into<String>) -> Self {
        self.detail = detail.into();
        self
    }
}

fn verify_cortex(cx: &Cortex, out: &mut dyn Write) -> Result<()> {
    let config = Config::load();
    let mut checks = vec![
        check_user_config(&config),
        CheckResult::new(
            "cortex selection",
            CheckLevel::Ok,
            format!(
                "{} (source: {})",
                cx.name,
                if std::env::var_os("NOEMA_CORTEX").is_some_and(|value| !value.is_empty()) {
                    "$NOEMA_CORTEX"
                } else {
                    "config default"
                }
            ),
        ),
        check_cortex_layout(cx),
    ];
    checks.extend(check_manifest(cx));
    checks.push(check_database(cx));
    checks.push(check_access(cx));
    checks.push(check_tls(cx, Utc::now()));
    checks.extend(check_federation(cx));
    checks.push(check_watch(cx));
    checks.push(check_consolidation(cx));

    let header = format!("noema verify cortex — {} ({})", cx.name, cx.dir.display());
    writeln!(out, "{header}")?;
    writeln!(out, "{}", "─".repeat(header.chars().count()))?;
    let width = checks
        .iter()
        .map(|check| check.name.len())
        .max()
        .unwrap_or(0);
    for check in &checks {
        writeln!(
            out,
            "{} {:width$}  {}",
            check.level.tag(),
            check.name,
            check.summary,
            width = width
        )?;
        for line in check.detail.lines() {
            writeln!(out, "       {line}")?;
        }
    }
    let ok = checks
        .iter()
        .filter(|check| check.level == CheckLevel::Ok)
        .count();
    let warn = checks
        .iter()
        .filter(|check| check.level == CheckLevel::Warn)
        .count();
    let fail = checks
        .iter()
        .filter(|check| check.level == CheckLevel::Fail)
        .count();
    writeln!(out, "\n{ok} ok, {warn} warn, {fail} fail")?;
    if fail > 0 {
        bail!("cortex doctor: fail-level checks reported");
    }
    Ok(())
}

fn check_user_config(config: &Result<Config>) -> CheckResult {
    let config = match config {
        Ok(config) => config,
        Err(error) => {
            return CheckResult::new(
                "user config",
                CheckLevel::Fail,
                format!("could not load: {error:#}"),
            );
        }
    };
    if !config.default.is_empty() && !config.cortexes.contains_key(&config.default) {
        return CheckResult::new(
            "user config",
            CheckLevel::Fail,
            format!("default {:?} not in registered cortexes", config.default),
        );
    }
    CheckResult::new(
        "user config",
        CheckLevel::Ok,
        format!(
            "{} cortex(es) registered, default={}",
            config.cortexes.len(),
            if config.default.is_empty() {
                "(unset)"
            } else {
                &config.default
            }
        ),
    )
}

fn check_cortex_layout(cx: &Cortex) -> CheckResult {
    let required = [
        ("traces/", cx.traces_dir()),
        ("archive/traces/", cx.archive_dir()),
        ("trash/traces/", cx.trash_dir()),
        ("db/", cx.dir.join("db")),
    ];
    let missing = required
        .into_iter()
        .filter_map(|(label, path)| {
            fs::metadata(path)
                .ok()
                .filter(|metadata| metadata.is_dir())
                .is_none()
                .then_some(label)
        })
        .collect::<Vec<_>>();
    if missing.is_empty() {
        CheckResult::new("cortex layout", CheckLevel::Ok, "all required dirs present")
    } else {
        CheckResult::new(
            "cortex layout",
            CheckLevel::Fail,
            format!("missing required dirs: {}", missing.join(", ")),
        )
    }
}

fn check_manifest(cx: &Cortex) -> Vec<CheckResult> {
    let raw = match fs::read(cx.dir.join("cortex.md")) {
        Ok(raw) => raw,
        Err(error) => {
            return vec![CheckResult::new(
                "manifest",
                CheckLevel::Fail,
                format!("cannot read cortex.md: {error}"),
            )];
        }
    };
    let mut checks = vec![
        if raw.starts_with(b"---\n") || raw.starts_with(b"---\r\n") {
            CheckResult::new("manifest", CheckLevel::Ok, "framed YAML frontmatter")
        } else {
            CheckResult::new("manifest", CheckLevel::Warn, "legacy bare-YAML format")
                .detail("Will silently upgrade to framed form on the next manifest mutation.")
        },
    ];
    checks.push(
        match cx.manifest.version.cmp(&crate::cortex::MANIFEST_VERSION) {
            std::cmp::Ordering::Equal => CheckResult::new(
                "manifest version",
                CheckLevel::Ok,
                format!("v{} (current)", cx.manifest.version),
            ),
            std::cmp::Ordering::Less => CheckResult::new(
                "manifest version",
                CheckLevel::Fail,
                format!(
                    "v{} behind current v{}",
                    cx.manifest.version,
                    crate::cortex::MANIFEST_VERSION
                ),
            )
            .detail("Run `noema migrate cortex-id` to upgrade."),
            std::cmp::Ordering::Greater => CheckResult::new(
                "manifest version",
                CheckLevel::Warn,
                format!(
                    "v{} ahead of binary's v{} (newer noema?)",
                    cx.manifest.version,
                    crate::cortex::MANIFEST_VERSION
                ),
            ),
        },
    );
    checks.push(if cx.manifest.id.is_empty() {
        CheckResult::new(
            "manifest id",
            CheckLevel::Fail,
            "missing — required since manifest v2",
        )
        .detail("Run `noema migrate cortex-id` to assign one.")
    } else if ulid::Ulid::from_string(&cx.manifest.id).is_err() {
        CheckResult::new(
            "manifest id",
            CheckLevel::Fail,
            format!("not a valid ULID: {:?}", cx.manifest.id),
        )
    } else {
        CheckResult::new("manifest id", CheckLevel::Ok, cx.manifest.id.clone())
    });
    if cx.manifest.name.is_empty() {
        checks.push(CheckResult::new("manifest name", CheckLevel::Fail, "empty"));
    }
    checks
}

fn check_database(cx: &Cortex) -> CheckResult {
    match cx.database_health() {
        Err(error) => CheckResult::new(
            "db",
            CheckLevel::Fail,
            format!("schema health query failed: {error:#}"),
        ),
        Ok((version, journal)) if !journal.eq_ignore_ascii_case("wal") => CheckResult::new(
            "db",
            CheckLevel::Warn,
            format!("schema v{version}, journal_mode={journal} (expected wal)"),
        ),
        Ok((version, _)) => CheckResult::new(
            "db",
            CheckLevel::Ok,
            format!("schema v{version}, WAL enabled"),
        ),
    }
}

fn check_access(cx: &Cortex) -> CheckResult {
    let key = match crate::cortex::load_access_key(&cx.dir, cx.manifest.access.as_ref()) {
        Ok(key) => key,
        Err(error) => return CheckResult::new("access", CheckLevel::Fail, error.to_string()),
    };
    if !key.keyed() {
        return CheckResult::new(
            "access",
            CheckLevel::Ok,
            "open mode (no shared key)",
        )
        .detail(
            "Open mode is loopback-only. Set NOEMA_MCP_KEY or access.shared_key_file before exposing the HTTP transport.",
        );
    }
    let check = CheckResult::new(
        "access",
        CheckLevel::Ok,
        format!("keyed (source={}, fp={})", key.source, key.fingerprint),
    );
    if key.env_override() {
        check.detail(format!(
            "$NOEMA_MCP_KEY overrides configured shared_key_file ({}).",
            key.path.display()
        ))
    } else {
        check
    }
}

fn check_tls(cx: &Cortex, now: chrono::DateTime<Utc>) -> CheckResult {
    let (cert, key) = crate::cortex::resolve_tls_paths(&cx.dir, cx.manifest.access.as_ref());
    if cert.as_os_str().is_empty() && key.as_os_str().is_empty() {
        return CheckResult::new(
            "tls",
            CheckLevel::Ok,
            "no TLS configured (loopback-only OK; keyed federation requires TLS)",
        );
    }
    if cert.as_os_str().is_empty() || key.as_os_str().is_empty() {
        let (present, missing) = if cert.as_os_str().is_empty() {
            ("tls_key_path", "tls_cert_path")
        } else {
            ("tls_cert_path", "tls_key_path")
        };
        return CheckResult::new(
            "tls",
            CheckLevel::Fail,
            format!("access.{present} is set but access.{missing} is empty — configure both"),
        );
    }
    if let Err(error) = fs::metadata(&key) {
        return CheckResult::new(
            "tls",
            CheckLevel::Fail,
            format!("tls_key_path unreadable: {error}"),
        );
    }
    let validity = match crate::tlsutil::load_leaf(&cert) {
        Ok(validity) => validity,
        Err(error) => return CheckResult::new("tls", CheckLevel::Fail, error.to_string()),
    };
    let classification = crate::tlsutil::classify(validity, now);
    let not_after = chrono::DateTime::<Utc>::from_timestamp(classification.not_after, 0)
        .map(|date| date.format("%Y-%m-%d").to_string())
        .unwrap_or_else(|| classification.not_after.to_string());
    match classification.status {
        crate::tlsutil::ExpiryStatus::Expired => CheckResult::new(
            "tls",
            CheckLevel::Fail,
            format!(
                "expired {} day(s) ago (NotAfter={not_after})",
                -classification.days_remaining
            ),
        )
        .detail(format!(
            "Path: {}. Rotate the cert before restarting `noema serve`.",
            cert.display()
        )),
        crate::tlsutil::ExpiryStatus::NotYetValid => CheckResult::new(
            "tls",
            CheckLevel::Fail,
            format!("NotBefore is in the future (NotAfter={not_after})"),
        )
        .detail(format!(
            "Path: {}. Clock skew, or the wrong cert is configured.",
            cert.display()
        )),
        crate::tlsutil::ExpiryStatus::NearExpiry => CheckResult::new(
            "tls",
            CheckLevel::Warn,
            format!(
                "expires in {} day(s) (NotAfter={not_after})",
                classification.days_remaining
            ),
        )
        .detail(format!(
            "Path: {}. Rotate within the next week.",
            cert.display()
        )),
        crate::tlsutil::ExpiryStatus::Ok => CheckResult::new(
            "tls",
            CheckLevel::Ok,
            format!(
                "{} days until NotAfter={not_after}",
                classification.days_remaining
            ),
        ),
    }
}

fn check_federation(cx: &Cortex) -> Vec<CheckResult> {
    let Some(federation) = &cx.manifest.federation else {
        return vec![CheckResult::new(
            "federation",
            CheckLevel::Ok,
            "no peers configured",
        )];
    };
    if federation.peers.is_empty() {
        return vec![CheckResult::new(
            "federation",
            CheckLevel::Ok,
            "no peers configured",
        )];
    }
    let mode = if federation.mode.is_empty() {
        "sync"
    } else {
        &federation.mode
    };
    let mut checks = vec![CheckResult::new(
        "federation mode",
        CheckLevel::Ok,
        format!("{mode} ({} peer(s))", federation.peers.len()),
    )];
    let mut seen = std::collections::BTreeMap::<&str, usize>::new();
    let mut duplicates = Vec::new();
    let mut collisions = Vec::new();
    let mut incomplete = Vec::new();
    for peer in &federation.peers {
        let count = seen.entry(&peer.name).or_default();
        *count += 1;
        if *count == 2 {
            duplicates.push(peer.name.clone());
        }
        if !peer.name.is_empty() && peer.name == cx.manifest.name {
            collisions.push(peer.name.clone());
        }
        if peer.name.is_empty() || peer.endpoint.is_empty() {
            incomplete.push(format!("{:?}@{:?}", peer.name, peer.endpoint));
        }
    }
    for (summary, values) in [
        ("duplicate peer labels", duplicates),
        ("peer label(s) collide with this cortex's name", collisions),
        ("peer entries missing name or endpoint", incomplete),
    ] {
        if !values.is_empty() {
            checks.push(CheckResult::new(
                "federation peers",
                CheckLevel::Fail,
                format!("{summary}: {}", values.join(", ")),
            ));
        }
    }
    checks
}

fn check_watch(cx: &Cortex) -> CheckResult {
    let Some(watch) = &cx.manifest.watch else {
        return CheckResult::new(
            "watch",
            CheckLevel::Ok,
            "default settings (enabled, debounce 300ms)",
        );
    };
    let enabled = watch.enabled.unwrap_or(true);
    let debounce = if watch.debounce_ms == 0 {
        300
    } else {
        watch.debounce_ms
    };
    if watch.debounce_ms != 0 && !(50..=10_000).contains(&watch.debounce_ms) {
        CheckResult::new(
            "watch",
            CheckLevel::Warn,
            format!(
                "enabled={enabled}, debounce_ms={} (outside sane range 50–10000)",
                watch.debounce_ms
            ),
        )
    } else {
        CheckResult::new(
            "watch",
            CheckLevel::Ok,
            format!("enabled={enabled}, debounce_ms={debounce}"),
        )
    }
}

fn check_consolidation(cx: &Cortex) -> CheckResult {
    if cx.manifest.consolidation.is_none() {
        return CheckResult::new("consolidation", CheckLevel::Ok, "not configured");
    }
    let config = match cx.manifest.consolidation_config() {
        Ok(Some(config)) => config,
        Ok(None) => return CheckResult::new("consolidation", CheckLevel::Ok, "not configured"),
        Err(error) => {
            return CheckResult::new("consolidation", CheckLevel::Fail, error.to_string());
        }
    };
    if config.llm_enabled && config.local_llm_endpoint.is_empty() {
        return CheckResult::new(
            "consolidation",
            CheckLevel::Warn,
            "llm_enabled but local_llm_endpoint is empty",
        );
    }
    if config.llm_enabled
        && !config.local_llm_endpoint.is_empty()
        && reqwest::Url::parse(&config.local_llm_endpoint).is_err()
    {
        return CheckResult::new(
            "consolidation",
            CheckLevel::Fail,
            "local_llm_endpoint not a valid URL",
        );
    }
    if config
        .graduation
        .as_ref()
        .is_some_and(|graduation| graduation.min_age_days < 0 || graduation.min_read_count < 0)
    {
        return CheckResult::new(
            "consolidation",
            CheckLevel::Fail,
            "graduation thresholds must be non-negative",
        );
    }
    CheckResult::new(
        "consolidation",
        CheckLevel::Ok,
        format!(
            "enabled={}, llm_enabled={}",
            config.enabled, config.llm_enabled
        ),
    )
}

fn plugin_command(command: PluginCommand) -> Result<()> {
    match command {
        PluginCommand::List => {
            for definition in crate::plugin::DEFINITIONS {
                println!(
                    "{:<8}  {:<22}  {} managed files",
                    definition.name,
                    definition.description,
                    definition.files.len()
                );
            }
        }
        PluginCommand::Status {
            check,
            hermes_home,
            vault,
        } => {
            let hermes = resolve_hermes_target(hermes_home)?;
            let mut failed = render_plugin_status(crate::plugin::HERMES, &hermes)?
                != crate::plugin::State::UpToDate;
            println!();
            if let Some(vault) = vault {
                let obsidian = resolve_obsidian_target(&vault)?;
                failed |= render_plugin_status(crate::plugin::OBSIDIAN, &obsidian)?
                    != crate::plugin::State::UpToDate;
            } else {
                println!("obsidian: target not specified");
            }
            if check && failed {
                bail!("plugin check failed");
            }
        }
        PluginCommand::Hermes { command } => match command {
            HermesPluginAction::Status { check, hermes_home } => {
                let target = resolve_hermes_target(hermes_home)?;
                let state = render_plugin_status(crate::plugin::HERMES, &target)?;
                if check && state != crate::plugin::State::UpToDate {
                    bail!("plugin check failed");
                }
            }
            HermesPluginAction::Install {
                check,
                force,
                hermes_home,
            } => {
                let target = resolve_hermes_target(hermes_home)?;
                require_directory(target.parent().unwrap(), "Hermes plugin parent")?;
                render_plugin_install(crate::plugin::HERMES, &target, check, force)?;
            }
        },
        PluginCommand::Obsidian { command } => match command {
            ObsidianPluginAction::Status { check, vault } => {
                let target = resolve_obsidian_target(&vault)?;
                let state = render_plugin_status(crate::plugin::OBSIDIAN, &target)?;
                if check && state != crate::plugin::State::UpToDate {
                    bail!("plugin check failed");
                }
            }
            ObsidianPluginAction::Install {
                check,
                force,
                vault,
            } => {
                let target = resolve_obsidian_target(&vault)?;
                require_directory(
                    target.parent().unwrap().parent().unwrap(),
                    "Obsidian vault configuration",
                )?;
                render_plugin_install(crate::plugin::OBSIDIAN, &target, check, force)?;
            }
        },
    }
    Ok(())
}

fn render_plugin_status(
    definition: crate::plugin::Definition,
    target: &std::path::Path,
) -> Result<crate::plugin::State> {
    let report = crate::plugin::inspect(definition, target)
        .with_context(|| format!("{} status", definition.name))?;
    println!("{}: {}", report.plugin, report.state.label());
    println!("  target: {}", report.target.display());
    for file in report.files {
        println!("  {:<13} {}", file.state.label(), file.path);
        if file.state == crate::plugin::FileState::Changed {
            println!("    embedded:  {}", file.embedded_hash);
            println!("    installed: {}", file.installed_hash);
        }
    }
    Ok(report.state)
}

fn render_plugin_install(
    definition: crate::plugin::Definition,
    target: &std::path::Path,
    check: bool,
    force: bool,
) -> Result<()> {
    let report = crate::plugin::install(
        definition,
        target,
        crate::plugin::InstallOptions { check, force },
    )
    .with_context(|| format!("{} install", definition.name))?;
    println!(
        "{}: {}",
        report.plugin,
        if check { "check" } else { "install" }
    );
    println!("  target: {}", report.target.display());
    let mut counts = [0_usize; 6];
    for file in &report.files {
        let index = match file.action {
            crate::plugin::InstallAction::Installed => 0,
            crate::plugin::InstallAction::Replaced => 1,
            crate::plugin::InstallAction::Unchanged => 2,
            crate::plugin::InstallAction::Refused => 3,
            crate::plugin::InstallAction::WouldInstall => 4,
            crate::plugin::InstallAction::WouldReplace => 5,
        };
        counts[index] += 1;
        println!("  {:<13} {}", file.action.label(), file.path);
    }
    println!(
        "summary: installed={} replaced={} unchanged={} refused={} would_install={} would_replace={}",
        counts[0], counts[1], counts[2], counts[3], counts[4], counts[5]
    );
    if report.refused() || (check && report.pending()) {
        bail!("plugin check failed");
    }
    Ok(())
}

fn resolve_hermes_target(flag: Option<PathBuf>) -> Result<PathBuf> {
    let home = flag
        .or_else(|| std::env::var_os("HERMES_HOME").map(PathBuf::from))
        .or_else(|| {
            std::env::var_os("HOME")
                .map(PathBuf::from)
                .map(|home| home.join(".hermes/hermes-agent"))
        })
        .context("resolving home directory")?;
    Ok(std::path::absolute(home)
        .context("resolving Hermes home")?
        .join("plugins/memory/noema"))
}

fn resolve_obsidian_target(vault: &std::path::Path) -> Result<PathBuf> {
    Ok(std::path::absolute(vault)
        .with_context(|| format!("resolving Obsidian vault {:?}", vault))?
        .join(".obsidian/plugins/noema"))
}

fn require_directory(path: &std::path::Path, label: &str) -> Result<()> {
    match fs::metadata(path) {
        Ok(metadata) if metadata.is_dir() => Ok(()),
        Ok(_) => bail!("{label} at {} is not a directory", path.display()),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            bail!("{label} not found at {}", path.display())
        }
        Err(error) => {
            Err(error).with_context(|| format!("inspecting {label} at {}", path.display()))
        }
    }
}

fn config_command(command: ConfigCommand) -> Result<()> {
    let mut cfg = Config::load()?;
    match command {
        ConfigCommand::Get { key } => match key.as_str() {
            "ui.theme" => println!("{}", cfg.theme()),
            "default" => println!("{}", cfg.default),
            _ => bail!("unknown config key"),
        },
        ConfigCommand::Set { key, value } => match key.as_str() {
            "ui.theme" => {
                if !["auto", "dark", "light"].contains(&value.as_str()) {
                    bail!("invalid theme")
                };
                cfg.ui = Some(crate::config::UiConfig { theme: value });
                cfg.save()?
            }
            _ => bail!("unknown config key"),
        },
        ConfigCommand::List => println!("default: {}\nui.theme: {}", cfg.default, cfg.theme()),
    }
    Ok(())
}

async fn serve(selected: Option<&str>, args: ServeArgs) -> Result<()> {
    let cx = Cortex::resolve(selected)?;
    let watcher = (!args.no_watch
        && cx
            .manifest
            .watch
            .as_ref()
            .and_then(|watch| watch.enabled)
            .unwrap_or(true))
    .then(|| crate::watch::Settings {
        debounce: std::time::Duration::from_millis(
            cx.manifest
                .watch
                .as_ref()
                .map(|watch| watch.debounce_ms)
                .filter(|value| *value > 0)
                .unwrap_or(300),
        ),
        auto_onboard: cx
            .manifest
            .watch
            .as_ref()
            .and_then(|watch| watch.auto_onboard)
            .unwrap_or(true),
    });
    match args.transport.as_str() {
        "stdio" => crate::mcp::serve_stdio(cx.name, cx.dir, watcher).await,
        "http" => {
            let access_key = crate::cortex::load_access_key(&cx.dir, cx.manifest.access.as_ref())?;
            let tls = validate_http_access(
                &cx.manifest,
                &cx.dir,
                &args.host,
                &access_key,
                args.tls_cert.as_deref(),
                args.tls_key.as_deref(),
            )?;
            if let Some((certificate, _)) = tls.as_ref()
                && let Some(warning) = crate::tlsutil::gate_startup(
                    certificate,
                    args.insecure_allow_expired,
                    Utc::now(),
                )?
            {
                eprintln!("{warning}");
            }
            crate::mcp::serve_http(
                cx.name, cx.dir, args.host, args.port, access_key, tls, watcher,
            )
            .await
        }
        other => bail!("unknown transport {other:?}"),
    }
}

fn validate_http_access(
    manifest: &crate::cortex::Manifest,
    dir: &std::path::Path,
    host: &str,
    access_key: &crate::cortex::AccessKey,
    certificate_override: Option<&std::path::Path>,
    private_key_override: Option<&std::path::Path>,
) -> Result<Option<(PathBuf, PathBuf)>> {
    let (manifest_certificate, manifest_private_key) =
        crate::cortex::resolve_tls_paths(dir, manifest.access.as_ref());
    let certificate = certificate_override
        .map(PathBuf::from)
        .unwrap_or(manifest_certificate);
    let private_key = private_key_override
        .map(PathBuf::from)
        .unwrap_or(manifest_private_key);
    if certificate.as_os_str().is_empty() != private_key.as_os_str().is_empty() {
        bail!(
            "--tls-cert and --tls-key must resolve to a complete pair (CLI flags override access.tls_cert_path and access.tls_key_path independently)"
        )
    }
    let tls = (!certificate.as_os_str().is_empty()).then_some((certificate, private_key));
    if access_key.keyed() && tls.is_none() {
        bail!(
            "refusing to serve MCP bearer authentication over plaintext HTTP; configure access.tls_cert_path and access.tls_key_path"
        )
    }
    if !["127.0.0.1", "localhost", "::1"].contains(&host) && !access_key.keyed() {
        bail!(
            "unauthenticated Rust HTTP transport is restricted to loopback; configure a shared key and TLS before binding a network address"
        )
    }
    Ok(tls)
}

fn semantic_client(cx: &Cortex) -> Result<(HttpEmbedder, String, f64)> {
    let search = cx.manifest.search.as_ref().context(
        "semantic search needs search.embedding_model in cortex.md (then: noema embeddings backfill)",
    )?;
    if search.embedding_model.is_empty() {
        bail!(
            "semantic search needs search.embedding_model in cortex.md (then: noema embeddings backfill)"
        );
    }
    let endpoint = cx.manifest.resolved_embedding_endpoint()?;
    if endpoint.is_empty() {
        bail!(
            "semantic search needs search.embedding_endpoint (or consolidation.local_llm_endpoint) in cortex.md"
        );
    }
    let client = HttpEmbedder::new(&endpoint, &cx.manifest.resolved_embedding_api_key_env()?)?;
    Ok((
        client,
        search.embedding_model.clone(),
        search.effective_hybrid_weight(),
    ))
}

fn print_trace(row: &crate::cortex::Row, trace: &Trace) {
    println!(
        "ID:      {}\nTitle:   {}\nType:    {}",
        row.id, row.title, row.trace_type
    );
    if !row.author.is_empty() {
        println!("Author:  {}", row.author)
    }
    if !row.tags.is_empty() {
        println!("Tags:    {}", row.tags.join(", "))
    }
    println!(
        "Created: {}\nUpdated: {}\n\n{}",
        row.created_at, row.updated_at, trace.body
    );
}
fn print_rows(rows: Vec<crate::cortex::Row>) {
    if rows.is_empty() {
        println!("No traces found.");
        return;
    }
    println!("ID\tTITLE\tTYPE\tAUTHOR\tTAGS\tCREATED");
    for row in rows {
        println!(
            "{}\t{}\t{}\t{}\t{}\t{}",
            if row.archived_at.is_empty() {
                row.id
            } else {
                format!("[a] {}", row.id)
            },
            row.title,
            row.trace_type,
            row.author,
            row.tags.join(", "),
            row.created_at.get(..10).unwrap_or(&row.created_at)
        );
    }
}
fn read_stdin() -> Result<String> {
    let mut body = String::new();
    io::stdin()
        .read_to_string(&mut body)
        .context("reading stdin")?;
    Ok(body)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::cortex::{AccessConfig, FederationConfig, Manifest, PeerEntry, read_manifest};

    fn cortex(parent: &std::path::Path, name: &str) -> Cortex {
        Cortex::create(name, parent).unwrap();
        Cortex::open(name, parent.join(name)).unwrap()
    }

    #[test]
    fn trace_verifier_reports_and_backfills_files_without_changing_updated() {
        let temp = tempfile::tempdir().unwrap();
        let cx = cortex(temp.path(), "verify");
        let mut trace = Trace::new("Verify", "fact", "", vec![], "body");
        cx.add(&mut trace).unwrap();
        let path = cx.trace_file(&trace.frontmatter.id, false);
        let mut on_disk = Trace::parse_file(&path).unwrap();
        let updated = on_disk.frontmatter.updated.clone();
        on_disk.frontmatter.content_hash.clear();
        on_disk.write_preserving_updated(&path).unwrap();

        let mut output = Vec::new();
        verify_traces(&cx, false, &mut output).unwrap();
        let output = String::from_utf8(output).unwrap();
        assert!(output.contains("Checked 1 trace(s)."));
        assert!(output.contains("All hashes OK."));
        assert!(
            Trace::parse_file(&path)
                .unwrap()
                .frontmatter
                .content_hash
                .is_empty()
        );

        let mut output = Vec::new();
        verify_traces(&cx, true, &mut output).unwrap();
        let output = String::from_utf8(output).unwrap();
        assert!(output.contains("BACKFILL"));
        assert!(output.contains("1 trace(s) backfilled."));
        let repaired = Trace::parse_file(&path).unwrap();
        assert_eq!(repaired.frontmatter.updated, updated);
        assert_eq!(
            repaired.frontmatter.content_hash,
            crate::trace::content_hash("body")
        );
    }

    #[test]
    fn drift_verifier_uses_foreign_source_hash() {
        let temp = tempfile::tempdir().unwrap();
        let source = cortex(temp.path(), "source");
        let target = cortex(temp.path(), "target");
        let mut trace = Trace::new("Federated", "fact", "", vec![], "published");
        trace.frontmatter.source_hash = crate::trace::content_hash("published");
        trace.frontmatter.source_locked = true;
        source.add(&mut trace).unwrap();
        let event = source
            .history(&trace.frontmatter.id)
            .unwrap()
            .into_iter()
            .find(|event| event.action == "create")
            .unwrap();
        target.replay_event(&event).unwrap();
        let path = target.trace_file(&trace.frontmatter.id, false);
        let mut local = Trace::parse_file(&path).unwrap();
        local.body = "locally changed".into();
        local.write_preserving_updated(&path).unwrap();

        let mut output = Vec::new();
        verify_drift(&target, &mut output).unwrap();
        let output = String::from_utf8(output).unwrap();
        assert!(output.contains("DRIFTED"));
        assert!(output.contains("locked: yes"));
        assert!(output.contains("1 trace(s) have drifted from their source."));
    }

    #[test]
    fn cortex_doctor_reports_health_and_missing_layout() {
        let temp = tempfile::tempdir().unwrap();
        let cx = cortex(temp.path(), "doctor");
        let mut output = Vec::new();
        verify_cortex(&cx, &mut output).unwrap();
        let output = String::from_utf8(output).unwrap();
        assert!(output.contains("framed YAML frontmatter"));
        assert!(output.contains("WAL enabled"));
        assert!(output.contains("0 fail"));

        fs::remove_dir_all(cx.trash_dir()).unwrap();
        let mut output = Vec::new();
        assert!(verify_cortex(&cx, &mut output).is_err());
        let output = String::from_utf8(output).unwrap();
        assert!(output.contains("[fail] cortex layout"));
        assert!(output.contains("trash/traces/"));
    }

    #[tokio::test]
    async fn cli_resolve_dispatches_to_the_cortex_engine() {
        let temp = tempfile::tempdir().unwrap();
        let mut cx = cortex(temp.path(), "resolve");
        let mut original = Trace::new("Original", "fact", "", vec![], "old body");
        cx.add(&mut original).unwrap();
        let body = format!(
            "## Concurrent edits detected\n\n**Trace:** {}\n**Conflicting origins:** resolve (LOCAL123), peer (REMOTE12)\n\n### Version from resolve (LOCAL123)\n**Vector clock:** {{}}\n\nlocal body\n\n### Version from peer (REMOTE12)\n**Vector clock:** {{}}\n\nremote body",
            original.frontmatter.id
        );
        let mut divergence = Trace::new("Divergence: Original", "divergence", "", vec![], body);
        divergence.frontmatter.derived_from = vec![original.frontmatter.id.clone()];
        cx.add(&mut divergence).unwrap();

        execute_cortex_command(
            &mut cx,
            Command::Resolve {
                divergence_id: divergence.frontmatter.id.clone(),
                accept: Some("peer".into()),
                custom: None,
            },
        )
        .await
        .unwrap();
        assert_eq!(
            cx.get_trace(&original.frontmatter.id).unwrap().1.body,
            "remote body"
        );
        assert!(
            !cx.get(&divergence.frontmatter.id)
                .unwrap()
                .trashed_at
                .is_empty()
        );
    }

    #[test]
    fn experimental_http_transport_fails_closed() {
        let manifest = Manifest::default();
        let temp = tempfile::tempdir().unwrap();
        assert!(
            validate_http_access(
                &manifest,
                temp.path(),
                "127.0.0.1",
                &Default::default(),
                None,
                None
            )
            .unwrap()
            .is_none()
        );
        assert!(
            validate_http_access(
                &manifest,
                temp.path(),
                "0.0.0.0",
                &Default::default(),
                None,
                None
            )
            .is_err()
        );

        let protected = Manifest {
            access: Some(AccessConfig {
                tls_cert_path: "server.crt".into(),
                tls_key_path: "server.key".into(),
                ..Default::default()
            }),
            ..Default::default()
        };
        let keyed = crate::cortex::AccessKey {
            value: "test-key".into(),
            ..Default::default()
        };
        assert!(
            validate_http_access(&protected, temp.path(), "127.0.0.1", &keyed, None, None)
                .unwrap()
                .is_some()
        );
        assert!(
            validate_http_access(&manifest, temp.path(), "127.0.0.1", &keyed, None, None).is_err()
        );

        let cli_cert = temp.path().join("cli.crt");
        let cli_key = temp.path().join("cli.key");
        let resolved = validate_http_access(
            &protected,
            temp.path(),
            "127.0.0.1",
            &keyed,
            Some(&cli_cert),
            Some(&cli_key),
        )
        .unwrap()
        .unwrap();
        assert_eq!(resolved, (cli_cert.clone(), cli_key));

        let mixed = validate_http_access(
            &protected,
            temp.path(),
            "127.0.0.1",
            &keyed,
            Some(&cli_cert),
            None,
        )
        .unwrap()
        .unwrap();
        assert_eq!(mixed.0, cli_cert);
        assert_eq!(mixed.1, temp.path().join("server.key"));
    }

    #[test]
    fn repin_peer_preserves_cursor_and_pauses_until_explicit_resume() {
        let temp = tempfile::tempdir().unwrap();
        let mut manifest = Cortex::create("local", temp.path()).unwrap();
        let root = temp.path().join("local");
        manifest.federation = Some(FederationConfig {
            peers: vec![PeerEntry {
                name: "peer-a".into(),
                endpoint: "https://peer-a.example.com".into(),
                ..Default::default()
            }],
            ..Default::default()
        });
        write_manifest(&root, &manifest).unwrap();
        let mut cx = Cortex::open("local", &root).unwrap();
        cx.set_federation_state("peer:peer-a:cortex_id", "01KNOWNPEER")
            .unwrap();
        cx.set_federation_state("peer:peer-a:last_event", "01CURSOR")
            .unwrap();
        cx.set_federation_state("cortexkey:01KNOWNPEER", "old-key")
            .unwrap();
        let (_, public, _) = eventsig::generate().unwrap();

        repin_peer(&mut cx, "peer-a", &public).unwrap();

        let persisted = read_manifest(&root).unwrap();
        let peer = &persisted.federation.unwrap().peers[0];
        assert_eq!(peer.mode, "paused");
        assert_eq!(peer.pubkey, public);
        assert_eq!(
            cx.federation_state("peer:peer-a:last_event").unwrap(),
            "01CURSOR"
        );
        assert_eq!(
            cx.federation_state("cortexkey:01KNOWNPEER").unwrap(),
            "old-key"
        );
    }
}
