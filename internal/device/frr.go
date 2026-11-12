package device

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// Runner runs a command and returns its standard output.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner runs commands with os/exec.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out, fmt.Errorf("%s: %v: %s", name, err, strings.TrimSpace(string(ee.Stderr)))
	}
	return out, err
}

// FRR drives an FRRouting router through vtysh, inside the router's network namespace.
type FRR struct {
	Netns     string // run vtysh with "ip netns exec Netns" when set
	VtySocket string // vtysh --vty_socket directory, when not the default
	Run       Runner // defaults to ExecRunner
}

func (f *FRR) vtysh(ctx context.Context, cmds ...string) ([]byte, error) {
	name, args := "vtysh", []string{}
	if f.Netns != "" {
		name, args = "ip", []string{"netns", "exec", f.Netns, "vtysh"}
	}
	if f.VtySocket != "" {
		args = append(args, "--vty_socket", f.VtySocket)
	}
	for _, c := range cmds {
		args = append(args, "-c", c)
	}
	run := f.Run
	if run == nil {
		run = ExecRunner
	}
	out, err := run(ctx, name, args...)
	if err != nil {
		// vtysh fails when bgpd or zebra is down: the router itself is unreachable.
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	return out, nil
}

var frrVersion = regexp.MustCompile(`FRRouting (\S+) \(([^)]*)\)`)

func (f *FRR) Info(ctx context.Context) (Info, error) {
	out, err := f.vtysh(ctx, "show version")
	if err != nil {
		return Info{}, err
	}
	m := frrVersion.FindStringSubmatch(string(out))
	if m == nil {
		return Info{}, fmt.Errorf("frr: unexpected show version output %q", firstLine(out))
	}
	return Info{OS: "FRRouting", Version: m[1], Hostname: m[2]}, nil
}

func (f *FRR) BGPPeers(ctx context.Context) ([]BGPPeer, error) {
	out, err := f.vtysh(ctx, "show bgp ipv4 unicast summary json")
	if err != nil {
		return nil, err
	}
	return parseFRRSummary(out)
}

type frrPeers struct {
	Peers map[string]struct {
		RemoteAS uint32 `json:"remoteAs"`
		State    string `json:"state"`
		PfxRcd   int    `json:"pfxRcd"`
	} `json:"peers"`
}

// parseFRRSummary accepts both shapes FRR versions print: peers at the top level, or under
// "ipv4Unicast".
func parseFRRSummary(out []byte) ([]BGPPeer, error) {
	var v struct {
		frrPeers
		IPv4Unicast *frrPeers `json:"ipv4Unicast"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return nil, fmt.Errorf("frr: unexpected bgp summary: %v", err)
	}
	src := v.frrPeers
	if v.IPv4Unicast != nil {
		src = *v.IPv4Unicast
	}
	peers := make([]BGPPeer, 0, len(src.Peers))
	for addr, p := range src.Peers {
		peers = append(peers, BGPPeer{Address: addr, RemoteAS: p.RemoteAS, State: p.State, PrefixesReceived: p.PfxRcd})
	}
	sortPeers(peers)
	return peers, nil
}

// LLDPNeighbors is unsupported: FRRouting does not implement LLDP.
func (f *FRR) LLDPNeighbors(context.Context) ([]Neighbor, error) { return nil, ErrUnsupported }

func (f *FRR) SetInterfaceAdmin(ctx context.Context, port string, up bool) error {
	cmd := "shutdown"
	if up {
		cmd = "no shutdown"
	}
	_, err := f.vtysh(ctx, "configure terminal", "interface "+port, cmd)
	return err
}

func firstLine(b []byte) string {
	s, _, _ := strings.Cut(string(b), "\n")
	return s
}
