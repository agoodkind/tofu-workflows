package tack

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"

	"github.com/google/go-github/v93/github"

	"github.com/agoodkind/tofu-workflows/internal/release"
)

const (
	releaseDispatchAction = "tack-release-ready"
	maxDispatchArchive    = 32 * 1024
	maxDispatchEvent      = 96 * 1024
	tackRepositoryID      = 1204997790
)

var (
	errDispatchIdentity = errors.New("release dispatch must come from the Configs publisher App")
	errDispatchBounds   = errors.New("release dispatch exceeds its size limit")
	errDispatchPayload  = errors.New(
		"release dispatch requires an archive, artifact ID, run ID, and attempt",
	)
	errDispatchArtifact = errors.New("release archive does not match the Tack build artifact")
)

type releaseDispatch struct {
	Archive    []byte `json:"archive"`
	ArtifactID int64  `json:"artifact_id"`
	RunID      int64  `json:"run_id"`
	Attempt    int    `json:"attempt"`
}

func decodeDispatch(reader io.Reader, bot string) (*releaseDispatch, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxDispatchEvent+1))
	if err != nil {
		slog.Error("Read release dispatch failed", "err", err)

		return nil, fmt.Errorf("read release dispatch: %w", err)
	}

	if len(data) > maxDispatchEvent {
		return nil, errDispatchBounds
	}

	var event struct {
		Action string `json:"action"`
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Payload releaseDispatch `json:"client_payload"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		slog.Error("Decode release dispatch failed", "err", err)

		return nil, fmt.Errorf("decode release dispatch: %w", err)
	}

	if event.Action != releaseDispatchAction ||
		event.Repository.FullName != repoOwner+"/"+configsRepo ||
		event.Sender.Login != bot {
		return nil, errDispatchIdentity
	}

	return &event.Payload, nil
}

func verifyDispatch(
	ctx context.Context,
	source *github.Client,
	dispatch *releaseDispatch,
) (release.Manifest, error) {
	if len(dispatch.Archive) > maxDispatchArchive {
		return release.Manifest{}, errDispatchBounds
	}

	if len(dispatch.Archive) == 0 || dispatch.ArtifactID <= 0 || dispatch.RunID <= 0 ||
		dispatch.Attempt <= 0 {
		return release.Manifest{}, errDispatchPayload
	}

	manifest, err := release.ReadArtifact(
		bytes.NewReader(dispatch.Archive),
		dispatch.RunID,
		dispatch.Attempt,
	)
	if err != nil {
		slog.Error("Read Tack release artifact failed", "err", err)

		return release.Manifest{}, fmt.Errorf("read Tack release artifact: %w", err)
	}

	artifact, _, err := source.Actions.GetArtifact(ctx, repoOwner, tackRepo, dispatch.ArtifactID)
	if err != nil {
		slog.Error("Read Tack artifact metadata failed", "err", err)

		return release.Manifest{}, fmt.Errorf("read Tack artifact metadata: %w", err)
	}

	run := artifact.GetWorkflowRun()
	if artifact.GetName() != "tack-release-"+strconv.Itoa(dispatch.Attempt) ||
		artifact.GetID() != dispatch.ArtifactID || artifact.GetExpired() ||
		run.GetID() != dispatch.RunID || run.GetHeadSHA() != manifest.SourceSHA ||
		run.GetRepositoryID() != tackRepositoryID || run.GetHeadRepositoryID() != tackRepositoryID ||
		artifact.GetDigest() != fmt.Sprintf("sha256:%x", sha256.Sum256(dispatch.Archive)) {
		return release.Manifest{}, errDispatchArtifact
	}

	return manifest, nil
}
