// Package release validates bounded release manifests and artifact archives.
package release

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/agoodkind/tofu-workflows/internal/input"
)

const (
	maxManifestBytes = 64 * 1024
	maxArchiveBytes  = maxManifestBytes + 128*1024
)

// Manifest binds immutable artifacts and scalar variables to one build attempt.
type Manifest struct {
	Version      int                        `json:"version"`
	SourceSHA    string                     `json:"source_sha"`
	BuildRunID   int64                      `json:"build_run_id"`
	BuildAttempt int                        `json:"build_attempt"`
	Artifacts    map[string]string          `json:"artifacts"`
	Variables    map[string]json.RawMessage `json:"variables"`
}

// Load reads one release manifest with the archive entry size limit.
func Load(path string) (Manifest, error) {
	file, err := os.Open(path)
	if err != nil {
		slog.Error("Release manifest open failed", "err", err)
		return Manifest{}, fmt.Errorf("open release manifest: %w", err)
	}
	manifest, readErr := readManifest(file)
	return manifest, errors.Join(readErr, file.Close())
}

// ReadArtifact binds identity to run metadata that the caller authenticated.
// The archive itself cannot authenticate GitHub metadata.
func ReadArtifact(reader io.Reader, expectedRunID int64, expectedAttempt int) (Manifest, error) {
	if expectedRunID <= 0 || expectedAttempt <= 0 {
		return Manifest{}, errors.New("expected run ID and attempt must be positive")
	}
	data, err := io.ReadAll(io.LimitReader(reader, maxArchiveBytes+1))
	if err != nil {
		slog.Error("Release archive read failed", "err", err)
		return Manifest{}, fmt.Errorf("read release archive: %w", err)
	}
	if len(data) > maxArchiveBytes {
		return Manifest{}, errors.New("release archive exceeds the bounded ZIP size")
	}
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return Manifest{}, fmt.Errorf("read release ZIP: %w", err)
	}
	if len(archive.File) != 1 {
		return Manifest{}, errors.New("release archive must contain exactly one release.json entry")
	}
	entry := archive.File[0]
	if entry.Name != "release.json" || !entry.Mode().IsRegular() {
		return Manifest{}, errors.New("release archive requires a regular release.json entry")
	}
	if entry.UncompressedSize64 > maxManifestBytes {
		return Manifest{}, errors.New("release.json exceeds 64 KiB")
	}
	stream, err := entry.Open()
	if err != nil {
		return Manifest{}, fmt.Errorf("open release.json: %w", err)
	}
	manifest, readErr := readManifest(stream)
	if err := errors.Join(readErr, stream.Close()); err != nil {
		return Manifest{}, err
	}
	if manifest.BuildRunID != expectedRunID || manifest.BuildAttempt != expectedAttempt {
		return Manifest{}, errors.New("release manifest does not match the expected build run and attempt")
	}
	return manifest, nil
}

func readManifest(reader io.Reader) (Manifest, error) {
	var manifest Manifest
	data, err := io.ReadAll(io.LimitReader(reader, maxManifestBytes+1))
	if err != nil {
		slog.Error("Release manifest read failed", "err", err)
		return manifest, fmt.Errorf("read release manifest: %w", err)
	}
	if len(data) > maxManifestBytes {
		return manifest, errors.New("release.json exceeds 64 KiB")
	}
	decoder, err := input.ReadJSON(data)
	if err != nil {
		return manifest, fmt.Errorf("read release JSON: %w", err)
	}
	if err := decoder.Decode(&manifest); err != nil {
		return manifest, fmt.Errorf("decode release manifest: %w", err)
	}
	if err := validate(manifest); err != nil {
		return manifest, err
	}
	return manifest, nil
}

func validate(manifest Manifest) error {
	if manifest.Version != 1 {
		return errors.New("manifest version must be 1")
	}
	if !input.Commit(manifest.SourceSHA) {
		return errors.New("manifest source_sha must be an exact 40-character hexadecimal commit")
	}
	if manifest.BuildRunID <= 0 || manifest.BuildAttempt <= 0 {
		return errors.New("manifest build_run_id and build_attempt must be positive")
	}
	for _, image := range manifest.Artifacts {
		if !input.Image(image) {
			return errors.New("manifest artifact must identify a repository and sha256 digest")
		}
	}
	for name, value := range manifest.Variables {
		if _, err := ScalarKind(value); err != nil {
			slog.Error("Release variable validation failed", "variable", name, "err", err)
			return fmt.Errorf("manifest variable %q: %w", name, err)
		}
	}
	return nil
}

// ScalarKind classifies JSON without converting exact numeric values to floats.
func ScalarKind(value json.RawMessage) (string, error) {
	value = bytes.TrimSpace(value)
	if !json.Valid(value) || len(value) == 0 {
		return "", errors.New("variable must contain valid scalar JSON")
	}
	switch value[0] {
	case '"':
		return "string", nil
	case 't', 'f':
		return "bool", nil
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return "number", nil
	default:
		return "", errors.New("variable must be a string, number, or bool")
	}
}
