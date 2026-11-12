package server

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/yasaswinivardhireddy-png/labq/internal/device"
	"github.com/yasaswinivardhireddy-png/labq/internal/lab"
)

// Config describes a lab: its devices and how to reach them, the cables between them, the teams
// that use it, and reusable job presets.
type Config struct {
	Devices []DeviceConfig    `json:"devices"`
	Links   []lab.Link        `json:"links"`
	Teams   []lab.Team        `json:"teams"`
	Presets map[string]Preset `json:"presets"`
}

// DeviceConfig is a device and the driver that reaches it.
type DeviceConfig struct {
	ID           string       `json:"id"`
	Vendor       string       `json:"vendor"`
	Model        string       `json:"model"`
	Capabilities []string     `json:"capabilities"`
	Driver       DriverConfig `json:"driver"`
}

// DriverConfig selects and configures a device driver.
type DriverConfig struct {
	Kind        string `json:"kind"`                  // nanonos, frr or sim
	Socket      string `json:"socket,omitempty"`      // nanonos: CLI socket path
	Netns       string `json:"netns,omitempty"`       // frr: network namespace
	VtySocket   string `json:"vtySocket,omitempty"`   // frr: vtysh socket directory
	ASN         uint32 `json:"asn,omitempty"`         // sim: BGP AS number
	Unreachable bool   `json:"unreachable,omitempty"` // sim: start broken, to demonstrate quarantine
}

// Preset is a reusable job template, for the submit form and for GitHub-triggered jobs.
type Preset struct {
	Description string      `json:"description"`
	Roles       []lab.Role  `json:"roles"`
	Cables      []lab.Cable `json:"cables"`
	Tests       []string    `json:"tests"`
	TimeoutSec  int         `json:"timeoutSec"`
}

// LoadConfig reads a lab configuration file.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	return ParseConfig(data)
}

// ParseConfig parses a lab configuration.
func ParseConfig(data []byte) (Config, error) {
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("lab config: %w", err)
	}
	ids := map[string]bool{}
	for _, d := range c.Devices {
		if d.ID == "" || ids[d.ID] {
			return c, fmt.Errorf("lab config: device IDs must be unique and non-empty (%q)", d.ID)
		}
		ids[d.ID] = true
		switch d.Driver.Kind {
		case "nanonos", "frr", "sim":
		default:
			return c, fmt.Errorf("lab config: device %s has unknown driver %q", d.ID, d.Driver.Kind)
		}
	}
	for _, l := range c.Links {
		if !ids[l.A.Device] || !ids[l.B.Device] {
			return c, fmt.Errorf("lab config: link %s-%s names an unknown device", l.A.Device, l.B.Device)
		}
	}
	if len(c.Teams) == 0 {
		return c, fmt.Errorf("lab config: define at least one team")
	}
	return c, nil
}

// drivers builds a driver for every device. Simulated devices share one simulated lab, cabled
// like the configuration says.
func (c Config) drivers(converge time.Duration) map[string]device.Device {
	out := map[string]device.Device{}
	sim := device.NewSimLab(nil)
	if converge > 0 {
		sim.Converge = converge
	}
	isSim := map[string]bool{}
	for _, d := range c.Devices {
		switch d.Driver.Kind {
		case "nanonos":
			out[d.ID] = &device.NanoNOS{Socket: d.Driver.Socket}
		case "frr":
			out[d.ID] = &device.FRR{Netns: d.Driver.Netns, VtySocket: d.Driver.VtySocket}
		case "sim":
			sim.AddDevice(d.ID, d.Model, d.Driver.ASN, slices.Contains(d.Capabilities, "lldp"))
			if d.Driver.Unreachable {
				sim.SetUnreachable(d.ID, true)
			}
			out[d.ID] = sim.Device(d.ID)
			isSim[d.ID] = true
		}
	}
	for _, l := range c.Links {
		if isSim[l.A.Device] && isSim[l.B.Device] {
			sim.Connect(l.A.Device, l.A.Port, l.A.IP, l.B.Device, l.B.Port, l.B.IP)
		}
	}
	return out
}
