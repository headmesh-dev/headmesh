// Package dockerx runs the docker CLI for the host commands.
package dockerx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Docker invokes the docker binary.
type Docker struct {
	Bin    string
	Stdout io.Writer
	Stderr io.Writer
}

func (d Docker) bin() string {
	if d.Bin == "" {
		return "docker"
	}
	return d.Bin
}

// Path resolves the docker binary.
func (d Docker) Path() (string, error) {
	p, err := exec.LookPath(d.bin())
	if err != nil {
		return "", fmt.Errorf("docker CLI not found: %w", err)
	}
	return p, nil
}

// Run streams output; extra env is appended to the process environment.
func (d Docker) Run(ctx context.Context, env []string, args ...string) error {
	cmd := exec.CommandContext(ctx, d.bin(), args...) //nolint:gosec // G204: docker with argv
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = d.Stdout, d.Stderr
	if cmd.Stdout == nil {
		cmd.Stdout = os.Stdout
	}
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker %s: %w", strings.Join(args[:min(len(args), 3)], " "), err)
	}
	return nil
}

// Output captures stdout.
func (d Docker) Output(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, d.bin(), args...) //nolint:gosec // G204: docker with argv
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("docker %s: %w: %s", strings.Join(args[:min(len(args), 3)], " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// State is a container's state.
type State struct {
	Exists    bool      `json:"exists"`
	Running   bool      `json:"running"`
	Status    string    `json:"status"`
	Health    string    `json:"health,omitempty"`
	StartedAt time.Time `json:"started_at,omitzero"`
	Image     string    `json:"image,omitempty"`
}

// Inspect returns the state of a container (Exists=false if missing).
func (d Docker) Inspect(ctx context.Context, name string) (State, error) {
	out, err := d.Output(ctx, "inspect", "--type", "container", "--format", "{{json .State}}|{{.Config.Image}}", name)
	if err != nil {
		if strings.Contains(err.Error(), "No such") || strings.Contains(err.Error(), "no such") {
			return State{}, nil
		}
		return State{}, err
	}
	raw, img, _ := strings.Cut(strings.TrimSpace(out), "|")
	var s struct {
		Status    string
		Running   bool
		StartedAt time.Time
		Health    *struct{ Status string }
	}
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return State{}, fmt.Errorf("parse docker inspect: %w", err)
	}
	st := State{Exists: true, Running: s.Running, Status: s.Status, StartedAt: s.StartedAt, Image: img}
	if s.Health != nil {
		st.Health = s.Health.Status
	}
	return st, nil
}

// DataRoot returns Docker's data-root (empty if unknown).
func (d Docker) DataRoot(ctx context.Context) string {
	out, err := d.Output(ctx, "info", "--format", "{{.DockerRootDir}}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// Exec runs a command in a container and captures stdout and stderr.
func (d Docker) Exec(ctx context.Context, container, user string, argv ...string) (string, string, int, error) {
	args := []string{"exec"}
	if user != "" {
		args = append(args, "-u", user)
	}
	args = append(append(args, container), argv...)
	cmd := exec.CommandContext(ctx, d.bin(), args...) //nolint:gosec // G204: docker with argv
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return out.String(), errb.String(), -1, fmt.Errorf("docker exec: %w", err)
		}
		code = ee.ExitCode()
	}
	return out.String(), errb.String(), code, nil
}
