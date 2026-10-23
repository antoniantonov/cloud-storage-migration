package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDockerIgnoreBuildContext(t *testing.T) {
	if os.Getenv("RUNNER_DOCKER_IGNORE_TEST") != "1" {
		t.Skip("set RUNNER_DOCKER_IGNORE_TEST=1 to verify the allowlist with a local Docker daemon")
	}
	f := newFixture(t)
	_, source, _, _ := runtime.Caller(0)
	repo := filepath.Clean(filepath.Join(filepath.Dir(source), "../../.."))
	ignore, err := os.ReadFile(filepath.Join(repo, ".dockerignore"))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		".dockerignore": string(ignore),
		"go.mod":        "module example.com/context-check\n\ngo 1.26.0\n",
		"go.sum":        "",
		"src/main.go": `package main
import "example.com/context-check/src/internal/nested/deeper"
func main() { println(deeper.Value) }
`,
		"src/internal/nested/deeper/value.go": "package deeper\nconst Value = \"compiled\"\n",
		"src/custom/google.json":              `{"dummy":"must not enter context"}`,
		"src/custom-state/msal-cache":         "dummy private cache",
		"src/custom-state/nested/token":       "dummy nested private cache",
		"src/internal/nested/credentials":     "dummy extensionless credential file",
		"src/internal/nested/data.json":       `{"dummy":"must not enter context"}`,
		"src/.auth/private.go":                "// Private directories remain excluded even for .go files.\n",
		"src/go-build123/generated.go":        "// Generated build directories remain excluded.\n",
		"custom-google.json":                  `{"dummy":"root credentials stay excluded"}`,
	}
	for name, contents := range files {
		path := filepath.Join(f.root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	dockerfile := `FROM golang:1.27.1-alpine AS build
WORKDIR /build
COPY . .
RUN set -ex; test -f src/main.go \
    && test -f src/internal/nested/deeper/value.go \
    && test ! -e src/custom/google.json \
    && test ! -e src/custom-state/msal-cache \
    && test ! -e src/custom-state/nested/token \
    && test ! -e src/internal/nested/credentials \
    && test ! -e src/internal/nested/data.json \
    && test ! -e src/.auth/private.go \
    && test ! -e src/go-build123/generated.go \
    && test ! -e custom-google.json \
    && CGO_ENABLED=0 go build -mod=readonly -o /proof/context-check ./src
FROM scratch
COPY --from=build /proof /proof
`
	export := filepath.Join(filepath.Dir(f.root), "docker export")
	command := exec.Command("docker", "build", "--network=none", "--file", "-",
		"--output", "type=local,dest="+export, f.root)
	command.Stdin = strings.NewReader(dockerfile)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Docker context allowlist or compilation failed: %v\n%s", err, output)
	}
	if _, err := os.Stat(filepath.Join(export, "proof/context-check")); err != nil {
		t.Fatalf("Docker did not compile the allowed Go sources: %v\n%s", err, output)
	}
}
