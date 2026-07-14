// Package setup implements the host-bound phases shared by CLI and API setup.
package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/VikashLoomba/Portal/internal/app"
	"github.com/VikashLoomba/Portal/internal/config"
	"github.com/VikashLoomba/Portal/internal/doctorprobe"
	"github.com/VikashLoomba/Portal/pkg/api"
	"github.com/VikashLoomba/Portal/pkg/doctor"
	"github.com/VikashLoomba/Portal/pkg/run"
	"github.com/VikashLoomba/Portal/pkg/transport"
	"github.com/VikashLoomba/Portal/pkg/transport/sshctl"
)

// Sink receives setup phase events in emission order.
type Sink func(api.SetupEvent)

type validator interface {
	Validate(ctx context.Context, host string, stderrW io.Writer) error
	HasSS(ctx context.Context, host string) bool
}

var setupRunID atomic.Uint64

// Runner owns one setup run and its dedicated host-bound transport.
type Runner struct {
	paths     app.Paths
	cfg       *config.Store
	sink      Sink
	runner    run.Runner
	setupSock string

	tr           transport.Transport
	transportErr error
	validator    validator

	newTransport func(context.Context, string) (transport.Transport, error)
	newValidator func() validator
	doctor       func(context.Context, string, transport.Transport) *doctor.Report
}

// NormalizeHost strips all whitespace, matching the persisted host format.
func NormalizeHost(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		switch r {
		case ' ', '\t', '\n', '\r', '\v', '\f':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// New creates a Runner for one setup run.
func New(paths app.Paths, cfg *config.Store, sink Sink) *Runner {
	r := &Runner{
		paths:     paths,
		cfg:       cfg,
		sink:      sink,
		runner:    run.OSRunner{},
		setupSock: filepath.Join(paths.ConfigDir, fmt.Sprintf("setup-cm-%d-%d.sock", os.Getpid(), setupRunID.Add(1))),
		doctor:    doctorprobe.Run,
	}
	r.newTransport = r.defaultNewTransport
	r.newValidator = func() validator {
		return sshctl.New(r.setupSock, "", app.SSHOpts, r.runner)
	}
	return r
}

func (r *Runner) emit(ev api.SetupEvent) {
	if r.sink != nil {
		r.sink(ev)
	}
}

func errorDetail(code string, err error) *api.ErrorDetail {
	return &api.ErrorDetail{Code: code, Message: err.Error()}
}

func (r *Runner) setupValidator() validator {
	if r.validator == nil {
		r.validator = r.newValidator()
	}
	return r.validator
}

func (r *Runner) defaultNewTransport(ctx context.Context, host string) (transport.Transport, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	paths := r.paths
	paths.Sock = r.setupSock
	tr, _, err := app.NewTransportContext(ctx, paths, host, r.runner, r.cfg, nil)
	return tr, err
}

func (r *Runner) setupTransport(ctx context.Context, host string) (transport.Transport, error) {
	if r.tr != nil {
		return r.tr, r.transportErr
	}
	// A live system-ssh master does not verify that an existing ControlPath
	// belongs to host, so no setup run may inherit a prior run's socket.
	if err := os.Remove(r.setupSock); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	tr, err := r.newTransport(ctx, host)
	if err != nil {
		return nil, err
	}
	// Setup owns this transport, so system ssh must establish the dedicated
	// master before any ssh -S operation can use it. Cache establishment errors
	// with the transport so a live failure is surfaced once and Close can still
	// release partially-created native or system resources.
	_, err = tr.Ensure(ctx)
	r.tr = tr
	r.transportErr = err
	return tr, err
}

type stderrRelay struct {
	r *Runner
}

func (w stderrRelay) Write(p []byte) (int, error) {
	if len(p) > 0 {
		w.r.emit(api.SetupEvent{Step: "validate", Status: "running", Line: string(p)})
	}
	return len(p), nil
}

// Validate checks direct SSH reachability and required remote tooling.
func (r *Runner) Validate(ctx context.Context, host string, force bool) bool {
	if ctx.Err() != nil {
		return false
	}
	r.emit(api.SetupEvent{Step: "validate", Status: "running"})
	v := r.setupValidator()
	err := v.Validate(ctx, host, stderrRelay{r: r})
	if ctx.Err() != nil {
		return false
	}
	if err != nil {
		status := "fail"
		if force {
			status = "warn"
		}
		r.emit(api.SetupEvent{
			Step:   "validate",
			Status: status,
			Error:  errorDetail("validation_failed", err),
		})
		return force
	}
	if !v.HasSS(ctx, host) {
		if ctx.Err() != nil {
			return false
		}
		r.emit(api.SetupEvent{
			Step:   "validate",
			Status: "running",
			Line:   "WARNING: '" + host + "' is reachable but has no 'ss' command — is it Linux? Port discovery may not work.\n",
		})
		r.emit(api.SetupEvent{Step: "validate", Status: "warn"})
		return true
	}
	r.emit(api.SetupEvent{Step: "validate", Status: "ok"})
	return true
}

// Configure creates setup-owned local directories and persists host intent.
func (r *Runner) Configure(ctx context.Context, host string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.emit(api.SetupEvent{Step: "configure", Status: "running"})
	for _, dir := range []string{r.paths.ConfigDir, r.paths.BinDir, filepath.Dir(r.paths.Log)} {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			r.emit(api.SetupEvent{Step: "configure", Status: "fail", Error: errorDetail("configure_failed", err)})
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.cfg.WriteHost(host); err != nil {
		r.emit(api.SetupEvent{Step: "configure", Status: "fail", Error: errorDetail("configure_failed", err)})
		return err
	}
	r.emit(api.SetupEvent{Step: "configure", Status: "ok"})
	return nil
}

// Verify runs the shared remote doctor probe over the setup transport.
func (r *Runner) Verify(ctx context.Context, host string) *doctor.Report {
	tr, err := r.setupTransport(ctx, host)
	if ctx.Err() != nil {
		return &doctor.Report{Host: host}
	}
	r.emit(api.SetupEvent{Step: "doctor", Status: "running"})
	var rep *doctor.Report
	if err != nil {
		rep = &doctor.Report{Host: host}
		rep.Add("ssh master", doctor.Fail, err.Error())
	} else {
		rep = r.doctor(ctx, host, tr)
	}
	if ctx.Err() != nil {
		return rep
	}
	raw, _ := json.Marshal(rep)
	r.emit(api.SetupEvent{Step: "doctor", Status: "ok", Report: raw})
	return rep
}

// Close tears down the setup transport and unlinks its dedicated socket.
func (r *Runner) Close(ctx context.Context) {
	if r.tr != nil {
		_, _ = r.tr.Close(ctx)
		r.tr = nil
		r.transportErr = nil
	}
	_ = os.Remove(r.setupSock)
}
