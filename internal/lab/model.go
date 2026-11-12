// Package lab is the scheduler's state: the device inventory and its cabling, reservations, and
// the job queue. The state changes only by applying Events, which are also what the write-ahead
// log stores, so replaying the log after a crash rebuilds exactly the same state.
package lab

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

// DeviceStatus says whether the scheduler may use a device.
type DeviceStatus string

const (
	Available   DeviceStatus = "available"
	Quarantined DeviceStatus = "quarantined" // failed repeatedly; needs someone to look at it
	Maintenance DeviceStatus = "maintenance" // taken out of service on purpose
)

// QuarantineAfter is how many consecutive infrastructure failures quarantine a device.
const QuarantineAfter = 3

// MaxAttempts is how many times a job runs before infrastructure failures fail it for good.
const MaxAttempts = 3

// Endpoint is one end of a cable.
type Endpoint struct {
	Device string `json:"device"`
	Port   string `json:"port"`
	IP     string `json:"ip,omitempty"`
}

// Link is a cable between two device ports.
type Link struct {
	A Endpoint `json:"a"`
	B Endpoint `json:"b"`
}

// Device is a lab device as the scheduler sees it.
type Device struct {
	ID           string       `json:"id"`
	Vendor       string       `json:"vendor"`
	Model        string       `json:"model"`
	Capabilities []string     `json:"capabilities"`
	Status       DeviceStatus `json:"status"`
	Note         string       `json:"note,omitempty"`
	Job          string       `json:"job,omitempty"`
	Failures     int          `json:"failures"`
	JobsRun      int          `json:"jobsRun"`
}

// Has reports whether the device has every capability in caps.
func (d *Device) Has(caps []string) bool {
	for _, c := range caps {
		if !slices.Contains(d.Capabilities, c) {
			return false
		}
	}
	return true
}

// Team is a group that submits jobs; weights set each team's fair share.
type Team struct {
	Name   string `json:"name"`
	Weight int    `json:"weight"`
}

// Role is a device a job needs, described by vendor and capabilities rather than by name.
type Role struct {
	Name         string   `json:"name"`
	Vendor       string   `json:"vendor,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// Cable says two roles must be directly connected.
type Cable struct {
	A string `json:"a"`
	B string `json:"b"`
}

// Source records what triggered a job, such as a GitHub push.
type Source struct {
	Kind string `json:"kind"`
	Repo string `json:"repo"`
	SHA  string `json:"sha"`
	Ref  string `json:"ref,omitempty"`
	PR   int    `json:"pr,omitempty"`
	URL  string `json:"url,omitempty"`
}

// JobSpec is what a user or an integration submits.
type JobSpec struct {
	Name        string   `json:"name"`
	Team        string   `json:"team"`
	Priority    int      `json:"priority"` // 0 low, 1 normal, 2 high
	Roles       []Role   `json:"roles"`
	Cables      []Cable  `json:"cables,omitempty"`
	Tests       []string `json:"tests"`
	TimeoutSec  int      `json:"timeoutSec"`
	Reservation string   `json:"reservation,omitempty"`
	Source      *Source  `json:"source,omitempty"`
}

// Timeout is the most a job may run.
func (s *JobSpec) Timeout() time.Duration { return time.Duration(s.TimeoutSec) * time.Second }

// JobState is where a job is in its lifecycle.
type JobState string

const (
	Queued    JobState = "queued"
	Running   JobState = "running"
	Passed    JobState = "passed"
	Failed    JobState = "failed"
	Errored   JobState = "error" // the lab, not the feature under test, failed
	Cancelled JobState = "cancelled"
)

// Done reports whether the job has finished.
func (s JobState) Done() bool { return s != Queued && s != Running }

// TestResult is the outcome of one qualification test.
type TestResult struct {
	Test         string             `json:"test"`
	Passed       bool               `json:"passed"`
	Infra        bool               `json:"infra,omitempty"`
	Message      string             `json:"message"`
	DurationMS   int64              `json:"durationMs"`
	Measurements map[string]float64 `json:"measurements,omitempty"`
	Devices      []string           `json:"devices,omitempty"`
}

// Note is a line in a job's history.
type Note struct {
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}

// Job is a submitted job.
type Job struct {
	JobSpec
	ID         string            `json:"id"`
	State      JobState          `json:"state"`
	Submitted  time.Time         `json:"submitted"`
	Started    time.Time         `json:"started,omitzero"`
	Ended      time.Time         `json:"ended,omitzero"`
	Attempts   int               `json:"attempts"`
	Assignment map[string]string `json:"assignment,omitempty"`
	Results    []TestResult      `json:"results,omitempty"`
	Message    string            `json:"message,omitempty"`
	History    []Note            `json:"history"`
}

// Reservation holds devices for a person or team during a time window.
type Reservation struct {
	ID        string    `json:"id"`
	Owner     string    `json:"owner"`
	Purpose   string    `json:"purpose,omitempty"`
	Devices   []string  `json:"devices"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	Cancelled bool      `json:"cancelled,omitempty"`
}

// Overlaps reports whether the reservation is in effect at any time in [from, to).
func (r *Reservation) Overlaps(from, to time.Time) bool {
	return !r.Cancelled && r.Start.Before(to) && from.Before(r.End)
}

// Event types.
const (
	EvDevice       = "device"
	EvLink         = "link"
	EvTeam         = "team"
	EvSubmit       = "submit"
	EvAssign       = "assign"
	EvResult       = "result"
	EvFinish       = "finish"
	EvRequeue      = "requeue"
	EvCancel       = "cancel"
	EvReserve      = "reserve"
	EvUnreserve    = "unreserve"
	EvDeviceStatus = "device-status"
)

// Event is one change to the state.
type Event struct {
	Seq  uint64          `json:"seq"`
	At   time.Time       `json:"at"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// Payloads of events that are not plain records.
type (
	SubmitData struct {
		ID   string  `json:"id"`
		Spec JobSpec `json:"spec"`
	}
	AssignData struct {
		Job        string            `json:"job"`
		Assignment map[string]string `json:"assignment"`
	}
	ResultData struct {
		Job    string     `json:"job"`
		Result TestResult `json:"result"`
	}
	FinishData struct {
		Job     string   `json:"job"`
		State   JobState `json:"state"`
		Message string   `json:"message,omitempty"`
		// Infra lists the devices that failed (as opposed to the feature under test).
		Infra []string `json:"infra,omitempty"`
	}
	CancelData struct {
		Job    string `json:"job"`
		Reason string `json:"reason"`
	}
	UnreserveData struct {
		ID string `json:"id"`
	}
	DeviceStatusData struct {
		Device string       `json:"device"`
		Status DeviceStatus `json:"status"`
		Note   string       `json:"note,omitempty"`
	}
)

// NewEvent builds an event with a JSON payload. The state assigns Seq when it applies the event.
func NewEvent(at time.Time, typ string, data any) Event {
	raw, err := json.Marshal(data)
	if err != nil {
		panic(fmt.Sprintf("lab: cannot encode %s event: %v", typ, err))
	}
	return Event{At: at.UTC(), Type: typ, Data: raw}
}
