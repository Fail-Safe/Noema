use anyhow::Result;
use notify::{Config, RecommendedWatcher, RecursiveMode, Watcher};
use std::{path::PathBuf, sync::mpsc, time::Duration};

use crate::cortex::Cortex;

pub fn serve(cortex_name: String, cortex_dir: PathBuf, debounce: Duration) -> Result<()> {
    let (sender, receiver) = mpsc::channel();
    let mut watcher = RecommendedWatcher::new(sender, Config::default())?;
    for relative in ["traces", "archive/traces", "trash/traces"] {
        watcher.watch(&cortex_dir.join(relative), RecursiveMode::NonRecursive)?;
    }
    while let Ok(event) = receiver.recv() {
        let _ = event?;
        std::thread::sleep(debounce);
        Cortex::open(&cortex_name, &cortex_dir)?.sync()?;
    }
    Ok(())
}
