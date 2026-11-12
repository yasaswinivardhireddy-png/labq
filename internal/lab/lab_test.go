package lab

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)

// testLab builds a lab from a compact description and applies the events.
type testLab struct {
	*State
	t   *testing.T
	seq uint64
}

func newLab(t *testing.T) *testLab {
	l := &testLab{State: New(), t: t}
	l.emit(t0, EvTeam, Team{Name: "wan", Weight: 1})
	l.emit(t0, EvTeam, Team{Name: "dc", Weight: 1})
	return l
}

func (l *testLab) emit(at time.Time, typ string, data any) {
	l.t.Helper()
	e := NewEvent(at, typ, data)
	l.seq++
	e.Seq = l.seq
	if err := l.Apply(e); err != nil {
		l.t.Fatal(err)
	}
}

func (l *testLab) device(id, vendor string, caps ...string) {
	l.emit(t0, EvDevice, Device{ID: id, Vendor: vendor, Model: vendor + " box", Capabilities: caps})
}

func (l *testLab) cable(a, b string) {
	l.emit(t0, EvLink, Link{A: Endpoint{Device: a, Port: "p-" + b}, B: Endpoint{Device: b, Port: "p-" + a}})
}

func (l *testLab) submit(id string, at time.Time, spec JobSpec) {
	if spec.Team == "" {
		spec.Team = "wan"
	}
	if spec.TimeoutSec == 0 {
		spec.TimeoutSec = 600
	}
	spec.Name, spec.Tests = id, []string{"reachable"}
	l.emit(at, EvSubmit, SubmitData{ID: id, Spec: spec})
}

func pair(caps ...string) JobSpec {
	return JobSpec{Roles: []Role{{Name: "dut", Capabilities: caps}, {Name: "peer", Capabilities: caps}}, Cables: []Cable{{"dut", "peer"}}}
}

func single(caps ...string) JobSpec { return JobSpec{Roles: []Role{{Name: "dut", Capabilities: caps}}} }

func (l *testLab) start(as []Assignment, at time.Time) {
	for _, a := range as {
		l.emit(at, EvAssign, AssignData{Job: a.Job, Assignment: a.Devices})
	}
}

func jobs(as []Assignment) []string {
	var out []string
	for _, a := range as {
		out = append(out, a.Job)
	}
	return out
}

func TestMatchRespectsVendorCapabilitiesAndCables(t *testing.T) {
	l := newLab(t)
	l.device("n1", "nanonos", "bgp", "lldp")
	l.device("n2", "nanonos", "bgp", "lldp")
	l.device("f1", "frr", "bgp")
	l.cable("n1", "f1")
	l.cable("n1", "n2")
	all := func(*Device) bool { return true }

	roles := []Role{{Name: "dut", Vendor: "nanonos", Capabilities: []string{"lldp"}}, {Name: "peer", Vendor: "frr"}}
	got, ok := Match(roles, []Cable{{"dut", "peer"}}, l.DeviceList(), l.Connected, all)
	if !ok || got["dut"] != "n1" || got["peer"] != "f1" {
		t.Fatalf("got %v, %v; want dut=n1 (the only nanonos cabled to frr), peer=f1", got, ok)
	}
	// n2 is not cabled to f1, so excluding n1 leaves no solution.
	if _, ok := Match(roles, []Cable{{"dut", "peer"}}, l.DeviceList(), l.Connected, func(d *Device) bool { return d.ID != "n1" }); ok {
		t.Fatal("matched although no nanonos device other than n1 is cabled to frr")
	}
	// With no cable required, the device with fewer capabilities is preferred.
	got, _ = Match([]Role{{Name: "any", Capabilities: []string{"bgp"}}}, nil, l.DeviceList(), l.Connected, all)
	if got["any"] != "f1" {
		t.Errorf("got %v, want the least capable device f1", got)
	}
}

// Match must find a solution exactly when one exists, on random labs checked by brute force.
func TestMatchAgreesWithBruteForce(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	vendors, caps := []string{"a", "b"}, []string{"x", "y", "z"}
	pick := func() []string {
		var out []string
		for _, c := range caps {
			if r.IntN(2) == 0 {
				out = append(out, c)
			}
		}
		return out
	}
	for iter := range 3000 {
		l := &testLab{State: New(), t: t}
		n := 2 + r.IntN(6)
		for i := range n {
			l.device(fmt.Sprintf("d%d", i), vendors[r.IntN(2)], pick()...)
		}
		for i := range n {
			for j := i + 1; j < n; j++ {
				if r.IntN(10) < 4 {
					l.cable(fmt.Sprintf("d%d", i), fmt.Sprintf("d%d", j))
				}
			}
		}
		var roles []Role
		var cables []Cable
		for i := range 1 + r.IntN(4) {
			v := ""
			if r.IntN(3) == 0 {
				v = vendors[r.IntN(2)]
			}
			roles = append(roles, Role{Name: fmt.Sprintf("r%d", i), Vendor: v, Capabilities: pick()})
			for j := range i {
				if r.IntN(2) == 0 {
					cables = append(cables, Cable{fmt.Sprintf("r%d", j), fmt.Sprintf("r%d", i)})
				}
			}
		}
		eligible := func(d *Device) bool { return d.ID != "d0" || iter%2 == 0 }
		got, ok := Match(roles, cables, l.DeviceList(), l.Connected, eligible)
		want := bruteForce(roles, cables, l.DeviceList(), l.Connected, eligible)
		if ok != want {
			t.Fatalf("iteration %d: Match found=%v, brute force found=%v\nroles=%+v cables=%+v links=%+v", iter, ok, want, roles, cables, l.Links)
		}
		if ok && !valid(got, roles, cables, l.State, eligible) {
			t.Fatalf("iteration %d: invalid assignment %v", iter, got)
		}
	}
}

func valid(a map[string]string, roles []Role, cables []Cable, s *State, eligible func(*Device) bool) bool {
	seen := map[string]bool{}
	for _, r := range roles {
		d := s.Devices[a[r.Name]]
		if d == nil || seen[d.ID] || !eligible(d) || !d.Has(r.Capabilities) || (r.Vendor != "" && r.Vendor != d.Vendor) {
			return false
		}
		seen[d.ID] = true
	}
	for _, c := range cables {
		if !s.Connected(a[c.A], a[c.B]) {
			return false
		}
	}
	return true
}

func bruteForce(roles []Role, cables []Cable, devs []*Device, connected func(a, b string) bool, eligible func(*Device) bool) bool {
	s := &State{Devices: map[string]*Device{}}
	for _, d := range devs {
		s.Devices[d.ID] = d
	}
	a := map[string]string{}
	used := map[string]bool{}
	var try func(i int) bool
	try = func(i int) bool {
		if i == len(roles) {
			for _, c := range cables {
				if !connected(a[c.A], a[c.B]) {
					return false
				}
			}
			return true
		}
		for _, d := range devs {
			r := roles[i]
			if used[d.ID] || !eligible(d) || !d.Has(r.Capabilities) || (r.Vendor != "" && r.Vendor != d.Vendor) {
				continue
			}
			a[r.Name], used[d.ID] = d.ID, true
			if try(i + 1) {
				return true
			}
			used[d.ID] = false
		}
		return false
	}
	return try(0)
}

func TestPlanOrdersByPriorityThenFairShareThenAge(t *testing.T) {
	l := newLab(t)
	l.device("a", "nanonos", "bgp")
	l.device("b", "nanonos", "bgp")
	l.cable("a", "b")

	low, high := pair("bgp"), pair("bgp")
	low.Priority, high.Priority = 0, 2
	l.submit("low", t0, low)
	l.submit("high", t0.Add(time.Minute), high)
	if got := jobs(l.Plan(t0.Add(2*time.Minute), time.Hour)); !slices.Equal(got, []string{"high"}) {
		t.Fatalf("plan = %v, want the high-priority job first", got)
	}

	// Fair share: wan already runs a job, so dc's job goes before wan's older one.
	f := newLab(t)
	for _, id := range []string{"a", "b", "c"} {
		f.device(id, "nanonos", "bgp")
	}
	f.submit("wan-running", t0, single("bgp"))
	f.start(f.Plan(t0, time.Hour), t0)
	f.submit("wan-2", t0.Add(time.Minute), pair("bgp"))
	dc := single("bgp")
	dc.Team = "dc"
	f.submit("dc-1", t0.Add(2*time.Minute), dc)
	if pos := f.QueuePosition("dc-1"); pos != 1 {
		t.Errorf("dc-1 is at position %d, want 1 (its team is using less)", pos)
	}
}

func TestPlanRespectsReservations(t *testing.T) {
	l := newLab(t)
	l.device("a", "nanonos", "bgp")
	l.emit(t0, EvReserve, Reservation{ID: "r1", Owner: "alice", Devices: []string{"a"}, Start: t0.Add(20 * time.Minute), End: t0.Add(2 * time.Hour)})

	long, short := single("bgp"), single("bgp")
	long.TimeoutSec, short.TimeoutSec = 3600, 600
	l.submit("long", t0, long)
	l.submit("short", t0, short)
	if got := jobs(l.Plan(t0, time.Hour)); !slices.Equal(got, []string{"short"}) {
		t.Fatalf("plan = %v, want only the job that ends before the reservation starts", got)
	}

	// A job inside the reservation runs only during it, and only on its devices.
	in := single("bgp")
	in.Reservation, in.TimeoutSec = "r1", 600
	l.submit("inside", t0, in)
	if got := jobs(l.Plan(t0.Add(time.Minute), time.Hour)); slices.Contains(got, "inside") {
		t.Fatalf("plan = %v: the reservation has not started yet", got)
	}
	if got := jobs(l.Plan(t0.Add(30*time.Minute), time.Hour)); !slices.Equal(got, []string{"inside"}) {
		t.Fatalf("plan = %v, want the job inside the reservation (others must keep out)", got)
	}
}

func TestPlanHoldsDevicesForAStarvingJob(t *testing.T) {
	l := newLab(t)
	l.device("a", "nanonos", "bgp")
	l.device("b", "nanonos", "bgp")
	l.cable("a", "b")
	l.submit("blocker", t0, single("bgp"))
	l.start(l.Plan(t0, time.Hour), t0) // blocker takes one device
	l.submit("big", t0.Add(time.Minute), pair("bgp"))
	l.submit("small", t0.Add(2*time.Minute), single("bgp"))

	if got := jobs(l.Plan(t0.Add(3*time.Minute), 10*time.Minute)); !slices.Equal(got, []string{"small"}) {
		t.Fatalf("before the starvation limit, plan = %v, want small to use the free device", got)
	}
	if got := jobs(l.Plan(t0.Add(20*time.Minute), 10*time.Minute)); len(got) != 0 {
		t.Fatalf("after big waited 19 minutes, plan = %v, want the free device held for big", got)
	}
}

func TestInfrastructureFailuresQuarantineADevice(t *testing.T) {
	l := newLab(t)
	l.device("a", "nanonos", "bgp")
	for i := range QuarantineAfter {
		id := fmt.Sprintf("j%d", i)
		l.submit(id, t0, single("bgp"))
		l.start(l.Plan(t0, time.Hour), t0)
		if l.Jobs[id].State != Running {
			t.Fatalf("%s did not start; device is %s", id, l.Devices["a"].Status)
		}
		l.emit(t0, EvFinish, FinishData{Job: id, State: Errored, Message: "device unreachable", Infra: []string{"a"}})
	}
	if d := l.Devices["a"]; d.Status != Quarantined || d.Failures != QuarantineAfter || !strings.Contains(d.Note, "3 infrastructure failures") {
		t.Fatalf("device = %+v, want quarantined", d)
	}
	l.submit("next", t0, single("bgp"))
	if got := l.Plan(t0, time.Hour); len(got) != 0 {
		t.Fatalf("plan = %v; a quarantined device must not be used", got)
	}
	l.emit(t0, EvDeviceStatus, DeviceStatusData{Device: "a", Status: Available, Note: "cable replaced"})
	if got := jobs(l.Plan(t0, time.Hour)); !slices.Equal(got, []string{"next"}) || l.Devices["a"].Failures != 0 {
		t.Fatalf("after repair, plan = %v and failures = %d", got, l.Devices["a"].Failures)
	}
}

func TestRequeueReleasesDevicesAndCountsAttempts(t *testing.T) {
	l := newLab(t)
	l.device("a", "nanonos", "bgp")
	l.submit("j", t0, single("bgp"))
	l.start(l.Plan(t0, time.Hour), t0)
	l.emit(t0, EvRequeue, FinishData{Job: "j", Message: "lease lost: labq restarted"})
	j := l.Jobs["j"]
	if j.State != Queued || j.Assignment != nil || l.Devices["a"].Job != "" || j.Attempts != 1 {
		t.Fatalf("job = %+v, device job %q", j, l.Devices["a"].Job)
	}
	l.start(l.Plan(t0, time.Hour), t0)
	if j.State != Running || j.Attempts != 2 {
		t.Fatalf("second attempt: state %s, attempts %d", j.State, j.Attempts)
	}
}

// Replaying a log of JSON events must rebuild exactly the same state.
func TestReplayRebuildsTheSameState(t *testing.T) {
	var log []Event
	live := New()
	record := func(at time.Time, typ string, data any) {
		e := NewEvent(at, typ, data)
		e.Seq = uint64(len(log) + 1)
		if err := live.Apply(e); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(e)
		var back Event
		_ = json.Unmarshal(raw, &back)
		log = append(log, back)
	}
	record(t0, EvTeam, Team{Name: "wan", Weight: 2})
	record(t0, EvDevice, Device{ID: "a", Vendor: "nanonos", Capabilities: []string{"bgp"}})
	record(t0, EvDevice, Device{ID: "b", Vendor: "frr", Capabilities: []string{"bgp"}})
	record(t0, EvLink, Link{A: Endpoint{"a", "Ethernet0", "10.0.0.0"}, B: Endpoint{"b", "eth0", "10.0.0.1"}})
	spec := pair("bgp")
	spec.Name, spec.Team, spec.Tests, spec.TimeoutSec = "j1", "wan", []string{"bgp-established"}, 60
	record(t0, EvSubmit, SubmitData{ID: "j1", Spec: spec})
	for _, a := range live.Plan(t0, time.Hour) {
		record(t0, EvAssign, AssignData{Job: a.Job, Assignment: a.Devices})
	}
	record(t0.Add(time.Second), EvResult, ResultData{Job: "j1", Result: TestResult{Test: "bgp-established", Passed: true, Measurements: map[string]float64{"wait_s": 1.5}}})
	record(t0.Add(2*time.Second), EvFinish, FinishData{Job: "j1", State: Passed})
	record(t0, EvReserve, Reservation{ID: "r1", Owner: "bob", Devices: []string{"a"}, Start: t0, End: t0.Add(time.Hour)})

	replayed := New()
	for _, e := range log {
		if err := replayed.Apply(e); err != nil {
			t.Fatal(err)
		}
	}
	a, _ := json.Marshal(live)
	b, _ := json.Marshal(replayed)
	if string(a) != string(b) {
		t.Fatalf("replayed state differs:\nlive:     %s\nreplayed: %s", a, b)
	}
}

func TestApplyRejectsEventsThatDoNotFit(t *testing.T) {
	l := newLab(t)
	l.device("a", "nanonos", "bgp")
	for _, e := range []Event{
		NewEvent(t0, EvAssign, AssignData{Job: "missing", Assignment: map[string]string{"dut": "a"}}),
		NewEvent(t0, EvLink, Link{A: Endpoint{Device: "a"}, B: Endpoint{Device: "ghost"}}),
		NewEvent(t0, "bogus", struct{}{}),
	} {
		if err := l.Apply(e); err == nil {
			t.Errorf("Apply(%s) succeeded", e.Type)
		}
	}
}

func TestValidation(t *testing.T) {
	l := newLab(t)
	l.device("a", "nanonos", "bgp", "lldp")
	l.device("f", "frr", "bgp")
	l.cable("a", "f")
	known := func(name string) bool { return name == "reachable" }

	bad := JobSpec{Name: "Bad Name", Team: "nobody", Priority: 7, TimeoutSec: 1, Tests: []string{"nope"},
		Roles: []Role{{Name: "x"}, {Name: "x"}}, Cables: []Cable{{"x", "y"}}, Reservation: "none"}
	p := l.ValidateJob(bad, known)
	for _, f := range []string{"name", "team", "priority", "timeoutSec", "tests", "roles", "cables", "reservation"} {
		if p[f] == "" {
			t.Errorf("no problem reported for %s: %v", f, p)
		}
	}

	ok := pair("bgp")
	ok.Name, ok.Team, ok.TimeoutSec, ok.Tests = "interop", "wan", 60, []string{"reachable"}
	if p := l.ValidateJob(ok, known); len(p) != 0 {
		t.Errorf("valid job rejected: %v", p)
	}
	impossible := pair("lldp") // only one device speaks LLDP
	impossible.Name, impossible.Team, impossible.TimeoutSec, impossible.Tests = "lldp-pair", "wan", 60, []string{"reachable"}
	if p := l.ValidateJob(impossible, known); !strings.Contains(p["roles"], "No set of lab devices") {
		t.Errorf("infeasible job: %v", p)
	}

	l.emit(t0, EvReserve, Reservation{ID: "r1", Owner: "alice", Devices: []string{"a"}, Start: t0, End: t0.Add(time.Hour)})
	clash := Reservation{Owner: "bob", Devices: []string{"a"}, Start: t0.Add(30 * time.Minute), End: t0.Add(90 * time.Minute)}
	if p := l.ValidateReservation(clash, t0); !strings.Contains(p["devices"], "reserved by alice") {
		t.Errorf("overlapping reservation: %v", p)
	}
	free := Reservation{Owner: "bob", Devices: []string{"f"}, Start: t0, End: t0.Add(time.Hour)}
	if p := l.ValidateReservation(free, t0); len(p) != 0 {
		t.Errorf("valid reservation rejected: %v", p)
	}
}
