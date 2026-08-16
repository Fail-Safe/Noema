pub mod cli;
pub mod config;
pub mod consolidation;
pub mod cortex;
pub mod db;
pub mod embedding;
pub mod event;
pub mod eventsig;
pub mod federation;
pub mod lock;
pub mod mcp;
pub mod plugin;
pub mod tlsutil;
pub mod trace;
pub mod tui;
pub mod watch;

pub const VERSION: &str = env!("CARGO_PKG_VERSION");
