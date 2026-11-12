package server

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/yasaswinivardhireddy-png/labq/internal/integrations"
	"github.com/yasaswinivardhireddy-png/labq/internal/lab"
	"github.com/yasaswinivardhireddy-png/labq/internal/qual"
)

//go:embed templates/*.html static/*
var assets embed.FS

// Handler serves the JSON API, the dashboard, GitHub webhooks, exports and metrics.
func (s *Service) Handler() http.Handler {
	pages := parsePages()
	mux := http.NewServeMux()
	// JSON API
	mux.HandleFunc("GET /api/v1/devices", s.apiDevices)
	mux.HandleFunc("POST /api/v1/devices/{id}/status", s.apiDeviceStatus)
	mux.HandleFunc("GET /api/v1/jobs", s.apiJobs)
	mux.HandleFunc("POST /api/v1/jobs", s.apiSubmit)
	mux.HandleFunc("GET /api/v1/jobs/{id}", s.apiJob)
	mux.HandleFunc("POST /api/v1/jobs/{id}/cancel", s.apiCancel)
	mux.HandleFunc("GET /api/v1/reservations", s.apiReservations)
	mux.HandleFunc("POST /api/v1/reservations", s.apiReserve)
	mux.HandleFunc("DELETE /api/v1/reservations/{id}", s.apiUnreserve)
	mux.HandleFunc("GET /api/v1/tests", s.apiTests)
	mux.HandleFunc("POST /api/v1/github/webhook", s.apiWebhook)
	mux.HandleFunc("GET /api/v1/export/results.csv", s.exportCSV)
	mux.HandleFunc("GET /api/v1/export/jobs.ndjson", s.exportNDJSON)
	mux.HandleFunc("GET /metrics", s.metrics)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if err := s.Healthy(); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		problem(w, http.StatusNotFound, "Not found", "No API endpoint matches this path.", nil)
	})
	// Dashboard
	h := &html{s: s, pages: pages}
	mux.HandleFunc("GET /{$}", h.overview)
	mux.HandleFunc("GET /jobs/new", h.newJob)
	mux.HandleFunc("POST /jobs/new", h.newJob)
	mux.HandleFunc("GET /jobs/{id}", h.job)
	mux.HandleFunc("POST /jobs/{id}/cancel", h.cancelJob)
	mux.HandleFunc("GET /devices/{id}", h.device)
	mux.HandleFunc("POST /devices/{id}/status", h.deviceStatus)
	mux.HandleFunc("GET /reservations", h.reservations)
	mux.HandleFunc("POST /reservations", h.reservations)
	mux.HandleFunc("POST /reservations/{id}/cancel", h.cancelReservation)
	static, _ := fs.Sub(assets, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("/", h.notFound)

	// Browsers send Sec-Fetch-Site and Origin; cross-site form posts are rejected. GitHub and
	// command-line clients send neither and are allowed (the webhook checks its signature).
	return secure(http.NewCrossOriginProtection().Handler(mux))
}

func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; img-src 'self'; script-src 'none'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// ---- JSON API ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func problem(w http.ResponseWriter, status int, title, detail string, fields map[string]string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	body := map[string]any{"type": "about:blank", "title": title, "status": status, "detail": detail}
	if len(fields) > 0 {
		body["errors"] = fields
	}
	_ = json.NewEncoder(w).Encode(body)
}

// fail maps service errors to HTTP problems.
func fail(w http.ResponseWriter, err error) {
	var ve *ValidationError
	switch {
	case errors.As(err, &ve):
		problem(w, http.StatusUnprocessableEntity, "The request has problems", "Fix the fields listed in errors.", ve.Fields)
	case errors.Is(err, ErrNotFound):
		problem(w, http.StatusNotFound, "Not found", "Nothing has this ID.", nil)
	default:
		problem(w, http.StatusConflict, "Cannot do that now", err.Error(), nil)
	}
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		problem(w, http.StatusBadRequest, "Invalid request body", err.Error(), nil)
		return false
	}
	return true
}

func (s *Service) apiDevices(w http.ResponseWriter, _ *http.Request) {
	s.View(func(st *lab.State) {
		writeJSON(w, http.StatusOK, map[string]any{"devices": st.DeviceList(), "links": st.Links})
	})
}

func (s *Service) apiDeviceStatus(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Status lab.DeviceStatus `json:"status"`
		Note   string           `json:"note"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.SetDeviceStatus(r.PathValue("id"), req.Status, req.Note); err != nil {
		fail(w, err)
		return
	}
	s.View(func(st *lab.State) { writeJSON(w, http.StatusOK, st.Devices[r.PathValue("id")]) })
}

func (s *Service) apiJobs(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	s.View(func(st *lab.State) {
		jobs := []*lab.Job{}
		for _, j := range slices.Backward(st.JobList()) {
			if state == "" || string(j.State) == state {
				jobs = append(jobs, j)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
	})
}

type submitRequest struct {
	lab.JobSpec
	Preset string `json:"preset,omitempty"`
}

func (s *Service) apiSubmit(w http.ResponseWriter, r *http.Request) {
	var req submitRequest
	if !decode(w, r, &req) {
		return
	}
	spec := req.JobSpec
	if req.Preset != "" {
		var err error
		if spec, err = s.FromPreset(req.Name, req.Preset, req.Team, req.Priority, req.Reservation); err != nil {
			fail(w, err)
			return
		}
	}
	j, err := s.Submit(spec)
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Location", "/api/v1/jobs/"+j.ID)
	s.View(func(*lab.State) { writeJSON(w, http.StatusCreated, j) })
}

func (s *Service) apiJob(w http.ResponseWriter, r *http.Request) {
	s.View(func(st *lab.State) {
		j, ok := st.Jobs[r.PathValue("id")]
		if !ok {
			problem(w, http.StatusNotFound, "Job not found", "There is no job with this ID.", nil)
			return
		}
		writeJSON(w, http.StatusOK, struct {
			*lab.Job
			QueuePosition int `json:"queuePosition,omitempty"`
		}{j, st.QueuePosition(j.ID)})
	})
}

func (s *Service) apiCancel(w http.ResponseWriter, r *http.Request) {
	if err := s.Cancel(r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"job": r.PathValue("id"), "status": "cancelling"})
}

func (s *Service) apiReservations(w http.ResponseWriter, _ *http.Request) {
	s.View(func(st *lab.State) { writeJSON(w, http.StatusOK, map[string]any{"reservations": st.ReservationList()}) })
}

func (s *Service) apiReserve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Owner           string    `json:"owner"`
		Purpose         string    `json:"purpose"`
		Devices         []string  `json:"devices"`
		Start           time.Time `json:"start"`
		DurationMinutes int       `json:"durationMinutes"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Start.IsZero() {
		req.Start = s.now()
	}
	res, err := s.Reserve(lab.Reservation{Owner: req.Owner, Purpose: req.Purpose, Devices: req.Devices,
		Start: req.Start, End: req.Start.Add(time.Duration(req.DurationMinutes) * time.Minute)})
	if err != nil {
		fail(w, err)
		return
	}
	s.View(func(*lab.State) { writeJSON(w, http.StatusCreated, res) })
}

func (s *Service) apiUnreserve(w http.ResponseWriter, r *http.Request) {
	if err := s.Unreserve(r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) apiTests(w http.ResponseWriter, _ *http.Request) {
	type t struct{ Name, Description string }
	var out []t
	for _, x := range qual.Tests() {
		out = append(out, t{x.Name, x.Description})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tests": out, "presets": s.opts.Config.Presets})
}

func (s *Service) apiWebhook(w http.ResponseWriter, r *http.Request) {
	if len(s.opts.WebhookSecret) == 0 {
		problem(w, http.StatusNotFound, "Webhook disabled", "Start labq with a webhook secret to accept GitHub deliveries.", nil)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		problem(w, http.StatusRequestEntityTooLarge, "Payload too large", err.Error(), nil)
		return
	}
	if !integrations.VerifySignature(s.opts.WebhookSecret, body, r.Header.Get("X-Hub-Signature-256")) {
		problem(w, http.StatusUnauthorized, "Bad signature", "X-Hub-Signature-256 does not match the payload.", nil)
		return
	}
	j, err := s.Webhook(r.Header.Get("X-GitHub-Event"), body)
	if err != nil {
		fail(w, err)
		return
	}
	if j == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ignored": true})
		return
	}
	s.View(func(*lab.State) { writeJSON(w, http.StatusAccepted, map[string]any{"job": j.ID, "name": j.Name}) })
}

func (s *Service) exportCSV(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="labq-results.csv"`)
	s.View(func(st *lab.State) { _ = integrations.ResultsCSV(w, st.JobList(), st.Devices) })
}

func (s *Service) exportNDJSON(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", `attachment; filename="labq-jobs.ndjson"`)
	s.View(func(st *lab.State) { _ = integrations.JobsNDJSON(w, st.JobList(), st.Devices) })
}

func (s *Service) metrics(w http.ResponseWriter, _ *http.Request) {
	var b strings.Builder
	s.View(func(st *lab.State) {
		devices := map[string]int{}
		for _, d := range st.Devices {
			status := string(d.Status)
			if d.Job != "" {
				status = "busy"
			}
			devices[status]++
		}
		jobs, tests := map[string]int{}, map[[2]string]int{}
		var waitSum float64
		var waitCount int
		for _, j := range st.JobList() {
			jobs[string(j.State)]++
			if !j.Started.IsZero() {
				waitSum += j.Started.Sub(j.Submitted).Seconds()
				waitCount++
			}
			for _, r := range j.Results {
				result := "passed"
				if r.Infra {
					result = "infra"
				} else if !r.Passed {
					result = "failed"
				}
				tests[[2]string{r.Test, result}]++
			}
		}
		b.WriteString("# HELP labq_devices Devices by status.\n# TYPE labq_devices gauge\n")
		for _, st := range []string{"available", "busy", "quarantined", "maintenance"} {
			fmt.Fprintf(&b, "labq_devices{status=%q} %d\n", st, devices[st])
		}
		b.WriteString("# HELP labq_jobs Jobs by state.\n# TYPE labq_jobs gauge\n")
		for _, st := range []string{"queued", "running", "passed", "failed", "error", "cancelled"} {
			fmt.Fprintf(&b, "labq_jobs{state=%q} %d\n", st, jobs[st])
		}
		b.WriteString("# HELP labq_test_results_total Qualification test results.\n# TYPE labq_test_results_total counter\n")
		keys := make([][2]string, 0, len(tests))
		for k := range tests {
			keys = append(keys, k)
		}
		slices.SortFunc(keys, func(a, b [2]string) int { return strings.Compare(a[0]+a[1], b[0]+b[1]) })
		for _, k := range keys {
			fmt.Fprintf(&b, "labq_test_results_total{test=%q,result=%q} %d\n", k[0], k[1], tests[k])
		}
		fmt.Fprintf(&b, "# HELP labq_job_wait_seconds Time from submission to the last start.\n# TYPE labq_job_wait_seconds summary\nlabq_job_wait_seconds_sum %g\nlabq_job_wait_seconds_count %d\n", waitSum, waitCount)
	})
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = io.WriteString(w, b.String())
}

// ---- dashboard ----

type html struct {
	s     *Service
	pages map[string]*template.Template
}

var funcs = template.FuncMap{
	"when": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format("2006-01-02 15:04:05 UTC")
	},
	"secs": func(ms int64) string { return strconv.FormatFloat(float64(ms)/1000, 'f', 1, 64) + " s" },
	"inc":  func(i int) int { return i + 1 },
	// ferr returns a field's error message and the id its control's aria-describedby points to.
	"ferr": func(f form, field string) map[string]string {
		return map[string]string{"ID": field + "-error", "Msg": f.Errors[field]}
	},
	// describedby lists the hint and, when there is one, the error that describe a control.
	"describedby": func(f form, field string, hint bool) string {
		var ids []string
		if hint {
			ids = append(ids, field+"-hint")
		}
		if f.Errors[field] != "" {
			ids = append(ids, field+"-error")
		}
		return strings.Join(ids, " ")
	},
	"join":     strings.Join,
	"priority": func(p int) string { return [...]string{"Low", "Normal", "High"}[min(max(p, 0), 2)] },
	"measure": func(m map[string]float64) string {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		var parts []string
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s %.2f", k, m[k]))
		}
		return strings.Join(parts, ", ")
	},
	"title": func(s string) string {
		if s == "" {
			return s
		}
		return strings.ToUpper(s[:1]) + s[1:]
	},
	// anchor maps a validation error to the form control it is about.
	"anchor": func(field string) string {
		switch field {
		case "roles", "tests", "timeoutSec", "cables":
			return "preset"
		}
		return field
	},
	"needs": func(roles []lab.Role) string {
		var parts []string
		for _, r := range roles {
			p := r.Name + ": " + strings.TrimSpace(r.Vendor+" "+strings.Join(r.Capabilities, ", "))
			parts = append(parts, strings.TrimSuffix(p, ": "))
		}
		return strings.Join(parts, "; ")
	},
}

func parsePages() map[string]*template.Template {
	pages := map[string]*template.Template{}
	for _, name := range []string{"overview", "job", "device", "reservations", "newjob", "notfound"} {
		pages[name] = template.Must(template.New("base.html").Funcs(funcs).ParseFS(assets, "templates/base.html", "templates/"+name+".html"))
	}
	return pages
}

func (h *html) render(w http.ResponseWriter, status int, page string, data map[string]any) {
	if _, ok := data["Nav"]; !ok {
		data["Nav"] = ""
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := h.pages[page].Execute(w, data); err != nil {
		h.s.log.Error("render", "page", page, "error", err)
	}
}

func (h *html) notFound(w http.ResponseWriter, _ *http.Request) {
	h.render(w, http.StatusNotFound, "notfound", map[string]any{"Title": "Page not found"})
}

type deviceRow struct {
	*lab.Device
	Busy         bool
	Reservations []*lab.Reservation
}

func (h *html) overview(w http.ResponseWriter, _ *http.Request) {
	h.s.View(func(st *lab.State) {
		now := h.s.now()
		var rows []deviceRow
		counts := map[string]int{}
		for _, d := range st.DeviceList() {
			row := deviceRow{Device: d, Busy: d.Job != ""}
			for _, r := range st.ReservationList() {
				if slices.Contains(r.Devices, d.ID) && r.Overlaps(now, now.Add(24*time.Hour)) {
					row.Reservations = append(row.Reservations, r)
				}
			}
			rows = append(rows, row)
			if row.Busy {
				counts["busy"]++
			} else {
				counts[string(d.Status)]++
			}
		}
		var queue, recent []*lab.Job
		for _, j := range st.JobList() {
			if j.State == lab.Queued {
				queue = append(queue, j)
			}
		}
		slices.SortStableFunc(queue, func(a, b *lab.Job) int { return st.QueuePosition(a.ID) - st.QueuePosition(b.ID) })
		for _, j := range slices.Backward(st.JobList()) {
			if j.State != lab.Queued && len(recent) < 15 {
				recent = append(recent, j)
			}
		}
		h.render(w, http.StatusOK, "overview", map[string]any{
			"Title": "Lab overview", "Nav": "overview", "Devices": rows, "Counts": counts, "Queue": queue,
			"Recent": recent, "Now": now,
		})
	})
}

func (h *html) job(w http.ResponseWriter, r *http.Request) {
	h.s.View(func(st *lab.State) {
		j, ok := st.Jobs[r.PathValue("id")]
		if !ok {
			h.render(w, http.StatusNotFound, "notfound", map[string]any{"Title": "Job not found"})
			return
		}
		type member struct{ Role, Device string }
		var members []member
		for _, role := range j.Roles {
			if id, ok := j.Assignment[role.Name]; ok {
				members = append(members, member{role.Name, id})
			}
		}
		h.render(w, http.StatusOK, "job", map[string]any{
			"Title": "Job " + j.Name, "Job": j, "Members": members, "Position": st.QueuePosition(j.ID),
			"Cancellable": !j.State.Done(), "Cancelling": r.URL.Query().Get("cancelling") != "",
		})
	})
}

func (h *html) cancelJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.s.Cancel(id); err != nil && !errors.Is(err, ErrNotFound) {
		h.s.log.Warn("cancel", "job", id, "error", err)
	}
	http.Redirect(w, r, "/jobs/"+id+"?cancelling=1", http.StatusSeeOther)
}

func (h *html) device(w http.ResponseWriter, r *http.Request) {
	h.s.View(func(st *lab.State) {
		d, ok := st.Devices[r.PathValue("id")]
		if !ok {
			h.render(w, http.StatusNotFound, "notfound", map[string]any{"Title": "Device not found"})
			return
		}
		var cables []lab.Link
		for _, l := range st.Links {
			if l.A.Device == d.ID {
				cables = append(cables, l)
			} else if l.B.Device == d.ID {
				cables = append(cables, lab.Link{A: l.B, B: l.A})
			}
		}
		var res []*lab.Reservation
		for _, x := range st.ReservationList() {
			if slices.Contains(x.Devices, d.ID) && !x.Cancelled && x.End.After(h.s.now()) {
				res = append(res, x)
			}
		}
		var jobs []*lab.Job
		for _, j := range slices.Backward(st.JobList()) {
			if slices.Contains(slices.Collect(maps.Values(j.Assignment)), d.ID) && len(jobs) < 10 {
				jobs = append(jobs, j)
			}
		}
		h.render(w, http.StatusOK, "device", map[string]any{
			"Title": "Device " + d.ID, "Device": d, "Row": deviceRow{Device: d, Busy: d.Job != ""}, "Cables": cables, "Reservations": res, "Jobs": jobs,
		})
	})
}

func (h *html) deviceStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := r.ParseForm(); err == nil {
		if err := h.s.SetDeviceStatus(id, lab.DeviceStatus(r.PostForm.Get("status")), r.PostForm.Get("note")); err != nil {
			h.s.log.Warn("device status", "device", id, "error", err)
		}
	}
	http.Redirect(w, r, "/devices/"+id, http.StatusSeeOther)
}

// form holds a submitted form's values and problems for re-rendering.
type form struct {
	Values   map[string]string
	Selected map[string]bool
	Errors   map[string]string
}

func (f form) Order(fields ...string) []string {
	var out []string
	for _, k := range fields {
		if f.Errors[k] != "" {
			out = append(out, k)
		}
	}
	return out
}

func (h *html) reservations(w http.ResponseWriter, r *http.Request) {
	f := form{Values: map[string]string{"duration": "60"}, Selected: map[string]bool{}, Errors: map[string]string{}}
	created := r.URL.Query().Get("created")
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		for _, k := range []string{"owner", "purpose", "start", "duration"} {
			f.Values[k] = strings.TrimSpace(r.PostForm.Get(k))
		}
		for _, d := range r.PostForm["devices"] {
			f.Selected[d] = true
		}
		start := h.s.now()
		if v := f.Values["start"]; v != "" {
			t, err := time.Parse("2006-01-02T15:04", v)
			if err != nil {
				f.Errors["start"] = "Enter a date and time, or leave it empty to start now."
			}
			start = t
		}
		minutes, err := strconv.Atoi(f.Values["duration"])
		if err != nil || minutes <= 0 {
			f.Errors["duration"] = "Enter a whole number of minutes."
		}
		if len(f.Errors) == 0 {
			res, err := h.s.Reserve(lab.Reservation{Owner: f.Values["owner"], Purpose: f.Values["purpose"],
				Devices: slices.Sorted(maps.Keys(f.Selected)), Start: start, End: start.Add(time.Duration(minutes) * time.Minute)})
			var ve *ValidationError
			switch {
			case errors.As(err, &ve):
				f.Errors = ve.Fields
			case err != nil:
				f.Errors["devices"] = err.Error()
			default:
				http.Redirect(w, r, "/reservations?created="+res.ID, http.StatusSeeOther)
				return
			}
		}
	}
	h.s.View(func(st *lab.State) {
		now := h.s.now()
		var active []*lab.Reservation
		for _, x := range st.ReservationList() {
			if !x.Cancelled && x.End.After(now) {
				active = append(active, x)
			}
		}
		status := http.StatusOK
		title := "Reservations"
		if len(f.Errors) > 0 {
			status, title = http.StatusUnprocessableEntity, "Error: Reservations"
		}
		h.render(w, status, "reservations", map[string]any{
			"Title": title, "Nav": "reservations", "Active": active, "Devices": st.DeviceList(), "Form": f,
			"ErrorOrder": f.Order("owner", "devices", "start", "duration", "purpose"), "Created": created, "Now": now,
		})
	})
}

func (h *html) cancelReservation(w http.ResponseWriter, r *http.Request) {
	if err := h.s.Unreserve(r.PathValue("id")); err != nil {
		h.s.log.Warn("unreserve", "reservation", r.PathValue("id"), "error", err)
	}
	http.Redirect(w, r, "/reservations", http.StatusSeeOther)
}

func (h *html) newJob(w http.ResponseWriter, r *http.Request) {
	f := form{Values: map[string]string{"priority": "1"}, Selected: map[string]bool{}, Errors: map[string]string{}}
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		for _, k := range []string{"preset", "name", "team", "priority", "reservation"} {
			f.Values[k] = strings.TrimSpace(r.PostForm.Get(k))
		}
		prio, _ := strconv.Atoi(f.Values["priority"])
		spec, err := h.s.FromPreset(f.Values["name"], f.Values["preset"], f.Values["team"], prio, f.Values["reservation"])
		var j *lab.Job
		if err == nil {
			j, err = h.s.Submit(spec)
		}
		var ve *ValidationError
		switch {
		case errors.As(err, &ve):
			f.Errors = ve.Fields
			// Problems with roles, tests or timeouts come from the preset: show them on that field.
			for _, k := range []string{"roles", "tests", "timeoutSec", "cables"} {
				if msg := f.Errors[k]; msg != "" {
					f.Errors["preset"] = msg
					delete(f.Errors, k)
				}
			}
		case err != nil:
			f.Errors["preset"] = err.Error()
		default:
			http.Redirect(w, r, "/jobs/"+j.ID, http.StatusSeeOther)
			return
		}
	}
	h.s.View(func(st *lab.State) {
		type preset struct {
			Name string
			Preset
		}
		var presets []preset
		for name, p := range h.s.opts.Config.Presets {
			presets = append(presets, preset{name, p})
		}
		slices.SortFunc(presets, func(a, b preset) int { return strings.Compare(a.Name, b.Name) })
		var active []*lab.Reservation
		for _, x := range st.ReservationList() {
			if !x.Cancelled && x.End.After(h.s.now()) {
				active = append(active, x)
			}
		}
		status, title := http.StatusOK, "Submit a job"
		if len(f.Errors) > 0 {
			status, title = http.StatusUnprocessableEntity, "Error: Submit a job"
		}
		h.render(w, status, "newjob", map[string]any{
			"Title": title, "Nav": "newjob", "Presets": presets, "Teams": st.TeamOrder, "Reservations": active, "Form": f,
			"ErrorOrder": f.Order("preset", "name", "team", "priority", "reservation", "roles", "tests", "timeoutSec"),
		})
	})
}
