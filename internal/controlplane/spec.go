package controlplane

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	imagespec "unikraft.com/x/image-spec"
	"unikraft.com/x/image-spec/reference"
	"unikraft.com/x/image-spec/schemes"
)

const maxSourceBytes = 1 << 20

var metroCode = regexp.MustCompile(`^[a-z]{3}$`)

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var functionName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

// buildSpec is an API input, never a job-status response. Env and Auth are
// runtime configuration: neither is written into the generated build context.
type buildSpec struct {
	Metro    string            `json:"metro,omitempty"`
	Source   string            `json:"source"`
	Image    string            `json:"image"`
	Name     string            `json:"name,omitempty"`
	Entry    string            `json:"entry,omitempty"`
	MemoryMB int64             `json:"memory_mb,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Auth     invocationAuth    `json:"auth,omitempty"`
}

type invocationAuth struct {
	Mode  string `json:"mode,omitempty"`
	Token string `json:"token,omitempty"`
}

func (spec *buildSpec) normalize() error {
	if spec.Metro != "" && !metroCode.MatchString(spec.Metro) {
		return errors.New("metro must be a three-letter lowercase metro code")
	}
	if !utf8.ValidString(spec.Source) || strings.TrimSpace(spec.Source) == "" || len(spec.Source) > maxSourceBytes {
		return errors.New("source must contain Python code, up to 1 MiB of UTF-8")
	}
	if spec.Entry == "" {
		spec.Entry = "handler"
	}
	if !pythonIdentifier.MatchString(spec.Entry) {
		return errors.New("entry must be a Python identifier")
	}
	if spec.Name != "" && !functionName.MatchString(spec.Name) {
		return errors.New("name must start with a lowercase letter and contain up to 31 lowercase letters, digits, or hyphens")
	}
	if spec.MemoryMB == 0 {
		spec.MemoryMB = 128
	}
	if spec.MemoryMB < 128 || spec.MemoryMB > 2048 {
		return errors.New("memory_mb must be between 128 and 2048; the Python runtime requires at least 128 MiB")
	}
	if err := validateEnv(spec.Env); err != nil {
		return err
	}
	location, err := imagespec.GuessLocation(spec.Image)
	if spec.Image == "" || err != nil || location.Scheme != schemes.OCI {
		return errors.New("image must be a registry reference")
	}
	if _, err := reference.ParseNormalizedNamed(location.Path); err != nil {
		return errors.New("image must be a valid registry reference")
	}
	if spec.Auth.Mode == "" {
		spec.Auth.Mode = "bearer"
	}
	switch spec.Auth.Mode {
	case "public":
		if spec.Auth.Token != "" {
			return errors.New("public auth must not include a token")
		}
	case "bearer":
		if spec.Auth.Token == "" {
			spec.Auth.Token = newSecret()
		} else if !validSecret(spec.Auth.Token) {
			return errors.New("auth.token must be 32 to 256 ASCII letters, digits, or -_~.+/=")
		}
	default:
		return errors.New("auth.mode must be bearer or public")
	}
	return nil
}

func validateEnv(env map[string]string) error {
	if len(env) > 128 {
		return errors.New("at most 128 environment variables are allowed")
	}
	size := 0
	for name, value := range env {
		if !envName.MatchString(name) || strings.HasPrefix(name, "LAMBDA_") {
			return errors.New("environment variable names must be identifiers; LAMBDA_ is reserved")
		}
		if !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') {
			return errors.New("environment values must be UTF-8 without NUL bytes")
		}
		size += len(name) + len(value)
	}
	if size > 32<<10 {
		return fmt.Errorf("environment variables exceed 32 KiB")
	}
	return nil
}

func newSecret() string {
	return base64.RawURLEncoding.EncodeToString(randomBytes(32))
}

func randomBytes(length int) []byte {
	data := make([]byte, length)
	_, _ = rand.Read(data) // crypto/rand.Read terminates the process on failure.
	return data
}

func validSecret(secret string) bool {
	if len(secret) < 32 || len(secret) > 256 {
		return false
	}
	for _, char := range secret {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("-_~.+/=", char) {
			continue
		}
		return false
	}
	return true
}
