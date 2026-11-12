package integrations

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"maps"
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

// JobsNDJSON writes one JSON object per job, one per line: newline-delimited JSON that loads into
// BigQuery with the schema in bigquery_schema.json. Every row has that one shape: times that have
// not happened yet are null rather than empty strings, lists are empty rather than null, and
// measurements are name/value pairs, because their names differ from test to test.
func JobsNDJSON(w io.Writer, jobs []*lab.Job, devices map[string]*lab.Device) error {
	enc := json.NewEncoder(w)
	for _, j := range jobs {
		if err := enc.Encode(newJobRow(j, devices)); err != nil {
			return err
		}
	}
	return nil
}

type jobRow struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Team        string       `json:"team"`
	Priority    int          `json:"priority"`
	State       lab.JobState `json:"state"`
	Attempts    int          `json:"attempts"`
	SubmittedAt *string      `json:"submitted_at"`
	StartedAt   *string      `json:"started_at"`
	EndedAt     *string      `json:"ended_at"`
	WaitS       *float64     `json:"wait_s"`
	RunS        *float64     `json:"run_s"`
	Devices     []deviceRow  `json:"devices"`
	Tests       []testRow    `json:"tests"`
	Source      *lab.Source  `json:"source"`
}

type deviceRow struct {
	Role   string `json:"role"`
	Device string `json:"device"`
	Vendor string `json:"vendor"`
}

type testRow struct {
	Test         string           `json:"test"`
	Passed       bool             `json:"passed"`
	Infra        bool             `json:"infra"`
	Message      string           `json:"message"`
	DurationMS   int64            `json:"duration_ms"`
	Measurements []measurementRow `json:"measurements"`
}

type measurementRow struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
}

func newJobRow(j *lab.Job, devices map[string]*lab.Device) jobRow {
	row := jobRow{
		ID: j.ID, Name: j.Name, Team: j.Team, Priority: j.Priority, State: j.State, Attempts: j.Attempts,
		SubmittedAt: timestamp(j.Submitted), StartedAt: timestamp(j.Started), EndedAt: timestamp(j.Ended),
		Devices: []deviceRow{}, Tests: []testRow{}, Source: j.Source,
	}
	if !j.Started.IsZero() {
		wait := seconds(j.Started.Sub(j.Submitted))
		row.WaitS = &wait
		if !j.Ended.IsZero() {
			run := seconds(j.Ended.Sub(j.Started))
			row.RunS = &run
		}
	}
	for _, r := range j.Roles {
		if id, ok := j.Assignment[r.Name]; ok {
			row.Devices = append(row.Devices, deviceRow{r.Name, id, vendorOf(devices, id)})
		}
	}
	for _, r := range j.Results {
		t := testRow{Test: r.Test, Passed: r.Passed, Infra: r.Infra, Message: r.Message, DurationMS: r.DurationMS,
			Measurements: []measurementRow{}}
		for _, k := range slices.Sorted(maps.Keys(r.Measurements)) {
			t.Measurements = append(t.Measurements, measurementRow{k, r.Measurements[k]})
		}
		row.Tests = append(row.Tests, t)
	}
	return row
}

// timestamp formats a time the way BigQuery loads it, or returns nil when the time is unset.
func timestamp(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

// seconds rounds a duration to whole milliseconds.
func seconds(d time.Duration) float64 { return d.Round(time.Millisecond).Seconds() }

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
