package lab

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Assignment gives a queued job its devices (role -> device ID).
type Assignment struct {
	Job     string
	Devices map[string]string
}

// Plan decides which queued jobs start now and on which devices. It does not change the state;
// the caller turns each assignment into an EvAssign event.
//
// Jobs are considered by priority, then by their team's fair share (running jobs divided by the
// team's weight), then by age. A job may use a device only if the device is available and, for
// the job's whole timeout, either inside the job's own reservation or outside every reservation.
// If the job at the front has waited longer than starveAfter and still cannot start, the devices
// it could use are held for it, so that a stream of smaller jobs cannot postpone it forever.
func (s *State) Plan(now time.Time, starveAfter time.Duration) []Assignment {
	queued := s.queueOrder()
	taken := map[string]bool{}
	for _, d := range s.Devices {
		if d.Job != "" {
			taken[d.ID] = true
		}
	}
	var held map[string]bool
	var out []Assignment
	for _, j := range queued {
		eligible := func(d *Device) bool {
			return d.Status == Available && !taken[d.ID] && !held[d.ID] && s.windowFree(j, d.ID, now)
		}
		if a, ok := Match(j.Roles, j.Cables, s.DeviceList(), s.Connected, eligible); ok {
			out = append(out, Assignment{Job: j.ID, Devices: a})
			for _, id := range a {
				taken[id] = true
			}
			continue
		}
		if held == nil && now.Sub(j.Submitted) >= starveAfter {
			held = map[string]bool{}
			for _, d := range s.Devices {
				if d.Status == Available && s.windowFree(j, d.ID, now) && s.servesSomeRole(j, d) {
					held[d.ID] = true
				}
			}
		}
	}
	return out
}

func (s *State) servesSomeRole(j *Job, d *Device) bool {
	for _, r := range j.Roles {
		if (r.Vendor == "" || r.Vendor == d.Vendor) && d.Has(r.Capabilities) {
			return true
		}
	}
	return false
}

// queueOrder returns queued jobs in the order the scheduler considers them.
func (s *State) queueOrder() []*Job {
	running := map[string]int{}
	var queued []*Job
	for _, j := range s.JobList() {
		switch j.State {
		case Running:
			running[j.Team]++
		case Queued:
			queued = append(queued, j)
		}
	}
	share := func(team string) float64 {
		w := 1
		if t, ok := s.Teams[team]; ok && t.Weight > 0 {
			w = t.Weight
		}
		return float64(running[team]) / float64(w)
	}
	slices.SortStableFunc(queued, func(a, b *Job) int {
		if c := cmp.Compare(b.Priority, a.Priority); c != 0 {
			return c
		}
		if c := cmp.Compare(share(a.Team), share(b.Team)); c != 0 {
			return c
		}
		return a.Submitted.Compare(b.Submitted)
	})
	return queued
}

// QueuePosition returns a queued job's 1-based place in scheduling order, or 0.
func (s *State) QueuePosition(id string) int {
	for i, j := range s.queueOrder() {
		if j.ID == id {
			return i + 1
		}
	}
	return 0
}

// windowFree reports whether job j may hold device id from now until its timeout.
func (s *State) windowFree(j *Job, id string, now time.Time) bool {
	end := now.Add(j.Timeout())
	if j.Reservation != "" {
		r := s.Reservations[j.Reservation]
		return r != nil && !r.Cancelled && slices.Contains(r.Devices, id) && !now.Before(r.Start) && !end.After(r.End)
	}
	for _, r := range s.Reservations {
		if slices.Contains(r.Devices, id) && r.Overlaps(now, end) {
			return false
		}
	}
	return true
}

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidateJob checks a job spec against the lab. It maps fields to problems; an empty map means
// the job is valid and some set of lab devices can run it.
func (s *State) ValidateJob(spec JobSpec, knownTests func(string) bool) map[string]string {
	p := map[string]string{}
	if !namePattern.MatchString(spec.Name) {
		p["name"] = "Use 1 to 64 lowercase letters, numbers, dots, dashes or underscores."
	}
	if _, ok := s.Teams[spec.Team]; !ok {
		p["team"] = "Choose one of the lab's teams."
	}
	if spec.Priority < 0 || spec.Priority > 2 {
		p["priority"] = "Choose low, normal or high."
	}
	if spec.TimeoutSec < 10 || spec.TimeoutSec > 24*3600 {
		p["timeoutSec"] = "Enter a timeout from 10 seconds to 24 hours."
	}
	if len(spec.Tests) == 0 || len(spec.Tests) > 10 {
		p["tests"] = "Choose 1 to 10 tests."
	}
	for _, t := range spec.Tests {
		if !knownTests(t) {
			p["tests"] = fmt.Sprintf("Unknown test %q.", t)
		}
	}
	names := map[string]bool{}
	if len(spec.Roles) == 0 || len(spec.Roles) > 8 {
		p["roles"] = "A job needs 1 to 8 device roles."
	}
	for _, r := range spec.Roles {
		if !namePattern.MatchString(r.Name) || names[r.Name] {
			p["roles"] = fmt.Sprintf("Role names must be unique lowercase names; %q is not.", r.Name)
		}
		names[r.Name] = true
	}
	for _, c := range spec.Cables {
		if !names[c.A] || !names[c.B] || c.A == c.B {
			p["cables"] = fmt.Sprintf("Cable %s-%s must join two different roles of this job.", c.A, c.B)
		}
	}
	if spec.Reservation != "" {
		if r, ok := s.Reservations[spec.Reservation]; !ok || r.Cancelled {
			p["reservation"] = "Choose an active reservation."
		}
	}
	if _, bad := p["roles"]; !bad {
		if _, bad := p["cables"]; !bad {
			usable := func(d *Device) bool { return d.Status != Maintenance }
			if spec.Reservation != "" {
				if r := s.Reservations[spec.Reservation]; r != nil {
					usable = func(d *Device) bool { return slices.Contains(r.Devices, d.ID) }
				}
			}
			if _, ok := Match(spec.Roles, spec.Cables, s.DeviceList(), s.Connected, usable); !ok {
				p["roles"] = "No set of lab devices has these vendors, capabilities and cables."
			}
		}
	}
	return p
}

// ValidateReservation checks a reservation request. It maps fields to problems.
func (s *State) ValidateReservation(r Reservation, now time.Time) map[string]string {
	p := map[string]string{}
	if strings.TrimSpace(r.Owner) == "" || len(r.Owner) > 64 {
		p["owner"] = "Enter who the reservation is for (up to 64 characters)."
	}
	if len(r.Purpose) > 200 {
		p["purpose"] = "Keep the purpose under 200 characters."
	}
	if r.Start.Before(now.Add(-time.Minute)) {
		p["start"] = "Start now or later."
	}
	if !r.End.After(r.Start) || r.End.Sub(r.Start) > 7*24*time.Hour {
		p["duration"] = "Reserve for at least a minute and at most 7 days."
	}
	if len(r.Devices) == 0 {
		p["devices"] = "Choose at least one device."
	}
	var conflicts []string
	for _, id := range r.Devices {
		d, ok := s.Devices[id]
		if !ok {
			p["devices"] = fmt.Sprintf("There is no device %q.", id)
			continue
		}
		for _, other := range s.ReservationList() {
			if slices.Contains(other.Devices, id) && other.Overlaps(r.Start, r.End) {
				conflicts = append(conflicts, fmt.Sprintf("%s is reserved by %s until %s", id, other.Owner, other.End.Format("15:04 MST")))
			}
		}
		if d.Job != "" {
			if j := s.Jobs[d.Job]; j != nil && r.Start.Before(j.Started.Add(j.Timeout())) {
				conflicts = append(conflicts, fmt.Sprintf("%s may be running job %s until %s", id, j.ID, j.Started.Add(j.Timeout()).Format("15:04 MST")))
			}
		}
	}
	if len(conflicts) > 0 {
		p["devices"] = "Those times conflict: " + strings.Join(conflicts, "; ") + "."
	}
	return p
}
