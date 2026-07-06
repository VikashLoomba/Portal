package bootstrap

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
)

type artifact struct {
	goos   string
	goarch string
	bytes  []byte
	sha    string
}

var (
	artifactMatrixOnce sync.Once
	artifactMatrix     map[string]artifact
)

func artifacts() map[string]artifact {
	artifactMatrixOnce.Do(func() {
		artifactMatrix = map[string]artifact{
			"linux/amd64": newArtifact("linux", "amd64", agentBinaryAMD64),
			"linux/arm64": newArtifact("linux", "arm64", agentBinaryARM64),
		}
	})
	return artifactMatrix
}

func newArtifact(goos, goarch string, content []byte) artifact {
	sum := sha256.Sum256(content)
	return artifact{
		goos:   goos,
		goarch: goarch,
		bytes:  content,
		sha:    hex.EncodeToString(sum[:]),
	}
}

func mapUname(unameSM string) (goos, goarch string, err error) {
	observed := strings.TrimSpace(unameSM)
	fields := strings.Fields(observed)
	if len(fields) != 2 {
		return "", "", unsupportedUnameError(observed)
	}

	switch {
	case fields[0] == "Linux" && fields[1] == "x86_64":
		return "linux", "amd64", nil
	case fields[0] == "Linux" && (fields[1] == "aarch64" || fields[1] == "arm64"):
		return "linux", "arm64", nil
	default:
		return "", "", unsupportedUnameError(observed)
	}
}

func selectArtifact(unameSM string) (artifact, error) {
	goos, goarch, err := mapUname(unameSM)
	if err != nil {
		return artifact{}, err
	}

	key := goos + "/" + goarch
	art, ok := artifacts()[key]
	if !ok {
		return artifact{}, unsupportedUnameError(strings.TrimSpace(unameSM))
	}
	return art, nil
}

func unsupportedUnameError(observed string) error {
	return fmt.Errorf("bootstrap: unsupported box architecture %q (supported: Linux x86_64 -> linux/amd64, Linux aarch64/arm64 -> linux/arm64)", observed)
}
