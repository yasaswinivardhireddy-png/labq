package server

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yasaswinivardhireddy-png/labq/internal/integrations"
	"github.com/yasaswinivardhireddy-png/labq/internal/lab"
)

const testConfig = `{
  "devices": [
    {"id": "sw1", "vendor": "acme", "model": "SimOS", "capabilities": ["bgp", "lldp", "admin-shutdown"], "driver": {"kind": "sim", "asn": 65001}},
    {"id": "sw2", "vendor": "acme", "model": "SimOS", "capabilities": ["bgp", "lldp", "admin-shutdown"], "driver": {"kind": "sim", "asn": 65002}},
    {"id": "dead", "vendor": "broken", "model": "SimOS", "capabilities": ["bgp"], "driver": {"kind": "sim", "asn": 65009, "unreachable": true}}
  ],
  "links": [
    {"a": {"device": "sw1", "port": "p1", "ip": "10.0.0.0"}, "b": {"device": "sw2", "port": "p1", "ip": "10.0.0.1"}}
  ],
  "teams": [{"name": "wan", "weight": 2}, {"name": "dc", "weight": 1}],
  "presets": {
    "fabric": {"description": "Two switches.", "timeoutSec": 30,
      "roles": [{"name": "dut", "vendor": "acme", "capabilities": ["bgp", "admin-shutdown"]}, {"name": "peer", "vendor": "acme", "capabilities": ["bgp"]}],
      "cables": [{"a": "dut", "b": "peer"}],
      "tests": ["reachable", "bgp-established", "routes-exchanged", "lldp-neighbors", "link-failover"]},
    "broken": {"description": "Needs the dead device.", "timeoutSec": 30,
      "roles": [{"name": "dut", "vendor": "broken"}], "tests": ["reachable"]}
  }
}`

type env struct {
	t   *testing.T
	svc *Service
	srv *httptest.Server
	wal string
}

func open(t *testing.T, walPath string, mutate func(*Options)) *env {
	t.Helper()
	cfg, err := ParseConfig([]byte(testConfig))
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{Config: cfg, WALPath: walPath, TestPoll: 10 * time.Millisecond, TestWait: 3 * time.Second, SimConverge: 100 * time.Millisecond}
	if mutate != nil {
		mutate(&opts)
	}
	svc, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	svc.Start()
	e := &env{t: t, svc: svc, srv: httptest.NewServer(svc.Handler()), wal: walPath}
	t.Cleanup(e.close)
	return e
}

func (e *env) close() {
	if e.srv != nil {
		e.srv.Close()
		e.srv = nil
		_ = e.svc.Close()
	}
}

func (e *env) do(method, path string, body any, header map[string]string) (*http.Response, []byte) {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	case []byte:
		rd = bytes.NewReader(b)
	default:
		data, _ := json.Marshal(b)
		rd = bytes.NewReader(data)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

// waitJob polls a job until it finishes.
func (e *env) waitJob(id string) lab.Job {
	e.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		_, body := e.do("GET", "/api/v1/jobs/"+id, nil, nil)
		var j lab.Job
		_ = json.Unmarshal(body, &j)
		if j.State.Done() {
			return j
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("job %s still %s: %s", id, j.State, body)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func submit(e *env, preset, name string) string {
	e.t.Helper()
	resp, body := e.do("POST", "/api/v1/jobs", map[string]any{"preset": preset, "name": name, "team": "wan", "priority": 1}, nil)
	if resp.StatusCode != http.StatusCreated {
		e.t.Fatalf("submit: %d %s", resp.StatusCode, body)
	}
	var j lab.Job
	_ = json.Unmarshal(body, &j)
	return j.ID
}

func TestJobRunsEveryTestOnMatchingDevices(t *testing.T) {
	e := open(t, filepath.Join(t.TempDir(), "lab.wal"), nil)
	j := e.waitJob(submit(e, "fabric", "fabric-1"))
	if j.State != lab.Passed || len(j.Results) != 5 || j.Assignment["dut"] == "" || j.Assignment["dut"] == j.Assignment["peer"] {
		t.Fatalf("job = %+v", j)
	}
	for _, r := range j.Results {
		if !r.Passed {
			t.Errorf("%s failed: %s", r.Test, r.Message)
		}
	}
	// Exports and metrics include the finished job.
	_, csvBody := e.do("GET", "/api/v1/export/results.csv", nil, nil)
	rows, err := csv.NewReader(bytes.NewReader(csvBody)).ReadAll()
	if err != nil || len(rows) != 6 {
		t.Fatalf("CSV rows = %d, %v", len(rows), err)
	}
	_, nd := e.do("GET", "/api/v1/export/jobs.ndjson", nil, nil)
	if n := strings.Count(strings.TrimSpace(string(nd)), "\n") + 1; n != 1 || !strings.Contains(string(nd), `"state":"passed"`) {
		t.Fatalf("NDJSON = %s", nd)
	}
	_, m := e.do("GET", "/metrics", nil, nil)
	for _, want := range []string{`labq_jobs{state="passed"} 1`, `labq_test_results_total{test="link-failover",result="passed"} 1`, `labq_devices{status="available"} 3`} {
		if !strings.Contains(string(m), want) {
			t.Errorf("metrics missing %q:\n%s", want, m)
		}
	}
}

// A job that was running when labq stopped is requeued from the write-ahead log and runs again.
func TestRestartRequeuesRunningJobs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lab.wal")
	first := open(t, path, func(o *Options) { o.TestWait = 10 * time.Second })
	// Shut the peer's port so the job waits for a session that never comes up, then stop labq.
	_ = first.svc.drivers["sw2"].SetInterfaceAdmin(t.Context(), "p1", false)
	id := submit(first, "fabric", "survives-restart")
	deadline := time.Now().Add(5 * time.Second)
	for {
		var state lab.JobState
		first.svc.View(func(st *lab.State) { state = st.Jobs[id].State })
		if state == lab.Running || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	first.close()

	second := open(t, path, nil)
	j := second.waitJob(id)
	if j.State != lab.Passed || j.Attempts != 2 {
		t.Fatalf("after restart: state %s, attempts %d, history %+v", j.State, j.Attempts, j.History)
	}
	found := false
	for _, n := range j.History {
		found = found || strings.Contains(n.Text, "lease lost")
	}
	if !found {
		t.Errorf("history does not mention the lost lease: %+v", j.History)
	}
}

func TestUnreachableDeviceIsRetriedThenQuarantined(t *testing.T) {
	e := open(t, filepath.Join(t.TempDir(), "lab.wal"), nil)
	j := e.waitJob(submit(e, "broken", "needs-dead"))
	if j.State != lab.Errored || j.Attempts != lab.MaxAttempts || !strings.Contains(j.Message, "gave up after 3 attempts") {
		t.Fatalf("job = %s, %d attempts, %q", j.State, j.Attempts, j.Message)
	}
	_, body := e.do("GET", "/api/v1/devices", nil, nil)
	var v struct{ Devices []lab.Device }
	_ = json.Unmarshal(body, &v)
	for _, d := range v.Devices {
		if d.ID == "dead" && d.Status != lab.Quarantined {
			t.Fatalf("dead device = %+v, want quarantined", d)
		}
	}
	// Returning it to service clears the strikes.
	resp, _ := e.do("POST", "/api/v1/devices/dead/status", map[string]string{"status": "available", "note": "replaced the power supply"}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("repair: %d", resp.StatusCode)
	}
}

func TestReservationsKeepSharedJobsOff(t *testing.T) {
	e := open(t, filepath.Join(t.TempDir(), "lab.wal"), nil)
	resp, body := e.do("POST", "/api/v1/reservations", map[string]any{"owner": "alice", "purpose": "manual debugging", "devices": []string{"sw1"}, "durationMinutes": 60}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("reserve: %d %s", resp.StatusCode, body)
	}
	resp, body = e.do("POST", "/api/v1/reservations", map[string]any{"owner": "bob", "devices": []string{"sw1"}, "durationMinutes": 30}, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(body), "reserved by alice") {
		t.Fatalf("overlap: %d %s", resp.StatusCode, body)
	}
	id := submit(e, "fabric", "shared")
	time.Sleep(300 * time.Millisecond)
	_, body = e.do("GET", "/api/v1/jobs/"+id, nil, nil)
	if !strings.Contains(string(body), `"state":"queued"`) {
		t.Fatalf("a shared job ran on a reserved device: %s", body)
	}
	// The same job inside the reservation cannot run either: it needs two devices, alice has one.
	resp, body = e.do("POST", "/api/v1/jobs", map[string]any{"preset": "fabric", "name": "inside", "team": "wan", "reservation": "r-1"}, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(body), "No set of lab devices") {
		t.Fatalf("infeasible reservation job: %d %s", resp.StatusCode, body)
	}
	resp, _ = e.do("DELETE", "/api/v1/reservations/r-1", nil, nil)
	if resp.StatusCode != http.StatusNoContent || e.waitJob(id).State != lab.Passed {
		t.Fatal("the job did not run once the reservation was cancelled")
	}
}

func TestGitHubWebhookQueuesAJobAndReportsStatus(t *testing.T) {
	var mu sync.Mutex
	var statuses []map[string]string
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var s map[string]string
		_ = json.NewDecoder(r.Body).Decode(&s)
		s["path"] = r.URL.Path
		mu.Lock()
		statuses = append(statuses, s)
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	defer gh.Close()
	secret := []byte("hook-secret")
	e := open(t, filepath.Join(t.TempDir(), "lab.wal"), func(o *Options) {
		o.GitHub = &integrations.GitHub{API: gh.URL, Token: "t"}
		o.WebhookSecret, o.WebhookPreset, o.WebhookTeam, o.BaseURL = secret, "fabric", "wan", "http://lab.example"
	})
	payload := []byte(`{"ref":"refs/heads/main","after":"0123456789abcdef0123456789abcdef01234567","repository":{"full_name":"acme/nos"}}`)

	resp, _ := e.do("POST", "/api/v1/github/webhook", payload, map[string]string{"X-GitHub-Event": "push", "X-Hub-Signature-256": "sha256=00"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad signature: %d", resp.StatusCode)
	}
	resp, _ = e.do("POST", "/api/v1/github/webhook", []byte(`{"zen":"hi"}`), map[string]string{"X-GitHub-Event": "ping", "X-Hub-Signature-256": integrations.Sign(secret, []byte(`{"zen":"hi"}`))})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ping: %d", resp.StatusCode)
	}
	resp, body := e.do("POST", "/api/v1/github/webhook", payload, map[string]string{"X-GitHub-Event": "push", "X-Hub-Signature-256": integrations.Sign(secret, payload)})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("push: %d %s", resp.StatusCode, body)
	}
	var v struct{ Job, Name string }
	_ = json.Unmarshal(body, &v)
	if j := e.waitJob(v.Job); j.State != lab.Passed || v.Name != "push-0123456" || j.Source == nil || j.Source.Repo != "acme/nos" {
		t.Fatalf("job %+v, name %q", j, v.Name)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(statuses) != 2 || statuses[0]["state"] != "pending" || statuses[1]["state"] != "success" {
		t.Fatalf("statuses = %v", statuses)
	}
	if s := statuses[1]; s["path"] != "/repos/acme/nos/statuses/0123456789abcdef0123456789abcdef01234567" || s["target_url"] != "http://lab.example/jobs/"+v.Job || s["context"] != "labq/qualification" {
		t.Errorf("success status = %v", s)
	}
}

func TestDashboardPages(t *testing.T) {
	e := open(t, filepath.Join(t.TempDir(), "lab.wal"), nil)
	id := submit(e, "fabric", "dash-1")
	e.waitJob(id)
	for path, want := range map[string]string{
		"/":              "<h1>Lab overview</h1>",
		"/jobs/" + id:    "dash-1",
		"/devices/sw1":   "<h1>sw1</h1>",
		"/jobs/new":      "<h1>Submit a job</h1>",
		"/reservations":  "<h1>Reservations</h1>",
		"/no-such-page":  "There is nothing at this address",
		"/devices/ghost": "Device not found",
	} {
		resp, body := e.do("GET", path, nil, nil)
		if !strings.Contains(string(body), want) {
			t.Errorf("GET %s (%d) does not contain %q", path, resp.StatusCode, want)
		}
		if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "script-src 'none'") {
			t.Errorf("GET %s: CSP %q", path, resp.Header.Get("Content-Security-Policy"))
		}
	}
}

func TestFormErrorsAreAccessible(t *testing.T) {
	e := open(t, filepath.Join(t.TempDir(), "lab.wal"), nil)
	form := url.Values{"duration": {"abc"}}.Encode()
	resp, body := e.do("POST", "/reservations", form, map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	page := string(body)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(page, "<title>Error: Reservations · LabQ</title>") {
		t.Fatalf("status %d; page title missing the error prefix", resp.StatusCode)
	}
	for _, want := range []string{"There is a problem", `href="#duration"`, `id="duration-error"`, `aria-describedby="duration-hint duration-error"`, `aria-invalid="true"`} {
		if !strings.Contains(page, want) {
			t.Errorf("error page lacks %q", want)
		}
	}

	resp, body = e.do("POST", "/jobs/new", url.Values{"preset": {"fabric"}, "name": {"Bad Name"}, "team": {""}, "priority": {"1"}}.Encode(),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(body), `href="#name"`) || !strings.Contains(string(body), `href="#team"`) {
		t.Fatalf("submit form errors: %d", resp.StatusCode)
	}

	resp, _ = e.do("POST", "/jobs/new", url.Values{"preset": {"fabric"}, "name": {"form-job"}, "team": {"dc"}, "priority": {"2"}}.Encode(),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if resp.Request.URL.Path != "/jobs/j-1" {
		t.Fatalf("a valid form should redirect to the new job, ended at %s", resp.Request.URL.Path)
	}
}

func TestCrossSiteFormPostsAreRejected(t *testing.T) {
	e := open(t, filepath.Join(t.TempDir(), "lab.wal"), nil)
	resp, _ := e.do("POST", "/reservations", url.Values{"owner": {"mallory"}, "devices": {"sw1"}, "duration": {"60"}}.Encode(),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Sec-Fetch-Site": "cross-site"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site post: %d, want 403", resp.StatusCode)
	}
}
