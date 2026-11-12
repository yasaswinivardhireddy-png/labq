// Package server runs a lab: it schedules jobs onto devices, runs their qualification tests,
// records every change in a write-ahead log, reports to GitHub, and serves an API and dashboard.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yasaswinivardhireddy-png/labq/internal/device"
	"github.com/yasaswinivardhireddy-png/labq/internal/integrations"
	"github.com/yasaswinivardhireddy-png/labq/internal/lab"
	"github.com/yasaswinivardhireddy-png/labq/internal/qual"
	"github.com/yasaswinivardhireddy-png/labq/internal/wal"
)

// Options configure a Service.
type Options struct {
	Config        Config
	WALPath       string
	GitHub        *integrations.GitHub // nil disables commit statuses
	WebhookSecret []byte               // empty disables the GitHub webhook
	WebhookPreset string               // preset for GitHub-triggered jobs
	WebhookTeam   string               // team for GitHub-triggered jobs
	BaseURL       string               // public URL of the dashboard, for links in commit statuses
	StarveAfter   time.Duration        // default 15 minutes
	TestPoll      time.Duration        // how often tests re-check a condition
	TestWait      time.Duration        // how long tests wait for a condition
	SimConverge   time.Duration        // BGP convergence time of simulated devices (default 2 s)
	Logger        *slog.Logger
	Now           func() time.Time
}

// Service is a running lab.
type Service struct {
	opts    Options
	log     *slog.Logger
	now     func() time.Time
	mu      sync.Mutex
	state   *lab.State
	wal     *wal.Log
	drivers map[string]device.Device
	cancels map[string]context.CancelFunc
	wake    chan struct{}
	workers sync.WaitGroup
	ctx     context.Context
	stop    context.CancelFunc
	loop    sync.WaitGroup
	broken  error // set when the log cannot be written; scheduling stops
}

// Open replays the write-ahead log, reconciles the lab configuration, and requeues jobs that were
// running when the previous process stopped (their workers died with it).
func Open(opts Options) (*Service, error) {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.StarveAfter == 0 {
		opts.StarveAfter = 15 * time.Minute
	}
	w, recs, err := wal.Open(opts.WALPath)
	if err != nil {
		return nil, err
	}
	s := &Service{
		opts: opts, log: opts.Logger, now: opts.Now, state: lab.New(), wal: w,
		drivers: opts.Config.drivers(opts.SimConverge), cancels: map[string]context.CancelFunc{}, wake: make(chan struct{}, 1),
	}
	for _, r := range recs {
		var e lab.Event
		if err := json.Unmarshal(r, &e); err != nil {
			w.Close()
			return nil, fmt.Errorf("replaying %s: %w", opts.WALPath, err)
		}
		if err := s.state.Apply(e); err != nil {
			w.Close()
			return nil, fmt.Errorf("replaying %s: %w", opts.WALPath, err)
		}
	}
	s.log.Info("replayed write-ahead log", "events", len(recs), "jobs", len(s.state.Jobs))

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reconcile(); err != nil {
		w.Close()
		return nil, err
	}
	for _, j := range s.state.JobList() {
		if j.State == lab.Running {
			if _, err := s.emit(lab.EvRequeue, lab.FinishData{Job: j.ID, Message: "lease lost: labq restarted while the job was running"}); err != nil {
				w.Close()
				return nil, err
			}
		}
	}
	return s, nil
}

// reconcile brings the inventory, cabling and teams in the state up to date with the config.
func (s *Service) reconcile() error {
	for _, d := range s.opts.Config.Devices {
		cur, ok := s.state.Devices[d.ID]
		if ok && cur.Vendor == d.Vendor && cur.Model == d.Model && slices.Equal(cur.Capabilities, d.Capabilities) {
			continue
		}
		if _, err := s.emit(lab.EvDevice, lab.Device{ID: d.ID, Vendor: d.Vendor, Model: d.Model, Capabilities: d.Capabilities}); err != nil {
			return err
		}
	}
	for _, l := range s.opts.Config.Links {
		if !slices.Contains(s.state.Links, l) {
			if _, err := s.emit(lab.EvLink, l); err != nil {
				return err
			}
		}
	}
	for _, t := range s.opts.Config.Teams {
		if cur, ok := s.state.Teams[t.Name]; !ok || cur.Weight != t.Weight {
			if _, err := s.emit(lab.EvTeam, t); err != nil {
				return err
			}
		}
	}
	return nil
}

// emit applies an event and appends it to the log. The caller holds s.mu.
func (s *Service) emit(typ string, data any) (lab.Event, error) {
	if s.broken != nil {
		return lab.Event{}, s.broken
	}
	e := lab.NewEvent(s.now(), typ, data)
	e.Seq = s.state.Seq + 1
	if err := s.state.Apply(e); err != nil {
		return e, err
	}
	raw, _ := json.Marshal(e)
	if err := s.wal.Append(raw); err != nil {
		s.broken = fmt.Errorf("write-ahead log failed, scheduling stopped: %w", err)
		s.log.Error("write-ahead log failed", "error", err)
		return e, s.broken
	}
	return e, nil
}

// Start runs the scheduler until Close.
func (s *Service) Start() {
	s.ctx, s.stop = context.WithCancel(context.Background())
	s.loop.Add(1)
	go func() {
		defer s.loop.Done()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			s.schedule()
			select {
			case <-s.ctx.Done():
				return
			case <-s.wake:
			case <-t.C:
			}
		}
	}()
}

// Close stops scheduling, cancels running jobs, waits for their workers and closes the log.
func (s *Service) Close() error {
	if s.stop != nil {
		s.stop()
		s.loop.Wait()
	}
	s.workers.Wait()
	return s.wal.Close()
}

func (s *Service) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) schedule() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broken != nil || s.ctx.Err() != nil {
		return
	}
	for _, a := range s.state.Plan(s.now(), s.opts.StarveAfter) {
		if _, err := s.emit(lab.EvAssign, lab.AssignData{Job: a.Job, Assignment: a.Devices}); err != nil {
			s.log.Error("assign", "job", a.Job, "error", err)
			return
		}
		j := s.state.Jobs[a.Job]
		ctx, cancel := context.WithTimeout(s.ctx, j.Timeout())
		s.cancels[j.ID] = cancel
		env := s.env(j)
		s.workers.Add(1)
		go s.work(ctx, cancel, j.ID, env, slices.Clone(j.Tests), j.Source)
	}
}

// env builds the test environment for a job that was just assigned. The caller holds s.mu.
func (s *Service) env(j *lab.Job) *qual.Env {
	env := &qual.Env{Members: map[string]qual.Member{}, Poll: s.opts.TestPoll, Wait: s.opts.TestWait}
	for _, r := range j.Roles {
		id := j.Assignment[r.Name]
		env.Members[r.Name] = qual.Member{Device: id, Driver: s.drivers[id], Capabilities: slices.Clone(s.state.Devices[id].Capabilities)}
	}
	for _, c := range j.Cables {
		if l, ok := s.state.LinkBetween(j.Assignment[c.A], j.Assignment[c.B]); ok {
			env.Wires = append(env.Wires, qual.Wire{A: c.A, B: c.B, Link: l})
		}
	}
	return env
}

func (s *Service) work(ctx context.Context, cancel context.CancelFunc, id string, env *qual.Env, tests []string, src *lab.Source) {
	defer s.workers.Done()
	defer cancel()
	devs := memberList(env)
	s.status(src, "pending", "Running qualification on "+devs, id)
	state, msg, infra := qual.Run(ctx, env, tests, func(r lab.TestResult) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, err := s.emit(lab.EvResult, lab.ResultData{Job: id, Result: r}); err != nil {
			s.log.Error("record result", "job", id, "error", err)
		}
	})
	if errors.Is(ctx.Err(), context.Canceled) && s.ctx.Err() != nil {
		// labq is shutting down: leave the job running in the log; the next start requeues it.
		return
	}

	s.mu.Lock()
	delete(s.cancels, id)
	j := s.state.Jobs[id]
	requeue := state == lab.Errored && j.Attempts < lab.MaxAttempts
	var err error
	if requeue {
		_, err = s.emit(lab.EvRequeue, lab.FinishData{Job: id, Message: msg, Infra: infra})
	} else {
		if state == lab.Errored {
			msg = fmt.Sprintf("%s (gave up after %d attempts)", msg, j.Attempts)
		}
		_, err = s.emit(lab.EvFinish, lab.FinishData{Job: id, State: state, Message: msg, Infra: infra})
	}
	s.mu.Unlock()
	if err != nil {
		s.log.Error("finish", "job", id, "error", err)
	}
	s.log.Info("job attempt finished", "job", id, "state", state, "requeued", requeue, "message", msg)
	switch {
	case requeue:
		s.status(src, "pending", "Retrying on other devices: "+msg, id)
	case state == lab.Passed:
		s.status(src, "success", msg+" on "+devs, id)
	case state == lab.Failed:
		s.status(src, "failure", msg+" on "+devs, id)
	default:
		s.status(src, "error", string(state)+": "+msg, id)
	}
	s.poke()
}

func memberList(env *qual.Env) string {
	var parts []string
	for _, m := range env.Members {
		parts = append(parts, m.Device)
	}
	slices.Sort(parts)
	return strings.Join(parts, ", ")
}

// status reports a job's progress to GitHub when GitHub triggered it.
func (s *Service) status(src *lab.Source, state, description, jobID string) {
	if src == nil || s.opts.GitHub == nil || src.Repo == "" {
		return
	}
	target := ""
	if s.opts.BaseURL != "" {
		target = strings.TrimRight(s.opts.BaseURL, "/") + "/jobs/" + jobID
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.opts.GitHub.SetStatus(ctx, src.Repo, src.SHA, state, description, target); err != nil {
		s.log.Warn("github status", "job", jobID, "state", state, "error", err)
		return
	}
	s.log.Info("github status", "job", jobID, "repo", src.Repo, "sha", src.SHA, "state", state)
}

// ErrNotFound is returned for unknown jobs, devices and reservations.
var ErrNotFound = errors.New("not found")

// ValidationError lists what is wrong with a request, by field.
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string {
	var parts []string
	for k, v := range e.Fields {
		parts = append(parts, k+": "+v)
	}
	slices.Sort(parts)
	return strings.Join(parts, "; ")
}

// Submit queues a job.
func (s *Service) Submit(spec lab.JobSpec) (*lab.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.state.ValidateJob(spec, qual.Known); len(p) > 0 {
		return nil, &ValidationError{p}
	}
	id := fmt.Sprintf("j-%d", len(s.state.Jobs)+1)
	if _, err := s.emit(lab.EvSubmit, lab.SubmitData{ID: id, Spec: spec}); err != nil {
		return nil, err
	}
	s.poke()
	return s.state.Jobs[id], nil
}

// FromPreset builds a job spec from a preset.
func (s *Service) FromPreset(name, preset, team string, priority int, reservation string) (lab.JobSpec, error) {
	p, ok := s.opts.Config.Presets[preset]
	if !ok {
		return lab.JobSpec{}, &ValidationError{map[string]string{"preset": "Choose one of the presets."}}
	}
	return lab.JobSpec{
		Name: name, Team: team, Priority: priority, Roles: p.Roles, Cables: p.Cables, Tests: p.Tests,
		TimeoutSec: p.TimeoutSec, Reservation: reservation,
	}, nil
}

// Cancel cancels a queued or running job.
func (s *Service) Cancel(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.state.Jobs[id]
	if !ok {
		return ErrNotFound
	}
	switch j.State {
	case lab.Queued:
		_, err := s.emit(lab.EvCancel, lab.CancelData{Job: id, Reason: "cancelled by a user"})
		return err
	case lab.Running:
		s.cancels[id]()
		return nil
	}
	return fmt.Errorf("job %s already %s", id, j.State)
}

// Reserve holds devices for a time window.
func (s *Service) Reserve(r lab.Reservation) (*lab.Reservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.state.ValidateReservation(r, s.now()); len(p) > 0 {
		return nil, &ValidationError{p}
	}
	r.ID = fmt.Sprintf("r-%d", len(s.state.Reservations)+1)
	r.Start, r.End = r.Start.UTC(), r.End.UTC()
	if _, err := s.emit(lab.EvReserve, r); err != nil {
		return nil, err
	}
	s.poke()
	return s.state.Reservations[r.ID], nil
}

// Unreserve cancels a reservation.
func (s *Service) Unreserve(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.state.Reservations[id]
	if !ok {
		return ErrNotFound
	}
	if r.Cancelled {
		return nil
	}
	_, err := s.emit(lab.EvUnreserve, lab.UnreserveData{ID: id})
	s.poke()
	return err
}

// SetDeviceStatus puts a device back in service, in maintenance, or in quarantine.
func (s *Service) SetDeviceStatus(id string, status lab.DeviceStatus, note string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state.Devices[id]; !ok {
		return ErrNotFound
	}
	switch status {
	case lab.Available, lab.Maintenance, lab.Quarantined:
	default:
		return &ValidationError{map[string]string{"status": "Use available, maintenance or quarantined."}}
	}
	_, err := s.emit(lab.EvDeviceStatus, lab.DeviceStatusData{Device: id, Status: status, Note: note})
	s.poke()
	return err
}

// Webhook handles a verified GitHub delivery. It returns the job it queued, or nil when the
// event is one LabQ ignores.
func (s *Service) Webhook(event string, body []byte) (*lab.Job, error) {
	src, ok, err := integrations.ParseWebhook(event, body)
	if err != nil || !ok {
		return nil, err
	}
	name := fmt.Sprintf("%s-%s", src.Kind, src.SHA[:7])
	if src.PR > 0 {
		name = fmt.Sprintf("pr-%d-%s", src.PR, src.SHA[:7])
	}
	spec, err := s.FromPreset(strings.ReplaceAll(name, "_", "-"), s.opts.WebhookPreset, s.opts.WebhookTeam, 1, "")
	if err != nil {
		return nil, err
	}
	spec.Source = &src
	// The worker posts "pending" when the job starts; posting here as well could race it and
	// leave an out-of-order status on the commit.
	return s.Submit(spec)
}

// View runs f with the lab state locked. f must not keep references after it returns.
func (s *Service) View(f func(st *lab.State)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s.state)
}

// Healthy reports nil unless the write-ahead log failed.
func (s *Service) Healthy() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.broken
}
