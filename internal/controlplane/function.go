package controlplane

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

//go:embed runtime/Dockerfile runtime/server.py
var runtimeFiles embed.FS

var pythonIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// prepareFunction writes Python source into an isolated build context.
// It never imports or executes user code on the orchestration host.
func prepareFunction(source, entry string) (string, func(), error) {
	if !pythonIdentifier.MatchString(entry) {
		return "", nil, fmt.Errorf("entry must be a Python identifier")
	}
	if len(source) == 0 {
		return "", nil, fmt.Errorf("function source is empty")
	}
	code := []byte(source)

	directory, err := os.MkdirTemp("", "lambda-function-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(directory) }
	command, _ := json.Marshal([]string{"/usr/bin/python3", "-u", "/src/server.py", entry})
	files := map[string][]byte{
		"function.py": code,
		"Kraftfile":   []byte("spec: v0.7\nruntime: base-compat:latest\ntargets:\n  - kraftcloud/x86_64\nrootfs:\n  source: ./Dockerfile\n  format: erofs\ncmd: " + string(command) + "\n"),
	}
	for _, name := range []string{"Dockerfile", "server.py"} {
		files[name], err = runtimeFiles.ReadFile("runtime/" + name)
		if err != nil {
			cleanup()
			return "", nil, err
		}
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(directory, name), contents, 0600); err != nil {
			cleanup()
			return "", nil, err
		}
	}
	return directory, cleanup, nil
}
