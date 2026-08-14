use anyhow::{Result, bail};

const CODEC_VERSION: u8 = 1;

pub fn encode(vector: &[f32]) -> Vec<u8> {
    let mut out = Vec::with_capacity(1 + vector.len() * 4);
    out.push(CODEC_VERSION);
    for value in vector {
        out.extend_from_slice(&value.to_le_bytes());
    }
    out
}

pub fn decode(blob: &[u8]) -> Result<Vec<f32>> {
    if blob.first() != Some(&CODEC_VERSION) || !(blob.len() - 1).is_multiple_of(4) {
        bail!("invalid embedding blob");
    }
    Ok(blob[1..]
        .chunks_exact(4)
        .map(|bytes| f32::from_le_bytes(bytes.try_into().unwrap()))
        .collect())
}

pub fn normalize(vector: &mut [f32]) {
    let norm = vector.iter().map(|value| value * value).sum::<f32>().sqrt();
    if norm > 0.0 {
        for value in vector {
            *value /= norm;
        }
    }
}

pub fn cosine(left: &[f32], right: &[f32]) -> Option<f32> {
    (left.len() == right.len()).then(|| left.iter().zip(right).map(|(a, b)| a * b).sum())
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn codec_round_trip() {
        let v = vec![1.0, -2.5, f32::INFINITY];
        assert_eq!(decode(&encode(&v)).unwrap(), v);
    }
}
