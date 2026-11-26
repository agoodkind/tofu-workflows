// Package terrakubeexec deploys promoted Configs revisions.
package terrakubeexec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	configsOrigin  = "https://github.com/agoodkind/configs"
	configsctl     = "/usr/local/bin/configsctl"
	lockTimeout    = "-lock-timeout=5m"
	changesPending = 2
	planFailed     = 1
)

const commandCancelGrace = 15 * time.Second

type deployment struct {
	selector  string
	workspace string
	branch    string
}

type command struct {
	name   string
	args   []string
	input  io.Reader
	output io.Writer
}

type commandRunner func(context.Context, command) error

// Run applies the saved plan for the matching deployment target.
//
// The executor verifies the promoted revision and isolates the credentials
// before running configsctl. A failed or empty plan never starts an apply.
//
// Terrakube calls Run with the checkout as its working directory:
//
//	terrakube-configs suburban/tack-qa
func Run(
	ctx context.Context,
	args []string,
	getenv func(string) string,
	stdout io.Writer,
	stderr io.Writer,
) error {
	if len(args) != 1 {
		return errSelectorRequired
	}

	selected := deployment{selector: args[0], workspace: getenv("workspaceName")}
	if !deploymentWorkspace.MatchString(selected.workspace) {
		return errWorkspaceAndBranch
	}

	selected.branch = getenv("branch")
	if !promotionBranch.MatchString(selected.branch) ||
		!workspaceSelector.MatchString(selected.selector) {
		return errWorkspaceAndBranch
	}

	origin := getenv("source")
	if origin != configsOrigin && origin != configsOrigin+".git" {
		return errUntrustedSource
	}

	gitToken := getenv("accessToken")
	if gitToken == "" {
		return errMissingVCSReadToken
	}

	receipt := deploymentReceipt{
		Version:     1,
		JobID:       getenv("jobId"),
		WorkspaceID: getenv("workspaceId"),
		Outcome:     "plan_failed",
	}
	if !jobIDPattern.MatchString(receipt.JobID) ||
		!workspaceIDPattern.MatchString(receipt.WorkspaceID) {
		return errMissingJobIdentity
	}

	return deployFromCheckout(ctx, selected, origin, gitToken, getenv, stdout, stderr, receipt)
}

func deployFromCheckout(
	ctx context.Context,
	selected deployment,
	origin string,
	gitToken string,
	getenv func(string) string,
	stdout io.Writer,
	stderr io.Writer,
	receipt deploymentReceipt,
) error {
	root, err := os.Getwd()
	if err != nil {
		return executionError("locate Terrakube checkout", err)
	}

	private, err := os.MkdirTemp("/tmp", "terrakube-configs-")
	if err != nil {
		return executionError("create private job directory", err)
	}
	defer os.RemoveAll(private)

	paths, err := prepareDirectories(private)
	if err != nil {
		return err
	}

	environment := isolatedEnvironment(paths, origin, gitToken)
	git := runner(root, environment, stderr)
	commit, err := prepareCheckout(ctx, git, origin, selected.branch, root)
	if err != nil {
		return err
	}

	if err := requireEnrollment(ctx, git, commit, selected); err != nil {
		return err
	}

	providerToken, err := prepareCredentials(paths, getenv)
	if err != nil {
		return err
	}

	socket, stopAgent, err := prepareSSH(ctx, root, environment, paths, getenv)
	if err != nil {
		return err
	}
	defer stopAgent()

	environment = append(
		environment,
		"SSH_AUTH_SOCK="+socket,
		"CONFIGSCTL_VAULT_FILE="+paths.vaultPath(),
		"TF_IN_AUTOMATION=1",
	)

	receipt.CommitID = commit
	slog.Info(
		"Verified Terrakube deployment source",
		"commit",
		commit,
		"workspace",
		selected.selector,
	)

	verify := func(ctx context.Context) error {
		current, err := verifyCheckout(ctx, git, origin, selected.branch)
		if err != nil {
			return err
		}

		if current != commit {
			return errCheckoutChanged
		}

		return requireCleanCheckout(ctx, git)
	}

	lockFile, err := gitOutput(
		ctx,
		git,
		"ls-files",
		"--",
		"opentofu/"+selected.selector+"/.terraform.lock.hcl",
	)
	if err != nil {
		return err
	}

	job := operation{
		selected:     selected,
		paths:        paths,
		initReadOnly: lockFile != "",
		run:          runner(root, environment, stderr),
		install:      runner(root, append(environment, "GH_TOKEN="+providerToken), stderr),
		verify:       verify,
		output:       stdout,
		publishPlan:  func() error { return publishPlanLog(paths.tmp, stdout) },
		failPlan:     func() error { return receipt.write(stdout) },
	}

	return job.execute(ctx)
}

func prepareCheckout(
	ctx context.Context,
	git commandRunner,
	origin string,
	branch string,
	root string,
) (string, error) {
	commit, err := verifyCheckout(ctx, git, origin, branch)
	if err != nil {
		return "", err
	}

	if err := excludeExecutorFiles(root); err != nil {
		return "", err
	}

	return commit, requireCleanCheckout(ctx, git)
}

type processStatusError int

func (code processStatusError) Error() string {
	return fmt.Sprintf("exited with status %d", code)
}

func runner(directory string, environment []string, stderr io.Writer) commandRunner {
	return func(ctx context.Context, task command) error {
		var process *exec.Cmd
		switch task.name {
		case configsctl:
			process = exec.CommandContext(ctx, configsctl)
		case "/usr/bin/git":
			process = exec.CommandContext(ctx, "/usr/bin/git")
		case "/usr/bin/ssh-agent":
			process = exec.CommandContext(ctx, "/usr/bin/ssh-agent")
		case "/usr/bin/ssh-add":
			process = exec.CommandContext(ctx, "/usr/bin/ssh-add")
		default:
			return errUnsupportedProcess
		}

		process.Args = append(process.Args, task.args...)

		process.Dir = directory
		process.Env = environment

		process.Stdin = task.input
		process.Stdout = task.output
		var guardedOutput, guardedError subprocessOutput
		if task.name == configsctl && task.output != nil {
			guardedOutput.destination = task.output
			process.Stdout = &guardedOutput
		}

		if stderr != nil {
			guardedError.destination = stderr
			process.Stderr = &guardedError
		}

		process.Cancel = func() error { return process.Process.Signal(os.Interrupt) }
		process.WaitDelay = commandCancelGrace

		slog.Debug("Run deployment command", "command", filepath.Base(task.name))

		err := errors.Join(process.Run(), guardedOutput.finish(), guardedError.finish())
		if err != nil {
			if exited, ok := errors.AsType[*exec.ExitError](err); ok {
				return processStatusError(exited.ExitCode())
			}

			return executionError(filepath.Base(task.name), err)
		}

		return nil
	}
}

type operation struct {
	selected     deployment
	paths        jobPaths
	initReadOnly bool
	run          commandRunner
	install      commandRunner
	verify       func(context.Context) error
	output       io.Writer
	publishPlan  func() error
	failPlan     func() error
}

func (o operation) execute(ctx context.Context) error {
	if err := o.install(
		ctx,
		command{
			name:   configsctl,
			args:   []string{"providers", "install"},
			output: o.output,
		},
	); err != nil {
		return executionError("install pinned Configs providers", err)
	}

	init := []string{"init", "-input=false", "-no-color"}
	if o.initReadOnly {
		init = append(init, "-lockfile=readonly")
	}

	if err := o.tofu(ctx, init...); err != nil {
		return executionError("initialize Configs workspace", err)
	}

	plan := filepath.Join(o.paths.root, "deployment.tfplan")
	err := o.tofu(
		ctx,
		"plan",
		"-input=false",
		"-no-color",
		lockTimeout,
		"-detailed-exitcode",
		"-out="+plan,
	)
	changed := errors.Is(err, processStatusError(changesPending))
	if err != nil && !changed {
		result := errors.Join(executionError("plan Configs workspace", err), o.publishPlan())
		if ctx.Err() == nil && errors.Is(err, processStatusError(planFailed)) {
			result = errors.Join(result, o.failPlan())
		}

		return result
	}

	if logErr := o.publishPlan(); logErr != nil {
		return logErr
	}

	if !changed {
		return nil
	}

	if err := o.verify(ctx); err != nil {
		return executionError("verify Configs source before applying its saved plan", err)
	}

	if err := o.tofu(
		ctx,
		"apply",
		"-input=false",
		"-no-color",
		"-auto-approve",
		lockTimeout,
		plan,
	); err != nil {
		return executionError("apply saved Configs plan", err)
	}

	return nil
}

func (o operation) tofu(ctx context.Context, arguments ...string) error {
	return o.run(
		ctx,
		command{
			name:   configsctl,
			args:   append([]string{"tofu", o.selected.selector}, arguments...),
			output: o.output,
		},
	)
}

func publishPlanLog(tmp string, output io.Writer) error {
	files, err := filepath.Glob(filepath.Join(tmp, "configs-runs", "tofu-plan-*.log"))
	if err != nil {
		return executionError("locate the redacted plan log", err)
	}

	if len(files) != 1 {
		slog.Error(
			"Expected one redacted Configs plan log",
			"files",
			len(files),
			"err",
			errMissingPlanLog,
		)

		return errMissingPlanLog
	}

	file, err := os.Open(files[0])
	if err != nil {
		return executionError("open the redacted Configs plan log", err)
	}

	guarded := &subprocessOutput{destination: output}
	_, err = io.Copy(guarded, file)
	if err := errors.Join(err, file.Close(), guarded.finish()); err != nil {
		return executionError("publish the redacted Configs plan log", err)
	}

	return nil
}

func gitOutput(ctx context.Context, git commandRunner, arguments ...string) (string, error) {
	var output strings.Builder
	if err := git(
		ctx,
		command{name: "/usr/bin/git", args: arguments, output: &output},
	); err != nil {
		return "", executionError("git "+arguments[0], err)
	}

	return strings.TrimSpace(output.String()), nil
}

var (
	errMissingJobIdentity = errors.New(
		"the exact Terrakube job and workspace identity is required",
	)
	errMissingPlanLog     = errors.New("expected one redacted Configs plan log")
	errUnsupportedProcess = errors.New("unsupported executor process")
	errSelectorRequired   = errors.New("provide one enrolled Configs workspace selector")
	errWorkspaceAndBranch = errors.New(
		"configured workspace and promotion branch do not match the Terrakube deployment template",
	)
	errUntrustedSource = errors.New(
		"select the trusted Configs repository as the Terrakube source",
	)
	errMissingVCSReadToken = errors.New("the Configs VCS read token is required")
	errCheckoutChanged     = errors.New("the Configs checkout changed after planning")
)

func executionError(operation string, err error) error {
	slog.Error("Terrakube deployment operation failed", "operation", operation, "err", err)

	return fmt.Errorf("%s: %w", operation, err)
}
