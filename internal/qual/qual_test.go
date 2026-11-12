package qual

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yasaswinivardhireddy-png/labq/internal/device"
	"github.com/yasaswinivardhireddy-png/labq/internal/lab"
)

// simEnv builds a two-device simulated lab cabled dut:p1 <-> peer:p1.
func simEnv(t *testing.T) (*Env, *device.SimLab) {
	t.Helper()
	sim := device.NewSimLab(nil)
	sim.Converge = 150 * time.Millisecond
	sim.AddDevice("sw1", "SimOS", 65001, true)
	sim.AddDevice("sw2", "SimOS", 65002, true)
	sim.Connect("sw1", "p1", "10.0.0.0", "sw2", "p1", "10.0.0.1")
	caps := []string{"bgp", "lldp", "admin-shutdown"}
	env := &Env{
		Members: map[string]Member{
			"dut":  {Device: "sw1", Driver: sim.Device("sw1"), Capabilities: caps},
			"peer": {Device: "sw2", Driver: sim.Device("sw2"), Capabilities: caps},
		},
		Wires: []Wire{{A: "dut", B: "peer", Link: lab.Link{
			A: lab.Endpoint{Device: "sw1", Port: "p1", IP: "10.0.0.0"},
			B: lab.Endpoint{Device: "sw2", Port: "p1", IP: "10.0.0.1"},
		}}},
		Poll: 10 * time.Millisecond, Wait: 2 * time.Second,
	}
	return env, sim
}

func names() []string {
	var out []string
	for _, t := range Tests() {
		out = append(out, t.Name)
	}
	return out
}

func TestHealthyLabPassesEveryTest(t *testing.T) {
	env, _ := simEnv(t)
	var results []lab.TestResult
	state, msg, infra := Run(context.Background(), env, names(), func(r lab.TestResult) { results = append(results, r) })
	if state != lab.Passed || infra != nil || msg != "5 of 5 tests passed" {
		t.Fatalf("Run = %s, %q, %v; results %+v", state, msg, infra, results)
	}
	fo := results[len(results)-1]
	// The simulator drops a session the moment a port goes down, so detection takes ~0 s here;
	// recovery takes the simulated 150 ms convergence.
	if fo.Test != "link-failover" || fo.Measurements["detect_s"] < 0 || fo.Measurements["recover_s"] < 0.1 {
		t.Fatalf("failover result = %+v; want the 150 ms convergence measured", fo)
	}
	if !strings.Contains(results[3].Message, "sw1 sees sw2 p1 on p1") {
		t.Errorf("lldp message = %q", results[3].Message)
	}
}

func TestDownPortFailsTheFeatureNotTheLab(t *testing.T) {
	env, sim := simEnv(t)
	env.Wait = 200 * time.Millisecond
	_ = sim.Device("sw2").SetInterfaceAdmin(context.Background(), "p1", false)
	var got lab.TestResult
	state, _, infra := Run(context.Background(), env, []string{"bgp-established"}, func(r lab.TestResult) { got = r })
	if state != lab.Failed || infra != nil || got.Infra || !strings.Contains(got.Message, "not Established") {
		t.Fatalf("state %s, infra %v, result %+v", state, infra, got)
	}
}

func TestUnreachableDeviceIsAnInfrastructureFailure(t *testing.T) {
	env, sim := simEnv(t)
	sim.SetUnreachable("sw2", true)
	var got lab.TestResult
	state, msg, infra := Run(context.Background(), env, []string{"reachable", "bgp-established"}, func(r lab.TestResult) { got = r })
	if state != lab.Errored || !slices.Equal(infra, []string{"sw2"}) || !got.Infra || !strings.Contains(msg, "sw2") {
		t.Fatalf("state %s, msg %q, infra %v, last result %+v", state, msg, infra, got)
	}
}

func TestFailoverRestoresThePortWhenCancelled(t *testing.T) {
	env, sim := simEnv(t)
	env.Wait = 5 * time.Second
	// Cancel part-way through the test; the shut port must end up restored either way.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	state, _, _ := Run(ctx, env, []string{"link-failover"}, func(lab.TestResult) {})
	if state != lab.Cancelled && state != lab.Passed {
		t.Fatalf("state = %s", state)
	}
	time.Sleep(200 * time.Millisecond)
	peers, _ := sim.Device("sw2").BGPPeers(context.Background())
	if !peers[0].Established() {
		t.Fatal("the shut port was not restored")
	}
}

func TestTestsWithoutSuitableCablesFail(t *testing.T) {
	env, _ := simEnv(t)
	env.Wires = nil
	state, msg, _ := Run(context.Background(), env, []string{"reachable", "bgp-established", "lldp-neighbors", "link-failover"}, func(lab.TestResult) {})
	if state != lab.Failed || !strings.Contains(msg, "bgp-established, lldp-neighbors, link-failover") {
		t.Fatalf("Run = %s, %q", state, msg)
	}
	if !Known("reachable") || Known("nope") {
		t.Error("Known is wrong")
	}
}
