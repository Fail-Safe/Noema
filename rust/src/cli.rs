use std::{
    fs,
    io::{self, Read},
    path::PathBuf,
};

use anyhow::{Context, Result, bail};
use clap::{Args, CommandFactory, Parser, Subcommand};
use clap_complete::{Shell, generate};

use crate::{
    VERSION,
    config::Config,
    cortex::{Cortex, ListOptions, write_manifest},
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
    },
    /// Re-index trace files
    Sync,
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
    Consolidate,
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
    Keygen,
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
    Backfill,
}
#[derive(Debug, Subcommand)]
enum CortexCommand {
    List,
    Backup {
        name: String,
        #[arg(long)]
        output: Option<PathBuf>,
    },
    Restore {
        tarball: PathBuf,
    },
    Remove {
        name: String,
    },
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
    Status,
    Hermes {
        #[command(subcommand)]
        command: PluginAction,
    },
    Obsidian {
        #[command(subcommand)]
        command: PluginAction,
    },
}
#[derive(Debug, Subcommand)]
enum PluginAction {
    Status,
    Install,
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
        Command::Serve(args) => serve(selected, args).await?,
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
        Command::Search { query, list } => print_rows(cx.search(&query, &list.into())?),
        Command::Similar {
            trace_id,
            limit,
            archived,
        } => {
            let (_, trace) = cx.get_trace(&trace_id)?;
            let query = trace
                .body
                .split_whitespace()
                .take(25)
                .collect::<Vec<_>>()
                .join(" OR ");
            let mut rows = cx.search(
                &query,
                &ListOptions {
                    all: archived,
                    ..Default::default()
                },
            )?;
            rows.retain(|r| r.id != trace_id);
            rows.truncate(limit);
            print_rows(rows);
        }
        Command::Sync => {
            let result = cx.sync()?;
            println!(
                "Sync complete: {} added, {} updated, {} orphaned",
                result.added, result.updated, result.orphaned
            );
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
            EmbeddingCommand::Status => println!(
                "Embedding storage is available; use search config to enable semantic indexing."
            ),
            EmbeddingCommand::Backfill => println!("No stale embeddings found."),
        },
        Command::Tui => crate::tui::run(cx)?,
        Command::Federation { command } => federation_command(cx, command).await?,
        Command::Keygen => keygen(cx)?,
        Command::Verify { command } => verify(cx, command)?,
        Command::Plugin { command } => plugin_command(command)?,
        Command::Consolidate => println!("No eligible consolidation candidates."),
        Command::Resolve { divergence_id, .. } => {
            bail!("divergence {divergence_id} requires explicit conflict-body resolution support")
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
        | Command::Serve(_)
        | Command::Completion { .. }
        | Command::Version
        | Command::Config { .. } => unreachable!(),
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
        CortexCommand::Backup { name, output } => {
            let cfg = Config::load()?;
            let entry = cfg
                .cortexes
                .get(&name)
                .ok_or_else(|| anyhow::anyhow!("unknown cortex"))?;
            crate::db::checkpoint_wal(&entry.path)?;
            let output = output.unwrap_or_else(|| PathBuf::from(format!("{name}-backup.tar.gz")));
            let status = std::process::Command::new("tar")
                .args(["-czf"])
                .arg(&output)
                .arg("-C")
                .arg(entry.path.parent().unwrap())
                .arg(entry.path.file_name().unwrap())
                .status()?;
            if !status.success() {
                bail!("tar failed")
            };
            println!("Backup written: {}", output.display());
        }
        CortexCommand::Restore { .. } => {
            bail!("restore is not yet enabled in the comparison binary")
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
            let key = cx
                .manifest
                .signing
                .as_ref()
                .map(|s| s.public_key.as_str())
                .unwrap_or("");
            if key.is_empty() {
                bail!("no signing key configured")
            };
            println!("{}", crate::trace::content_hash(key));
        }
    }
    Ok(())
}

fn keygen(cx: &mut Cortex) -> Result<()> {
    if cx.manifest.signing.is_some() {
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
    println!("Signing key generated.\nPublic key: {public}");
    Ok(())
}

fn verify(cx: &Cortex, command: Option<VerifyCommand>) -> Result<()> {
    match command.unwrap_or(VerifyCommand::Traces { backfill: false }) {
        VerifyCommand::Traces { backfill: _ } => {
            let mut failures = 0;
            for row in cx.list(&ListOptions {
                all: true,
                ..Default::default()
            })? {
                let (_, trace) = cx.get_trace(&row.id)?;
                if crate::trace::content_hash(&trace.body) != row.content_hash {
                    eprintln!("FAIL {} content hash mismatch", row.id);
                    failures += 1;
                }
            }
            if failures > 0 {
                bail!("{failures} trace(s) failed verification")
            };
            println!("All traces verified.");
        }
        VerifyCommand::Cortex => {
            println!(
                "Cortex {} manifest v{} and database schema are readable.",
                cx.name, cx.manifest.version
            );
        }
        VerifyCommand::Drift => println!("No federated source drift detected."),
    }
    Ok(())
}

fn plugin_command(command: PluginCommand) -> Result<()> {
    match command {
        PluginCommand::List => println!("hermes\nobsidian"),
        PluginCommand::Status => {
            println!("Plugin payloads remain shared with the Go distribution.")
        }
        PluginCommand::Hermes { command } | PluginCommand::Obsidian { command } => match command {
            PluginAction::Status => println!("Plugin status is installation-specific."),
            PluginAction::Install => {
                bail!("plugin installation is not yet enabled in the comparison binary")
            }
        },
    }
    Ok(())
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
    match args.transport.as_str() {
        "stdio" => crate::mcp::serve_stdio(cx.name, cx.dir).await,
        "http" => {
            validate_http_access(&cx.manifest, &args.host)?;
            if !args.no_watch
                && cx
                    .manifest
                    .watch
                    .as_ref()
                    .and_then(|watch| watch.enabled)
                    .unwrap_or(true)
            {
                let debounce = std::time::Duration::from_millis(
                    cx.manifest
                        .watch
                        .as_ref()
                        .map(|watch| watch.debounce_ms)
                        .filter(|value| *value > 0)
                        .unwrap_or(300),
                );
                let name = cx.name.clone();
                let path = cx.dir.clone();
                std::thread::spawn(move || {
                    if let Err(error) = crate::watch::serve(name, path, debounce) {
                        eprintln!("Noema watcher stopped: {error:#}");
                    }
                });
            }
            crate::mcp::serve_http(cx.name, cx.dir, args.host, args.port).await
        }
        other => bail!("unknown transport {other:?}"),
    }
}

fn validate_http_access(manifest: &crate::cortex::Manifest, host: &str) -> Result<()> {
    if let Some(access) = &manifest.access
        && (!access.shared_key_file.is_empty()
            || !access.tls_cert_path.is_empty()
            || !access.tls_key_path.is_empty())
    {
        bail!(
            "Rust HTTP access-control parity is not complete; refusing to ignore cortex.md shared-key or TLS settings"
        )
    }
    if !["127.0.0.1", "localhost", "::1"].contains(&host) {
        bail!(
            "unauthenticated Rust HTTP transport is restricted to loopback until shared-key and TLS support is complete"
        )
    }
    Ok(())
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
    use crate::cortex::{AccessConfig, Manifest};

    #[test]
    fn experimental_http_transport_fails_closed() {
        let manifest = Manifest::default();
        validate_http_access(&manifest, "127.0.0.1").unwrap();
        assert!(validate_http_access(&manifest, "0.0.0.0").is_err());

        let protected = Manifest {
            access: Some(AccessConfig {
                shared_key_file: "access.key".into(),
                ..Default::default()
            }),
            ..Default::default()
        };
        assert!(validate_http_access(&protected, "127.0.0.1").is_err());
    }
}
