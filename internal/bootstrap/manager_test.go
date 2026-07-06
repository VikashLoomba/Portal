package bootstrap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
)

func TestEnsureArtifactIdempotent(t *testing.T) {
	content := []byte("helper payload")
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	wantPath := remoteDir + "/helper-" + digest
	matchProbe := fmt.Sprintf("%d %s\n", len(content), digest)

	tr := &recordingTransport{
		probeOuts: []string{"MISSING\n", matchProbe},
	}
	m := New(tr, testLogger())

	gotPath, err := m.EnsureArtifact(context.Background(), "helper", content)
	if err != nil {
		t.Fatalf("EnsureArtifact first: %v", err)
	}
	if gotPath != wantPath {
		t.Fatalf("EnsureArtifact first path = %q, want %q", gotPath, wantPath)
	}

	gotPath, err = m.EnsureArtifact(context.Background(), "helper", content)
	if err != nil {
		t.Fatalf("EnsureArtifact second: %v", err)
	}
	if gotPath != wantPath {
		t.Fatalf("EnsureArtifact second path = %q, want %q", gotPath, wantPath)
	}

	uploads := tr.uploadStdin()
	if len(uploads) != 1 {
		t.Fatalf("uploads = %d, want 1", len(uploads))
	}
	if !bytes.Equal(uploads[0], content) {
		t.Fatal("uploaded content does not match")
	}
}

func TestEnsureUploadedAMD64PathAndSymlinkUnchanged(t *testing.T) {
	sha := EmbeddedSHA()
	if sha == "" {
		t.Skip("embedded SHA empty; run `make agent` first")
	}

	tr := &recordingTransport{
		unameOut:  "Linux x86_64\n",
		probeOuts: []string{"MISSING\n"},
	}
	m := New(tr, testLogger())

	gotPath, err := m.EnsureUploaded(context.Background())
	if err != nil {
		t.Fatalf("EnsureUploaded: %v", err)
	}
	wantPath := remoteDir + "/agent-" + sha
	if gotPath != wantPath {
		t.Fatalf("EnsureUploaded path = %q, want %q", gotPath, wantPath)
	}

	uploads := tr.uploadStdin()
	if len(uploads) != 1 {
		t.Fatalf("uploads = %d, want 1", len(uploads))
	}
	if !bytes.Equal(uploads[0], EmbeddedAgent()) {
		t.Fatal("uploaded bytes do not match amd64 embedded agent")
	}
	if got := tr.unameCount(); got != 1 {
		t.Fatalf("uname probes = %d, want 1", got)
	}
	if got := tr.scriptCount("ln -sf " + wantPath + " " + remoteDir + "/portald"); got != 1 {
		t.Fatalf("portald symlink scripts = %d, want 1", got)
	}
}
