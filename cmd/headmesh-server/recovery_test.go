package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/headmesh-dev/headmesh/api"
	"github.com/headmesh-dev/headmesh/internal/service"
)

type recoveryStub struct {
	sweep func() error
	wait  func() error
}

func (s recoveryStub) SweepOverdueGrants(context.Context) (int, error) { return 0, s.sweep() }

func (s recoveryStub) AwaitCellsCurrent(context.Context, service.RecoveryHub, *slog.Logger) error {
	return s.wait()
}

func TestRecoveryRetriesAfterCellTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	gate := &api.Recovery{}
	gate.Begin()
	sweeps, attempts := 0, 0
	svc := recoveryStub{
		sweep: func() error { sweeps++; return nil },
		wait: func() error {
			if !gate.Active() {
				t.Fatal("gate opened before the cells confirmed their policies")
			}
			attempts++
			if attempts == 1 {
				return context.DeadlineExceeded
			}
			return nil
		},
	}
	recoverControlPlane(ctx, svc, nil, gate, time.Second, slog.New(slog.DiscardHandler))
	if gate.Active() || attempts != 2 || sweeps != 2 {
		t.Fatalf("gate active=%v after %d sweep(s) and %d cell wait(s)", gate.Active(), sweeps, attempts)
	}
}

func TestRecoveryGate(t *testing.T) {
	for _, failure := range []string{"", "sweep", "cell-timeout"} {
		name := failure
		if name == "" {
			name = "success"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			gate := &api.Recovery{}
			gate.Begin()
			svc := recoveryStub{
				sweep: func() error {
					if failure == "sweep" {
						cancel()
						return errors.New("sweep unavailable")
					}
					return nil
				},
				wait: func() error {
					if failure == "sweep" {
						t.Fatal("recovery waited for cells before a successful sweep")
					}
					if failure == "cell-timeout" {
						cancel()
						return context.DeadlineExceeded
					}
					return nil
				},
			}
			recoverControlPlane(ctx, svc, nil, gate, time.Second, slog.New(slog.DiscardHandler))
			if gate.Active() != (failure != "") {
				t.Fatalf("gate active = %v after %s", gate.Active(), name)
			}
		})
	}
}
