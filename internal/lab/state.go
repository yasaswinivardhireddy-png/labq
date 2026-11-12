package lab

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// State is the whole lab. Only Apply changes it.
type State struct {
	Devices      map[string]*Device
	DeviceOrder  []string
	Links        []Link
	Teams        map[string]*Team
	TeamOrder    []string
	Jobs         map[string]*Job
	JobOrder     []string
	Reservations map[string]*Reservation
	ResOrder     []string
	Seq          uint64 // sequence number of the last applied event
}

// New returns an empty lab.
func New() *State {
	return &State{
		Devices: map[string]*Device{}, Teams: map[string]*Team{},
		Jobs: map[string]*Job{}, Reservations: map[string]*Reservation{},
	}
}

// Apply applies one event. It returns an error, and changes nothing, if the event does not fit
// the current state; callers validate commands before turning them into events.
func (s *State) Apply(e Event) error {
	if err := s.apply(e); err != nil {
		return fmt.Errorf("event %d (%s): %w", e.Seq, e.Type, err)
	}
	if e.Seq > s.Seq {
		s.Seq = e.Seq
	}
	return nil
}

func (s *State) apply(e Event) error {
	switch e.Type {
	case EvDevice:
		var d Device
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return err
		}
		if old, ok := s.Devices[d.ID]; ok {
			// Inventory updates change the description, not the operational state.
			old.Vendor, old.Model, old.Capabilities = d.Vendor, d.Model, d.Capabilities
			return nil
		}
		if d.Status == "" {
			d.Status = Available
		}
		s.Devices[d.ID] = &d
		s.DeviceOrder = append(s.DeviceOrder, d.ID)
	case EvLink:
		var l Link
		if err := json.Unmarshal(e.Data, &l); err != nil {
			return err
		}
		if s.Devices[l.A.Device] == nil || s.Devices[l.B.Device] == nil {
			return fmt.Errorf("link between unknown devices %s and %s", l.A.Device, l.B.Device)
		}
		if !slices.Contains(s.Links, l) {
			s.Links = append(s.Links, l)
		}
	case EvTeam:
		var t Team
		if err := json.Unmarshal(e.Data, &t); err != nil {
			return err
		}
		if _, ok := s.Teams[t.Name]; !ok {
			s.TeamOrder = append(s.TeamOrder, t.Name)
		}
		s.Teams[t.Name] = &t
	case EvSubmit:
		var d SubmitData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return err
		}
		if _, dup := s.Jobs[d.ID]; dup {
			return fmt.Errorf("job %s already exists", d.ID)
		}
		s.Jobs[d.ID] = &Job{JobSpec: d.Spec, ID: d.ID, State: Queued, Submitted: e.At, History: []Note{{e.At, "submitted"}}}
		s.JobOrder = append(s.JobOrder, d.ID)
	case EvAssign:
		var d AssignData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return err
		}
		j, err := s.job(d.Job, Queued)
		if err != nil {
			return err
		}
		var parts []string
		for _, r := range j.Roles {
			dev := s.Devices[d.Assignment[r.Name]]
			if dev == nil || dev.Job != "" {
				return fmt.Errorf("device %q for role %s is unknown or busy", d.Assignment[r.Name], r.Name)
			}
			parts = append(parts, r.Name+"="+dev.ID)
		}
		for _, id := range d.Assignment {
			s.Devices[id].Job = j.ID
		}
		j.State, j.Started, j.Assignment, j.Results = Running, e.At, d.Assignment, nil
		j.Attempts++
		j.History = append(j.History, Note{e.At, fmt.Sprintf("attempt %d running on %s", j.Attempts, strings.Join(parts, ", "))})
	case EvResult:
		var d ResultData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return err
		}
		j, err := s.job(d.Job, Running)
		if err != nil {
			return err
		}
		j.Results = append(j.Results, d.Result)
	case EvFinish, EvRequeue:
		var d FinishData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return err
		}
		j, err := s.job(d.Job, Running)
		if err != nil {
			return err
		}
		s.release(j, d.Infra, e.At, d.State == Passed || d.State == Failed)
		if e.Type == EvRequeue {
			j.State, j.Assignment = Queued, nil
			j.History = append(j.History, Note{e.At, "requeued: " + d.Message})
			return nil
		}
		j.State, j.Ended, j.Message = d.State, e.At, d.Message
		j.History = append(j.History, Note{e.At, string(d.State) + messageSuffix(d.Message)})
	case EvCancel:
		var d CancelData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return err
		}
		j, err := s.job(d.Job, Queued)
		if err != nil {
			return err
		}
		j.State, j.Ended, j.Message = Cancelled, e.At, d.Reason
		j.History = append(j.History, Note{e.At, "cancelled" + messageSuffix(d.Reason)})
	case EvReserve:
		var r Reservation
		if err := json.Unmarshal(e.Data, &r); err != nil {
			return err
		}
		if _, dup := s.Reservations[r.ID]; dup {
			return fmt.Errorf("reservation %s already exists", r.ID)
		}
		s.Reservations[r.ID] = &r
		s.ResOrder = append(s.ResOrder, r.ID)
	case EvUnreserve:
		var d UnreserveData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return err
		}
		r, ok := s.Reservations[d.ID]
		if !ok {
			return fmt.Errorf("no reservation %s", d.ID)
		}
		r.Cancelled = true
	case EvDeviceStatus:
		var d DeviceStatusData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return err
		}
		dev, ok := s.Devices[d.Device]
		if !ok {
			return fmt.Errorf("no device %s", d.Device)
		}
		dev.Status, dev.Note = d.Status, d.Note
		if d.Status == Available {
			dev.Failures = 0
		}
	default:
		return fmt.Errorf("unknown event type %q", e.Type)
	}
	return nil
}

// release frees a job's devices. Devices that failed get a strike, and enough strikes in a row
// quarantine them; devices that worked (healthy) have their strikes cleared.
func (s *State) release(j *Job, infra []string, at time.Time, healthy bool) {
	for _, id := range j.Assignment {
		dev := s.Devices[id]
		dev.Job = ""
		dev.JobsRun++
		switch {
		case slices.Contains(infra, id):
			dev.Failures++
			if dev.Failures >= QuarantineAfter && dev.Status == Available {
				dev.Status = Quarantined
				dev.Note = fmt.Sprintf("quarantined at %s after %d infrastructure failures in a row", at.Format(time.RFC3339), dev.Failures)
			}
		case healthy:
			dev.Failures = 0
		}
	}
}

func (s *State) job(id string, want JobState) (*Job, error) {
	j, ok := s.Jobs[id]
	if !ok {
		return nil, fmt.Errorf("no job %s", id)
	}
	if j.State != want {
		return nil, fmt.Errorf("job %s is %s, not %s", id, j.State, want)
	}
	return j, nil
}

func messageSuffix(m string) string {
	if m == "" {
		return ""
	}
	return ": " + m
}

// Connected reports whether a cable joins the two devices.
func (s *State) Connected(a, b string) bool {
	_, ok := s.LinkBetween(a, b)
	return ok
}

// LinkBetween returns the first cable between two devices, oriented from a to b.
func (s *State) LinkBetween(a, b string) (Link, bool) {
	for _, l := range s.Links {
		switch {
		case l.A.Device == a && l.B.Device == b:
			return l, true
		case l.A.Device == b && l.B.Device == a:
			return Link{A: l.B, B: l.A}, true
		}
	}
	return Link{}, false
}

// DeviceList returns devices in inventory order.
func (s *State) DeviceList() []*Device {
	out := make([]*Device, len(s.DeviceOrder))
	for i, id := range s.DeviceOrder {
		out[i] = s.Devices[id]
	}
	return out
}

// JobList returns jobs in submission order.
func (s *State) JobList() []*Job {
	out := make([]*Job, len(s.JobOrder))
	for i, id := range s.JobOrder {
		out[i] = s.Jobs[id]
	}
	return out
}

// ReservationList returns reservations in creation order.
func (s *State) ReservationList() []*Reservation {
	out := make([]*Reservation, len(s.ResOrder))
	for i, id := range s.ResOrder {
		out[i] = s.Reservations[id]
	}
	return out
}
