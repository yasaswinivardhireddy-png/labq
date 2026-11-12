package lab

import (
	"cmp"
	"slices"
)

// Match assigns each role a distinct device that has the role's vendor and capabilities and
// passes eligible, such that every cable the job needs exists between the assigned devices.
//
// This is a small subgraph-matching problem. Backtracking tries the most constrained role first
// (the one with the fewest candidate devices), checks cables as soon as both ends are placed,
// and prefers devices with fewer capabilities so versatile devices stay free for jobs that need
// them. Labs have tens of devices, and jobs a handful of roles, so this is fast.
func Match(roles []Role, cables []Cable, devices []*Device, connected func(a, b string) bool, eligible func(*Device) bool) (map[string]string, bool) {
	cands := make([][]*Device, len(roles))
	for i, r := range roles {
		for _, d := range devices {
			if (r.Vendor == "" || r.Vendor == d.Vendor) && d.Has(r.Capabilities) && eligible(d) {
				cands[i] = append(cands[i], d)
			}
		}
		if len(cands[i]) == 0 {
			return nil, false
		}
		slices.SortStableFunc(cands[i], func(a, b *Device) int {
			if c := cmp.Compare(len(a.Capabilities), len(b.Capabilities)); c != 0 {
				return c
			}
			return cmp.Compare(a.JobsRun, b.JobsRun)
		})
	}
	order := make([]int, len(roles))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return cmp.Compare(len(cands[a]), len(cands[b])) })

	index := map[string]int{}
	for i, r := range roles {
		index[r.Name] = i
	}
	chosen := make([]*Device, len(roles))
	used := map[string]bool{}
	var place func(k int) bool
	place = func(k int) bool {
		if k == len(order) {
			return true
		}
		i := order[k]
		for _, d := range cands[i] {
			if used[d.ID] || !cablesOK(i, d, roles, cables, index, chosen, connected) {
				continue
			}
			chosen[i], used[d.ID] = d, true
			if place(k + 1) {
				return true
			}
			chosen[i], used[d.ID] = nil, false
		}
		return false
	}
	if !place(0) {
		return nil, false
	}
	out := make(map[string]string, len(roles))
	for i, r := range roles {
		out[r.Name] = chosen[i].ID
	}
	return out, true
}

// cablesOK checks the cables between role i (about to get device d) and roles already placed.
func cablesOK(i int, d *Device, roles []Role, cables []Cable, index map[string]int, chosen []*Device, connected func(a, b string) bool) bool {
	for _, c := range cables {
		var other int
		switch roles[i].Name {
		case c.A:
			other = index[c.B]
		case c.B:
			other = index[c.A]
		default:
			continue
		}
		if o := chosen[other]; o != nil && !connected(d.ID, o.ID) {
			return false
		}
	}
	return true
}
