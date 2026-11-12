package device

import (
	"bufio"
	"context"
	"errors"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSwitch serves canned replies on a Unix socket, the way nanonosd's CLI does.
func fakeSwitch(t *testing.T, replies map[string]string) (socket string, got func() []string) {
	t.Helper()
	socket = filepath.Join(t.TempDir(), "sw.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	var mu sync.Mutex
	var cmds []string
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(conn).ReadString('\n')
			cmd := strings.TrimSpace(line)
			mu.Lock()
			cmds = append(cmds, cmd)
			mu.Unlock()
			reply, ok := replies[cmd]
			if !ok {
				reply = "% Error: unknown command (try \"help\")\n"
			}
			_, _ = conn.Write([]byte(reply))
			conn.Close()
		}
	}()
	return socket, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(cmds)
	}
}

func TestNanoNOSDriver(t *testing.T) {
	socket, got := fakeSwitch(t, map[string]string{
		"show version json":               `{"version":"0.1.0","hostname":"leaf1","uptime_s":12}` + "\n",
		"show bgp summary json":           `[{"neighbor":"10.0.0.1","remote_as":65002,"state":"Established","uptime_s":9,"prefixes_received":3,"prefixes_sent":1}]` + "\n",
		"show lldp neighbors json":        `[{"local_port":"Ethernet1","system_name":"leaf2","port_id":"Ethernet0","chassis_id":"02:00:00:00:00:02","ttl":8}]` + "\n",
		"interface Ethernet1 shutdown":    "ok\n",
		"interface Ethernet1 no shutdown": "ok\n",
	})
	sw := &NanoNOS{Socket: socket}
	ctx := context.Background()

	info, err := sw.Info(ctx)
	if err != nil || info != (Info{OS: "NanoNOS", Version: "0.1.0", Hostname: "leaf1"}) {
		t.Fatalf("Info = %+v, %v", info, err)
	}
	peers, err := sw.BGPPeers(ctx)
	if err != nil || len(peers) != 1 || !peers[0].Established() || peers[0].PrefixesReceived != 3 || peers[0].RemoteAS != 65002 {
		t.Fatalf("BGPPeers = %+v, %v", peers, err)
	}
	nbrs, err := sw.LLDPNeighbors(ctx)
	if err != nil || len(nbrs) != 1 || nbrs[0] != (Neighbor{LocalPort: "Ethernet1", SystemName: "leaf2", PortID: "Ethernet0"}) {
		t.Fatalf("LLDPNeighbors = %+v, %v", nbrs, err)
	}
	if err := sw.SetInterfaceAdmin(ctx, "Ethernet1", false); err != nil {
		t.Fatal(err)
	}
	if err := sw.SetInterfaceAdmin(ctx, "Ethernet1", true); err != nil {
		t.Fatal(err)
	}
	if err := sw.SetInterfaceAdmin(ctx, "Ethernet9", true); err == nil || errors.Is(err, ErrUnreachable) {
		t.Errorf("unknown interface: err = %v, want a command error", err)
	}
	if c := got(); !slices.Contains(c, "interface Ethernet1 shutdown") || !slices.Contains(c, "interface Ethernet1 no shutdown") {
		t.Errorf("commands sent: %q", c)
	}
}

func TestNanoNOSUnreachable(t *testing.T) {
	sw := &NanoNOS{Socket: filepath.Join(t.TempDir(), "missing.sock"), Timeout: time.Second}
	if _, err := sw.Info(context.Background()); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
}

func TestFRRDriver(t *testing.T) {
	var calls [][]string
	replies := map[string]string{
		"show version": "FRRouting 8.4.4 (frr1) on Linux(6.8.0-1017-azure).\nCopyright 1996-2005 Kunihiro Ishiguro, et al.\n",
		"show bgp ipv4 unicast summary json": `{"routerId":"10.255.0.2","as":65002,"peers":{
			"10.0.0.0":{"remoteAs":4200000001,"state":"Established","pfxRcd":1},
			"10.0.2.0":{"remoteAs":65003,"state":"Active","pfxRcd":0}}}`,
	}
	frr := &FRR{Netns: "frr", VtySocket: "/tmp/frr", Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		cmd := args[len(args)-1]
		if r, ok := replies[cmd]; ok {
			return []byte(r), nil
		}
		return []byte(""), nil
	}}
	ctx := context.Background()
	info, err := frr.Info(ctx)
	if err != nil || info != (Info{OS: "FRRouting", Version: "8.4.4", Hostname: "frr1"}) {
		t.Fatalf("Info = %+v, %v", info, err)
	}
	want := []string{"ip", "netns", "exec", "frr", "vtysh", "--vty_socket", "/tmp/frr", "-c", "show version"}
	if !slices.Equal(calls[0], want) {
		t.Errorf("command = %q, want %q", calls[0], want)
	}
	peers, err := frr.BGPPeers(ctx)
	if err != nil || len(peers) != 2 || peers[0].Address != "10.0.0.0" || !peers[0].Established() || peers[1].Established() {
		t.Fatalf("BGPPeers = %+v, %v", peers, err)
	}
	if err := frr.SetInterfaceAdmin(ctx, "eth0", false); err != nil {
		t.Fatal(err)
	}
	if got := calls[len(calls)-1]; !slices.Equal(got[len(got)-6:], []string{"-c", "configure terminal", "-c", "interface eth0", "-c", "shutdown"}) {
		t.Errorf("shutdown command = %q", got)
	}
	if _, err := frr.LLDPNeighbors(ctx); !errors.Is(err, ErrUnsupported) {
		t.Errorf("LLDP: %v, want ErrUnsupported", err)
	}
}

func TestFRRSummaryNestedUnderAddressFamily(t *testing.T) {
	peers, err := parseFRRSummary([]byte(`{"ipv4Unicast":{"peers":{"10.0.0.0":{"remoteAs":1,"state":"Established","pfxRcd":2}}}}`))
	if err != nil || len(peers) != 1 || peers[0].PrefixesReceived != 2 {
		t.Fatalf("peers = %+v, %v", peers, err)
	}
}

func TestFRRDownIsUnreachable(t *testing.T) {
	frr := &FRR{Run: func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("exit status 1: bgpd is not running")
	}}
	if _, err := frr.BGPPeers(context.Background()); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
}

func TestSimLabSessionsFollowPortState(t *testing.T) {
	now := time.Unix(1000, 0)
	lab := NewSimLab(func() time.Time { return now })
	lab.AddDevice("a", "SimOS", 65001, true)
	lab.AddDevice("b", "SimOS", 65002, true)
	lab.Connect("a", "p1", "10.0.0.0", "b", "p1", "10.0.0.1")
	a, b := lab.Device("a"), lab.Device("b")
	ctx := context.Background()

	peers, _ := a.BGPPeers(ctx)
	if p, ok := Peer(peers, "10.0.0.1"); !ok || !p.Established() || p.RemoteAS != 65002 {
		t.Fatalf("initial session: %+v", peers)
	}
	nbrs, _ := b.LLDPNeighbors(ctx)
	if len(nbrs) != 1 || nbrs[0].SystemName != "a" || nbrs[0].PortID != "p1" {
		t.Fatalf("LLDP = %+v", nbrs)
	}

	_ = a.SetInterfaceAdmin(ctx, "p1", false)
	peers, _ = b.BGPPeers(ctx)
	if p, _ := Peer(peers, "10.0.0.0"); p.Established() {
		t.Fatal("session should drop when a port goes down")
	}
	_ = a.SetInterfaceAdmin(ctx, "p1", true)
	now = now.Add(time.Second)
	if peers, _ = b.BGPPeers(ctx); peers[0].Established() {
		t.Fatal("session should take Converge to come back")
	}
	now = now.Add(2 * time.Second)
	if peers, _ = b.BGPPeers(ctx); !peers[0].Established() {
		t.Fatal("session should be back after Converge")
	}

	lab.SetUnreachable("a", true)
	if _, err := a.Info(ctx); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("Info on an unreachable device: %v", err)
	}
	if peers, _ = b.BGPPeers(ctx); peers[0].Established() {
		t.Fatal("the peer of an unreachable device must see the session down")
	}
}
