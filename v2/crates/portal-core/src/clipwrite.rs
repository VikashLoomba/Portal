//! The Mac-side clipboard-WRITE handler (box → Mac, port of v1's
//! runClipWriteHandler): a box shim ran `wl-copy`/`pbcopy`, the blob sits in
//! the box's clip store, and the Mac (a) pulls the bytes by sha over exec,
//! (b) verifies the hash, (c) sets the pasteboard, (d) answers ok — and only
//! then (e) shows the security banner notification (outside the reply
//! budget, v1 doctrine). Gated by the `clip-write` capability, re-read per
//! request.

use std::sync::Arc;

use portal_proto::messages::{ClipWriteRequest, ClipWriteResponse};
use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;

use crate::agentclient::ServiceRequest;
use crate::agentclient::session::Outbound;
use portal_clip::ClipboardWriter;

/// Max accepted write (same generous bound as the read side).
pub const MAX_WRITE_BYTES: i64 = 256 << 20;

pub struct ClipWriteHandler {
    pub writer: Option<Arc<dyn ClipboardWriter>>,
    pub transport: Arc<dyn portal_transport::Transport>,
    pub gates: Arc<dyn Fn(&str) -> bool + Send + Sync>,
    /// Security banner sink (box-attributed notification after each write).
    pub notify: Arc<dyn Fn(super::supervisor::NotifyEvent) + Send + Sync>,
    pub box_name: String,
    pub outbound: mpsc::Sender<Outbound>,
}

impl ClipWriteHandler {
    pub async fn run(&self, rx: &mut mpsc::Receiver<ServiceRequest>, cancel: CancellationToken) {
        loop {
            let req = tokio::select! {
                _ = cancel.cancelled() => return,
                r = rx.recv() => match r { Some(r) => r, None => return },
            };
            let ServiceRequest::ClipWrite(req) = req else {
                continue;
            };
            let resp = self.serve_one(&req).await;
            let ok = resp.ok;
            if let Ok(out) = Outbound::clip_write_response(&resp) {
                let _ = self.outbound.send(out).await;
            }
            // Banner AFTER the response (never inside the reply budget).
            if ok {
                let detail = match req.kind.as_str() {
                    "clear" => "cleared your clipboard".to_string(),
                    k => format!(
                        "wrote {} ({} bytes) to your clipboard",
                        k,
                        req.size.unwrap_or(0)
                    ),
                };
                (self.notify)(super::supervisor::NotifyEvent {
                    box_name: self.box_name.clone(),
                    title: format!("clipboard write from {}", self.box_name),
                    body: Some(detail),
                    urgency: 1,
                    verified: true, // our own daemon raised it
                });
            }
        }
    }

    async fn serve_one(&self, req: &ClipWriteRequest) -> ClipWriteResponse {
        let deny = |reason: &str| ClipWriteResponse {
            nonce: req.nonce,
            epoch: req.epoch,
            ok: false,
            err: Some(reason.to_string()),
        };
        let ok = ClipWriteResponse {
            nonce: req.nonce,
            epoch: req.epoch,
            ok: true,
            err: None,
        };
        if !(self.gates)("clip-write") {
            return deny("disabled");
        }
        let Some(writer) = &self.writer else {
            return deny("unavailable");
        };

        match req.kind.as_str() {
            "clear" => {
                let writer = writer.clone();
                match tokio::task::spawn_blocking(move || writer.clear()).await {
                    Ok(Ok(())) => ok,
                    _ => deny("pasteboard"),
                }
            }
            kind @ ("text" | "image") => {
                let (Some(sha), Some(size)) = (&req.sha, req.size) else {
                    return deny("rejected");
                };
                if size <= 0 || size > MAX_WRITE_BYTES || !valid_sha(sha) {
                    return deny("rejected");
                }
                if kind == "image" && req.format.as_deref() != Some("png") {
                    return deny("rejected");
                }
                // Pull by sha over exec — the path is reconstructed from the
                // sha ALONE (v1 §6.1: a hostile frame can never name a path).
                let data = match self.pull_blob(sha, size).await {
                    Ok(d) => d,
                    Err(reason) => return deny(&reason),
                };
                let writer = writer.clone();
                let is_image = kind == "image";
                let set = tokio::task::spawn_blocking(move || {
                    if is_image {
                        writer.write_image_png(&data)
                    } else {
                        writer.write_text(&String::from_utf8_lossy(&data))
                    }
                })
                .await;
                match set {
                    Ok(Ok(())) => ok,
                    _ => deny("pasteboard"),
                }
            }
            _ => deny("rejected"),
        }
    }

    /// `cat` the store blob (path derived from the sha, sha-verified after).
    async fn pull_blob(&self, sha: &str, size: i64) -> Result<Vec<u8>, String> {
        let path = format!("$HOME/.cache/portal/clip/blob-{sha}");
        let out = self
            .transport
            .exec(b"", &["cat".into(), path])
            .await
            .map_err(|e| format!("pull: {e}"))?;
        if out.stdout.len() as i64 != size {
            return Err("size-mismatch".into());
        }
        let actual = {
            use sha2::{Digest, Sha256};
            hex::encode(Sha256::digest(&out.stdout))
        };
        if actual != sha {
            return Err("hash-mismatch".into());
        }
        Ok(out.stdout)
    }
}

fn valid_sha(sha: &str) -> bool {
    sha.len() == 64
        && sha
            .bytes()
            .all(|b| b.is_ascii_hexdigit() && !b.is_ascii_uppercase())
}

#[cfg(test)]
mod tests {
    use super::*;
    use portal_clip::mock::MockClipboard;
    use portal_transport::testing::FakeTransport;
    use std::sync::Mutex;

    fn sha_of(data: &[u8]) -> String {
        use sha2::{Digest, Sha256};
        hex::encode(Sha256::digest(data))
    }

    struct Rig {
        handler: ClipWriteHandler,
        clipboard: Arc<MockClipboard>,
        transport: Arc<FakeTransport>,
        banners: Arc<Mutex<Vec<super::super::supervisor::NotifyEvent>>>,
        outbound_rx: mpsc::Receiver<Outbound>,
        gate_on: Arc<std::sync::atomic::AtomicBool>,
    }

    fn rig() -> Rig {
        let clipboard = Arc::new(MockClipboard::default());
        let transport = FakeTransport::new("devbox1");
        let banners: Arc<Mutex<Vec<super::super::supervisor::NotifyEvent>>> = Arc::default();
        let gate_on = Arc::new(std::sync::atomic::AtomicBool::new(true));
        let (tx, outbound_rx) = mpsc::channel(4);
        let handler = ClipWriteHandler {
            writer: Some(clipboard.clone()),
            transport: transport.clone(),
            gates: {
                let g = gate_on.clone();
                Arc::new(move |name| {
                    name != "clip-write" || g.load(std::sync::atomic::Ordering::SeqCst)
                })
            },
            notify: {
                let b = banners.clone();
                Arc::new(move |ev| b.lock().unwrap().push(ev))
            },
            box_name: "devbox1".into(),
            outbound: tx,
        };
        Rig {
            handler,
            clipboard,
            transport,
            banners,
            outbound_rx,
            gate_on,
        }
    }

    fn req(kind: &str, sha: Option<String>, size: Option<i64>) -> ClipWriteRequest {
        ClipWriteRequest {
            nonce: 3,
            epoch: 1,
            kind: kind.into(),
            format: (kind == "image").then(|| "png".to_string()),
            sha,
            size,
        }
    }

    #[tokio::test]
    async fn text_write_pulls_verifies_and_sets() {
        let r = rig();
        let data = b"copied on the box";
        r.transport.push_exec_ok(std::str::from_utf8(data).unwrap());
        let resp = r
            .handler
            .serve_one(&req("text", Some(sha_of(data)), Some(data.len() as i64)))
            .await;
        assert!(resp.ok, "{resp:?}");
        use portal_clip::Clipboard;
        assert_eq!(r.clipboard.text().unwrap(), "copied on the box");
        // The pull was `cat <sha-derived path>` — never a wire-named path.
        let calls = r.transport.exec_calls();
        assert!(calls[0].0[1].contains(&sha_of(data)), "{:?}", calls[0].0);
    }

    #[tokio::test]
    async fn hash_mismatch_fails_closed() {
        let r = rig();
        r.transport.push_exec_ok("tampered bytes!!");
        let resp = r
            .handler
            .serve_one(&req("text", Some(sha_of(b"original")), Some(16)))
            .await;
        assert!(!resp.ok);
        assert_eq!(resp.err.as_deref(), Some("hash-mismatch"));
        use portal_clip::Clipboard;
        assert!(r.clipboard.text().is_err(), "pasteboard untouched");
    }

    #[tokio::test]
    async fn gate_off_denies_disabled() {
        let r = rig();
        r.gate_on.store(false, std::sync::atomic::Ordering::SeqCst);
        let resp = r
            .handler
            .serve_one(&req("text", Some(sha_of(b"x")), Some(1)))
            .await;
        assert_eq!(resp.err.as_deref(), Some("disabled"));
    }

    #[tokio::test]
    async fn rejects_bad_shapes() {
        let r = rig();
        // Missing sha/size; bad sha; oversized; non-png image.
        assert!(!r.handler.serve_one(&req("text", None, None)).await.ok);
        assert!(
            !r.handler
                .serve_one(&req("text", Some("nothex".into()), Some(1)))
                .await
                .ok
        );
        assert!(
            !r.handler
                .serve_one(&req("text", Some(sha_of(b"x")), Some(MAX_WRITE_BYTES + 1)))
                .await
                .ok
        );
        let mut bad_img = req("image", Some(sha_of(b"x")), Some(1));
        bad_img.format = Some("jpeg".into());
        assert!(!r.handler.serve_one(&bad_img).await.ok);
    }

    #[tokio::test]
    async fn clear_needs_no_pull_and_run_loop_banners() {
        let mut r = rig();
        let (tx, rx) = mpsc::channel(2);
        let cancel = CancellationToken::new();
        let handler = std::mem::replace(
            &mut r.handler,
            ClipWriteHandler {
                writer: None,
                transport: r.transport.clone(),
                gates: Arc::new(|_| true),
                notify: Arc::new(|_| {}),
                box_name: "x".into(),
                outbound: mpsc::channel(1).0,
            },
        );
        let task = tokio::spawn({
            let cancel = cancel.clone();
            async move {
                let mut rx2 = rx;
                handler.run(&mut rx2, cancel).await;
            }
        });
        tx.send(ServiceRequest::ClipWrite(req("clear", None, None)))
            .await
            .unwrap();
        // Response lands on outbound, banner recorded.
        let out = tokio::time::timeout(std::time::Duration::from_secs(5), r.outbound_rx.recv())
            .await
            .unwrap()
            .unwrap();
        assert_eq!((out.service, out.kind), ("clipwrite", "resp"));
        let ok = tokio::time::timeout(std::time::Duration::from_secs(2), async {
            loop {
                if !r.banners.lock().unwrap().is_empty() {
                    return true;
                }
                tokio::time::sleep(std::time::Duration::from_millis(10)).await;
            }
        })
        .await
        .unwrap_or(false);
        assert!(ok, "banner after successful write");
        assert!(
            r.banners.lock().unwrap()[0]
                .body
                .as_ref()
                .unwrap()
                .contains("cleared")
        );
        cancel.cancel();
        let _ = task.await;
    }
}
