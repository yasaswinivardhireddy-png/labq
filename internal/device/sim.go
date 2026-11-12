package device

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sync"
	"time"
)

// SimLab simulates devices and the cables between them, for demos and tests. A BGP session on a
// cable comes up Converge after both ends are up, and drops as soon as either end goes down.
type SimLab struct {
	mu       sync.Mutex
	now      func() time.Time
	Converge time.Duration
	devices  map[string]*simDevice
	links    []simLink
}

type simDevice struct {
	hostname, os string
	asn          uint32
	lldp         bool
	unreachable  bool
	ports        map[string]bool // admin state; ports start up
	changed      map[string]time.Time
}

type simEnd struct{ dev, port, ip string }

type simLink struct{ a, b simEnd }

// NewSimLab returns an empty simulated lab. now may be nil (wall clock).
func NewSimLab(now func() time.Time) *SimLab {
	if now == nil {
		now = time.Now
	}
	return &SimLab{now: now, Converge: 2 * time.Second, devices: map[string]*simDevice{}}
}

// AddDevice adds a device running osName with BGP AS asn, optionally speaking LLDP.
func (l *SimLab) AddDevice(id, osName string, asn uint32, lldp bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.devices[id] = &simDevice{hostname: id, os: osName, asn: asn, lldp: lldp, ports: map[string]bool{}, changed: map[string]time.Time{}}
}

// Connect cables two ports, with a BGP session between the given addresses.
func (l *SimLab) Connect(a, aPort, aIP, b, bPort, bIP string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.links = append(l.links, simLink{simEnd{a, aPort, aIP}, simEnd{b, bPort, bIP}})
}

// SetUnreachable makes a device stop (or resume) answering.
func (l *SimLab) SetUnreachable(id string, v bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.devices[id].unreachable = v
}

// Device returns the driver for a simulated device.
func (l *SimLab) Device(id string) Device { return &SimDevice{lab: l, id: id} }

// SimDevice is a driver for a device in a SimLab.
type SimDevice struct {
	lab *SimLab
	id  string
}

func (d *SimDevice) get() (*simDevice, error) {
	dev, ok := d.lab.devices[d.id]
	if !ok {
		return nil, fmt.Errorf("%w: no simulated device %q", ErrUnreachable, d.id)
	}
	if dev.unreachable {
		return nil, fmt.Errorf("%w: %s does not answer", ErrUnreachable, d.id)
	}
	return dev, nil
}

func (l *SimLab) portUp(e simEnd) bool {
	up, set := l.devices[e.dev].ports[e.port]
	return !set || up
}

// sessionUp reports whether the BGP session on a link is established at the current time.
func (l *SimLab) sessionUp(k simLink) bool {
	if !l.portUp(k.a) || !l.portUp(k.b) || l.devices[k.a.dev].unreachable || l.devices[k.b.dev].unreachable {
		return false
	}
	last := l.devices[k.a.dev].changed[k.a.port]
	if t := l.devices[k.b.dev].changed[k.b.port]; t.After(last) {
		last = t
	}
	return last.IsZero() || l.now().Sub(last) >= l.Converge
}

func (d *SimDevice) Info(context.Context) (Info, error) {
	d.lab.mu.Lock()
	defer d.lab.mu.Unlock()
	dev, err := d.get()
	if err != nil {
		return Info{}, err
	}
	return Info{OS: dev.os, Version: "sim-1.0", Hostname: dev.hostname}, nil
}

func (d *SimDevice) BGPPeers(context.Context) ([]BGPPeer, error) {
	d.lab.mu.Lock()
	defer d.lab.mu.Unlock()
	if _, err := d.get(); err != nil {
		return nil, err
	}
	var peers []BGPPeer
	for _, k := range d.lab.links {
		remote := k.b
		if k.b.dev == d.id {
			remote = k.a
		} else if k.a.dev != d.id {
			continue
		}
		p := BGPPeer{Address: remote.ip, RemoteAS: d.lab.devices[remote.dev].asn, State: "Active"}
		if d.lab.sessionUp(k) {
			p.State, p.PrefixesReceived = "Established", 1
		}
		peers = append(peers, p)
	}
	sortPeers(peers)
	return peers, nil
}

func (d *SimDevice) LLDPNeighbors(context.Context) ([]Neighbor, error) {
	d.lab.mu.Lock()
	defer d.lab.mu.Unlock()
	dev, err := d.get()
	if err != nil {
		return nil, err
	}
	if !dev.lldp {
		return nil, ErrUnsupported
	}
	var out []Neighbor
	for _, k := range d.lab.links {
		local, remote := k.a, k.b
		if k.b.dev == d.id {
			local, remote = k.b, k.a
		} else if k.a.dev != d.id {
			continue
		}
		if d.lab.devices[remote.dev].lldp && d.lab.portUp(local) && d.lab.portUp(remote) {
			out = append(out, Neighbor{LocalPort: local.port, SystemName: d.lab.devices[remote.dev].hostname, PortID: remote.port})
		}
	}
	return out, nil
}

func (d *SimDevice) SetInterfaceAdmin(_ context.Context, port string, up bool) error {
	d.lab.mu.Lock()
	defer d.lab.mu.Unlock()
	dev, err := d.get()
	if err != nil {
		return err
	}
	if cur, set := dev.ports[port]; (!set || cur) != up {
		dev.changed[port] = d.lab.now()
	}
	dev.ports[port] = up
	return nil
}

func sortPeers(p []BGPPeer) {
	slices.SortFunc(p, func(a, b BGPPeer) int { return cmp.Compare(a.Address, b.Address) })
}
