use std::io::Write;

use anyhow::{Context, Result, bail};
use base64::{Engine, engine::general_purpose::STANDARD};
use ed25519_dalek::{Signature, Signer, SigningKey, Verifier, VerifyingKey};
use rand::TryRngCore;
use sha2::{Digest, Sha256};

use crate::event::Event;

const DOMAIN_TAG: &[u8] = b"noema-event-sig-v1\n";
const PREFIX: &str = "ed25519:";

pub fn preimage(event: &Event) -> Vec<u8> {
    let mut out = DOMAIN_TAG.to_vec();
    for field in [
        event.id.as_bytes(),
        event.action.as_bytes(),
        event.trace_id.as_bytes(),
        event.cortex_id.as_bytes(),
        event.origin.as_bytes(),
        event.timestamp.as_bytes(),
    ] {
        write_field(&mut out, field);
    }
    let mut clock = Vec::new();
    for (key, value) in &event.vclock {
        write_field(&mut clock, key.as_bytes());
        clock.extend_from_slice(&value.to_be_bytes());
    }
    write_field(&mut out, &clock);
    let digest = Sha256::digest(event.normalized_data());
    write_field(&mut out, &digest);
    out
}

pub fn sign(key: &SigningKey, event: &Event) -> String {
    format!(
        "{PREFIX}{}",
        STANDARD.encode(key.sign(&preimage(event)).to_bytes())
    )
}

pub fn verify(public_key: &str, event: &Event, signature: &str) -> Result<()> {
    let key = parse_public(public_key)?;
    let bytes = decode_prefixed(signature, 64)?;
    let signature = Signature::from_slice(&bytes)?;
    key.verify(&preimage(event), &signature)
        .context("signature verification failed")
}

pub fn generate() -> Result<(SigningKey, String, String)> {
    let mut seed = [0u8; 32];
    rand::rngs::OsRng.try_fill_bytes(&mut seed)?;
    let key = SigningKey::from_bytes(&seed);
    let public = encode_public(&key.verifying_key());
    Ok((key, public, STANDARD.encode(seed)))
}

pub fn signing_key_from_seed(seed: &str) -> Result<SigningKey> {
    let bytes = STANDARD.decode(seed.trim())?;
    let seed: [u8; 32] = bytes
        .try_into()
        .map_err(|_| anyhow::anyhow!("seed must decode to 32 bytes"))?;
    Ok(SigningKey::from_bytes(&seed))
}

pub fn encode_public(key: &VerifyingKey) -> String {
    format!("{PREFIX}{}", STANDARD.encode(key.as_bytes()))
}

pub fn parse_public(value: &str) -> Result<VerifyingKey> {
    let bytes = decode_prefixed(value, 32)?;
    let bytes: [u8; 32] = bytes.try_into().unwrap();
    VerifyingKey::from_bytes(&bytes).context("invalid public key")
}

fn decode_prefixed(value: &str, expected: usize) -> Result<Vec<u8>> {
    let encoded = value
        .trim()
        .strip_prefix(PREFIX)
        .ok_or_else(|| anyhow::anyhow!("missing ed25519 scheme prefix"))?;
    let decoded = STANDARD.decode(encoded)?;
    if decoded.len() != expected {
        bail!("decoded {} bytes, want {expected}", decoded.len());
    }
    Ok(decoded)
}

fn write_field(out: &mut Vec<u8>, field: &[u8]) {
    out.write_all(&(field.len() as u32).to_be_bytes()).unwrap();
    out.write_all(field).unwrap();
}

#[cfg(test)]
mod tests {
    use std::collections::BTreeMap;

    use serde_json::json;

    use super::*;

    #[test]
    fn signs_and_verifies() {
        let (key, public, _) = generate().unwrap();
        let event = Event::new(
            "create",
            "20260814-example",
            "01JTEST",
            "test",
            json!({"body":"hello"}),
            BTreeMap::new(),
        );
        let signature = sign(&key, &event);
        verify(&public, &event, &signature).unwrap();
    }
}
