// Package device gives every network device in the lab the same API, whatever operating system
// it runs. The scheduler and the qualification tests only use Device; drivers translate to each
// operating system's own interface (a CLI socket, vtysh, or a simulation).
package device

import (
	"context"
	"errors"
)

var (
	// ErrUnreachable means the device did not answer. The lab treats it as an infrastructure
	// failure of the device, not as a failure of the feature under test.
	ErrUnreachable = errors.New("device unreachable")
	// ErrUnsupported means the device's operating system does not have the feature.
	ErrUnsupported = errors.New("not supported by this device")
)

// Info identifies a device's software.
type Info struct {
	OS       string `json:"os"`
	Version  string `json:"version"`
	Hostname string `json:"hostname"`
}

// BGPPeer is one BGP session as the device sees it.
type BGPPeer struct {
	Address          string `json:"address"`
	RemoteAS         uint32 `json:"remoteAs"`
	State            string `json:"state"`
	PrefixesReceived int    `json:"prefixesReceived"`
}

// Established reports whether the session is up.
func (p BGPPeer) Established() bool { return p.State == "Established" }

// Neighbor is a device seen through LLDP on a local port.
type Neighbor struct {
	LocalPort  string `json:"localPort"`
	SystemName string `json:"systemName"`
	PortID     string `json:"portId"`
}

// Device is the vendor-neutral device API.
type Device interface {
	Info(ctx context.Context) (Info, error)
	BGPPeers(ctx context.Context) ([]BGPPeer, error)
	LLDPNeighbors(ctx context.Context) ([]Neighbor, error)
	// SetInterfaceAdmin brings a port administratively up or down.
	SetInterfaceAdmin(ctx context.Context, port string, up bool) error
}

// Peer returns the session with the given neighbor address.
func Peer(peers []BGPPeer, address string) (BGPPeer, bool) {
	for _, p := range peers {
		if p.Address == address {
			return p, true
		}
	}
	return BGPPeer{}, false
}
