package bootstrap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

var quotedTildeRE = regexp.MustCompile(`'~/\.cache/portal`)

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

func TestEnsureArtifactNameValidation(t *testing.T) {
	for _, name := range []string{"xdg-open", "agent"} {
		t.Run("valid "+name, func(t *testing.T) {
			if err := validArtifactName(name); err != nil {
				t.Fatalf("validArtifactName(%q): %v", name, err)
			}
		})
	}

	tests := []struct {
		name string
		in   string
	}{
		{name: "space", in: "has space"},
		{name: "dotdot", in: "../etc"},
		{name: "command substitution", in: "$(rm -rf ~)"},
		{name: "single quote", in: "bad'name"},
		{name: "double quote", in: `bad"name`},
		{name: "too long", in: strings.Repeat("a", 65)},
		{name: "leading dash", in: "-flag"},
		{name: "leading dot", in: ".hidden"},
		{name: "empty", in: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := &recordingTransport{}
			m := New(tr, testLogger())

			_, err := m.EnsureArtifact(context.Background(), tt.in, []byte("payload"))
			if !errors.Is(err, errInvalidArtifactName) {
				t.Fatalf("EnsureArtifact error = %v, want errInvalidArtifactName", err)
			}
			if got := tr.count(func(execRecord) bool { return true }); got != 0 {
				t.Fatalf("Exec records = %d, want 0", got)
			}
		})
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

func TestEnsureUploadedBashArgGolden(t *testing.T) {
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

	scripts := recordedBashArgs(tr)
	if len(scripts) < 3 {
		t.Fatalf("bash -c scripts = %d, want at least 3", len(scripts))
	}

	wantProbe := fmt.Sprintf(`'test -x ~/.cache/portal/agent-%[1]s && printf '\''%%s %%s'\'' "$(stat -c %%s ~/.cache/portal/agent-%[1]s 2>/dev/null || stat -f %%z ~/.cache/portal/agent-%[1]s)" "$(sha256sum ~/.cache/portal/agent-%[1]s 2>/dev/null | awk '\''{print $1}'\'' || sha256 -q ~/.cache/portal/agent-%[1]s 2>/dev/null || openssl dgst -sha256 -hex ~/.cache/portal/agent-%[1]s 2>/dev/null | awk '\''{print $NF}'\'')" || echo MISSING'`, sha)
	wantUpload := fmt.Sprintf(`'set -e; install -d -m 0700 ~/.cache/portal && tmp=$(mktemp ~/.cache/portal/.agent.tmp.XXXXXX) && trap '\''rm -f "$tmp"'\'' EXIT && cat > "$tmp" && [ "$(wc -c < "$tmp")" = "%[2]d" ] && chmod 0755 "$tmp" && mv "$tmp" ~/.cache/portal/agent-%[1]s && trap - EXIT'`, sha, len(EmbeddedAgent()))
	wantSymlink := fmt.Sprintf(`'ln -sf ~/.cache/portal/agent-%[1]s ~/.cache/portal/portald'`, sha)

	if scripts[0] != wantProbe {
		t.Fatalf("probe argv mismatch\ngot:  %q\nwant: %q", scripts[0], wantProbe)
	}
	if scripts[1] != wantUpload {
		t.Fatalf("upload argv mismatch\ngot:  %q\nwant: %q", scripts[1], wantUpload)
	}
	if scripts[2] != wantSymlink {
		t.Fatalf("symlink argv mismatch\ngot:  %q\nwant: %q", scripts[2], wantSymlink)
	}
}

func TestBootstrapScriptsKeepTildeUnquoted(t *testing.T) {
	sha := EmbeddedSHA()
	if sha == "" {
		t.Skip("embedded SHA empty; run `make agent` first")
	}

	tr := &recordingTransport{
		unameOut:  "Linux x86_64\n",
		probeOuts: []string{"MISSING\n"},
	}
	m := New(tr, testLogger())
	if _, err := m.EnsureUploaded(context.Background()); err != nil {
		t.Fatalf("EnsureUploaded: %v", err)
	}
	agentPath := "~/.cache/portal/agent-" + sha
	assertNoInteriorQuotedTilde(t, bashArgContaining(t, tr, "test -x "+agentPath))
	assertNoInteriorQuotedTilde(t, bashArgContaining(t, tr, "ln -sf "+agentPath+" ~/.cache/portal/portald"))

	content := []byte("helper payload")
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	artifactPath := "~/.cache/portal/xdg-open-" + digest
	artifactTr := &recordingTransport{
		probeOuts: []string{"MISSING\n"},
	}
	artifactManager := New(artifactTr, testLogger())
	if _, err := artifactManager.EnsureArtifact(context.Background(), "xdg-open", content); err != nil {
		t.Fatalf("EnsureArtifact: %v", err)
	}
	assertNoInteriorQuotedTilde(t, bashArgContaining(t, artifactTr, "test -x "+artifactPath))
	assertNoInteriorQuotedTilde(t, bashArgContaining(t, artifactTr, "ln -sf "+artifactPath+" ~/.cache/portal/xdg-open"))
}

func TestEnsureUploadedProbeHitShortCircuits(t *testing.T) {
	sha := EmbeddedSHA()
	if sha == "" {
		t.Skip("embedded SHA empty; run `make agent` first")
	}

	agent := EmbeddedAgent()
	sum := sha256.Sum256(agent)
	digest := hex.EncodeToString(sum[:])
	wantPath := remoteDir + "/agent-" + sha

	tr := &recordingTransport{
		unameOut:  "Linux x86_64\n",
		probeOuts: []string{fmt.Sprintf("%d %s\n", len(agent), digest)},
	}
	m := New(tr, testLogger())

	gotPath, err := m.EnsureUploaded(context.Background())
	if err != nil {
		t.Fatalf("EnsureUploaded: %v", err)
	}
	if gotPath != wantPath {
		t.Fatalf("EnsureUploaded path = %q, want %q", gotPath, wantPath)
	}
	if uploads := tr.uploadStdin(); len(uploads) != 0 {
		t.Fatalf("uploads = %d, want 0", len(uploads))
	}
	if got := tr.scriptCount("ln -sf " + wantPath + " " + remoteDir + "/portald"); got != 0 {
		t.Fatalf("portald symlink scripts = %d, want 0", got)
	}
}

func recordedBashArgs(tr *recordingTransport) []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()

	var scripts []string
	for _, rec := range tr.records {
		if len(rec.argv) >= 3 && rec.argv[0] == "bash" && rec.argv[1] == "-c" {
			scripts = append(scripts, rec.argv[2])
		}
	}
	return scripts
}

func bashArgContaining(t *testing.T, tr *recordingTransport, substr string) string {
	t.Helper()

	scripts := recordedBashArgs(tr)
	for _, script := range scripts {
		if strings.Contains(script, substr) {
			return script
		}
	}
	t.Fatalf("no bash -c argv containing %q; recorded scripts: %q", substr, scripts)
	return ""
}

func assertNoInteriorQuotedTilde(t *testing.T, script string) {
	t.Helper()

	if quotedTildeRE.MatchString(script) {
		t.Fatalf("script contains interior-quoted tilde path: %q", script)
	}
}
