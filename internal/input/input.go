// Package input validates file boundaries and immutable input references.
package input

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/distribution/reference"
)

var (
	commitPattern = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// Commit requires a complete Git object identifier.
func Commit(value string) bool { return commitPattern.MatchString(value) }

// Digest requires a SHA-256 digest with its algorithm prefix.
func Digest(value string) bool { return digestPattern.MatchString(value) }

// ImageRepository accepts an image repository without a tag or digest.
func ImageRepository(value string) bool {
	parsed, err := reference.Parse(value)
	if err != nil {
		return false
	}
	named, ok := parsed.(reference.Named)
	return ok && named.Name() == value && strings.Contains(value, "/")
}

// Image requires a repository and immutable SHA-256 digest.
func Image(value string) bool {
	repository, digest, found := strings.Cut(value, "@")
	return found && ImageRepository(repository) && Digest(digest)
}

// ReadJSON checks document boundaries and duplicate members before typed decoding.
func ReadJSON(data []byte) (*json.Decoder, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := readValue(decoder); err != nil {
		slog.Error("JSON validation failed", "err", err)
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("JSON must contain exactly one document")
	}
	decoder = json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder, nil
}

func readValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		slog.Error("JSON token read failed", "err", err)
		return fmt.Errorf("read JSON value: %w", err)
	}
	delimiter, container := token.(json.Delim)
	if !container {
		return nil
	}
	keys := make(map[string]bool)
	for decoder.More() {
		if delimiter == '{' {
			keyToken, err := decoder.Token()
			if err != nil {
				return fmt.Errorf("read JSON member: %w", err)
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON member must be a string")
			}
			if keys[key] {
				return fmt.Errorf("duplicate JSON member %q", key)
			}
			keys[key] = true
		}
		if err := readValue(decoder); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return fmt.Errorf("close JSON container: %w", err)
	}
	return nil
}

// Resolve requires an existing path beneath the checkout, including symlink targets.
func Resolve(baseDir, name string) (string, error) {
	if !filepath.IsLocal(name) {
		return "", errors.New("path must be relative and remain beneath the checkout")
	}
	base, err := filepath.EvalSymlinks(baseDir)
	if err != nil {
		slog.Error("Checkout resolution failed", "err", err)
		return "", fmt.Errorf("resolve checkout: %w", err)
	}
	base, err = filepath.Abs(base)
	if err != nil {
		return "", fmt.Errorf("resolve checkout: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(base, name))
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", name, err)
	}
	relative, err := filepath.Rel(base, resolved)
	if err != nil || !filepath.IsLocal(relative) {
		return "", errors.New("path resolves outside the checkout")
	}
	return resolved, nil
}

// RegularFile requires an existing regular file beneath the checkout.
func RegularFile(baseDir, name string) (string, error) {
	resolved, err := Resolve(baseDir, name)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		slog.Error("Input file inspection failed", "err", err)
		return "", fmt.Errorf("inspect input file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("input must be a regular file")
	}
	return resolved, nil
}
