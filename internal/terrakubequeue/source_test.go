package terrakubequeue

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	"github.com/go-git/go-git/v5/plumbing/transport/file"
	"github.com/go-git/go-git/v5/plumbing/transport/server"
)

func TestGitPromotionClaimsReviewedCommitsInOrder(t *testing.T) {
	remote := t.TempDir()
	local := t.TempDir()
	remoteRepo, err := git.PlainInit(remote, true)
	if err != nil {
		t.Fatal(err)
	}

	localRepo, err := git.PlainInitWithOptions(
		local,
		&git.PlainInitOptions{
			InitOptions: git.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName("main")},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	worktree, err := localRepo.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	// Use the Go receive-pack server for the temporary bare repository.
	client.InstallProtocol("file", server.DefaultServer)
	t.Cleanup(func() { client.InstallProtocol("file", file.DefaultClient) })

	origin, err := localRepo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{remote},
	})
	if err != nil {
		t.Fatal(err)
	}

	push := func(spec config.RefSpec) {
		t.Helper()
		if err := origin.PushContext(
			t.Context(),
			&git.PushOptions{RefSpecs: []config.RefSpec{spec}},
		); err != nil {
			t.Fatal(err)
		}
	}

	src := gitSource{
		Directory:        local,
		Root:             "opentofu/qa",
		Branch:           "terrakube/tack-qa",
		allowLocalRemote: true,
	}
	write := func(name, body string) {
		name = filepath.Join(local, name)
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commit := func() string {
		t.Helper()
		if err := worktree.AddWithOptions(&git.AddOptions{All: true}); err != nil {
			t.Fatal(err)
		}

		hash, err := worktree.Commit(
			"Update fixture",
			&git.CommitOptions{
				Author: &object.Signature{
					Name:  "Queue Test",
					Email: "queue@example.invalid",
					When:  time.Unix(0, 0),
				},
			},
		)
		if err != nil {
			t.Fatal(err)
		}

		return hash.String()
	}
	write("opentofu/qa/main.tf", `module "shared" { source = "../modules/shared" }`)
	write(
		"opentofu/modules/shared/main.tf",
		`module "external" { source = "../../../external/module" }`,
	)
	write("external/module/main.tf", "")
	write(
		"opentofu/qa/ndppd.tf",
		`locals {
 service = file("${path.module}/../modules/docker-guest/files/ndppd.service")
 config = templatefile("${path.module}/../modules/docker-guest/files/ndppd.conf.tftpl", {})
}`,
	)
	write(
		registryPath,
		`{"version":1,"apps":[{"name":"first","repository":"org/first","deployments":[{"name":"qa","root":"opentofu/qa","promotion_branch":"terrakube/qa","environment":"qa","enabled":false,"shared_inputs":["external/shared"]}]}]}`,
	)
	base := commit()
	push("refs/heads/main:refs/heads/terrakube/tack-qa")
	write("opentofu/modules/unrelated/main.tf", "")
	commit()
	write("opentofu/modules/shared/config", "first")
	first := commit()
	write("opentofu/qa/input", "second")
	second := commit()
	patch := "opentofu/modules/docker-guest/files/ndppd.service"
	write(patch, "service")
	service := commit()
	write("opentofu/modules/docker-guest/files/ndppd.conf.tftpl", "config")
	config := commit()
	if _, err := worktree.Remove(patch); err != nil {
		t.Fatal(err)
	}

	removed := commit()
	write("external/module/config", "new")
	external := commit()
	push("refs/heads/main:refs/heads/main")
	write(registryPath, "this unreviewed worktree content must not become deployment input")
	entries, err := readTargets(t.Context(), local)
	if err != nil || len(entries) != 1 || entries[0].Name != "qa" || *entries[0].Enabled {
		t.Fatalf("expected disabled target from reviewed main: %v", err)
	}

	if err := runDeployment(
		t.Context(),
		[]string{"queue", "qa", local},
		entries,
	); !errors.Is(err, errDisabled) {
		t.Fatal("disabled target should fail before authorization", err)
	}

	for _, pair := range [][2]string{
		{base, first},
		{first, second},
		{second, service},
		{service, config},
		{config, removed},
		{removed, external},
	} {
		next, err := src.nextCommit(t.Context(), pair[0])
		if err != nil || next != pair[1] {
			t.Fatalf("next %s: %v", next, err)
		}

		if err := src.promote(t.Context(), pair[0], next); err != nil {
			t.Fatal(err)
		}

		if err := src.promote(t.Context(), pair[0], next); err == nil {
			t.Fatal("a duplicate promotion won the claim")
		}

		if pair[0] == base {
			if err := src.promote(t.Context(), base, second); err == nil {
				t.Fatal("a stale promotion skipped the current commit")
			}
		}

		ref, err := remoteRepo.Reference(plumbing.NewBranchReferenceName(src.Branch), true)
		if err != nil || ref.Hash().String() != next {
			t.Fatalf("remote promotion ref = %v, want %s (%v)", ref, next, err)
		}
	}

	if next, err := src.nextCommit(t.Context(), external); err != nil || next != "" {
		t.Fatalf("caught up %q: %v", next, err)
	}

	settings, err := localRepo.Config()
	if err != nil {
		t.Fatal(err)
	}

	settings.Raw.Section("extensions").SetOption("worktreeConfig", "true")
	if err := localRepo.SetConfig(settings); err != nil {
		t.Fatal(err)
	}

	entries, err = readTargets(t.Context(), local)
	if err != nil || len(entries) != 1 || entries[0].Name != "qa" {
		t.Fatalf("expected reviewed enrollment from worktree-config repository: %v", err)
	}
}
