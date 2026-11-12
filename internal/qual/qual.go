// Package qual runs qualification tests against the devices assigned to a job. Tests use only the
// vendor-neutral device API, so the same suite qualifies NanoNOS, FRRouting or simulated devices.
//
// A test fails when the feature under test misbehaves. When a device does not answer at all, the
// result is marked Infra instead: the lab, not the feature, failed, and the job can be retried on
// other devices.
package qual

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/yasaswinivardhireddy-png/labq/internal/device"
	"github.com/yasaswinivardhireddy-png/labq/internal/lab"
)

// Member is a device assigned to one of the job's roles.
type Member struct {
	Device       string
	Driver       device.Device
	Capabilities []string
}

func (m Member) can(c string) bool { return slices.Contains(m.Capabilities, c) }

// Wire is a cable the job asked for, oriented so that Link.A belongs to role A.
type Wire struct {
	A, B string
	Link lab.Link
}

// Env is what a test runs against.
type Env struct {
	Members map[string]Member // by role
	Wires   []Wire
	Poll    time.Duration // how often to re-check a condition; default 250 ms
	Wait    time.Duration // how long to wait for a condition; default 30 s
}

func (e *Env) poll() time.Duration {
	if e.Poll > 0 {
		return e.Poll
	}
	return 250 * time.Millisecond
}

func (e *Env) wait() time.Duration {
	if e.Wait > 0 {
		return e.Wait
	}
	return 30 * time.Second
}

// Test is one qualification test.
type Test struct {
	Name        string
	Description string
	run         func(ctx context.Context, env *Env) lab.TestResult
}

var registry = []Test{
	{"reachable", "Every assigned device answers and reports its software version.", reachable},
	{"bgp-established", "The BGP session on every required cable is Established in both directions.", bgpEstablished},
	{"routes-exchanged", "Both ends of every required cable receive routes from each other.", routesExchanged},
	{"lldp-neighbors", "Each LLDP-capable device sees its cabled neighbor on the right port.", lldpNeighbors},
	{"link-failover", "Shut a cabled port: the far end must notice and the session must come back when the port does.", linkFailover},
}

// Tests returns every test, in the order they are listed.
func Tests() []Test { return registry }

// Known reports whether a test exists.
func Known(name string) bool {
	return slices.ContainsFunc(registry, func(t Test) bool { return t.Name == name })
}

// Run runs the named tests in order and reports each result as it finishes. It stops at the first
// infrastructure failure. It returns the job's outcome, a summary, and the devices that failed as
// infrastructure.
func Run(ctx context.Context, env *Env, names []string, report func(lab.TestResult)) (lab.JobState, string, []string) {
	var failed []string
	var infra []string
	for _, name := range names {
		i := slices.IndexFunc(registry, func(t Test) bool { return t.Name == name })
		if i < 0 {
			return lab.Errored, "unknown test " + name, nil
		}
		start := time.Now()
		res := registry[i].run(ctx, env)
		res.Test, res.DurationMS = name, time.Since(start).Milliseconds()
		if err := ctx.Err(); err != nil && !res.Passed {
			res.Infra = false
			if errors.Is(err, context.Canceled) {
				res.Message = "cancelled"
				report(res)
				return lab.Cancelled, "cancelled while running " + name, nil
			}
			res.Message = "the job timed out during this test"
		}
		report(res)
		if res.Infra {
			// The lab failed, not the feature. Later tests would fail for the same reason, and on
			// a hung device each of them would first wait out its timeout.
			infra = slices.Clone(res.Devices)
			break
		}
		if !res.Passed {
			failed = append(failed, name)
		}
		if ctx.Err() != nil {
			break
		}
	}
	switch {
	case len(infra) > 0:
		return lab.Errored, "device(s) did not answer: " + strings.Join(infra, ", "), infra
	case len(failed) > 0:
		return lab.Failed, "failed: " + strings.Join(failed, ", "), nil
	}
	return lab.Passed, fmt.Sprintf("%d of %d tests passed", len(names), len(names)), nil
}

// devErr ties a driver error to the device that returned it.
type devErr struct {
	dev string
	err error
}

func (e devErr) Error() string { return e.dev + ": " + e.err.Error() }
func (e devErr) Unwrap() error { return e.err }

func fail(err error, format string, args ...any) lab.TestResult {
	var de devErr
	if errors.As(err, &de) && errors.Is(err, device.ErrUnreachable) {
		return lab.TestResult{Infra: true, Devices: []string{de.dev}, Message: de.Error()}
	}
	msg := fmt.Sprintf(format, args...)
	if err != nil {
		msg += ": " + err.Error()
	}
	return lab.TestResult{Message: msg}
}

// waitFor polls cond until it holds, it errors, or the wait runs out. It returns how long it took.
func waitFor(ctx context.Context, env *Env, cond func() (bool, error)) (time.Duration, error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, env.wait())
	defer cancel()
	t := time.NewTicker(env.poll())
	defer t.Stop()
	for {
		ok, err := cond()
		if err != nil {
			return time.Since(start), err
		}
		if ok {
			return time.Since(start), nil
		}
		select {
		case <-ctx.Done():
			return time.Since(start), errTimeout
		case <-t.C:
		}
	}
}

var errTimeout = errors.New("timed out")

func roles(env *Env) []string {
	out := make([]string, 0, len(env.Members))
	for r := range env.Members {
		out = append(out, r)
	}
	slices.Sort(out)
	return out
}

func reachable(ctx context.Context, env *Env) lab.TestResult {
	var parts []string
	for _, r := range roles(env) {
		m := env.Members[r]
		info, err := m.Driver.Info(ctx)
		if err != nil {
			return fail(devErr{m.Device, err}, "%s did not report its version", m.Device)
		}
		parts = append(parts, fmt.Sprintf("%s runs %s %s", m.Device, info.OS, info.Version))
	}
	return lab.TestResult{Passed: true, Message: strings.Join(parts, "; ")}
}

// sessions returns each end's view of the BGP session on a wire.
func sessions(ctx context.Context, env *Env, w Wire) (a, b device.BGPPeer, err error) {
	ma, mb := env.Members[w.A], env.Members[w.B]
	pa, err := ma.Driver.BGPPeers(ctx)
	if err != nil {
		return a, b, devErr{ma.Device, err}
	}
	pb, err := mb.Driver.BGPPeers(ctx)
	if err != nil {
		return a, b, devErr{mb.Device, err}
	}
	a, _ = device.Peer(pa, w.Link.B.IP)
	b, _ = device.Peer(pb, w.Link.A.IP)
	return a, b, nil
}

func bgpWires(env *Env) []Wire {
	var out []Wire
	for _, w := range env.Wires {
		if env.Members[w.A].can("bgp") && env.Members[w.B].can("bgp") {
			out = append(out, w)
		}
	}
	return out
}

func state(p device.BGPPeer) string {
	if p.State == "" {
		return "no session"
	}
	return p.State
}

func bgpEstablished(ctx context.Context, env *Env) lab.TestResult {
	return eachSession(ctx, env, "Established", func(a, b device.BGPPeer) bool { return a.Established() && b.Established() })
}

func routesExchanged(ctx context.Context, env *Env) lab.TestResult {
	return eachSession(ctx, env, "exchanging routes", func(a, b device.BGPPeer) bool {
		return a.Established() && b.Established() && a.PrefixesReceived > 0 && b.PrefixesReceived > 0
	})
}

// eachSession waits until ok holds for the session on every BGP-capable wire.
func eachSession(ctx context.Context, env *Env, what string, ok func(a, b device.BGPPeer) bool) lab.TestResult {
	wires := bgpWires(env)
	if len(wires) == 0 {
		return lab.TestResult{Message: "the job has no cable between two BGP-capable devices"}
	}
	res := lab.TestResult{Passed: true, Measurements: map[string]float64{}}
	var parts []string
	for _, w := range wires {
		var last [2]device.BGPPeer
		took, err := waitFor(ctx, env, func() (bool, error) {
			a, b, err := sessions(ctx, env, w)
			last = [2]device.BGPPeer{a, b}
			return ok(a, b), err
		})
		ida, idb := env.Members[w.A].Device, env.Members[w.B].Device
		if err != nil {
			return fail(err, "session %s-%s not %s after %s (%s sees %s with %d routes, %s sees %s with %d routes)",
				ida, idb, what, took.Round(time.Millisecond), ida, state(last[0]), last[0].PrefixesReceived, idb, state(last[1]), last[1].PrefixesReceived)
		}
		res.Measurements["wait_s"] = max(res.Measurements["wait_s"], took.Seconds())
		parts = append(parts, fmt.Sprintf("%s-%s %s (%d and %d routes received)", ida, idb, what, last[0].PrefixesReceived, last[1].PrefixesReceived))
	}
	res.Message = strings.Join(parts, "; ")
	return res
}

func lldpNeighbors(ctx context.Context, env *Env) lab.TestResult {
	res := lab.TestResult{Passed: true}
	var parts []string
	for _, w := range env.Wires {
		ma, mb := env.Members[w.A], env.Members[w.B]
		if !ma.can("lldp") || !mb.can("lldp") {
			continue
		}
		for _, side := range [2]struct {
			local, remote Member
			port, peer    string
		}{{ma, mb, w.Link.A.Port, w.Link.B.Port}, {mb, ma, w.Link.B.Port, w.Link.A.Port}} {
			info, err := side.remote.Driver.Info(ctx)
			if err != nil {
				return fail(devErr{side.remote.Device, err}, "%s did not report its hostname", side.remote.Device)
			}
			var seen []device.Neighbor
			_, err = waitFor(ctx, env, func() (bool, error) {
				n, err := side.local.Driver.LLDPNeighbors(ctx)
				if err != nil {
					return false, devErr{side.local.Device, err}
				}
				seen = n
				return slices.Contains(n, device.Neighbor{LocalPort: side.port, SystemName: info.Hostname, PortID: side.peer}), nil
			})
			if err != nil {
				return fail(err, "%s does not see %s port %s on %s (sees %v)", side.local.Device, info.Hostname, side.peer, side.port, seen)
			}
			parts = append(parts, fmt.Sprintf("%s sees %s %s on %s", side.local.Device, info.Hostname, side.peer, side.port))
		}
	}
	if len(parts) == 0 {
		return lab.TestResult{Message: "the job has no cable between two LLDP-capable devices"}
	}
	res.Message = strings.Join(parts, "; ")
	return res
}

func linkFailover(ctx context.Context, env *Env) lab.TestResult {
	var w Wire
	found := false
	for _, x := range bgpWires(env) {
		if env.Members[x.A].can("admin-shutdown") {
			w, found = x, true
			break
		}
		if env.Members[x.B].can("admin-shutdown") {
			w, found = Wire{A: x.B, B: x.A, Link: lab.Link{A: x.Link.B, B: x.Link.A}}, true
			break
		}
	}
	if !found {
		return lab.TestResult{Message: "the job has no BGP cable with a port the lab can shut down"}
	}
	ma, mb := env.Members[w.A], env.Members[w.B]
	up := func(a, b device.BGPPeer) bool { return a.Established() && b.Established() }
	if _, err := waitFor(ctx, env, func() (bool, error) {
		a, b, err := sessions(ctx, env, w)
		return up(a, b), err
	}); err != nil {
		return fail(err, "session %s-%s was not up before the failover", ma.Device, mb.Device)
	}

	if err := ma.Driver.SetInterfaceAdmin(ctx, w.Link.A.Port, false); err != nil {
		return fail(devErr{ma.Device, err}, "could not shut %s on %s", w.Link.A.Port, ma.Device)
	}
	restored := false
	defer func() {
		if !restored {
			// Always leave the lab as it was, even when the job is cancelled.
			bg, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = ma.Driver.SetInterfaceAdmin(bg, w.Link.A.Port, true)
		}
	}()
	detect, err := waitFor(ctx, env, func() (bool, error) {
		_, b, err := sessions(ctx, env, w)
		return !b.Established(), err
	})
	if err != nil {
		return fail(err, "%s still reports the session up %s after %s went down", mb.Device, detect.Round(time.Millisecond), w.Link.A.Port)
	}
	if err := ma.Driver.SetInterfaceAdmin(ctx, w.Link.A.Port, true); err != nil {
		return fail(devErr{ma.Device, err}, "could not bring %s on %s back up", w.Link.A.Port, ma.Device)
	}
	restored = true
	back, err := waitFor(ctx, env, func() (bool, error) {
		a, b, err := sessions(ctx, env, w)
		return up(a, b), err
	})
	if err != nil {
		return fail(err, "session %s-%s did not come back within %s of %s coming up", ma.Device, mb.Device, env.wait(), w.Link.A.Port)
	}
	return lab.TestResult{
		Passed: true,
		Message: fmt.Sprintf("with %s on %s shut, %s saw the session drop in %.1f s; it was Established again %.1f s after the port came up",
			w.Link.A.Port, ma.Device, mb.Device, detect.Seconds(), back.Seconds()),
		Measurements: map[string]float64{"detect_s": detect.Seconds(), "recover_s": back.Seconds()},
	}
}
