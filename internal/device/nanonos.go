package device

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// NanoNOS drives a NanoNOS switch through its CLI socket. Each command uses one connection: the
// driver writes a line and reads the one-line reply (compact JSON, "ok", or "% Error ..."). It
// does not wait for the switch to close the connection, so it never depends on close timing.
type NanoNOS struct {
	Socket  string
	Timeout time.Duration // per command; default 5 s
}

func (n *NanoNOS) run(ctx context.Context, cmd string) ([]byte, error) {
	timeout := n.Timeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", n.Socket)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)
	if _, err := io.WriteString(conn, cmd+"\n"); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	out, err := bufio.NewReaderSize(conn, 64<<10).ReadBytes('\n')
	if err != nil && (err != io.EOF || len(out) == 0) {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	if bytes.HasPrefix(out, []byte("% Error")) {
		return nil, fmt.Errorf("nanonos %q: %s", cmd, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (n *NanoNOS) runJSON(ctx context.Context, cmd string, v any) error {
	out, err := n.run(ctx, cmd)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(out, v); err != nil {
		return fmt.Errorf("nanonos %q: unexpected reply: %v", cmd, err)
	}
	return nil
}

func (n *NanoNOS) Info(ctx context.Context) (Info, error) {
	var v struct {
		Version  string `json:"version"`
		Hostname string `json:"hostname"`
	}
	if err := n.runJSON(ctx, "show version json", &v); err != nil {
		return Info{}, err
	}
	return Info{OS: "NanoNOS", Version: v.Version, Hostname: v.Hostname}, nil
}

func (n *NanoNOS) BGPPeers(ctx context.Context) ([]BGPPeer, error) {
	var v []struct {
		Neighbor         string `json:"neighbor"`
		RemoteAS         uint32 `json:"remote_as"`
		State            string `json:"state"`
		PrefixesReceived int    `json:"prefixes_received"`
	}
	if err := n.runJSON(ctx, "show bgp summary json", &v); err != nil {
		return nil, err
	}
	peers := make([]BGPPeer, len(v))
	for i, p := range v {
		peers[i] = BGPPeer{Address: p.Neighbor, RemoteAS: p.RemoteAS, State: p.State, PrefixesReceived: p.PrefixesReceived}
	}
	return peers, nil
}

func (n *NanoNOS) LLDPNeighbors(ctx context.Context) ([]Neighbor, error) {
	var v []struct {
		LocalPort  string `json:"local_port"`
		SystemName string `json:"system_name"`
		PortID     string `json:"port_id"`
	}
	if err := n.runJSON(ctx, "show lldp neighbors json", &v); err != nil {
		return nil, err
	}
	out := make([]Neighbor, len(v))
	for i, x := range v {
		out[i] = Neighbor{LocalPort: x.LocalPort, SystemName: x.SystemName, PortID: x.PortID}
	}
	return out, nil
}

func (n *NanoNOS) SetInterfaceAdmin(ctx context.Context, port string, up bool) error {
	cmd := "interface " + port + " shutdown"
	if up {
		cmd = "interface " + port + " no shutdown"
	}
	_, err := n.run(ctx, cmd)
	return err
}
