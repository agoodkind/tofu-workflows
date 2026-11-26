package terrakubequeue

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestGitPromotionClaimsReviewedCommitsInOrder(t *testing.T) {
	remote := t.TempDir()
	local := t.TempDir()
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}

		return string(out)
	}
	run(remote, "init", "--bare", "--initial-branch=main")
	run(local, "init", "--initial-branch=main")
	run(local, "config", "user.name", "Queue Test")
	run(local, "config", "user.email", "queue@example.invalid")
	run(local, "remote", "add", "origin", remote)
	src := gitSource{
		Directory: local,
		Root:      "opentofu/qa",
		Branch:    "terrakube/tack-qa",
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
		run(local, "add", ".")
		run(local, "commit", "-m", "Update fixture")
		rev, err := src.git(t.Context(), "rev-parse", "HEAD")
		if err != nil {
			t.Fatal(err)
		}

		return rev
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
	base := commit()
	run(local, "push", "origin", "HEAD:refs/heads/terrakube/tack-qa")
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
	run(local, "rm", patch)
	removed := commit()
	write("external/module/config", "new")
	external := commit()
	run(local, "push", "origin", "main")

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
			t.Fatal("duplicate git push won a claim")
		}
	}

	if next, err := src.nextCommit(t.Context(), external); err != nil || next != "" {
		t.Fatalf("caught up %q: %v", next, err)
	}
}
