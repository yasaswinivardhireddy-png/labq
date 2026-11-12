package integrations

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/yasaswinivardhireddy-png/labq/internal/lab"
)

// ResultsCSV writes one row per test result: the flat table a BI tool or spreadsheet loads.
func ResultsCSV(w io.Writer, jobs []*lab.Job, devices map[string]*lab.Device) error {
	cw := csv.NewWriter(w)
	header := []string{"job_id", "job_name", "team", "priority", "job_state", "attempts", "test", "passed", "infra",
		"duration_ms", "devices", "vendors", "measurements", "repo", "sha", "submitted_at", "started_at", "ended_at", "wait_s"}
	if err := cw.Write(header); err != nil {
		return err
	}
	for _, j := range jobs {
		devs, vendors := assigned(j, devices)
		for _, r := range j.Results {
			repo, sha := "", ""
			if j.Source != nil {
				repo, sha = j.Source.Repo, j.Source.SHA
			}
			row := []string{j.ID, j.Name, j.Team, strconv.Itoa(j.Priority), string(j.State), strconv.Itoa(j.Attempts),
				r.Test, strconv.FormatBool(r.Passed), strconv.FormatBool(r.Infra), strconv.FormatInt(r.DurationMS, 10),
				devs, vendors, measurements(r.Measurements), repo, sha, stamp(j.Submitted), stamp(j.Started), stamp(j.Ended),
				waitSeconds(j)}
			if err := cw.Write(row); err != nil {
				return err
			}
		}
	}
	cw.Flush()
	return cw.Error()
}

// JobsNDJSON writes one JSON object per job, one per line: the newline-delimited JSON format that
// BigQuery and most warehouses load directly.
func JobsNDJSON(w io.Writer, jobs []*lab.Job, devices map[string]*lab.Device) error {
	enc := json.NewEncoder(w)
	for _, j := range jobs {
		type devRow struct {
			Role   string `json:"role"`
			Device string `json:"device"`
			Vendor string `json:"vendor"`
		}
		var devs []devRow
		for _, r := range j.Roles {
			if id, ok := j.Assignment[r.Name]; ok {
				devs = append(devs, devRow{r.Name, id, vendorOf(devices, id)})
			}
		}
		row := map[string]any{
			"id": j.ID, "name": j.Name, "team": j.Team, "priority": j.Priority, "state": j.State,
			"attempts": j.Attempts, "submitted_at": stamp(j.Submitted), "started_at": stamp(j.Started),
			"ended_at": stamp(j.Ended), "devices": devs, "tests": j.Results, "source": j.Source,
		}
		if s := waitSeconds(j); s != "" {
			row["wait_s"], _ = strconv.ParseFloat(s, 64)
		}
		if !j.Ended.IsZero() && !j.Started.IsZero() {
			row["run_s"] = j.Ended.Sub(j.Started).Seconds()
		}
		if err := enc.Encode(row); err != nil {
			return err
		}
	}
	return nil
}

func assigned(j *lab.Job, devices map[string]*lab.Device) (devs, vendors string) {
	var d, v []string
	for _, r := range j.Roles {
		if id, ok := j.Assignment[r.Name]; ok {
			d = append(d, r.Name+"="+id)
			if vn := vendorOf(devices, id); !slices.Contains(v, vn) {
				v = append(v, vn)
			}
		}
	}
	return strings.Join(d, ";"), strings.Join(v, ";")
}

func vendorOf(devices map[string]*lab.Device, id string) string {
	if d, ok := devices[id]; ok {
		return d.Vendor
	}
	return ""
}

func measurements(m map[string]float64) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%.3f", k, m[k])
	}
	return strings.Join(parts, ";")
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func waitSeconds(j *lab.Job) string {
	if j.Started.IsZero() {
		return ""
	}
	return strconv.FormatFloat(j.Started.Sub(j.Submitted).Seconds(), 'f', 1, 64)
}
