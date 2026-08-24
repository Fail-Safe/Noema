use std::{
    fmt,
    fs::{self, OpenOptions},
    io::Write,
    path::{Path, PathBuf},
};

use anyhow::{Context, Result, bail};
use serde_json::{Value, json};

const BOOTSTRAP: &str = "[mem-bootstrap-v1]: call the Noema get_instructions tool first, then follow its startup policy. Surface tool failures explicitly.";
const CODEX_START: &str = "# >>> noema integrate codex v1";
const CODEX_END: &str = "# <<< noema integrate codex v1";
const CLAUDE_HOOK_MARKER: &str = "noema-managed-session-bootstrap-v1";
const LEGACY_OPENCODE_PLUGIN_V1: &str = r#"import type { Plugin } from "@opencode-ai/plugin"

const bootstrap =
  "[mem-bootstrap-v1]: read and follow ~/.config/noema/noema-memory.md at session start (canonical Memory policy). Surface tool failures explicitly."

export const NoemaSessionBootstrap: Plugin = async () => ({
  "experimental.chat.system.transform": async (_input, output) => {
    output.system.push(bootstrap)
  },
})
"#;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Client {
    Codex,
    ClaudeCode,
    OpenCode,
}

impl Client {
    pub const ALL: [Self; 3] = [Self::Codex, Self::ClaudeCode, Self::OpenCode];

    pub fn name(self) -> &'static str {
        match self {
            Self::Codex => "codex",
            Self::ClaudeCode => "claude-code",
            Self::OpenCode => "opencode",
        }
    }

    pub fn description(self) -> &'static str {
        match self {
            Self::Codex => "Codex MCP configuration and SessionStart hook",
            Self::ClaudeCode => "Claude Code MCP configuration and SessionStart hook",
            Self::OpenCode => "OpenCode MCP configuration and bootstrap plugin",
        }
    }
}

impl fmt::Display for Client {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.name())
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Scope {
    User,
    Project,
}

impl Scope {
    pub fn name(self) -> &'static str {
        match self {
            Self::User => "user",
            Self::Project => "project",
        }
    }
}

impl fmt::Display for Scope {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.name())
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Transport {
    Stdio,
    Http,
}

impl Transport {
    pub fn name(self) -> &'static str {
        match self {
            Self::Stdio => "stdio",
            Self::Http => "http",
        }
    }
}

#[derive(Debug, Clone)]
pub struct Target {
    pub client: Client,
    pub scope: Scope,
    pub project_root: PathBuf,
    pub home: PathBuf,
    pub xdg_config_home: Option<PathBuf>,
}

impl Target {
    pub fn from_environment(client: Client, scope: Scope) -> Result<Self> {
        let home = std::env::var_os("HOME")
            .map(PathBuf::from)
            .context("HOME is not set")?;
        let cwd = std::env::current_dir().context("resolving current directory")?;
        Ok(Self {
            client,
            scope,
            project_root: find_project_root(&cwd),
            home,
            xdg_config_home: std::env::var_os("XDG_CONFIG_HOME").map(PathBuf::from),
        })
    }

    fn codex_config(&self) -> PathBuf {
        match self.scope {
            Scope::User => self.home.join(".codex/config.toml"),
            Scope::Project => self.project_root.join(".codex/config.toml"),
        }
    }

    fn claude_mcp_config(&self) -> PathBuf {
        match self.scope {
            Scope::User => self.home.join(".claude.json"),
            Scope::Project => self.project_root.join(".mcp.json"),
        }
    }

    fn claude_settings(&self) -> PathBuf {
        match self.scope {
            Scope::User => self.home.join(".claude/settings.json"),
            Scope::Project => self.project_root.join(".claude/settings.json"),
        }
    }

    fn opencode_root(&self) -> PathBuf {
        match self.scope {
            Scope::User => self
                .xdg_config_home
                .clone()
                .unwrap_or_else(|| self.home.join(".config"))
                .join("opencode"),
            Scope::Project => self.project_root.join(".opencode"),
        }
    }

    fn opencode_config(&self) -> PathBuf {
        match self.scope {
            Scope::User => self.opencode_root().join("opencode.jsonc"),
            Scope::Project => self.project_root.join("opencode.jsonc"),
        }
    }

    fn opencode_plugin(&self) -> PathBuf {
        self.opencode_root()
            .join("plugins/noema-session-bootstrap.ts")
    }
}

#[derive(Debug, Clone)]
pub struct ConnectionSpec {
    pub cortex: String,
    pub binary: PathBuf,
    pub transport: Transport,
    pub url: Option<String>,
    pub bearer_token_env: Option<String>,
}

impl ConnectionSpec {
    fn validate(&self) -> Result<()> {
        match self.transport {
            Transport::Stdio => {
                if self.url.is_some() {
                    bail!("--url requires --transport http");
                }
                if self.bearer_token_env.is_some() {
                    bail!("--bearer-token-env requires --transport http");
                }
            }
            Transport::Http => {
                let url = self
                    .url
                    .as_deref()
                    .context("--transport http requires --url")?;
                let parsed = reqwest::Url::parse(url).context("parsing integration URL")?;
                if !matches!(parsed.scheme(), "http" | "https") {
                    bail!("integration URL must use http or https");
                }
                if parsed.path() != "/mcp" {
                    bail!("integration URL path must be /mcp");
                }
            }
        }
        if let Some(name) = &self.bearer_token_env
            && !valid_env_name(name)
        {
            bail!("invalid bearer-token environment variable {name:?}");
        }
        Ok(())
    }
}

#[derive(Debug, Clone)]
pub struct Request {
    pub target: Target,
    pub connection: ConnectionSpec,
}

impl Request {
    pub fn from_environment(
        client: Client,
        scope: Scope,
        cortex: String,
        transport: Transport,
        url: Option<String>,
        bearer_token_env: Option<String>,
    ) -> Result<Self> {
        let request = Self {
            target: Target::from_environment(client, scope)?,
            connection: ConnectionSpec {
                cortex,
                binary: std::env::current_exe().context("resolving noema executable")?,
                transport,
                url,
                bearer_token_env,
            },
        };
        request.connection.validate()?;
        Ok(request)
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum State {
    Configured,
    UpToDate,
    NotInstalled,
    Drift,
    Conflict,
    Installed,
    Replaced,
    Removed,
    Unchanged,
    WouldInstall,
    WouldReplace,
    WouldRemove,
}

impl State {
    pub fn label(self) -> &'static str {
        match self {
            Self::Configured => "configured",
            Self::UpToDate => "up to date",
            Self::NotInstalled => "not installed",
            Self::Drift => "drift",
            Self::Conflict => "conflict",
            Self::Installed => "installed",
            Self::Replaced => "replaced",
            Self::Removed => "removed",
            Self::Unchanged => "unchanged",
            Self::WouldInstall => "would install",
            Self::WouldReplace => "would replace",
            Self::WouldRemove => "would remove",
        }
    }

    pub fn healthy(self) -> bool {
        matches!(
            self,
            Self::Configured | Self::UpToDate | Self::Unchanged | Self::Installed | Self::Replaced
        )
    }

    pub fn refused(self) -> bool {
        matches!(self, Self::Drift | Self::Conflict)
    }

    pub fn pending(self) -> bool {
        matches!(
            self,
            Self::WouldInstall | Self::WouldReplace | Self::WouldRemove
        )
    }
}

#[derive(Debug, Clone)]
pub struct ComponentReport {
    pub component: &'static str,
    pub path: PathBuf,
    pub state: State,
    pub transport: Option<Transport>,
}

#[derive(Debug, Clone)]
pub struct Report {
    pub client: Client,
    pub scope: Scope,
    pub components: Vec<ComponentReport>,
}

impl Report {
    pub fn healthy(&self) -> bool {
        self.components
            .iter()
            .all(|component| component.state.healthy())
    }

    pub fn refused(&self) -> bool {
        self.components
            .iter()
            .any(|component| component.state.refused())
    }

    pub fn pending(&self) -> bool {
        self.components
            .iter()
            .any(|component| component.state.pending())
    }
}

pub fn status(target: &Target) -> Result<Report> {
    match target.client {
        Client::Codex => codex_status(target),
        Client::ClaudeCode => claude_status(target),
        Client::OpenCode => opencode_status(target),
    }
}

pub fn install(request: &Request, check: bool, force: bool) -> Result<Report> {
    match request.target.client {
        Client::Codex => codex_install(request, check, force),
        Client::ClaudeCode => claude_install(request, check, force),
        Client::OpenCode => opencode_install(request, check, force),
    }
}

pub fn remove(target: &Target, check: bool, force: bool) -> Result<Report> {
    match target.client {
        Client::Codex => codex_remove(target, check, force),
        Client::ClaudeCode => claude_remove(target, check, force),
        Client::OpenCode => opencode_remove(target, check, force),
    }
}

pub fn print(request: &Request) -> Result<String> {
    match request.target.client {
        Client::Codex => Ok(format!(
            "# target: {}\n{}",
            request.target.codex_config().display(),
            codex_block(request)?
        )),
        Client::ClaudeCode => Ok(format!(
            "# MCP target: {}\n{}\n\n# hook target: {}\n{}\n",
            request.target.claude_mcp_config().display(),
            serde_json::to_string_pretty(
                &json!({"mcpServers": {"noema": mcp_value(request, Client::ClaudeCode)?}})
            )?,
            request.target.claude_settings().display(),
            serde_json::to_string_pretty(
                &json!({"hooks": {"SessionStart": [claude_hook_value()]}})
            )?
        )),
        Client::OpenCode => {
            require_opencode_v1(&request.target)?;
            Ok(format!(
                "# MCP target: {}\n{}\n\n# plugin target: {}\n{}",
                request.target.opencode_config().display(),
                serde_json::to_string_pretty(
                    &json!({"mcp": {"noema": mcp_value(request, Client::OpenCode)?}})
                )?,
                request.target.opencode_plugin().display(),
                opencode_plugin_source()
            ))
        }
    }
}

fn codex_status(target: &Target) -> Result<Report> {
    let path = target.codex_config();
    let (state, transport) = inspect_codex_block(&path)?;
    Ok(report(
        target,
        vec![("mcp + bootstrap", path, state, transport)],
    ))
}

fn codex_install(request: &Request, check: bool, force: bool) -> Result<Report> {
    let path = request.target.codex_config();
    let state = install_codex_block(&path, request, check, force)?;
    Ok(report(
        &request.target,
        vec![(
            "mcp + bootstrap",
            path,
            state,
            Some(request.connection.transport),
        )],
    ))
}

fn codex_remove(target: &Target, check: bool, force: bool) -> Result<Report> {
    let path = target.codex_config();
    let state = remove_text_block(&path, CODEX_START, CODEX_END, check, force)?;
    Ok(report(target, vec![("mcp + bootstrap", path, state, None)]))
}

fn codex_block(request: &Request) -> Result<String> {
    Ok(format!(
        "{CODEX_START}\n{}\n{}",
        codex_mcp_table(request)?,
        codex_hook_tail()?
    ))
}

fn codex_mcp_table(request: &Request) -> Result<String> {
    let mut lines = vec!["[mcp_servers.noema]".to_owned()];
    match request.connection.transport {
        Transport::Stdio => {
            lines.push(format!(
                "command = {}",
                toml_string(&request.connection.binary.to_string_lossy())?
            ));
            lines.push(format!(
                "args = [{}]",
                [
                    "--cortex",
                    &request.connection.cortex,
                    "serve",
                    "--transport",
                    "stdio",
                ]
                .iter()
                .map(|value| toml_string(value))
                .collect::<Result<Vec<_>>>()?
                .join(", ")
            ));
        }
        Transport::Http => {
            lines.push(format!(
                "url = {}",
                toml_string(request.connection.url.as_deref().unwrap())?
            ));
            if let Some(name) = &request.connection.bearer_token_env {
                lines.push(format!("bearer_token_env_var = {}", toml_string(name)?));
            }
        }
    }
    Ok(format!("{}\n", lines.join("\n")))
}

fn codex_hook_tail() -> Result<String> {
    Ok(format!(
        "{}\n",
        [
            "[[hooks.SessionStart]]".into(),
            "matcher = \"^(startup|resume|clear|compact)$\"".into(),
            String::new(),
            "[[hooks.SessionStart.hooks]]".into(),
            "type = \"command\"".into(),
            format!("command = {}", toml_string(&bootstrap_command())?),
            "timeout = 5".into(),
            CODEX_END.into(),
        ]
        .join("\n")
    ))
}

fn claude_status(target: &Target) -> Result<Report> {
    let mcp_path = target.claude_mcp_config();
    let hook_path = target.claude_settings();
    let (mcp, transport) = inspect_json_mcp(&mcp_path, &["mcpServers"], "noema")?;
    let hook = inspect_json_array_value(
        &hook_path,
        &["hooks", "SessionStart"],
        &claude_hook_value(),
        is_noema_hook,
    )?;
    Ok(report(
        target,
        vec![
            ("mcp", mcp_path, mcp, transport),
            ("bootstrap", hook_path, hook, None),
        ],
    ))
}

fn claude_install(request: &Request, check: bool, force: bool) -> Result<Report> {
    if !check {
        let preview = claude_install(request, true, force)?;
        if preview.refused() {
            return Ok(preview);
        }
    }
    let mcp_path = request.target.claude_mcp_config();
    let hook_path = request.target.claude_settings();
    let mcp = install_json_member(
        &mcp_path,
        &["mcpServers"],
        "noema",
        &mcp_value(request, Client::ClaudeCode)?,
        is_noema_mcp,
        check,
        force,
    )?;
    let hook = install_json_array_value(
        &hook_path,
        &["hooks", "SessionStart"],
        &claude_hook_value(),
        is_noema_hook,
        check,
        force,
    )?;
    Ok(report(
        &request.target,
        vec![
            ("mcp", mcp_path, mcp, Some(request.connection.transport)),
            ("bootstrap", hook_path, hook, None),
        ],
    ))
}

fn claude_remove(target: &Target, check: bool, force: bool) -> Result<Report> {
    if !check {
        let preview = claude_remove(target, true, force)?;
        if preview.refused() {
            return Ok(preview);
        }
    }
    let mcp_path = target.claude_mcp_config();
    let hook_path = target.claude_settings();
    let mcp = remove_json_member(
        &mcp_path,
        &["mcpServers"],
        "noema",
        is_noema_mcp,
        check,
        force,
    )?;
    let hook = remove_json_array_value(
        &hook_path,
        &["hooks", "SessionStart"],
        is_noema_hook,
        check,
        force,
    )?;
    Ok(report(
        target,
        vec![
            ("mcp", mcp_path, mcp, None),
            ("bootstrap", hook_path, hook, None),
        ],
    ))
}

fn opencode_status(target: &Target) -> Result<Report> {
    require_opencode_v1(target)?;
    let config = target.opencode_config();
    let plugin = target.opencode_plugin();
    let (mcp, transport) = inspect_json_mcp(&config, &["mcp"], "noema")?;
    let plugin_state = inspect_opencode_plugin(&plugin)?;
    Ok(report(
        target,
        vec![
            ("mcp", config, mcp, transport),
            ("bootstrap", plugin, plugin_state, None),
        ],
    ))
}

fn opencode_install(request: &Request, check: bool, force: bool) -> Result<Report> {
    require_opencode_v1(&request.target)?;
    if !check {
        let preview = opencode_install(request, true, force)?;
        if preview.refused() {
            return Ok(preview);
        }
    }
    let config = request.target.opencode_config();
    let plugin = request.target.opencode_plugin();
    let mcp = install_json_member(
        &config,
        &["mcp"],
        "noema",
        &mcp_value(request, Client::OpenCode)?,
        is_noema_mcp,
        check,
        force,
    )?;
    let plugin_state = install_opencode_plugin(&plugin, check, force)?;
    Ok(report(
        &request.target,
        vec![
            ("mcp", config, mcp, Some(request.connection.transport)),
            ("bootstrap", plugin, plugin_state, None),
        ],
    ))
}

fn opencode_remove(target: &Target, check: bool, force: bool) -> Result<Report> {
    require_opencode_v1(target)?;
    if !check {
        let preview = opencode_remove(target, true, force)?;
        if preview.refused() {
            return Ok(preview);
        }
    }
    let config = target.opencode_config();
    let plugin = target.opencode_plugin();
    let mcp = remove_json_member(&config, &["mcp"], "noema", is_noema_mcp, check, force)?;
    let plugin_state = remove_opencode_plugin(&plugin, check, force)?;
    Ok(report(
        target,
        vec![
            ("mcp", config, mcp, None),
            ("bootstrap", plugin, plugin_state, None),
        ],
    ))
}

fn report(
    target: &Target,
    components: Vec<(&'static str, PathBuf, State, Option<Transport>)>,
) -> Report {
    Report {
        client: target.client,
        scope: target.scope,
        components: components
            .into_iter()
            .map(|(component, path, state, transport)| ComponentReport {
                component,
                path,
                state,
                transport,
            })
            .collect(),
    }
}

fn mcp_value(request: &Request, client: Client) -> Result<Value> {
    match (request.connection.transport, client) {
        (Transport::Stdio, Client::Codex) => unreachable!(),
        (Transport::Stdio, Client::ClaudeCode) => Ok(json!({
            "type": "stdio",
            "command": request.connection.binary,
            "args": ["--cortex", request.connection.cortex, "serve", "--transport", "stdio"]
        })),
        (Transport::Stdio, Client::OpenCode) => Ok(json!({
            "type": "local",
            "command": [request.connection.binary, "--cortex", request.connection.cortex, "serve", "--transport", "stdio"],
            "enabled": true
        })),
        (Transport::Http, Client::ClaudeCode) => {
            let mut value =
                json!({"type": "http", "url": request.connection.url.as_deref().unwrap()});
            if let Some(name) = &request.connection.bearer_token_env {
                value["headers"] = json!({"Authorization": format!("Bearer ${{{name}}}")});
            }
            Ok(value)
        }
        (Transport::Http, Client::OpenCode) => {
            let mut value = json!({
                "type": "remote",
                "url": request.connection.url.as_deref().unwrap(),
                "enabled": true,
                "oauth": false
            });
            if let Some(name) = &request.connection.bearer_token_env {
                value["headers"] = json!({"Authorization": format!("Bearer {{env:{name}}}")});
            }
            Ok(value)
        }
        (Transport::Http, Client::Codex) => unreachable!(),
    }
}

fn claude_hook_value() -> Value {
    json!({
        "matcher": "startup|resume|clear|compact",
        "hooks": [{
            "type": "command",
            "command": bootstrap_command(),
            "timeout": 5
        }]
    })
}

fn bootstrap_command() -> String {
    let payload = json!({
        "hookSpecificOutput": {
            "hookEventName": "SessionStart",
            "additionalContext": BOOTSTRAP
        }
    });
    format!("printf '%s\\n' '{}' # {CLAUDE_HOOK_MARKER}", payload)
}

fn opencode_plugin_source() -> String {
    format!(
        "const bootstrap =\n  {}\n\nexport const NoemaSessionBootstrap = async () => ({{\n  \"experimental.chat.system.transform\": async (_input, output) => {{\n    output.system.push(bootstrap)\n  }},\n}})\n",
        serde_json::to_string(BOOTSTRAP).unwrap()
    )
}

fn require_opencode_v1(target: &Target) -> Result<()> {
    if let Some(source) = read_optional(&target.opencode_config())?
        && find_object_path(&source, &["mcp", "servers"])?.is_some()
    {
        bail!(
            "OpenCode v2 configuration detected; its plugin API requires a separate Noema adapter"
        );
    }
    let Ok(output) = std::process::Command::new("opencode")
        .arg("--version")
        .output()
    else {
        return Ok(());
    };
    if output.status.success()
        && String::from_utf8_lossy(&output.stdout)
            .trim()
            .split('.')
            .next()
            .and_then(|major| major.parse::<u32>().ok())
            .is_some_and(|major| major >= 2)
    {
        bail!("OpenCode v2 detected; its plugin API requires a separate Noema adapter");
    }
    Ok(())
}

fn is_noema_hook(value: &Value) -> bool {
    value
        .get("hooks")
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
        .filter_map(|hook| hook.get("command").and_then(Value::as_str))
        .any(|command| command.contains(CLAUDE_HOOK_MARKER))
}

fn is_noema_mcp(value: &Value) -> bool {
    let object = match value.as_object() {
        Some(object) => object,
        None => return false,
    };
    if let Some(command) = object.get("command") {
        if let Some(command) = command.as_str() {
            return Path::new(command)
                .file_name()
                .is_some_and(|name| name == "noema")
                && object
                    .get("args")
                    .and_then(Value::as_array)
                    .is_some_and(|args| args.iter().any(|arg| arg == "serve"));
        }
        if let Some(command) = command.as_array() {
            return command
                .first()
                .and_then(Value::as_str)
                .and_then(|command| Path::new(command).file_name())
                .is_some_and(|name| name == "noema")
                && command.iter().any(|arg| arg == "serve");
        }
    }
    object
        .get("url")
        .and_then(Value::as_str)
        .and_then(|url| reqwest::Url::parse(url).ok())
        .is_some_and(|url| url.path() == "/mcp")
}

fn inspect_codex_block(path: &Path) -> Result<(State, Option<Transport>)> {
    let Some(source) = read_optional(path)? else {
        return Ok((State::NotInstalled, None));
    };
    match managed_block_span(&source, CODEX_START, CODEX_END)? {
        Some((begin, finish)) => {
            let block = &source[begin..finish];
            let (current, transport) = inspect_codex_managed_block(block)?;
            if current {
                Ok((State::Configured, transport))
            } else {
                Ok((State::Drift, transport))
            }
        }
        None => match toml_table_span(&source, "[mcp_servers.noema]")? {
            Some((begin, finish)) => match codex_mcp_table_transport(&source[begin..finish]) {
                Some(transport) => Ok((State::Drift, Some(transport))),
                None => Ok((State::Conflict, None)),
            },
            None => Ok((State::NotInstalled, None)),
        },
    }
}

fn install_codex_block(path: &Path, request: &Request, check: bool, force: bool) -> Result<State> {
    let source = read_optional(path)?.unwrap_or_default();
    let expected = codex_block(request)?;
    let (state, replacement) = match managed_block_span(&source, CODEX_START, CODEX_END)? {
        Some((begin, finish))
            if codex_managed_block_compatible(&source[begin..finish], request)? =>
        {
            return Ok(State::Unchanged);
        }
        Some((begin, finish)) if force => {
            let mut next = source.clone();
            next.replace_range(begin..finish, &expected);
            (State::WouldReplace, next)
        }
        Some(_) => return Ok(State::Drift),
        None => {
            if source.contains(CLAUDE_HOOK_MARKER) {
                return Ok(State::Conflict);
            }
            if let Some((begin, finish)) = toml_table_span(&source, "[mcp_servers.noema]")? {
                if !codex_unmanaged_mcp_compatible(&source, begin, finish, request)? {
                    return Ok(State::Conflict);
                }
                let mut block = adopted_codex_block(&source[begin..finish])?;
                if finish < source.len() {
                    block.push('\n');
                }
                let mut next = source.clone();
                next.replace_range(begin..finish, &block);
                (State::WouldReplace, next)
            } else {
                let mut next = source.clone();
                if !next.is_empty() && !next.ends_with('\n') {
                    next.push('\n');
                }
                if !next.is_empty() {
                    next.push('\n');
                }
                next.push_str(&expected);
                (State::WouldInstall, next)
            }
        }
    };
    if check {
        return Ok(state);
    }
    write_atomic(path, replacement.as_bytes())?;
    Ok(match state {
        State::WouldInstall => State::Installed,
        State::WouldReplace => State::Replaced,
        _ => unreachable!(),
    })
}

fn codex_managed_block_compatible(block: &str, request: &Request) -> Result<bool> {
    let prefix = format!("{CODEX_START}\n");
    let Some(content) = block.strip_prefix(&prefix) else {
        return Ok(false);
    };
    let hook_header = "[[hooks.SessionStart]]";
    let Some(hook) = content.find(hook_header) else {
        return Ok(false);
    };
    Ok(codex_mcp_table_compatible(&content[..hook], request)?
        && content[hook..] == codex_hook_tail()?)
}

fn inspect_codex_managed_block(block: &str) -> Result<(bool, Option<Transport>)> {
    let prefix = format!("{CODEX_START}\n");
    let Some(content) = block.strip_prefix(&prefix) else {
        return Ok((false, None));
    };
    let hook_header = "[[hooks.SessionStart]]";
    let Some(hook) = content.find(hook_header) else {
        return Ok((false, codex_mcp_table_transport(content)));
    };
    let transport = codex_mcp_table_transport(&content[..hook]);
    Ok((
        transport.is_some() && content[hook..] == codex_hook_tail()?,
        transport,
    ))
}

fn adopted_codex_block(table: &str) -> Result<String> {
    let mut block = format!("{CODEX_START}\n{table}");
    if !block.ends_with('\n') {
        block.push('\n');
    }
    if !block.ends_with("\n\n") {
        block.push('\n');
    }
    block.push_str(&codex_hook_tail()?);
    Ok(block)
}

fn codex_unmanaged_mcp_compatible(
    source: &str,
    begin: usize,
    finish: usize,
    request: &Request,
) -> Result<bool> {
    let next_header = source[finish..]
        .lines()
        .next()
        .map(str::trim)
        .unwrap_or_default();
    if next_header.starts_with("[mcp_servers.noema.")
        || next_header.starts_with("[[mcp_servers.noema.")
    {
        return Ok(false);
    }
    codex_mcp_table_compatible(&source[begin..finish], request)
}

fn codex_mcp_table_compatible(table: &str, request: &Request) -> Result<bool> {
    const CONNECTION_KEYS: [&str; 4] = ["command", "args", "url", "bearer_token_env_var"];

    let expected_table = codex_mcp_table(request)?;
    let expected = expected_table
        .lines()
        .skip(1)
        .filter_map(toml_assignment)
        .collect::<Vec<_>>();
    let mut seen = vec![false; expected.len()];
    let mut header_seen = false;

    for line in table.lines().map(str::trim) {
        if line.is_empty() || line.starts_with('#') {
            continue;
        }
        if !header_seen {
            if line != "[mcp_servers.noema]" {
                return Ok(false);
            }
            header_seen = true;
            continue;
        }
        if line.starts_with('[') {
            return Ok(false);
        }
        let Some((key, value)) = toml_assignment(line) else {
            return Ok(false);
        };
        if !CONNECTION_KEYS.contains(&key) {
            continue;
        }
        let Some(index) = expected
            .iter()
            .position(|(expected_key, _)| *expected_key == key)
        else {
            return Ok(false);
        };
        if seen[index] || expected[index].1 != value {
            return Ok(false);
        }
        seen[index] = true;
    }

    Ok(header_seen && seen.into_iter().all(|field| field))
}

fn codex_mcp_table_transport(table: &str) -> Option<Transport> {
    const CONNECTION_KEYS: [&str; 4] = ["command", "args", "url", "bearer_token_env_var"];

    let mut header_seen = false;
    let mut fields = Vec::new();
    for line in table.lines().map(str::trim) {
        if line.is_empty() || line.starts_with('#') {
            continue;
        }
        if !header_seen {
            if line != "[mcp_servers.noema]" {
                return None;
            }
            header_seen = true;
            continue;
        }
        if line.starts_with('[') {
            return None;
        }
        let (key, value) = toml_assignment(line)?;
        if CONNECTION_KEYS.contains(&key) {
            if fields.iter().any(|(existing, _)| *existing == key) {
                return None;
            }
            fields.push((key, value));
        }
    }
    if !header_seen {
        return None;
    }

    let field = |name| {
        fields
            .iter()
            .find(|(key, _)| *key == name)
            .map(|(_, value)| *value)
    };
    if let Some(url) = field("url") {
        if field("command").is_some() || field("args").is_some() {
            return None;
        }
        let url = serde_json::from_str::<String>(url).ok()?;
        return reqwest::Url::parse(&url)
            .ok()
            .filter(|url| matches!(url.scheme(), "http" | "https") && url.path() == "/mcp")
            .map(|_| Transport::Http);
    }

    if field("bearer_token_env_var").is_some() {
        return None;
    }
    let command = serde_json::from_str::<String>(field("command")?).ok()?;
    let args = serde_json::from_str::<Vec<String>>(field("args")?).ok()?;
    (Path::new(&command)
        .file_name()
        .is_some_and(|name| name == "noema")
        && args.iter().any(|arg| arg == "serve"))
    .then_some(Transport::Stdio)
}

fn toml_assignment(line: &str) -> Option<(&str, &str)> {
    let (key, value) = line.split_once('=')?;
    Some((key.trim(), value.trim()))
}

fn toml_table_span(source: &str, header: &str) -> Result<Option<(usize, usize)>> {
    let mut headers = Vec::new();
    let mut offset = 0;
    for line in source.split_inclusive('\n') {
        if line.trim() == header {
            headers.push(offset);
        }
        offset += line.len();
    }
    if headers.len() > 1 {
        bail!("duplicate TOML integration table {header}");
    }
    let Some(begin) = headers.first().copied() else {
        return Ok(None);
    };
    let first_line_end = source[begin..]
        .find('\n')
        .map_or(source.len(), |relative| begin + relative + 1);
    let mut finish = source.len();
    let mut line_offset = first_line_end;
    for line in source[first_line_end..].split_inclusive('\n') {
        let trimmed = line.trim();
        if trimmed.starts_with('[') && (trimmed.ends_with(']') || trimmed.ends_with("]]")) {
            finish = line_offset;
            break;
        }
        line_offset += line.len();
    }
    Ok(Some((begin, finish)))
}

fn remove_text_block(
    path: &Path,
    start: &str,
    end: &str,
    check: bool,
    _force: bool,
) -> Result<State> {
    let Some(source) = read_optional(path)? else {
        return Ok(State::Unchanged);
    };
    let Some((begin, finish)) = managed_block_span(&source, start, end)? else {
        return Ok(State::Unchanged);
    };
    if check {
        return Ok(State::WouldRemove);
    }
    let mut next = source;
    let mut remove_begin = begin;
    if remove_begin > 0 && next.as_bytes()[remove_begin - 1] == b'\n' {
        remove_begin -= 1;
    }
    next.replace_range(remove_begin..finish, "");
    write_atomic(path, next.as_bytes())?;
    Ok(State::Removed)
}

fn managed_block_span(source: &str, start: &str, end: &str) -> Result<Option<(usize, usize)>> {
    let Some(begin) = source.find(start) else {
        if source.contains(end) {
            bail!("managed integration block has an end marker without a start marker");
        }
        return Ok(None);
    };
    let relative_end = source[begin..]
        .find(end)
        .context("managed integration block is missing its end marker")?;
    let mut finish = begin + relative_end + end.len();
    if source.as_bytes().get(finish) == Some(&b'\n') {
        finish += 1;
    }
    Ok(Some((begin, finish)))
}

fn inspect_opencode_plugin(path: &Path) -> Result<State> {
    let expected = opencode_plugin_source();
    match fs::read(path) {
        Ok(bytes) if bytes == expected.as_bytes() => Ok(State::UpToDate),
        Ok(_) => Ok(State::Drift),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(State::NotInstalled),
        Err(error) => Err(error).with_context(|| format!("reading {}", path.display())),
    }
}

fn install_opencode_plugin(path: &Path, check: bool, force: bool) -> Result<State> {
    let expected = opencode_plugin_source();
    match fs::read(path) {
        Err(error) if error.kind() == std::io::ErrorKind::NotFound && check => {
            Ok(State::WouldInstall)
        }
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            write_atomic(path, expected.as_bytes())?;
            Ok(State::Installed)
        }
        Err(error) => Err(error).with_context(|| format!("reading {}", path.display())),
        Ok(bytes) if bytes == expected.as_bytes() => Ok(State::Unchanged),
        Ok(bytes) if bytes == LEGACY_OPENCODE_PLUGIN_V1.as_bytes() && check => {
            Ok(State::WouldReplace)
        }
        Ok(bytes) if bytes == LEGACY_OPENCODE_PLUGIN_V1.as_bytes() => {
            write_atomic(path, expected.as_bytes())?;
            Ok(State::Replaced)
        }
        Ok(_) if !force => Ok(State::Drift),
        Ok(_) if check => Ok(State::WouldReplace),
        Ok(_) => {
            write_atomic(path, expected.as_bytes())?;
            Ok(State::Replaced)
        }
    }
}

fn remove_opencode_plugin(path: &Path, check: bool, force: bool) -> Result<State> {
    let expected = opencode_plugin_source();
    match fs::read(path) {
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(State::Unchanged),
        Err(error) => Err(error).with_context(|| format!("reading {}", path.display())),
        Ok(bytes)
            if bytes != expected.as_bytes()
                && bytes != LEGACY_OPENCODE_PLUGIN_V1.as_bytes()
                && !force =>
        {
            Ok(State::Drift)
        }
        Ok(_) if check => Ok(State::WouldRemove),
        Ok(_) => {
            fs::remove_file(path).with_context(|| format!("removing {}", path.display()))?;
            Ok(State::Removed)
        }
    }
}

fn inspect_json_mcp(
    path: &Path,
    object_path: &[&str],
    key: &str,
) -> Result<(State, Option<Transport>)> {
    let Some(source) = read_optional(path)? else {
        return Ok((State::NotInstalled, None));
    };
    let Some(container) = find_object_path(&source, object_path)? else {
        return Ok((State::NotInstalled, None));
    };
    let Some(member) = find_member(&source, container, key)? else {
        return Ok((State::NotInstalled, None));
    };
    let value = parse_jsonc_value(&source[member.value_start..member.value_end])?;
    if is_noema_mcp(&value) {
        Ok((State::Configured, mcp_transport(&value)))
    } else {
        Ok((State::Conflict, None))
    }
}

fn mcp_transport(value: &Value) -> Option<Transport> {
    if !is_noema_mcp(value) {
        return None;
    }
    if value.get("command").is_some() {
        Some(Transport::Stdio)
    } else if value.get("url").is_some() {
        Some(Transport::Http)
    } else {
        None
    }
}

#[allow(clippy::too_many_arguments)]
fn install_json_member(
    path: &Path,
    object_path: &[&str],
    key: &str,
    expected: &Value,
    managed: fn(&Value) -> bool,
    check: bool,
    force: bool,
) -> Result<State> {
    let source = read_optional(path)?.unwrap_or_else(|| "{}\n".into());
    let existing = find_object_path(&source, object_path)?
        .and_then(|container| find_member(&source, container, key).transpose())
        .transpose()?;
    if let Some(member) = existing {
        let current = parse_jsonc_value(&source[member.value_start..member.value_end])?;
        if json_contains(&current, expected) {
            return Ok(State::Unchanged);
        }
        if !managed(&current) {
            return Ok(State::Conflict);
        }
        if !force {
            return Ok(State::Drift);
        }
        if check {
            return Ok(State::WouldReplace);
        }
        let mut next = source;
        next.replace_range(
            member.value_start..member.value_end,
            &serde_json::to_string_pretty(expected)?,
        );
        write_atomic(path, next.as_bytes())?;
        return Ok(State::Replaced);
    }
    if check {
        return Ok(State::WouldInstall);
    }
    let next = insert_json_path_member(&source, object_path, key, expected)?;
    write_atomic(path, next.as_bytes())?;
    Ok(State::Installed)
}

fn json_contains(current: &Value, expected: &Value) -> bool {
    match (current, expected) {
        (Value::Object(current), Value::Object(expected)) => expected.iter().all(|(key, value)| {
            current
                .get(key)
                .is_some_and(|current| json_contains(current, value))
        }),
        _ => current == expected,
    }
}

fn remove_json_member(
    path: &Path,
    object_path: &[&str],
    key: &str,
    managed: fn(&Value) -> bool,
    check: bool,
    force: bool,
) -> Result<State> {
    let Some(source) = read_optional(path)? else {
        return Ok(State::Unchanged);
    };
    let Some(container) = find_object_path(&source, object_path)? else {
        return Ok(State::Unchanged);
    };
    let members = object_members(&source, container)?;
    let Some(index) = members.iter().position(|member| member.key == key) else {
        return Ok(State::Unchanged);
    };
    let current = parse_jsonc_value(&source[members[index].value_start..members[index].value_end])?;
    if !managed(&current) && !force {
        return Ok(State::Conflict);
    }
    if check {
        return Ok(State::WouldRemove);
    }
    let next = remove_member_at(source, &members, index);
    write_atomic(path, next.as_bytes())?;
    Ok(State::Removed)
}

fn inspect_json_array_value(
    path: &Path,
    array_path: &[&str],
    expected: &Value,
    managed: fn(&Value) -> bool,
) -> Result<State> {
    let Some(source) = read_optional(path)? else {
        return Ok(State::NotInstalled);
    };
    let Some(array) = find_array_path(&source, array_path)? else {
        return Ok(State::NotInstalled);
    };
    for element in array_elements(&source, array)? {
        let value = parse_jsonc_value(&source[element.value_start..element.value_end])?;
        if value == *expected {
            return Ok(State::UpToDate);
        }
        if managed(&value) {
            return Ok(State::Drift);
        }
    }
    Ok(State::NotInstalled)
}

fn install_json_array_value(
    path: &Path,
    array_path: &[&str],
    expected: &Value,
    managed: fn(&Value) -> bool,
    check: bool,
    force: bool,
) -> Result<State> {
    let source = read_optional(path)?.unwrap_or_else(|| "{}\n".into());
    if let Some(array) = find_array_path(&source, array_path)? {
        for element in array_elements(&source, array)? {
            let current = parse_jsonc_value(&source[element.value_start..element.value_end])?;
            if managed(&current) {
                if current == *expected {
                    return Ok(State::Unchanged);
                }
                if !force {
                    return Ok(State::Drift);
                }
                if check {
                    return Ok(State::WouldReplace);
                }
                let mut next = source;
                next.replace_range(
                    element.value_start..element.value_end,
                    &serde_json::to_string_pretty(expected)?,
                );
                write_atomic(path, next.as_bytes())?;
                return Ok(State::Replaced);
            }
        }
        if check {
            return Ok(State::WouldInstall);
        }
        let next = append_array_value(&source, array, expected)?;
        write_atomic(path, next.as_bytes())?;
        return Ok(State::Installed);
    }
    if check {
        return Ok(State::WouldInstall);
    }
    let (last, parents) = array_path.split_last().context("empty JSON array path")?;
    let next = insert_json_path_member(
        &source,
        parents,
        last,
        &Value::Array(vec![expected.clone()]),
    )?;
    write_atomic(path, next.as_bytes())?;
    Ok(State::Installed)
}

fn remove_json_array_value(
    path: &Path,
    array_path: &[&str],
    managed: fn(&Value) -> bool,
    check: bool,
    _force: bool,
) -> Result<State> {
    let Some(source) = read_optional(path)? else {
        return Ok(State::Unchanged);
    };
    let Some(array) = find_array_path(&source, array_path)? else {
        return Ok(State::Unchanged);
    };
    let elements = array_elements(&source, array)?;
    let mut found = None;
    for (index, element) in elements.iter().enumerate() {
        let value = parse_jsonc_value(&source[element.value_start..element.value_end])?;
        if managed(&value) {
            found = Some(index);
            break;
        }
    }
    let Some(index) = found else {
        return Ok(State::Unchanged);
    };
    if check {
        return Ok(State::WouldRemove);
    }
    let next = remove_element_at(source, &elements, index);
    write_atomic(path, next.as_bytes())?;
    Ok(State::Removed)
}

#[derive(Debug, Clone)]
struct JsonSpan {
    key: String,
    key_start: usize,
    value_start: usize,
    value_end: usize,
    comma_after: Option<usize>,
}

#[derive(Debug, Clone)]
struct ArraySpan {
    value_start: usize,
    value_end: usize,
    comma_after: Option<usize>,
}

fn root_object(source: &str) -> Result<usize> {
    let start = skip_trivia(source, 0)?;
    if source.as_bytes().get(start) != Some(&b'{') {
        bail!("integration target must contain a JSON object");
    }
    Ok(start)
}

fn find_object_path(source: &str, path: &[&str]) -> Result<Option<usize>> {
    let mut object = root_object(source)?;
    for key in path {
        let Some(member) = find_member(source, object, key)? else {
            return Ok(None);
        };
        if source.as_bytes().get(member.value_start) != Some(&b'{') {
            bail!("JSON member {key:?} is not an object");
        }
        object = member.value_start;
    }
    Ok(Some(object))
}

fn find_array_path(source: &str, path: &[&str]) -> Result<Option<usize>> {
    let (last, parents) = path.split_last().context("empty JSON array path")?;
    let Some(object) = find_object_path(source, parents)? else {
        return Ok(None);
    };
    let Some(member) = find_member(source, object, last)? else {
        return Ok(None);
    };
    if source.as_bytes().get(member.value_start) != Some(&b'[') {
        bail!("JSON member {last:?} is not an array");
    }
    Ok(Some(member.value_start))
}

fn find_member(source: &str, object: usize, key: &str) -> Result<Option<JsonSpan>> {
    Ok(object_members(source, object)?
        .into_iter()
        .find(|member| member.key == key))
}

fn object_members(source: &str, object: usize) -> Result<Vec<JsonSpan>> {
    if source.as_bytes().get(object) != Some(&b'{') {
        bail!("expected JSON object");
    }
    let mut members = Vec::new();
    let mut index = object + 1;
    loop {
        index = skip_trivia(source, index)?;
        match source.as_bytes().get(index) {
            Some(b'}') => return Ok(members),
            Some(b'\"') => {}
            _ => bail!("expected JSON object key at byte {index}"),
        }
        let key_start = index;
        let key_end = string_end(source, index)?;
        let key: String = serde_json::from_str(&source[key_start..key_end])?;
        index = skip_trivia(source, key_end)?;
        if source.as_bytes().get(index) != Some(&b':') {
            bail!("expected colon after JSON key {key:?}");
        }
        index = skip_trivia(source, index + 1)?;
        let value_start = index;
        let value_end = value_end(source, value_start)?;
        index = skip_trivia(source, value_end)?;
        let comma_after = match source.as_bytes().get(index) {
            Some(b',') => Some(index),
            Some(b'}') => None,
            _ => bail!("expected comma or object end after JSON member {key:?}"),
        };
        members.push(JsonSpan {
            key,
            key_start,
            value_start,
            value_end,
            comma_after,
        });
        if comma_after.is_some() {
            index += 1;
        } else {
            return Ok(members);
        }
    }
}

fn array_elements(source: &str, array: usize) -> Result<Vec<ArraySpan>> {
    if source.as_bytes().get(array) != Some(&b'[') {
        bail!("expected JSON array");
    }
    let mut elements = Vec::new();
    let mut index = array + 1;
    loop {
        index = skip_trivia(source, index)?;
        if source.as_bytes().get(index) == Some(&b']') {
            return Ok(elements);
        }
        let value_start = index;
        let value_end = value_end(source, value_start)?;
        index = skip_trivia(source, value_end)?;
        let comma_after = match source.as_bytes().get(index) {
            Some(b',') => Some(index),
            Some(b']') => None,
            _ => bail!("expected comma or array end at byte {index}"),
        };
        elements.push(ArraySpan {
            value_start,
            value_end,
            comma_after,
        });
        if comma_after.is_some() {
            index += 1;
        } else {
            return Ok(elements);
        }
    }
}

fn insert_json_path_member(
    source: &str,
    path: &[&str],
    key: &str,
    value: &Value,
) -> Result<String> {
    let mut object = root_object(source)?;
    for (index, parent) in path.iter().enumerate() {
        if let Some(member) = find_member(source, object, parent)? {
            if source.as_bytes().get(member.value_start) != Some(&b'{') {
                bail!("JSON member {parent:?} is not an object");
            }
            object = member.value_start;
            continue;
        }
        let mut nested = json!({key: value});
        for remaining in path[index + 1..].iter().rev() {
            nested = json!({*remaining: nested});
        }
        return insert_object_member(source, object, parent, &nested);
    }
    insert_object_member(source, object, key, value)
}

fn insert_object_member(source: &str, object: usize, key: &str, value: &Value) -> Result<String> {
    let members = object_members(source, object)?;
    let closing = container_end(source, object)?;
    let closing_indent = line_indent(source, closing);
    let child_indent = format!("{closing_indent}  ");
    let encoded = indent_multiline(&serde_json::to_string_pretty(value)?, &child_indent);
    let addition = format!("{child_indent}{}: {encoded}", serde_json::to_string(key)?);
    let mut next = source.to_owned();
    if let Some(last) = members.last() {
        next.insert_str(last.value_end, &format!(",\n{addition}"));
    } else {
        next.insert_str(closing, &format!("\n{addition}\n{closing_indent}"));
    }
    Ok(next)
}

fn append_array_value(source: &str, array: usize, value: &Value) -> Result<String> {
    let elements = array_elements(source, array)?;
    let closing = container_end(source, array)?;
    let closing_indent = line_indent(source, closing);
    let child_indent = format!("{closing_indent}  ");
    let encoded = indent_multiline(&serde_json::to_string_pretty(value)?, &child_indent);
    let mut next = source.to_owned();
    if let Some(last) = elements.last() {
        next.insert_str(last.value_end, &format!(",\n{child_indent}{encoded}"));
    } else {
        next.insert_str(
            closing,
            &format!("\n{child_indent}{encoded}\n{closing_indent}"),
        );
    }
    Ok(next)
}

fn remove_member_at(mut source: String, members: &[JsonSpan], index: usize) -> String {
    let member = &members[index];
    if let Some(comma) = member.comma_after {
        source.replace_range(member.key_start..=comma, "");
    } else if index > 0 {
        let comma = members[index - 1].comma_after.unwrap();
        source.replace_range(comma..member.value_end, "");
    } else {
        source.replace_range(member.key_start..member.value_end, "");
    }
    source
}

fn remove_element_at(mut source: String, elements: &[ArraySpan], index: usize) -> String {
    let element = &elements[index];
    if let Some(comma) = element.comma_after {
        source.replace_range(element.value_start..=comma, "");
    } else if index > 0 {
        let comma = elements[index - 1].comma_after.unwrap();
        source.replace_range(comma..element.value_end, "");
    } else {
        source.replace_range(element.value_start..element.value_end, "");
    }
    source
}

fn container_end(source: &str, start: usize) -> Result<usize> {
    let finish = value_end(source, start)?;
    Ok(finish - 1)
}

fn value_end(source: &str, start: usize) -> Result<usize> {
    let bytes = source.as_bytes();
    match bytes.get(start) {
        Some(b'\"') => string_end(source, start),
        Some(b'{') | Some(b'[') => {
            let mut stack = vec![bytes[start]];
            let mut index = start + 1;
            while index < bytes.len() {
                match bytes[index] {
                    b'\"' => index = string_end(source, index)?,
                    b'/' if bytes.get(index + 1) == Some(&b'/') => {
                        index = skip_line_comment(source, index + 2);
                    }
                    b'/' if bytes.get(index + 1) == Some(&b'*') => {
                        index = skip_block_comment(source, index + 2)?;
                    }
                    b'{' | b'[' => {
                        stack.push(bytes[index]);
                        index += 1;
                    }
                    b'}' => {
                        if stack.pop() != Some(b'{') {
                            bail!("mismatched JSON object delimiter");
                        }
                        index += 1;
                        if stack.is_empty() {
                            return Ok(index);
                        }
                    }
                    b']' => {
                        if stack.pop() != Some(b'[') {
                            bail!("mismatched JSON array delimiter");
                        }
                        index += 1;
                        if stack.is_empty() {
                            return Ok(index);
                        }
                    }
                    _ => index += 1,
                }
            }
            bail!("unterminated JSON container")
        }
        Some(_) => {
            let mut index = start;
            while index < bytes.len()
                && !matches!(
                    bytes[index],
                    b',' | b'}' | b']' | b' ' | b'\t' | b'\r' | b'\n'
                )
            {
                index += 1;
            }
            Ok(index)
        }
        None => bail!("missing JSON value"),
    }
}

fn string_end(source: &str, start: usize) -> Result<usize> {
    let bytes = source.as_bytes();
    let mut index = start + 1;
    while index < bytes.len() {
        match bytes[index] {
            b'\\' => index += 2,
            b'\"' => return Ok(index + 1),
            _ => index += 1,
        }
    }
    bail!("unterminated JSON string")
}

fn skip_trivia(source: &str, mut index: usize) -> Result<usize> {
    let bytes = source.as_bytes();
    loop {
        while index < bytes.len() && matches!(bytes[index], b' ' | b'\t' | b'\r' | b'\n') {
            index += 1;
        }
        if bytes.get(index) == Some(&b'/') && bytes.get(index + 1) == Some(&b'/') {
            index = skip_line_comment(source, index + 2);
        } else if bytes.get(index) == Some(&b'/') && bytes.get(index + 1) == Some(&b'*') {
            index = skip_block_comment(source, index + 2)?;
        } else {
            return Ok(index);
        }
    }
}

fn skip_line_comment(source: &str, mut index: usize) -> usize {
    let bytes = source.as_bytes();
    while index < bytes.len() && bytes[index] != b'\n' {
        index += 1;
    }
    index
}

fn skip_block_comment(source: &str, mut index: usize) -> Result<usize> {
    let bytes = source.as_bytes();
    while index + 1 < bytes.len() {
        if bytes[index] == b'*' && bytes[index + 1] == b'/' {
            return Ok(index + 2);
        }
        index += 1;
    }
    bail!("unterminated JSON block comment")
}

fn parse_jsonc_value(source: &str) -> Result<Value> {
    let mut stripped = String::with_capacity(source.len());
    let bytes = source.as_bytes();
    let mut index = 0;
    while index < bytes.len() {
        match bytes[index] {
            b'\"' => {
                let end = string_end(source, index)?;
                stripped.push_str(&source[index..end]);
                index = end;
            }
            b'/' if bytes.get(index + 1) == Some(&b'/') => {
                index = skip_line_comment(source, index + 2);
                stripped.push('\n');
            }
            b'/' if bytes.get(index + 1) == Some(&b'*') => {
                index = skip_block_comment(source, index + 2)?;
                stripped.push(' ');
            }
            byte => {
                stripped.push(byte as char);
                index += 1;
            }
        }
    }
    serde_json::from_str(&without_trailing_commas(&stripped))
        .context("parsing JSON integration fragment")
}

fn without_trailing_commas(source: &str) -> String {
    let bytes = source.as_bytes();
    let mut output = String::with_capacity(source.len());
    let mut index = 0;
    while index < bytes.len() {
        if bytes[index] == b'\"' {
            let start = index;
            index += 1;
            while index < bytes.len() {
                match bytes[index] {
                    b'\\' => index += 2,
                    b'\"' => {
                        index += 1;
                        break;
                    }
                    _ => index += 1,
                }
            }
            output.push_str(&source[start..index]);
            continue;
        }
        if bytes[index] == b',' {
            let mut next = index + 1;
            while next < bytes.len() && matches!(bytes[next], b' ' | b'\t' | b'\r' | b'\n') {
                next += 1;
            }
            if matches!(bytes.get(next), Some(b'}' | b']')) {
                index += 1;
                continue;
            }
        }
        output.push(bytes[index] as char);
        index += 1;
    }
    output
}

fn indent_multiline(value: &str, indent: &str) -> String {
    value.replace('\n', &format!("\n{indent}"))
}

fn line_indent(source: &str, index: usize) -> String {
    let start = source[..index]
        .rfind('\n')
        .map_or(0, |position| position + 1);
    source[start..index]
        .chars()
        .take_while(|character| matches!(character, ' ' | '\t'))
        .collect()
}

fn toml_string(value: &str) -> Result<String> {
    serde_json::to_string(value).context("encoding TOML string")
}

fn read_optional(path: &Path) -> Result<Option<String>> {
    match fs::read_to_string(path) {
        Ok(source) => Ok(Some(source)),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(None),
        Err(error) => Err(error).with_context(|| format!("reading {}", path.display())),
    }
}

fn write_atomic(path: &Path, bytes: &[u8]) -> Result<()> {
    let parent = path
        .parent()
        .context("integration target has no parent directory")?;
    fs::create_dir_all(parent).with_context(|| format!("creating {}", parent.display()))?;
    let existing_permissions = match fs::metadata(path) {
        Ok(metadata) => {
            drop(OpenOptions::new().write(true).open(path)?);
            Some(metadata.permissions())
        }
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => None,
        Err(error) => {
            return Err(error).with_context(|| format!("inspecting {}", path.display()));
        }
    };
    let temporary = parent.join(format!(".noema-integrate-{}.tmp", ulid::Ulid::new()));
    let result = (|| -> Result<()> {
        let mut file = OpenOptions::new()
            .write(true)
            .create_new(true)
            .open(&temporary)?;
        if let Some(permissions) = existing_permissions {
            file.set_permissions(permissions)?;
        } else {
            #[cfg(unix)]
            {
                use std::os::unix::fs::PermissionsExt;
                file.set_permissions(fs::Permissions::from_mode(0o600))?;
            }
        }
        file.write_all(bytes)?;
        file.sync_all()?;
        drop(file);
        fs::rename(&temporary, path)?;
        Ok(())
    })();
    if result.is_err() {
        let _ = fs::remove_file(&temporary);
    }
    result.with_context(|| format!("writing {}", path.display()))
}

fn find_project_root(cwd: &Path) -> PathBuf {
    cwd.ancestors()
        .find(|path| path.join(".git").exists())
        .unwrap_or(cwd)
        .to_path_buf()
}

fn valid_env_name(value: &str) -> bool {
    let mut chars = value.chars();
    chars
        .next()
        .is_some_and(|character| character == '_' || character.is_ascii_alphabetic())
        && chars.all(|character| character == '_' || character.is_ascii_alphanumeric())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn target(root: &Path, client: Client) -> Target {
        Target {
            client,
            scope: Scope::User,
            project_root: root.join("project"),
            home: root.join("home"),
            xdg_config_home: Some(root.join("config")),
        }
    }

    fn request(root: &Path, client: Client) -> Request {
        Request {
            target: target(root, client),
            connection: ConnectionSpec {
                cortex: "shared".into(),
                binary: PathBuf::from("/usr/local/bin/noema"),
                transport: Transport::Stdio,
                url: None,
                bearer_token_env: None,
            },
        }
    }

    #[test]
    fn codex_install_preserves_existing_config_and_detects_drift() {
        let temp = tempfile::tempdir().unwrap();
        let request = request(temp.path(), Client::Codex);
        let path = request.target.codex_config();
        fs::create_dir_all(path.parent().unwrap()).unwrap();
        fs::write(&path, "model = \"gpt-5\"\n").unwrap();

        let report = install(&request, false, false).unwrap();
        assert!(report.healthy());
        let installed = fs::read_to_string(&path).unwrap();
        assert!(installed.starts_with("model = \"gpt-5\"\n"));
        assert!(installed.contains("[mcp_servers.noema]"));
        assert!(installed.contains("[[hooks.SessionStart]]"));

        fs::write(&path, installed.replace("timeout = 5", "timeout = 9")).unwrap();
        assert_eq!(
            status(&request.target).unwrap().components[0].state,
            State::Drift
        );
        assert_eq!(
            status(&request.target).unwrap().components[0].transport,
            Some(Transport::Stdio)
        );
        assert_eq!(
            install(&request, false, false).unwrap().components[0].state,
            State::Drift
        );
        assert_eq!(
            install(&request, false, true).unwrap().components[0].state,
            State::Replaced
        );
    }

    #[test]
    fn codex_install_adopts_compatible_http_mcp_and_preserves_extras() {
        let temp = tempfile::tempdir().unwrap();
        let mut request = request(temp.path(), Client::Codex);
        request.connection.transport = Transport::Http;
        request.connection.url = Some("https://memory.example.com/mcp".into());
        request.connection.bearer_token_env = Some("NOEMA_MCP_KEY".into());
        request.connection.validate().unwrap();

        let path = request.target.codex_config();
        fs::create_dir_all(path.parent().unwrap()).unwrap();
        let table = r#"[mcp_servers.noema]
# preserve this comment and field order
url = "https://memory.example.com/mcp"
default_tools_approval_mode = "approve"
bearer_token_env_var = "NOEMA_MCP_KEY"

"#;
        let original = format!(
            "model = \"gpt-5\"\n\n{table}[mcp_servers.docs]\nurl = \"https://docs.example.com/mcp\"\n"
        );
        fs::write(&path, &original).unwrap();

        assert_eq!(
            status(&request.target).unwrap().components[0].state,
            State::Drift
        );
        assert_eq!(
            status(&request.target).unwrap().components[0].transport,
            Some(Transport::Http)
        );
        assert_eq!(
            install(&request, true, false).unwrap().components[0].state,
            State::WouldReplace
        );
        assert_eq!(fs::read_to_string(&path).unwrap(), original);

        assert_eq!(
            install(&request, false, false).unwrap().components[0].state,
            State::Replaced
        );
        let installed = fs::read_to_string(&path).unwrap();
        assert!(installed.starts_with("model = \"gpt-5\"\n\n"));
        assert!(installed.contains(table));
        assert!(installed.contains(CODEX_START));
        assert!(installed.contains(CLAUDE_HOOK_MARKER));
        assert!(
            installed.ends_with("[mcp_servers.docs]\nurl = \"https://docs.example.com/mcp\"\n")
        );
        let report = status(&request.target).unwrap();
        assert_eq!(report.components[0].state, State::Configured);
        assert_eq!(report.components[0].transport, Some(Transport::Http));

        let before_idempotence = installed.clone();
        assert_eq!(
            install(&request, false, false).unwrap().components[0].state,
            State::Unchanged
        );
        assert_eq!(fs::read_to_string(&path).unwrap(), before_idempotence);

        assert_eq!(
            remove(&request.target, false, false).unwrap().components[0].state,
            State::Removed
        );
        let removed = fs::read_to_string(path).unwrap();
        assert!(removed.starts_with("model = \"gpt-5\"\n"));
        assert!(removed.ends_with("[mcp_servers.docs]\nurl = \"https://docs.example.com/mcp\"\n"));
        assert!(!removed.contains("mcp_servers.noema"));
        assert!(!removed.contains(CLAUDE_HOOK_MARKER));
    }

    #[test]
    fn codex_install_does_not_adopt_incompatible_mcp_even_with_force() {
        let temp = tempfile::tempdir().unwrap();
        let mut request = request(temp.path(), Client::Codex);
        request.connection.transport = Transport::Http;
        request.connection.url = Some("https://memory.example.com/mcp".into());
        request.connection.bearer_token_env = Some("NOEMA_MCP_KEY".into());
        request.connection.validate().unwrap();

        let path = request.target.codex_config();
        fs::create_dir_all(path.parent().unwrap()).unwrap();
        let original = r#"[mcp_servers.noema]
url = "https://other.example.com/mcp"
bearer_token_env_var = "NOEMA_MCP_KEY"
"#;
        fs::write(&path, original).unwrap();

        let report = status(&request.target).unwrap();
        assert_eq!(report.components[0].state, State::Drift);
        assert_eq!(report.components[0].transport, Some(Transport::Http));
        assert_eq!(
            install(&request, false, false).unwrap().components[0].state,
            State::Conflict
        );
        assert_eq!(
            install(&request, false, true).unwrap().components[0].state,
            State::Conflict
        );
        assert_eq!(fs::read_to_string(path).unwrap(), original);
    }

    #[test]
    fn claude_install_and_remove_preserve_unrelated_json_members() {
        let temp = tempfile::tempdir().unwrap();
        let request = request(temp.path(), Client::ClaudeCode);
        let mcp_path = request.target.claude_mcp_config();
        let hook_path = request.target.claude_settings();
        fs::create_dir_all(mcp_path.parent().unwrap()).unwrap();
        fs::create_dir_all(hook_path.parent().unwrap()).unwrap();
        fs::write(&mcp_path, "{\n  \"projects\": {\"keep\": true}\n}\n").unwrap();
        fs::write(
            &hook_path,
            "{\n  \"hooks\": {\n    \"Stop\": [{\"hooks\": []}]\n  },\n  \"theme\": \"dark\"\n}\n",
        )
        .unwrap();

        assert!(install(&request, false, false).unwrap().healthy());
        let mcp: Value = serde_json::from_str(&fs::read_to_string(&mcp_path).unwrap()).unwrap();
        let settings: Value =
            serde_json::from_str(&fs::read_to_string(&hook_path).unwrap()).unwrap();
        assert_eq!(mcp["projects"]["keep"], true);
        assert!(mcp["mcpServers"]["noema"].is_object());
        assert_eq!(settings["theme"], "dark");
        assert_eq!(settings["hooks"]["Stop"].as_array().unwrap().len(), 1);
        assert_eq!(
            settings["hooks"]["SessionStart"].as_array().unwrap().len(),
            1
        );

        let report = status(&request.target).unwrap();
        assert!(report.healthy());
        assert_eq!(report.components[0].state, State::Configured);
        assert_eq!(report.components[0].transport, Some(Transport::Stdio));
        assert_eq!(report.components[1].state, State::UpToDate);

        let removed = remove(&request.target, false, false).unwrap();
        assert!(
            removed
                .components
                .iter()
                .all(|component| component.state == State::Removed)
        );
        let mcp: Value = serde_json::from_str(&fs::read_to_string(&mcp_path).unwrap()).unwrap();
        let settings: Value =
            serde_json::from_str(&fs::read_to_string(&hook_path).unwrap()).unwrap();
        assert_eq!(mcp["projects"]["keep"], true);
        assert!(mcp["mcpServers"].get("noema").is_none());
        assert_eq!(settings["hooks"]["Stop"].as_array().unwrap().len(), 1);
        assert_eq!(
            settings["hooks"]["SessionStart"].as_array().unwrap().len(),
            0
        );
    }

    #[test]
    fn claude_install_preflights_all_components_before_writing() {
        let temp = tempfile::tempdir().unwrap();
        let request = request(temp.path(), Client::ClaudeCode);
        let hook_path = request.target.claude_settings();
        fs::create_dir_all(hook_path.parent().unwrap()).unwrap();
        let drifted_hook = claude_hook_value()
            .to_string()
            .replace("\"timeout\":5", "\"timeout\":9");
        fs::write(
            &hook_path,
            format!("{{\"hooks\":{{\"SessionStart\":[{drifted_hook}]}}}}"),
        )
        .unwrap();

        let report = install(&request, false, false).unwrap();
        assert!(report.refused());
        assert!(!request.target.claude_mcp_config().exists());
        assert!(
            fs::read_to_string(hook_path)
                .unwrap()
                .contains("\"timeout\":9")
        );
    }

    #[test]
    fn opencode_jsonc_install_preserves_comments_and_unrelated_entries() {
        let temp = tempfile::tempdir().unwrap();
        let request = request(temp.path(), Client::OpenCode);
        let path = request.target.opencode_config();
        fs::create_dir_all(path.parent().unwrap()).unwrap();
        fs::write(
            &path,
            "{\n  // keep this comment\n  \"theme\": \"dark\",\n  \"mcp\": {\n    \"docs\": {\"type\": \"remote\", \"url\": \"https://example.com/mcp\"},\n  },\n}\n",
        )
        .unwrap();

        assert!(install(&request, false, false).unwrap().healthy());
        let installed = fs::read_to_string(&path).unwrap();
        assert!(installed.contains("// keep this comment"));
        assert!(installed.contains("\"docs\""));
        assert!(installed.contains("\"noema\""));
        assert!(parse_jsonc_value(&installed).is_ok());
        assert!(
            request
                .target
                .opencode_plugin()
                .ends_with("noema-session-bootstrap.ts")
        );
        assert!(!opencode_plugin_source().contains("@opencode-ai/plugin"));
    }

    #[test]
    fn opencode_install_migrates_legacy_bootstrap_and_preserves_mcp_extras() {
        let temp = tempfile::tempdir().unwrap();
        let mut request = request(temp.path(), Client::OpenCode);
        request.connection.transport = Transport::Http;
        request.connection.url = Some("https://memory.example.com/mcp".into());
        request.connection.bearer_token_env = Some("NOEMA_MCP_KEY".into());
        request.connection.validate().unwrap();

        let config = request.target.opencode_config();
        let plugin = request.target.opencode_plugin();
        fs::create_dir_all(config.parent().unwrap()).unwrap();
        fs::create_dir_all(plugin.parent().unwrap()).unwrap();
        let original = r#"{
  // preserve this file verbatim
  "mcp": {
    "noema": {
      "type": "remote",
      "enabled": true,
      "timeout": 10000,
      "oauth": false,
      "url": "https://memory.example.com/mcp",
      "headers": {
        "Authorization": "Bearer {env:NOEMA_MCP_KEY}",
        "X-Client": "opencode"
      }
    }
  }
}
"#;
        fs::write(&config, original).unwrap();
        fs::write(&plugin, LEGACY_OPENCODE_PLUGIN_V1).unwrap();

        let preview = install(&request, true, false).unwrap();
        assert_eq!(preview.components[0].state, State::Unchanged);
        assert_eq!(preview.components[1].state, State::WouldReplace);
        assert_eq!(fs::read_to_string(&config).unwrap(), original);
        assert_eq!(
            fs::read_to_string(&plugin).unwrap(),
            LEGACY_OPENCODE_PLUGIN_V1
        );

        let installed = install(&request, false, false).unwrap();
        assert_eq!(installed.components[0].state, State::Unchanged);
        assert_eq!(installed.components[1].state, State::Replaced);
        assert_eq!(fs::read_to_string(&config).unwrap(), original);
        assert_eq!(
            fs::read_to_string(&plugin).unwrap(),
            opencode_plugin_source()
        );
        let report = status(&request.target).unwrap();
        assert!(report.healthy());
        assert_eq!(report.components[0].transport, Some(Transport::Http));

        fs::write(
            &config,
            original.replace("memory.example.com", "other.example.com"),
        )
        .unwrap();
        let report = status(&request.target).unwrap();
        assert_eq!(report.components[0].state, State::Configured);
        assert_eq!(report.components[0].transport, Some(Transport::Http));
        assert_eq!(
            install(&request, true, false).unwrap().components[0].state,
            State::Drift
        );
    }

    #[test]
    fn opencode_install_does_not_adopt_unknown_bootstrap_variants() {
        let temp = tempfile::tempdir().unwrap();
        let request = request(temp.path(), Client::OpenCode);
        let plugin = request.target.opencode_plugin();
        fs::create_dir_all(plugin.parent().unwrap()).unwrap();
        let unknown = LEGACY_OPENCODE_PLUGIN_V1.replace(
            "Surface tool failures explicitly.",
            "Continue after tool failures.",
        );
        fs::write(&plugin, &unknown).unwrap();

        let report = install(&request, false, false).unwrap();
        assert!(report.refused());
        assert!(!request.target.opencode_config().exists());
        assert_eq!(fs::read_to_string(plugin).unwrap(), unknown);
    }

    #[test]
    fn opencode_v2_fails_before_writing_legacy_configuration() {
        let temp = tempfile::tempdir().unwrap();
        let request = request(temp.path(), Client::OpenCode);
        let path = request.target.opencode_config();
        fs::create_dir_all(path.parent().unwrap()).unwrap();
        let original = "{\n  \"mcp\": {\n    \"servers\": {}\n  }\n}\n";
        fs::write(&path, original).unwrap();

        let error = install(&request, false, false).unwrap_err().to_string();
        assert!(error.contains("OpenCode v2"));
        assert_eq!(fs::read_to_string(path).unwrap(), original);
    }

    #[test]
    fn http_rendering_uses_environment_references_without_secret_values() {
        let temp = tempfile::tempdir().unwrap();
        let mut request = request(temp.path(), Client::OpenCode);
        request.connection.transport = Transport::Http;
        request.connection.url = Some("https://memory.example.com/mcp".into());
        request.connection.bearer_token_env = Some("NOEMA_MCP_KEY".into());
        request.connection.validate().unwrap();

        let rendered = print(&request).unwrap();
        assert!(rendered.contains("{env:NOEMA_MCP_KEY}"));
        assert!(!rendered.contains("Bearer secret"));
    }

    #[test]
    fn invalid_http_options_fail_closed() {
        let temp = tempfile::tempdir().unwrap();
        let mut request = request(temp.path(), Client::Codex);
        request.connection.transport = Transport::Http;
        request.connection.url = Some("http://memory.example.com/not-mcp".into());
        assert!(request.connection.validate().is_err());
        request.connection.url = Some("https://memory.example.com/mcp".into());
        request.connection.bearer_token_env = Some("bad-name".into());
        assert!(request.connection.validate().is_err());
    }
}
