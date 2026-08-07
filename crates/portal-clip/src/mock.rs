//! In-memory clipboard for daemon/service tests.

use std::sync::Mutex;

use crate::{ClipError, ClipKind, Clipboard, ClipboardWriter};

#[derive(Debug, Default)]
pub struct MockClipboard {
    inner: Mutex<State>,
}

#[derive(Debug, Default)]
struct State {
    image_png: Option<Vec<u8>>,
    text: Option<String>,
    concealed: bool,
}

impl MockClipboard {
    pub fn with_image(png: Vec<u8>) -> Self {
        let m = Self::default();
        m.inner.lock().unwrap().image_png = Some(png);
        m
    }

    pub fn with_text(text: impl Into<String>) -> Self {
        let m = Self::default();
        m.inner.lock().unwrap().text = Some(text.into());
        m
    }

    pub fn set_concealed(&self, concealed: bool) {
        self.inner.lock().unwrap().concealed = concealed;
    }
}

impl Clipboard for MockClipboard {
    fn has_image(&self) -> bool {
        self.inner.lock().unwrap().image_png.is_some()
    }
    fn has_text(&self) -> bool {
        self.inner.lock().unwrap().text.is_some()
    }
    fn is_concealed(&self) -> bool {
        self.inner.lock().unwrap().concealed
    }
    fn image_png(&self) -> Result<Vec<u8>, ClipError> {
        self.inner
            .lock()
            .unwrap()
            .image_png
            .clone()
            .ok_or(ClipError::Empty(ClipKind::Image))
    }
    fn text(&self) -> Result<String, ClipError> {
        self.inner
            .lock()
            .unwrap()
            .text
            .clone()
            .ok_or(ClipError::Empty(ClipKind::Text))
    }
}

impl ClipboardWriter for MockClipboard {
    fn write_text(&self, text: &str) -> Result<(), ClipError> {
        let mut st = self.inner.lock().unwrap();
        st.text = Some(text.to_string());
        st.image_png = None;
        Ok(())
    }
    fn write_image_png(&self, png: &[u8]) -> Result<(), ClipError> {
        let mut st = self.inner.lock().unwrap();
        st.image_png = Some(png.to_vec());
        st.text = None;
        Ok(())
    }
    fn clear(&self) -> Result<(), ClipError> {
        let mut st = self.inner.lock().unwrap();
        st.text = None;
        st.image_png = None;
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn mock_roundtrip() {
        let m = MockClipboard::with_text("hello");
        assert!(m.has_text() && !m.has_image());
        assert_eq!(m.text().unwrap(), "hello");
        m.write_image_png(b"\x89PNG").unwrap();
        assert!(m.has_image() && !m.has_text());
        m.clear().unwrap();
        assert!(!m.has_image() && !m.has_text());
    }
}
