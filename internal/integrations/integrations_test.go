package integrations

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yasaswinivardhireddy-png/labq/internal/lab"
)

func TestWebhookSignature(t *testing.T) {
	secret, body := []byte("s3cret"), []byte(`{"zen":"Keep it logically awesome."}`)
	sig := Sign(secret, body)
	if !VerifySignature(secret, body, sig) {
		t.Fatal("a correct signature was rejected")
	}
	for _, bad := range []string{"", "sha1=abc", "sha256=zz", Sign([]byte("other"), body), Sign(secret, append(body, ' '))} {
		if VerifySignature(secret, body, bad) {
			t.Errorf("signature %q was accepted", bad)
		}
	}
	if VerifySignature(nil, body, sig) {
		t.Error("a delivery was accepted without a configured secret")
	}
}

func TestParseWebhook(t *testing.T) {
	push := `{"ref":"refs/heads/main","after":"0123456789abcdef0123456789abcdef01234567","repository":{"full_name":"acme/nos"},"head_commit":{"url":"https://github.com/acme/nos/commit/0123456"}}`
	src, ok, err := ParseWebhook("push", []byte(push))
	if err != nil || !ok || src.Kind != "push" || src.Repo != "acme/nos" || src.Ref != "refs/heads/main" || !strings.HasPrefix(src.SHA, "0123456") {
		t.Fatalf("push = %+v, %v, %v", src, ok, err)
	}
	pr := `{"action":"synchronize","number":42,"repository":{"full_name":"acme/nos"},"pull_request":{"html_url":"https://github.com/acme/nos/pull/42","head":{"sha":"fedcba9876543210fedcba9876543210fedcba98","ref":"fix-bgp"}}}`
	src, ok, err = ParseWebhook("pull_request", []byte(pr))
	if err != nil || !ok || src.PR != 42 || src.Ref != "fix-bgp" || src.Kind != "pull_request" {
		t.Fatalf("pull_request = %+v, %v, %v", src, ok, err)
	}
	for event, body := range map[string]string{
		"ping":         `{"zen":"hi"}`,
		"push":         `{"ref":"refs/heads/old","after":"0000000000000000000000000000000000000000","deleted":true,"repository":{"full_name":"acme/nos"}}`,
		"pull_request": `{"action":"closed","number":42,"repository":{"full_name":"acme/nos"},"pull_request":{"head":{"sha":"fedcba9"}}}`,
	} {
		if _, ok, err := ParseWebhook(event, []byte(body)); ok || err != nil {
			t.Errorf("%s: ok=%v err=%v, want ignored", event, ok, err)
		}
	}
	if _, _, err := ParseWebhook("push", []byte(`{not json`)); err == nil {
		t.Error("invalid JSON accepted")
	}
}

func TestSetStatusRetriesServerErrors(t *testing.T) {
	var calls atomic.Int32
	var got map[string]string
	var auth, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "upstream hiccup", http.StatusBadGateway)
			return
		}
		auth, path = r.Header.Get("Authorization"), r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	gh := &GitHub{API: srv.URL, Token: "t0k3n"}
	err := gh.SetStatus(context.Background(), "acme/nos", "abc1234", "success", strings.Repeat("x", 200), "https://lab.example/jobs/j-1")
	if err != nil || calls.Load() != 2 {
		t.Fatalf("err = %v after %d calls", err, calls.Load())
	}
	if auth != "Bearer t0k3n" || path != "/repos/acme/nos/statuses/abc1234" {
		t.Errorf("auth %q, path %q", auth, path)
	}
	if got["state"] != "success" || got["context"] != "labq/qualification" || len(got["description"]) != 140 || got["target_url"] == "" {
		t.Errorf("body = %v", got)
	}
}

func TestSetStatusDoesNotRetryClientErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, `{"message":"Resource not accessible by integration"}`, http.StatusForbidden)
	}))
	defer srv.Close()
	err := (&GitHub{API: srv.URL}).SetStatus(context.Background(), "acme/nos", "abc1234", "success", "ok", "")
	if err == nil || calls.Load() != 1 || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v after %d calls", err, calls.Load())
	}
}

func sampleJobs() ([]*lab.Job, map[string]*lab.Device) {
	t0 := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	j := &lab.Job{
		JobSpec: lab.JobSpec{Name: "interop", Team: "wan", Priority: 1,
			Roles: []lab.Role{{Name: "dut"}, {Name: "peer"}}, Source: &lab.Source{Kind: "push", Repo: "acme/nos", SHA: "abc1234"}},
		ID: "j-1", State: lab.Passed, Attempts: 1, Submitted: t0, Started: t0.Add(5 * time.Second), Ended: t0.Add(65 * time.Second),
		Assignment: map[string]string{"dut": "n1", "peer": "f1"},
		Results: []lab.TestResult{
			{Test: "bgp-established", Passed: true, DurationMS: 1500, Measurements: map[string]float64{"wait_s": 1.5}},
			{Test: "link-failover", Passed: true, DurationMS: 9000, Measurements: map[string]float64{"recover_s": 4.25, "detect_s": 2.9}},
		},
	}
	devs := map[string]*lab.Device{"n1": {ID: "n1", Vendor: "nanonos"}, "f1": {ID: "f1", Vendor: "frr"}}
	return []*lab.Job{j}, devs
}

func TestResultsCSV(t *testing.T) {
	jobs, devs := sampleJobs()
	var buf bytes.Buffer
	if err := ResultsCSV(&buf, jobs, devs); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(&buf).ReadAll()
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows = %v, %v", rows, err)
	}
	row := map[string]string{}
	for i, h := range rows[0] {
		row[h] = rows[2][i]
	}
	want := map[string]string{"test": "link-failover", "devices": "dut=n1;peer=f1", "vendors": "nanonos;frr",
		"measurements": "detect_s=2.900;recover_s=4.250", "repo": "acme/nos", "wait_s": "5.0", "started_at": "2026-11-12T09:00:05Z"}
	for k, v := range want {
		if row[k] != v {
			t.Errorf("%s = %q, want %q", k, row[k], v)
		}
	}
}

func TestJobsNDJSON(t *testing.T) {
	jobs, devs := sampleJobs()
	var buf bytes.Buffer
	if err := JobsNDJSON(&buf, jobs, devs); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d lines", len(lines))
	}
	var v struct {
		ID      string  `json:"id"`
		WaitS   float64 `json:"wait_s"`
		RunS    float64 `json:"run_s"`
		Devices []struct{ Role, Device, Vendor string }
		Tests   []struct {
			Test         string
			Measurements []struct {
				Name  string
				Value float64
			}
		}
	}
	if err := json.NewDecoder(io.LimitReader(strings.NewReader(lines[0]), 1<<20)).Decode(&v); err != nil {
		t.Fatal(err)
	}
	if v.ID != "j-1" || v.WaitS != 5 || v.RunS != 60 || len(v.Devices) != 2 || v.Devices[1].Vendor != "frr" || len(v.Tests) != 2 {
		t.Fatalf("decoded %+v", v)
	}
	if m := v.Tests[1].Measurements; len(m) != 2 || m[0].Name != "detect_s" || m[0].Value != 2.9 || m[1].Name != "recover_s" {
		t.Fatalf("measurements %+v, want detect_s and recover_s as sorted name/value pairs", m)
	}
}

// TestJobsNDJSONMatchesBigQuerySchema checks every exported row against bigquery_schema.json, the
// schema the README loads the export with, the way a BigQuery load job would.
func TestJobsNDJSONMatchesBigQuerySchema(t *testing.T) {
	raw, err := os.ReadFile("bigquery_schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema []bqField
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	jobs, devs := sampleJobs()
	// A job that has not started has no devices, results, start time or source.
	queued := &lab.Job{JobSpec: lab.JobSpec{Name: "waiting", Team: "wan", Roles: []lab.Role{{Name: "dut"}}},
		ID: "j-2", State: lab.Queued, Submitted: time.Date(2026, 11, 12, 9, 5, 0, 0, time.UTC)}
	var buf bytes.Buffer
	if err := JobsNDJSON(&buf, append(jobs, queued), devs); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(&buf)
	dec.UseNumber()
	rows := 0
	for dec.More() {
		var row map[string]any
		if err := dec.Decode(&row); err != nil {
			t.Fatal(err)
		}
		rows++
		for _, p := range checkRecord(schema, row, "") {
			t.Errorf("row %d: %s", rows, p)
		}
	}
	if rows != 2 {
		t.Fatalf("got %d rows, want 2", rows)
	}
}

type bqField struct {
	Name   string    `json:"name"`
	Type   string    `json:"type"`
	Mode   string    `json:"mode"`
	Fields []bqField `json:"fields"`
}

func TestSchemaCheckCatchesBadRows(t *testing.T) {
	schema := []bqField{
		{Name: "started_at", Type: "TIMESTAMP"},
		{Name: "tests", Type: "RECORD", Mode: "REPEATED", Fields: []bqField{
			{Name: "measurements", Type: "RECORD", Mode: "REPEATED", Fields: []bqField{{Name: "name", Type: "STRING", Mode: "REQUIRED"}}},
		}},
	}
	// The export's earlier shape: an empty string for a time that has not happened, and
	// measurements as an object whose keys differ from test to test.
	var row map[string]any
	dec := json.NewDecoder(strings.NewReader(`{"started_at": "", "tests": [{"measurements": {"detect_s": 2.9}}], "extra": 1}`))
	dec.UseNumber()
	if err := dec.Decode(&row); err != nil {
		t.Fatal(err)
	}
	if p := checkRecord(schema, row, ""); len(p) != 3 {
		t.Fatalf("problems = %q, want 3 (started_at, tests[0].measurements, extra)", p)
	}
}

// checkRecord lists the ways a JSON object breaks a BigQuery schema. It is stricter than BigQuery
// in one way: repeated fields must be arrays, never missing or null.
func checkRecord(fields []bqField, obj map[string]any, path string) []string {
	var problems []string
	known := map[string]bool{}
	for _, f := range fields {
		known[f.Name] = true
		v, present := obj[f.Name]
		name := path + f.Name
		switch {
		case f.Mode == "REPEATED":
			arr, ok := v.([]any)
			if !ok {
				problems = append(problems, fmt.Sprintf("%s: want an array, got %v", name, v))
				continue
			}
			for i, e := range arr {
				problems = append(problems, checkValue(f, e, fmt.Sprintf("%s[%d]", name, i))...)
			}
		case !present || v == nil:
			if f.Mode == "REQUIRED" {
				problems = append(problems, name+": required but missing")
			}
		default:
			problems = append(problems, checkValue(f, v, name)...)
		}
	}
	for k := range obj {
		if !known[k] {
			problems = append(problems, path+k+": not in the schema")
		}
	}
	return problems
}

func checkValue(f bqField, v any, name string) []string {
	ok := false
	switch f.Type {
	case "STRING":
		_, ok = v.(string)
	case "TIMESTAMP":
		if s, isString := v.(string); isString {
			_, err := time.Parse(time.RFC3339, s)
			ok = err == nil
		}
	case "INTEGER":
		if n, isNumber := v.(json.Number); isNumber {
			_, err := n.Int64()
			ok = err == nil
		}
	case "FLOAT":
		_, ok = v.(json.Number)
	case "BOOLEAN":
		_, ok = v.(bool)
	case "RECORD":
		if obj, isObject := v.(map[string]any); isObject {
			return checkRecord(f.Fields, obj, name+".")
		}
	}
	if !ok {
		return []string{fmt.Sprintf("%s: %v is not a %s", name, v, f.Type)}
	}
	return nil
}
