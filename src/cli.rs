use std::{
    fs::{self, OpenOptions},
    io::{self, IsTerminal, Read, Write},
    path::{Path, PathBuf},
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
    cortex::{
        Cortex, EmbedBackfillOptions, ListOptions, SemanticOptions, TraceIdExists, write_manifest,
    },
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
        #[arg(short = 'f', long)]
        force: bool,
    },
    /// Edit a Trace in $EDITOR
    Edit {
        id: String,
        #[arg(short = 'f', long)]
        force: bool,
    },
    /// Move a Trace to trash
    #[command(alias = "rm", alias = "delete")]
    Remove {
        id: String,
        #[arg(short = 'f', long)]
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
        #[command(subcommand)]
        command: Option<EventsCommand>,
        trace_id: Option<String>,
        #[arg(long)]
        since: Option<String>,
        #[arg(long, default_value_t = 50)]
        limit: usize,
        #[arg(long)]
        all: bool,
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
        #[arg(long)]
        backfill: bool,
        #[command(subcommand)]
        command: Option<VerifyCommand>,
    },
    /// Legacy alias for `verify drift`
    #[command(hide = true)]
    Drift,
    /// Serve MCP
    #[command(alias = "server")]
    Serve(ServeArgs),
    /// Open the terminal UI
    Tui {
        #[arg(long, env = "NOEMA_THEME", value_parser = ["auto", "dark", "light"])]
        theme: Option<String>,
    },
    /// Manage bundled plugins
    Plugin {
        #[command(subcommand)]
        command: PluginCommand,
    },
    /// Generate or install shell completions
    Completion {
        #[command(subcommand)]
        command: CompletionCommand,
    },
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
    title: Option<String>,
    #[arg(long="type", value_parser=["fact", "decision", "preference", "context", "skill", "intent", "observation", "note", "divergence"])]
    trace_type: Option<String>,
    #[arg(long)]
    author: Option<String>,
    #[arg(long = "tag")]
    tags: Vec<String>,
    #[arg(long)]
    body: Option<String>,
}

#[derive(Debug, Subcommand)]
enum CompletionCommand {
    /// Print bash completion script to stdout
    Bash,
    /// Print zsh completion script to stdout
    Zsh,
    /// Print fish completion script to stdout
    Fish,
    /// Install completions into your shell config
    Install {
        #[arg(long)]
        shell: Option<String>,
        #[arg(short = 'q', long)]
        quiet: bool,
    },
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
        #[arg(long, default_value = "text")]
        output: String,
        #[arg(long, default_value_t = 14)]
        zero_engagement_age_days: i64,
    },
    Popular {
        #[arg(long, default_value_t = 10)]
        top: usize,
        #[arg(long, default_value = "text")]
        output: String,
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
    #[command(alias = "rm", alias = "delete")]
    Remove {
        name: String,
        #[arg(long)]
        purge: bool,
        #[arg(long)]
        force: bool,
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
        #[arg(short = 'y', long)]
        yes: bool,
        #[arg(long)]
        key_rotated: bool,
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
        #[arg(short = 'y', long)]
        yes: bool,
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
enum EventsCommand {
    /// Synthesize create events for active traces missing one
    Backfill {
        #[arg(long)]
        dry_run: bool,
        #[arg(short = 'y', long)]
        yes: bool,
    },
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
    #[arg(long, action = clap::ArgAction::Append)]
    host: Vec<String>,
    #[arg(long, action = clap::ArgAction::Append)]
    host_dynamic: Vec<String>,
    #[arg(long)]
    port: Option<u16>,
    #[arg(long)]
    no_watch: bool,
    #[arg(long)]
    tls_cert: Option<PathBuf>,
    #[arg(long)]
    tls_key: Option<PathBuf>,
    #[arg(long)]
    insecure_allow_expired: bool,
    #[arg(long)]
    log_file: Option<PathBuf>,
    #[arg(long)]
    log_stderr: bool,
    #[arg(long)]
    print_config: bool,
    #[arg(long)]
    print_systemd_unit: bool,
    #[arg(long)]
    print_launchd_plist: bool,
}

pub async fn run() -> Result<()> {
    let cli = Cli::parse();
    let selected = cli.cortex.as_deref();
    match cli.command {
        Command::Init { name, path } => init(&name, path)?,
        Command::Use { name } => use_cortex(&name)?,
        Command::Cortex { command } => cortex_command(command)?,
        Command::Version => println!("noema v{VERSION}\nimplementation: Rust"),
        Command::Completion { command } => completion_command(command)?,
        Command::Config { command } => config_command(command)?,
        Command::Plugin { command } => plugin_command(command)?,
        Command::Serve(args) => serve(selected, args).await?,
        Command::Consolidate(args) => consolidate(selected, args).await?,
        Command::Migrate { command } => migrate_command(selected, command)?,
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
            let (title, trace_type, author, tags, body) = collect_add_args(args)?;
            add_trace_interactive(cx, title, trace_type, author, tags, body)?;
        }
        Command::List(args) => print_rows(cx.list(&args.into())?),
        Command::Get { id } => {
            let (row, trace) = cx
                .get_trace(&id)
                .map_err(|_| anyhow::anyhow!("trace {:?} not found", id))?;
            print_trace(&row, &trace);
        }
        Command::Append { id, content, force } => {
            cx.set_force_source_lock(force);
            let content = content.unwrap_or(read_stdin()?);
            cx.append(&id, &content, false)?;
            println!("Content appended to {id}");
        }
        Command::Edit { id, force } => {
            cx.set_force_source_lock(force);
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
            if force {
                cx.remove_hard(&id)?;
                println!("Trace {id} permanently deleted.");
            } else {
                cx.trash(&id)?;
                println!("Trace moved to trash: {id}");
            }
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
            command,
            trace_id,
            since,
            limit,
            all: _,
        } => {
            if let Some(EventsCommand::Backfill { dry_run, yes }) = command {
                events_backfill(cx, dry_run, yes)?;
                return Ok(());
            }
            if let Some(id) = trace_id {
                let events = cx.history(&id)?;
                if events.is_empty() {
                    println!("No events found.");
                } else {
                    for event in events {
                        println!(
                            "{}  {:<10}  {}  origin={}",
                            event.id, event.action, event.timestamp, event.origin
                        );
                    }
                }
            } else {
                let events = cx.events_since(since.as_deref().unwrap_or(""), limit)?;
                if events.is_empty() {
                    println!("No events found.");
                } else {
                    for event in &events {
                        println!(
                            "{}  {:<10}  {:<40}  {}  origin={}",
                            event.id, event.action, event.trace_id, event.timestamp, event.origin
                        );
                    }
                    if events.len() == limit {
                        println!(
                            "\n(showing {} events — use --since {} to see more)",
                            limit,
                            events.last().unwrap().id
                        );
                    }
                }
            }
        }
        Command::Drift => verify(cx, Some(VerifyCommand::Drift), false)?,
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
        Command::Tui { theme } => {
            let theme = match theme {
                Some(theme) => theme,
                None => Config::load()?.theme().to_owned(),
            };
            crate::tui::run(cx, &theme)?;
        }
        Command::Federation { command } => federation_command(cx, command).await?,
        Command::Keygen { force } => keygen(cx, force)?,
        Command::Verify { backfill, command } => verify(cx, command, backfill)?,
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
        Command::Init { .. }
        | Command::Use { .. }
        | Command::Cortex { .. }
        | Command::Migrate { .. }
        | Command::Consolidate(_)
        | Command::Serve(_)
        | Command::Completion { .. }
        | Command::Version
        | Command::Config { .. }
        | Command::Plugin { .. } => unreachable!(),
    }
    Ok(())
}

fn migrate_command(selected: Option<&str>, command: MigrateCommand) -> Result<()> {
    let MigrateCommand::CortexId { reset, yes } = command;
    let mut config = Config::load().context("loading config")?;
    let name = selected
        .filter(|name| !name.is_empty())
        .unwrap_or(&config.default)
        .to_owned();
    if name.is_empty() {
        bail!("no cortex specified: use --cortex, set NOEMA_CORTEX, or run `noema use <name>`");
    }
    let entry = config
        .cortexes
        .get(&name)
        .cloned()
        .ok_or_else(|| anyhow::anyhow!("unknown cortex {name:?}"))?;
    let manifest = crate::cortex::read_manifest(&entry.path).context("reading cortex.md")?;
    if !reset
        && manifest.version >= crate::cortex::MANIFEST_VERSION
        && !manifest.id.is_empty()
        && !crate::migration::has_pending_migration(&entry.path)
    {
        println!(
            "Cortex {:?} is already at manifest version {} (id={}); nothing to do.",
            name, manifest.version, manifest.id
        );
        return Ok(());
    }
    let new_id = crate::migration::planned_identity(&entry.path, &name, reset)?;
    println!(
        "About to migrate cortex {:?} at {}",
        name,
        entry.path.display()
    );
    println!("  current version: {}", manifest.version);
    println!(
        "  current id:      {}",
        if manifest.id.is_empty() {
            "(none)"
        } else {
            &manifest.id
        }
    );
    println!("  new version:     {}", crate::cortex::MANIFEST_VERSION);
    println!("  new id:          {new_id}");
    if reset {
        println!("  mode:            --reset (re-key local rows under new id)");
    }
    if !yes {
        print!("Proceed? [y/N]: ");
        io::stdout().flush()?;
        let mut response = String::new();
        io::stdin().read_line(&mut response)?;
        if !matches!(response.trim(), "y" | "Y" | "yes") {
            bail!("aborted by user");
        }
    }

    let result =
        crate::migration::migrate_cortex_id(&mut config, &name, &entry, reset, &new_id, None)?;
    println!("\nMigration complete.");
    println!(
        "  cortex.md updated: id={} version={}",
        result.new_id,
        crate::cortex::MANIFEST_VERSION
    );
    println!("  traces backfilled: {}", result.traces_updated);
    println!("  events backfilled: {}", result.events_updated);
    println!("  vector-clock buckets moved: {}", result.clock_moved);
    if result.peers_cleared > 0 {
        println!("  stale peer buckets cleared: {}", result.peers_cleared);
    }
    if result.pins_cleared > 0 {
        println!(
            "  peer cortex_id pins cleared: {} (peers will re-pin on next handshake)",
            result.pins_cleared
        );
    }
    if result.cursors_cleared > 0 {
        println!(
            "  peer sync cursors cleared: {} (peers will re-pull from the start of each log)",
            result.cursors_cleared
        );
    }
    println!(
        "  backups: cortex.md.{}.bak, db/noema.db.{}.bak",
        result.stamp, result.stamp
    );
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
        CortexCommand::Remove { name, purge, force } => {
            remove_cortex(&name, purge, force)?;
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

fn remove_cortex(name: &str, purge: bool, force: bool) -> Result<()> {
    let mut config = Config::load()?;
    let entry = config
        .cortexes
        .get(name)
        .cloned()
        .ok_or_else(|| anyhow::anyhow!("unknown cortex {name:?}"))?;
    if config.default == name && !force {
        bail!(
            "cortex {name:?} is the current default — switch with `noema use <other>` first, or re-run with --force"
        );
    }
    if !force {
        let mut references = Vec::new();
        for (other_name, other_entry) in &config.cortexes {
            if other_name == name {
                continue;
            }
            let Ok(manifest) = crate::cortex::read_manifest(&other_entry.path) else {
                continue;
            };
            if manifest
                .federation
                .as_ref()
                .is_some_and(|federation| federation.peers.iter().any(|peer| peer.name == name))
            {
                references.push(other_name.clone());
            }
        }
        references.sort();
        if !references.is_empty() {
            bail!(
                "cortex {name:?} is referenced as a federation peer in: {} — remove those peer entries first, or re-run with --force",
                references.join(", ")
            );
        }
    }
    if purge && !force {
        println!("This will permanently delete the cortex directory:");
        println!("  {}", entry.path.display());
        println!("Traces, event log, and federation state will all be lost.");
        print!("Proceed? [y/N]: ");
        io::stdout().flush()?;
        if !matches!(read_line()?.as_str(), "y" | "Y" | "yes") {
            bail!("aborted by user");
        }
    }
    if purge && !entry.path.join("cortex.md").is_file() {
        bail!(
            "refusing to purge {} because it does not contain cortex.md",
            entry.path.display()
        );
    }
    let was_default = config.default == name;
    config.cortexes.remove(name);
    let mut promoted = None;
    if was_default {
        config.default.clear();
        if config.cortexes.len() == 1 {
            let only = config.cortexes.keys().next().unwrap().clone();
            config.default = only.clone();
            promoted = Some(only);
        }
    }
    config.save()?;
    if purge {
        fs::remove_dir_all(&entry.path)
            .with_context(|| format!("removing {}", entry.path.display()))?;
        println!(
            "Cortex {name:?} removed from config and deleted from {}",
            entry.path.display()
        );
    } else {
        println!(
            "Cortex {name:?} unregistered from config (directory at {} was not touched)",
            entry.path.display()
        );
    }
    if let Some(promoted) = promoted {
        println!("Promoted {promoted:?} as the new default cortex.");
    } else if was_default {
        if config.cortexes.is_empty() {
            println!("No cortexes remain. Run `noema init --name <name>` to create one.");
        } else {
            println!("No default cortex set. Use `noema use <name>` to pick one.");
        }
    }
    Ok(())
}

fn memory_command(cx: &Cortex, command: MemoryCommand) -> Result<()> {
    match command {
        MemoryCommand::Stats {
            detailed,
            output,
            zero_engagement_age_days,
        } => {
            let tiers = cx.tier_stats()?;
            let engagement = detailed.then(|| cx.engagement_stats()).transpose()?;
            let lineage = detailed.then(|| cx.mid_lineage_breakdown()).transpose()?;
            let mid_engagement = detailed
                .then(|| {
                    cx.mid_engagement_snapshot(chrono::Duration::days(zero_engagement_age_days))
                })
                .transpose()?;
            if output == "json" {
                let mut report = serde_json::Map::new();
                report.insert("tiers".into(), serde_json::to_value(&tiers)?);
                if let Some(value) = &engagement {
                    report.insert("engagement".into(), serde_json::to_value(value)?);
                }
                if let Some(value) = &lineage {
                    report.insert("mid_lineage".into(), serde_json::to_value(value)?);
                }
                if let Some(value) = &mid_engagement {
                    report.insert("mid_engagement".into(), serde_json::to_value(value)?);
                }
                println!(
                    "{}",
                    serde_json::to_string_pretty(&serde_json::Value::Object(report))?
                );
            } else if output == "text" {
                println!(
                    "Tier\tCount\nshort\t{}\nmid\t{}\nlong\t{}\npurged\t{}",
                    tiers.short, tiers.mid, tiers.long, tiers.purged
                );
                if let (Some(engagement), Some(lineage), Some(mid)) =
                    (engagement, lineage, mid_engagement)
                {
                    println!(
                        "\nSignal\tTotal\nreads\t{}\nsearch hits\t{}\nmodifies\t{}",
                        engagement.total_reads,
                        engagement.total_search_hits,
                        engagement.total_modifies
                    );
                    println!(
                        "\nMid lineage\tCount\n0 sources\t{}\n1 source\t{}\n2+ sources\t{}",
                        lineage.no_sources, lineage.single_source, lineage.multi_source
                    );
                    println!(
                        "\nMid engagement\tCount\nzero engagement\t{}\nzero engagement ({}d+)\t{}",
                        mid.zero_engagement, zero_engagement_age_days, mid.zero_engagement_older
                    );
                }
            } else {
                bail!("unsupported --output {output:?} (try: text, json)");
            }
        }
        MemoryCommand::Popular { top, output } => {
            let top = if top == 0 { 10 } else { top };
            let traces = cx.top_searched_traces(top)?;
            let tags = cx.tag_activity(top)?;
            if output == "json" {
                println!(
                    "{}",
                    serde_json::to_string_pretty(&serde_json::json!({
                        "schema_version": 1,
                        "top": top,
                        "traces": traces,
                        "tags": tags,
                    }))?
                );
            } else if output == "text" {
                println!("Top {top} traces by search popularity");
                if traces.is_empty() {
                    println!("  (no traces with engagement yet)");
                } else {
                    println!("  Hits\tReads\tTier\tType\tTitle");
                    for trace in traces {
                        println!(
                            "  {}\t{}\t{}\t{}\t{}",
                            trace.search_hits,
                            trace.read_count,
                            trace.tier,
                            trace.trace_type,
                            trace.title
                        );
                    }
                }
                println!("\nTop {top} tags by aggregate engagement");
                if tags.is_empty() {
                    println!("  (no tagged traces with engagement yet)");
                } else {
                    println!("  Tag\tTraces\tHits\tReads\tModifies");
                    for tag in tags {
                        println!(
                            "  {}\t{}\t{}\t{}\t{}",
                            tag.tag,
                            tag.trace_count,
                            tag.search_hits,
                            tag.read_count,
                            tag.modify_count
                        );
                    }
                }
            } else {
                bail!("unsupported --output {output:?} (try: text, json)");
            }
        }
        MemoryCommand::Health { since, output } => {
            let since_duration = crate::cortex::parse_since(&since)?;
            let activity = cx.consolidation_activity(since_duration)?;
            let latency = cx.promotion_latency()?;
            let one_source_mid = cx.one_source_mid_count()?;
            let value = serde_json::json!({
                "schema_version": 1,
                "activity": activity,
                "latency": latency,
                "one_source_mid": one_source_mid,
            });
            match output.as_deref().unwrap_or("text") {
                "json" => println!("{}", serde_json::to_string_pretty(&value)?),
                "text" => print_memory_health_text(&since, &value),
                other => bail!("unsupported --output {other:?} (try: text, json)"),
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
            tier,
            reason,
            confirm,
            hard,
        } => {
            if !confirm {
                bail!("--confirm is required")
            };
            cx.admin_purge(&trace_id, &reason, &tier, hard)?;
            println!("Purged {trace_id}: {reason}");
        }
    }
    Ok(())
}

fn events_backfill(cx: &Cortex, dry_run: bool, assume_yes: bool) -> Result<()> {
    let preview = cx.backfill_create_events(true)?;
    if preview.backfilled_ids.is_empty() && preview.skipped_ids.is_empty() {
        println!("Nothing to backfill — every trace already has a create event in the log.");
        return Ok(());
    }
    println!(
        "Cortex {:?}: {} trace(s) would receive a backfill create event.",
        cx.name,
        preview.backfilled_ids.len()
    );
    for id in &preview.backfilled_ids {
        println!("  + {id}");
    }
    if !preview.skipped_ids.is_empty() {
        println!(
            "\n{} archived/trashed trace(s) lack a create event but will be skipped",
            preview.skipped_ids.len()
        );
        println!("(recover or unarchive them first if they need to federate):");
        for id in &preview.skipped_ids {
            println!("  - {id}");
        }
    }
    if dry_run {
        println!("\n(dry run — no events written)");
        return Ok(());
    }
    if preview.backfilled_ids.is_empty() {
        println!("\nNo active traces to backfill.");
        return Ok(());
    }
    if !assume_yes {
        print!("\nProceed? [y/N]: ");
        io::stdout().flush()?;
        let response = read_line()?;
        if !matches!(response.as_str(), "y" | "Y" | "yes") {
            bail!("aborted by user");
        }
    }
    let result = cx.backfill_create_events(false)?;
    println!(
        "\nBackfilled {} create event(s). Peers will replay them on the next sync poll.",
        result.backfilled_ids.len()
    );
    Ok(())
}

fn print_memory_health_text(since: &str, report: &serde_json::Value) {
    let activity = &report["activity"];
    println!("Consolidation activity — last {since}");
    let daily = activity["daily"]
        .as_array()
        .map(Vec::as_slice)
        .unwrap_or(&[]);
    if daily.is_empty() {
        println!("  (no events in window)");
    } else {
        println!("  Date\tClaim\tSuccess\tFail\tLostElec\tPromote\tDistill");
        for day in daily {
            println!(
                "  {}\t{}\t{}\t{}\t{}\t{}\t{}",
                day["date"].as_str().unwrap_or(""),
                day["claim"],
                day["success"],
                day["fail"],
                day["lost_election"],
                day["promote"],
                day["distill"]
            );
        }
        let totals = &activity["totals"];
        println!(
            "  Total\t{}\t{}\t{}\t{}\t{}\t{}",
            totals["claim"],
            totals["success"],
            totals["fail"],
            totals["lost_election"],
            totals["promote"],
            totals["distill"]
        );
    }
    let latency = &report["latency"];
    println!("\nPromotion latency (all-time)");
    println!("  Transition\tCount\tp50\tp95");
    for (label, key) in [("short→mid", "short_to_mid"), ("mid→long", "mid_to_long")] {
        println!(
            "  {label}\t{}\t{}\t{}",
            latency[key]["count"],
            latency[key]["p50"].as_str().unwrap_or("0s"),
            latency[key]["p95"].as_str().unwrap_or("0s")
        );
    }
    println!("\n1-source mid leak detector");
    println!(
        "  current: {}\n  promoted in last 7d: {}",
        report["one_source_mid"]["current"], report["one_source_mid"]["promoted_last_7d"]
    );
}

async fn federation_command(cx: &mut Cortex, command: FederationCommand) -> Result<()> {
    match command {
        FederationCommand::Status => {
            println!(
                "{}",
                serde_json::to_string_pretty(&crate::federation::status(cx)?)?
            );
        }
        FederationCommand::Peers => {
            let peers = cx
                .manifest
                .federation
                .as_ref()
                .map(|federation| federation.peers.as_slice())
                .unwrap_or(&[]);
            if peers.is_empty() {
                println!(
                    "No peers configured. Add peers to cortex.md under the federation section."
                );
            } else {
                for peer in peers {
                    println!("  {}  {}", peer.name, peer.endpoint);
                }
            }
        }
        FederationCommand::AddPeer { name, endpoint } => {
            if name == cx.manifest.name {
                bail!("peer name {name:?} matches this cortex's own name; pick a different label");
            }
            let fed = cx.manifest.federation.get_or_insert_with(Default::default);
            if fed.peers.iter().any(|p| p.name == name) {
                bail!("peer already exists")
            };
            let endpoint = endpoint.trim_end_matches('/').to_owned();
            fed.peers.push(crate::cortex::PeerEntry {
                name: name.clone(),
                endpoint: endpoint.clone(),
                ..Default::default()
            });
            write_manifest(&cx.dir, &cx.manifest)?;
            println!("Added peer {name:?} ({endpoint}) to cortex.md");
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
            println!(
                "Federation mode updated. Restart `noema serve` for the change to take effect."
            );
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
            println!("Peer {name:?} paused.");
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
            println!("Peer {name:?} resumed.");
        }
        FederationCommand::RePinPeer { name, pubkey } => {
            repin_peer(cx, &name, &pubkey)?;
        }
        FederationCommand::ResetPeer {
            names,
            yes,
            key_rotated,
        } => reset_federation_peers(cx, &names, yes, key_rotated)?,
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

fn reset_federation_peers(
    cx: &Cortex,
    names: &[String],
    assume_yes: bool,
    key_rotated_only: bool,
) -> Result<()> {
    if names.is_empty() {
        bail!("at least one peer name is required");
    }
    let peers = &cx
        .manifest
        .federation
        .as_ref()
        .map(|federation| federation.peers.as_slice())
        .unwrap_or_default();
    let mut snapshots = Vec::new();
    for name in names {
        let peer = peers
            .iter()
            .find(|peer| &peer.name == name)
            .ok_or_else(|| anyhow::anyhow!("peer {name:?} is not configured in cortex.md"))?;
        let cortex_id = cx.federation_state(&format!("peer:{name}:cortex_id"))?;
        let cursor = cx.federation_state(&format!("peer:{name}:last_event"))?;
        let last_seen = cx.federation_state(&format!("peer:{name}:last_seen"))?;
        snapshots.push((name, &peer.endpoint, cortex_id, cursor, last_seen));
    }
    if key_rotated_only {
        println!(
            "About to clear the pinned signing key for {} peer(s) in cortex {:?}.",
            snapshots.len(),
            cx.name
        );
    } else {
        println!(
            "About to reset federation state for {} peer(s) in cortex {:?}:",
            snapshots.len(),
            cx.name
        );
    }
    for (name, endpoint, cortex_id, cursor, last_seen) in &snapshots {
        println!("  {name} ({endpoint})");
        println!(
            "    pinned cortex_id: {}",
            if cortex_id.is_empty() { "-" } else { cortex_id }
        );
        if !key_rotated_only {
            println!(
                "    last_event:       {}",
                if cursor.is_empty() { "-" } else { cursor }
            );
            println!(
                "    last_seen:        {}",
                if last_seen.is_empty() { "-" } else { last_seen }
            );
        }
    }
    if !assume_yes {
        print!("Proceed? [y/N]: ");
        io::stdout().flush()?;
        if !matches!(read_line()?.as_str(), "y" | "Y" | "yes") {
            bail!("aborted by user");
        }
    }
    if key_rotated_only {
        let mut cleared = 0;
        for (name, _, cortex_id, _, _) in snapshots {
            if cortex_id.is_empty() {
                println!("  {name}: no pinned identity yet — nothing to clear");
                continue;
            }
            cx.delete_federation_state(&format!("cortexkey:{cortex_id}"))?;
            cleared += 1;
            println!("  {name}: signing-key pin cleared (cursor and identity kept)");
        }
        println!("\nCleared {cleared} signing-key pin(s).");
        return Ok(());
    }
    let mut clock = cx.get_clock()?;
    let mut buckets_dropped = 0;
    for (name, _, cortex_id, _, _) in snapshots {
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
            if clock.remove(&cortex_id).is_some() {
                buckets_dropped += 1;
            }
        }
        println!("  {name}: state cleared");
    }
    if buckets_dropped > 0 {
        cx.set_federation_state("vclock", &serde_json::to_string(&clock)?)?;
    }
    println!("\nReset complete.");
    if buckets_dropped > 0 {
        println!("  vector-clock buckets dropped: {buckets_dropped}");
    }
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

fn verify(cx: &Cortex, command: Option<VerifyCommand>, parent_backfill: bool) -> Result<()> {
    let stdout = io::stdout();
    let mut out = stdout.lock();
    match command {
        Some(VerifyCommand::Traces { backfill }) => {
            verify_traces(cx, backfill || parent_backfill, &mut out)?
        }
        None => verify_traces(cx, parent_backfill, &mut out)?,
        Some(VerifyCommand::Cortex) => verify_cortex(cx, &mut out)?,
        Some(VerifyCommand::Drift) => verify_drift(cx, &mut out)?,
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
            "trash_days" if cfg.trash_days == 0 => println!("0 (default: 30)"),
            "trash_days" => println!("{}", cfg.trash_days),
            _ => bail!("unknown config key"),
        },
        ConfigCommand::Set { key, value } => match key.as_str() {
            "ui.theme" => {
                if !["auto", "dark", "light"].contains(&value.as_str()) {
                    bail!("invalid theme")
                };
                cfg.ui = Some(crate::config::UiConfig { theme: value });
                cfg.save()?;
                println!("ui.theme = {}", cfg.theme());
            }
            "trash_days" => {
                let days: u32 = value.parse().with_context(|| {
                    format!("invalid trash_days {value:?}: must be a non-negative integer")
                })?;
                cfg.trash_days = days;
                cfg.save()?;
                if days == 0 {
                    println!("trash_days = 0 (default: 30)");
                } else {
                    println!("trash_days = {days}");
                }
            }
            _ => bail!("unknown config key"),
        },
        ConfigCommand::List => println!(
            "trash_days   {}\n             How many days trashed traces are kept before auto-purge (0 = default of 30)\nui.theme     {}\n             TUI color scheme — \"auto\", \"dark\", or \"light\"",
            if cfg.trash_days == 0 {
                "0 (default: 30)".into()
            } else {
                cfg.trash_days.to_string()
            },
            cfg.theme()
        ),
    }
    Ok(())
}

fn cortex_flag_was_explicit() -> bool {
    let mut arguments = std::env::args_os();
    while let Some(argument) = arguments.next() {
        if argument == "--cortex" {
            return arguments.next().is_some();
        }
        if argument.to_string_lossy().starts_with("--cortex=") {
            return true;
        }
    }
    false
}

fn print_mcp_config(
    selected: Option<&str>,
    transport: &str,
    hosts: &[String],
    dynamic_hosts: &[String],
    port: u16,
    certificate: Option<&Path>,
    private_key: Option<&Path>,
) -> Result<()> {
    let executable = std::env::current_exe()?;
    let name = selected
        .filter(|name| !name.is_empty())
        .map(str::to_owned)
        .unwrap_or(Config::load()?.default);
    let entry = match transport {
        "stdio" => {
            let mut arguments = vec![serde_json::json!("serve")];
            if !name.is_empty() {
                arguments.push(serde_json::json!("--cortex"));
                arguments.push(serde_json::json!(name));
            }
            serde_json::json!({"command": executable, "args": arguments})
        }
        "http" => {
            let host = dynamic_hosts
                .first()
                .or_else(|| hosts.first())
                .context("--print-config --transport http requires --host")?;
            if host
                .parse::<std::net::IpAddr>()
                .is_ok_and(|address| address.is_unspecified())
            {
                bail!("{host} is a wildcard bind address; pass the address clients should dial");
            }
            if certificate.is_some() != private_key.is_some() {
                bail!("--tls-cert and --tls-key must be provided together");
            }
            let scheme = if certificate.is_some() {
                "https"
            } else {
                "http"
            };
            let url_host = host
                .parse::<std::net::Ipv6Addr>()
                .map(|_| format!("[{host}]"))
                .unwrap_or_else(|_| host.clone());
            serde_json::json!({
                "url": format!("{scheme}://{url_host}:{port}/mcp"),
                "headers": {"Authorization": "Bearer ${NOEMA_MCP_KEY}"},
            })
        }
        other => bail!("--print-config does not support --transport {other:?}"),
    };
    println!(
        "{}",
        serde_json::to_string_pretty(&serde_json::json!({"mcpServers":{"noema":entry}}))?
    );
    Ok(())
}

fn serve_arguments(selected: Option<&str>, args: &ServeArgs, port: u16) -> Result<Vec<String>> {
    if !cortex_flag_was_explicit() {
        bail!("service output requires an explicit --cortex flag");
    }
    let cortex = selected
        .filter(|name| !name.is_empty())
        .context("service output requires an explicit --cortex flag")?;
    if args.transport != "http" {
        bail!("service output requires --transport http");
    }
    if args.host.is_empty() {
        bail!("service output requires at least one --host");
    }
    if args.tls_cert.is_some() != args.tls_key.is_some() {
        bail!("--tls-cert and --tls-key must be provided together");
    }
    let mut output = vec![
        "serve".into(),
        "--cortex".into(),
        cortex.into(),
        "--transport".into(),
        "http".into(),
    ];
    for host in &args.host {
        output.extend(["--host".into(), host.clone()]);
    }
    for host in &args.host_dynamic {
        output.extend(["--host-dynamic".into(), host.clone()]);
    }
    output.extend(["--port".into(), port.to_string()]);
    if let (Some(certificate), Some(private_key)) = (&args.tls_cert, &args.tls_key) {
        output.extend([
            "--tls-cert".into(),
            certificate.display().to_string(),
            "--tls-key".into(),
            private_key.display().to_string(),
        ]);
    }
    Ok(output)
}

fn print_systemd_unit(selected: Option<&str>, args: &ServeArgs, port: u16) -> Result<()> {
    let executable = std::env::current_exe()?;
    let arguments = serve_arguments(selected, args, port)?;
    let user = std::env::var("USER").unwrap_or_else(|_| "noema".into());
    let command = std::iter::once(systemd_escape(executable.display().to_string()))
        .chain(arguments.into_iter().map(systemd_escape))
        .collect::<Vec<_>>()
        .join(" ");
    println!(
        "[Unit]\nDescription=Noema MCP server ({})\nAfter=network-online.target\n\n[Service]\nType=simple\nUser={}\nExecStart={}\nRestart=on-failure\nEnvironment=NOEMA_MCP_KEY=replace-with-real-key\n\n[Install]\nWantedBy=multi-user.target",
        selected.unwrap_or(""),
        user,
        command
    );
    Ok(())
}

fn systemd_escape(argument: String) -> String {
    format!(
        "\"{}\"",
        argument.replace('\\', "\\\\").replace('"', "\\\"")
    )
}

fn print_launchd_plist(selected: Option<&str>, args: &ServeArgs, port: u16) -> Result<()> {
    let cortex = selected
        .filter(|name| !name.is_empty())
        .context("--print-launchd-plist requires an explicit --cortex flag")?;
    let executable = std::env::current_exe()?;
    let arguments = serve_arguments(selected, args, port)?;
    let mut array = format!(
        "    <string>{}</string>\n",
        xml_escape(&executable.display().to_string())
    );
    for argument in arguments {
        array.push_str(&format!("    <string>{}</string>\n", xml_escape(&argument)));
    }
    println!(
        "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n<plist version=\"1.0\">\n<dict>\n  <key>Label</key><string>com.fail-safe.noema.{}</string>\n  <key>ProgramArguments</key>\n  <array>\n{}  </array>\n  <key>RunAtLoad</key><true/>\n  <key>KeepAlive</key><true/>\n  <key>EnvironmentVariables</key><dict><key>NOEMA_MCP_KEY</key><string>replace-with-real-key</string></dict>\n</dict>\n</plist>",
        xml_escape(cortex),
        array
    );
    Ok(())
}

fn xml_escape(value: &str) -> String {
    value
        .replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
        .replace('"', "&quot;")
        .replace('\'', "&apos;")
}

fn setup_serve_logging(
    cortex_name: &str,
    transport: &str,
    explicit_path: Option<&Path>,
    log_stderr: bool,
) -> Result<()> {
    if log_stderr {
        return Ok(());
    }
    let path = if let Some(path) = explicit_path {
        Some(path.to_path_buf())
    } else if transport == "stdio" {
        let state = std::env::var_os("XDG_STATE_HOME")
            .map(PathBuf::from)
            .unwrap_or_else(|| {
                PathBuf::from(std::env::var_os("HOME").unwrap_or_default()).join(".local/state")
            });
        Some(state.join("noema").join(format!("{cortex_name}.log")))
    } else {
        None
    };
    let Some(path) = path else {
        return Ok(());
    };
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)?;
    }
    let file = OpenOptions::new().create(true).append(true).open(&path)?;
    #[cfg(unix)]
    {
        use std::os::{fd::AsRawFd, unix::fs::PermissionsExt};
        fs::set_permissions(&path, fs::Permissions::from_mode(0o640))?;
        eprintln!("[serve] logs -> {}", path.display());
        if unsafe { libc::dup2(file.as_raw_fd(), libc::STDERR_FILENO) } < 0 {
            return Err(std::io::Error::last_os_error()).context("redirecting stderr");
        }
    }
    #[cfg(not(unix))]
    {
        let _ = file;
        bail!("serve log redirection is not implemented on this platform");
    }
    Ok(())
}

async fn serve(selected: Option<&str>, args: ServeArgs) -> Result<()> {
    let port = args.port.unwrap_or(3000);
    if args.print_config {
        return print_mcp_config(
            selected,
            &args.transport,
            &args.host,
            &args.host_dynamic,
            port,
            args.tls_cert.as_deref(),
            args.tls_key.as_deref(),
        );
    }
    if args.print_systemd_unit {
        return print_systemd_unit(selected, &args, port);
    }
    if args.print_launchd_plist {
        return print_launchd_plist(selected, &args, port);
    }
    if args.transport == "stdio"
        && (!args.host.is_empty()
            || !args.host_dynamic.is_empty()
            || args.port.is_some()
            || args.tls_cert.is_some()
            || args.tls_key.is_some())
    {
        bail!("--host/--host-dynamic/--port/--tls-cert/--tls-key require --transport http");
    }
    if args.log_stderr && args.log_file.is_some() {
        bail!("--log-file and --log-stderr are mutually exclusive");
    }
    let cx = Cortex::resolve(selected)?;
    setup_serve_logging(
        &cx.name,
        &args.transport,
        args.log_file.as_deref(),
        args.log_stderr,
    )?;
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
            if args.host.is_empty() {
                bail!("--host is required for HTTP transport (for example --host 127.0.0.1)");
            }
            if !cortex_flag_was_explicit() {
                bail!("refusing to start HTTP without an explicit --cortex flag");
            }
            let host = &args.host[0];
            let access_key = crate::cortex::load_access_key(&cx.dir, cx.manifest.access.as_ref())?;
            let tls = validate_http_access(
                &cx.manifest,
                &cx.dir,
                host,
                &access_key,
                args.tls_cert.as_deref(),
                args.tls_key.as_deref(),
            )?;
            for additional_host in args.host.iter().skip(1) {
                validate_http_access(
                    &cx.manifest,
                    &cx.dir,
                    additional_host,
                    &access_key,
                    args.tls_cert.as_deref(),
                    args.tls_key.as_deref(),
                )?;
            }
            for dynamic_host in &args.host_dynamic {
                validate_http_access(
                    &cx.manifest,
                    &cx.dir,
                    dynamic_host,
                    &access_key,
                    args.tls_cert.as_deref(),
                    args.tls_key.as_deref(),
                )?;
            }
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
                cx.name,
                cx.dir,
                crate::mcp::HttpListenConfig {
                    hosts: args.host,
                    dynamic_hosts: args.host_dynamic,
                    port,
                },
                access_key,
                tls,
                watcher,
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
    if host
        .parse::<std::net::IpAddr>()
        .is_ok_and(|address| address.is_unspecified())
    {
        bail!("binding to {host} is not allowed — use an explicit loopback or network address");
    }
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

fn prompt(label: &str) -> Result<String> {
    print!("{label}: ");
    io::stdout().flush()?;
    read_line()
}

fn read_line() -> Result<String> {
    let mut line = String::new();
    io::stdin().read_line(&mut line)?;
    Ok(line.trim_end_matches(['\r', '\n']).to_owned())
}

fn collect_add_args(args: AddArgs) -> Result<(String, String, String, Vec<String>, String)> {
    let title = match args.title.filter(|value| !value.is_empty()) {
        Some(title) => title,
        None => {
            let title = prompt("Title")?;
            if title.is_empty() {
                bail!("title is required");
            }
            title
        }
    };
    let trace_type = match args.trace_type.filter(|value| !value.is_empty()) {
        Some(trace_type) => trace_type,
        None => {
            print!("Type [{}] (note): ", crate::trace::VALID_TYPES.join("/"));
            io::stdout().flush()?;
            let value = read_line()?;
            if value.is_empty() {
                "note".into()
            } else {
                value
            }
        }
    };
    if !crate::trace::VALID_TYPES.contains(&trace_type.as_str()) {
        bail!("invalid type {trace_type:?}");
    }
    let author = match args.author {
        Some(author) => author,
        None => prompt("Author (optional)")?,
    };
    let tags = if args.tags.is_empty() {
        split_prompt_tags(&prompt("Tags (comma-separated, optional)")?)
    } else {
        args.tags
    };
    let body = match args.body {
        Some(body) => body,
        None => {
            if io::stdin().is_terminal() {
                println!("Body (Ctrl+D to save, Ctrl+C to cancel):");
            }
            read_stdin()?
        }
    };
    Ok((title, trace_type, author, tags, body))
}

fn add_trace_interactive(
    cx: &Cortex,
    mut title: String,
    trace_type: String,
    author: String,
    tags: Vec<String>,
    body: String,
) -> Result<()> {
    loop {
        let mut trace = Trace::new(
            title.clone(),
            trace_type.clone(),
            author.clone(),
            tags.clone(),
            body.clone(),
        );
        match cx.add(&mut trace) {
            Ok(()) => {
                println!("Trace added: {}", trace.frontmatter.id);
                return Ok(());
            }
            Err(error) => {
                let Some(collision) = error.downcast_ref::<TraceIdExists>() else {
                    return Err(error);
                };
                eprintln!("{collision}");
                match prompt_collision_choice(&collision.state)? {
                    "r" => {
                        cx.recover(&collision.id)?;
                        println!(
                            "Recovered {}. New content was discarded — edit the recovered trace if you want to update it.",
                            collision.id
                        );
                        return Ok(());
                    }
                    "u" => {
                        cx.unarchive(&collision.id)?;
                        println!(
                            "Unarchived {}. New content was discarded — edit the unarchived trace if you want to update it.",
                            collision.id
                        );
                        return Ok(());
                    }
                    "p" => {
                        let row = cx.get(&collision.id)?;
                        cx.admin_purge(
                            &collision.id,
                            "freed by interactive `noema add` to recreate id",
                            &row.tier,
                            true,
                        )?;
                    }
                    "v" => {
                        title = loop {
                            let (candidate, eof) = read_choice_line("New title")?;
                            if eof {
                                bail!("input closed before a new title was supplied");
                            }
                            if !candidate.is_empty() {
                                break candidate;
                            }
                            eprintln!("  title cannot be empty");
                        };
                    }
                    "q" => return Err(anyhow::anyhow!(collision.to_string())),
                    _ => unreachable!(),
                }
            }
        }
    }
}

fn prompt_collision_choice(state: &str) -> Result<&'static str> {
    let (menu, valid): (&str, &[&str]) = match state {
        "trashed" => (
            "(R)ecover trashed / (P)urge & retry / (V)ary title / (Q)uit",
            &["r", "p", "v", "q"],
        ),
        "archived" => (
            "(U)narchive / (P)urge & retry / (V)ary title / (Q)uit",
            &["u", "p", "v", "q"],
        ),
        "purged" => (
            "(V)ary title / (Q)uit  (the slot can only be freed via `noema memory purge --hard`)",
            &["v", "q"],
        ),
        _ => (
            "(V)ary title / (Q)uit  (an active trace already holds this id)",
            &["v", "q"],
        ),
    };
    loop {
        let (line, eof) = read_choice_line(menu)?;
        if eof {
            return Ok("q");
        }
        let choice = line
            .trim()
            .to_ascii_lowercase()
            .chars()
            .next()
            .map(|character| character.to_string())
            .unwrap_or_default();
        if let Some(valid) = valid.iter().copied().find(|valid| *valid == choice) {
            return Ok(valid);
        }
        eprintln!("  unrecognised option");
    }
}

fn read_choice_line(label: &str) -> Result<(String, bool)> {
    print!("{label}: ");
    io::stdout().flush()?;
    let mut line = String::new();
    let bytes = io::stdin().read_line(&mut line)?;
    Ok((line.trim_end_matches(['\r', '\n']).to_owned(), bytes == 0))
}

fn split_prompt_tags(value: &str) -> Vec<String> {
    value
        .split([',', ';'])
        .map(str::trim)
        .filter(|tag| !tag.is_empty())
        .map(str::to_owned)
        .collect()
}

fn completion_command(command: CompletionCommand) -> Result<()> {
    match command {
        CompletionCommand::Bash => write_completion(Shell::Bash, &mut io::stdout()),
        CompletionCommand::Zsh => write_completion(Shell::Zsh, &mut io::stdout()),
        CompletionCommand::Fish => write_completion(Shell::Fish, &mut io::stdout()),
        CompletionCommand::Install { shell, quiet } => {
            let shell = shell
                .filter(|shell| !shell.is_empty())
                .or_else(detect_shell)
                .context("could not detect shell; use --shell bash|zsh|fish")?;
            install_completion(&shell, quiet)
        }
    }
}

fn write_completion(shell: Shell, output: &mut dyn Write) -> Result<()> {
    generate(shell, &mut Cli::command(), "noema", output);
    Ok(())
}

fn detect_shell() -> Option<String> {
    let shell = std::env::var_os("SHELL")?;
    let shell = Path::new(&shell).file_name()?.to_str()?;
    ["bash", "zsh", "fish"]
        .contains(&shell)
        .then(|| shell.to_owned())
}

fn install_completion(shell: &str, quiet: bool) -> Result<()> {
    let home = std::env::var_os("HOME").context("HOME is not set")?;
    let home = PathBuf::from(home);
    let (kind, path) = completion_target(shell, &home)?;
    let mut bytes = Vec::new();
    write_completion(kind, &mut bytes)?;
    write_completion_atomic(&path, &bytes)?;
    if quiet {
        return Ok(());
    }
    println!("Installed to {}", path.display());
    match shell {
        "bash" => {
            println!("\nAdd to ~/.bashrc if not already sourced:");
            println!("  [[ -f {} ]] && source {}", path.display(), path.display());
        }
        "zsh" => {
            println!("\nAdd to ~/.zshrc if not already present:");
            println!("  fpath+=(~/.zfunc)");
            println!("  autoload -Uz compinit && compinit");
        }
        "fish" => println!("Completions will be active in new fish sessions."),
        _ => unreachable!(),
    }
    Ok(())
}

fn completion_target(shell: &str, home: &Path) -> Result<(Shell, PathBuf)> {
    match shell {
        "bash" => Ok((Shell::Bash, home.join(".bash_completion.d/noema"))),
        "zsh" => Ok((Shell::Zsh, home.join(".zfunc/_noema"))),
        "fish" => Ok((
            Shell::Fish,
            home.join(".config/fish/completions/noema.fish"),
        )),
        _ => bail!("unsupported shell {shell:?} — supported: bash, zsh, fish"),
    }
}

fn write_completion_atomic(path: &Path, bytes: &[u8]) -> Result<()> {
    let directory = path
        .parent()
        .ok_or_else(|| anyhow::anyhow!("completion path has no parent directory"))?;
    fs::create_dir_all(directory)?;
    let temporary = directory.join(".noema-completion.tmp");
    if let Ok(metadata) = fs::symlink_metadata(&temporary) {
        if !metadata.file_type().is_file() {
            bail!("refusing to replace non-file completion temporary artifact");
        }
        fs::remove_file(&temporary)?;
    }
    let result = (|| -> Result<()> {
        let mut file = OpenOptions::new()
            .write(true)
            .create_new(true)
            .open(&temporary)?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            fs::set_permissions(&temporary, fs::Permissions::from_mode(0o644))?;
        }
        file.write_all(bytes)?;
        file.sync_all()?;
        drop(file);
        fs::rename(&temporary, path)?;
        crate::trace::sync_directory(directory)?;
        Ok(())
    })();
    if result.is_err() {
        let _ = fs::remove_file(temporary);
    }
    result
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
    fn add_accepts_guided_mode_and_parses_prompt_tags() {
        let cli = Cli::try_parse_from(["noema", "add"]).unwrap();
        let Command::Add(args) = cli.command else {
            panic!("expected add command");
        };
        assert!(args.title.is_none());
        assert!(args.trace_type.is_none());
        assert!(args.author.is_none());
        assert_eq!(
            split_prompt_tags(" alpha, beta ; gamma ,, "),
            vec!["alpha", "beta", "gamma"]
        );
        assert!(Cli::try_parse_from(["noema", "add", "--type", "invalid"]).is_err());
    }

    #[test]
    fn established_short_and_parent_flags_parse() {
        let cli = Cli::try_parse_from(["noema", "remove", "trace-id", "-f"]).unwrap();
        assert!(matches!(cli.command, Command::Remove { force: true, .. }));

        let cli = Cli::try_parse_from(["noema", "verify", "--backfill"]).unwrap();
        assert!(matches!(
            cli.command,
            Command::Verify {
                backfill: true,
                command: None
            }
        ));

        let cli = Cli::try_parse_from(["noema", "tui", "--theme", "light"]).unwrap();
        assert!(matches!(
            cli.command,
            Command::Tui {
                theme: Some(ref theme)
            } if theme == "light"
        ));
        assert!(Cli::try_parse_from(["noema", "tui", "--theme", "sepia"]).is_err());
    }

    #[test]
    fn completion_subcommands_generate_and_install_atomically() {
        for (name, shell) in [
            ("bash", Shell::Bash),
            ("zsh", Shell::Zsh),
            ("fish", Shell::Fish),
        ] {
            let mut output = Vec::new();
            write_completion(shell, &mut output).unwrap();
            let output = String::from_utf8(output).unwrap();
            assert!(output.contains("noema"), "{name} completion omitted binary");
        }
        let home = tempfile::tempdir().unwrap();
        let (_, path) = completion_target("zsh", home.path()).unwrap();
        write_completion_atomic(&path, b"first").unwrap();
        write_completion_atomic(&path, b"second").unwrap();
        assert_eq!(fs::read(&path).unwrap(), b"second");
        assert!(
            !path
                .parent()
                .unwrap()
                .join(".noema-completion.tmp")
                .exists()
        );
        assert!(completion_target("tcsh", home.path()).is_err());
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
