use anyhow::Result;
use sha2::{Digest, Sha256};
use std::{fs, path::Path};

pub fn file_hash(path: &Path) -> Result<String> {
    Ok(format!("sha256:{:x}", Sha256::digest(fs::read(path)?)))
}
