//! Frame codec: `"PF"` magic + u32 big-endian length + CBOR envelope.
//! Mirrors `pkg/protocol/codec.go`. Any framing error is fatal for the
//! connection — the caller closes the stream and reconnects.

use std::io::{Read, Write};

use crate::envelope::Envelope;
use crate::{FRAME_MAGIC, MAX_FRAME_BYTES};

#[derive(Debug, thiserror::Error)]
pub enum CodecError {
    #[error("protocol: bad frame magic")]
    BadMagic,
    #[error("protocol: frame exceeds MaxFrameBytes ({0} > {MAX_FRAME_BYTES})")]
    FrameTooLarge(usize),
    #[error("protocol: envelope has multiple non-nil fields")]
    MultipleFields,
    #[error("cbor encode: {0}")]
    Encode(#[from] ciborium::ser::Error<std::io::Error>),
    #[error("cbor decode: {0}")]
    Decode(#[from] ciborium::de::Error<std::io::Error>),
    #[error("io: {0}")]
    Io(#[from] std::io::Error),
}

/// Serialize `env` into `header || payload` bytes (shared by the sync and
/// async writers).
pub fn encode_frame(env: &Envelope) -> Result<Vec<u8>, CodecError> {
    let mut buf = vec![0u8; 6];
    ciborium::ser::into_writer(env, &mut buf)?;
    let payload_len = buf.len() - 6;
    if payload_len > MAX_FRAME_BYTES {
        return Err(CodecError::FrameTooLarge(payload_len));
    }
    buf[..2].copy_from_slice(&FRAME_MAGIC);
    let len_bytes = (payload_len as u32).to_be_bytes();
    buf[2..6].copy_from_slice(&len_bytes);
    Ok(buf)
}

/// Validate a header, returning the payload length.
pub fn decode_header(header: &[u8; 6]) -> Result<usize, CodecError> {
    if header[..2] != FRAME_MAGIC {
        return Err(CodecError::BadMagic);
    }
    let len = u32::from_be_bytes([header[2], header[3], header[4], header[5]]) as usize;
    if len > MAX_FRAME_BYTES {
        return Err(CodecError::FrameTooLarge(len));
    }
    Ok(len)
}

/// Decode + validate one frame payload.
pub fn decode_payload(buf: &[u8]) -> Result<Envelope, CodecError> {
    let env: Envelope = ciborium::de::from_reader(buf)?;
    if env.populated() > 1 {
        return Err(CodecError::MultipleFields);
    }
    Ok(env)
}

/// Serialize `env` into a single frame on `w`. Partial writes are not retried;
/// the caller's reconnect path must close the stream and start fresh.
pub fn write_frame<W: Write>(w: &mut W, env: &Envelope) -> Result<(), CodecError> {
    let buf = encode_frame(env)?;
    w.write_all(&buf)?;
    Ok(())
}

/// Block until a complete frame is read, or fail. Rejects bad magic, oversized
/// frames (before allocating), and envelopes with more than one populated field.
pub fn read_frame<R: Read>(r: &mut R) -> Result<Envelope, CodecError> {
    let mut header = [0u8; 6];
    r.read_exact(&mut header)?;
    let len = decode_header(&header)?;
    let mut buf = vec![0u8; len];
    r.read_exact(&mut buf)?;
    decode_payload(&buf)
}

/// Async twins of read/write_frame (feature `async`). Same validation, same
/// framing; used by the daemon's session loop and (later) the Rust portald.
#[cfg(feature = "async")]
pub mod asynchronous {
    use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};

    use super::{CodecError, decode_header, decode_payload, encode_frame};
    use crate::envelope::Envelope;

    pub async fn write_frame<W: AsyncWrite + Unpin>(
        w: &mut W,
        env: &Envelope,
    ) -> Result<(), CodecError> {
        let buf = encode_frame(env)?;
        w.write_all(&buf).await?;
        w.flush().await?;
        Ok(())
    }

    pub async fn read_frame<R: AsyncRead + Unpin>(r: &mut R) -> Result<Envelope, CodecError> {
        let mut header = [0u8; 6];
        r.read_exact(&mut header).await?;
        let len = decode_header(&header)?;
        let mut buf = vec![0u8; len];
        r.read_exact(&mut buf).await?;
        decode_payload(&buf)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::messages::Ping;

    #[test]
    fn roundtrip_ping() {
        let env = Envelope::of_ping(Ping { nonce: 42 });
        let mut buf = Vec::new();
        write_frame(&mut buf, &env).unwrap();
        assert_eq!(&buf[..2], b"PF");
        let got = read_frame(&mut &buf[..]).unwrap();
        assert_eq!(got, env);
    }

    #[test]
    fn rejects_bad_magic() {
        let buf = b"XX\x00\x00\x00\x01\xa0".to_vec();
        assert!(matches!(
            read_frame(&mut &buf[..]),
            Err(CodecError::BadMagic)
        ));
    }

    #[test]
    fn rejects_oversized_length_before_allocating() {
        let mut buf = b"PF".to_vec();
        buf.extend_from_slice(&(MAX_FRAME_BYTES as u32 + 1).to_be_bytes());
        assert!(matches!(
            read_frame(&mut &buf[..]),
            Err(CodecError::FrameTooLarge(_))
        ));
    }

    #[test]
    fn rejects_multi_field_envelope() {
        let env = Envelope {
            ping: Some(Ping { nonce: 1 }),
            req_snap: Some(crate::messages::ReqSnap {}),
            ..Envelope::default()
        };
        let mut buf = Vec::new();
        write_frame(&mut buf, &env).unwrap();
        assert!(matches!(
            read_frame(&mut &buf[..]),
            Err(CodecError::MultipleFields)
        ));
    }
}
