package actions

import (
	"context"
	"fmt"
	"os/exec"
	"time"
)

const maxOutput = 20_000

type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}
type commandRunner struct{}

func (commandRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

type Executor struct{ runner Runner }

func NewExecutor() Executor                   { return Executor{runner: commandRunner{}} }
func NewExecutorWithRunner(r Runner) Executor { return Executor{runner: r} }

func (e Executor) Execute(action Action) Result {
	var name string
	var args []string
	var timeout time.Duration
	switch action.Kind {
	case "apply_security_updates":
		name, args, timeout = "unattended-upgrade", []string{"--verbose"}, 30*time.Minute
	case "apply_all_updates":
		// Upgrades ALL installed packages, not only the security origins: it is the only action
		// that brings the `updates_available` counter down to zero (apt-check counts everything,
		// while unattended-upgrade only touches -security).
		//
		// `upgrade` and not `dist-upgrade`: it upgrades what is already installed without installing
		// or removing packages, so it does not pull in new kernels nor change the configuration.
		//
		// It goes through `env` because Runner has no environment variables: DEBIAN_FRONTEND keeps
		// apt from opening interactive prompts on a channel with nobody on the other end, and
		// --force-confold keeps the configuration files already on the host instead of asking.
		//
		// No `apt-get update` first: it installs exactly what apt-check counted on the same cache,
		// so the number shown on the page and the effect of the action match. The system apt-daily
		// timer refreshes the cache.
		name = "env"
		args = []string{"DEBIAN_FRONTEND=noninteractive", "apt-get", "-y", "-o", "Dpkg::Options::=--force-confold", "upgrade"}
		timeout = 30 * time.Minute
	case "reboot":
		name, args, timeout = "systemctl", []string{"reboot"}, 30*time.Second
	default:
		return Result{Status: "failed", ExitCode: -1, Error: "unsupported action kind"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := e.runner.Run(ctx, name, args...)
	if len(out) > maxOutput {
		out = out[:maxOutput]
	}
	if err != nil {
		return Result{Status: "failed", ExitCode: exitCode(err), Output: string(out), Error: err.Error()}
	}
	return Result{Status: "succeeded", ExitCode: 0, Output: string(out)}
}

func exitCode(err error) int {
	if value, ok := err.(*exec.ExitError); ok {
		return value.ExitCode()
	}
	return -1
}

func (e Executor) String() string { return fmt.Sprintf("Executor(%T)", e.runner) }
