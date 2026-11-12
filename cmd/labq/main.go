// Command labq schedules qualification jobs onto the network devices of a test lab.
//
//	labq serve -config lab.json [flags]   run a lab described by a config file
//	labq demo [flags]                     run a simulated eight-device lab with sample activity
//
// serve reads GITHUB_TOKEN (commit statuses) and LABQ_WEBHOOK_SECRET (GitHub webhook) from the
// environment when they are set.
package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/yasaswinivardhireddy-png/labq/internal/integrations"
	"github.com/yasaswinivardhireddy-png/labq/internal/lab"
	"github.com/yasaswinivardhireddy-png/labq/internal/server"
)

//go:embed demo.json
var demoConfig []byte

func main() {
	if len(os.Args) < 2 || (os.Args[1] != "serve" && os.Args[1] != "demo") {
		fmt.Fprintln(os.Stderr, "usage: labq serve -config lab.json [flags] | labq demo [flags]")
		os.Exit(2)
	}
	var err error
	if os.Args[1] == "serve" {
		err = serve(os.Args[2:])
	} else {
		err = demo(os.Args[2:])
	}
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, "labq:", err)
		os.Exit(1)
	}
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := fs.String("config", "", "lab configuration file (required)")
	addr := fs.String("addr", "127.0.0.1:8080", "listen address")
	walPath := fs.String("wal", "labq.wal", "write-ahead log file")
	baseURL := fs.String("base-url", "", "public URL of the dashboard, for links in GitHub statuses")
	githubAPI := fs.String("github-api", "https://api.github.com", "GitHub API URL")
	preset := fs.String("webhook-preset", "", "preset to run for GitHub pushes and pull requests")
	team := fs.String("webhook-team", "", "team that GitHub-triggered jobs belong to")
	testWait := fs.Duration("test-wait", 60*time.Second, "how long a test waits for a condition, such as a BGP session")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *configPath == "" {
		return errors.New("-config is required")
	}
	cfg, err := server.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	opts := server.Options{
		Config: cfg, WALPath: *walPath, BaseURL: *baseURL, WebhookPreset: *preset, WebhookTeam: *team, TestWait: *testWait,
		WebhookSecret: []byte(os.Getenv("LABQ_WEBHOOK_SECRET")),
	}
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		opts.GitHub = &integrations.GitHub{API: *githubAPI, Token: token}
	}
	return run(*addr, opts, nil)
}

func demo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "listen address")
	walPath := fs.String("wal", filepath.Join(os.TempDir(), "labq-demo.wal"), "write-ahead log file")
	fresh := fs.Bool("fresh", true, "start from an empty log")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *fresh {
		_ = os.Remove(*walPath)
	}
	cfg, err := server.ParseConfig(demoConfig)
	if err != nil {
		return err
	}
	return run(*addr, server.Options{Config: cfg, WALPath: *walPath, TestWait: 20 * time.Second}, seedDemo)
}

// seedDemo gives a fresh demo lab a reservation and a mix of jobs.
func seedDemo(s *server.Service) {
	empty := true
	s.View(func(st *lab.State) { empty = len(st.Jobs) == 0 })
	if !empty {
		return
	}
	now := time.Now()
	if _, err := s.Reserve(lab.Reservation{Owner: "wan-oncall", Purpose: "manual firmware upgrade on the ToR pair",
		Devices: []string{"tor-1", "tor-2"}, Start: now, End: now.Add(2 * time.Hour)}); err != nil {
		slog.Warn("demo reservation", "error", err)
	}
	jobs := []struct {
		name, preset, team, reservation string
		priority                        int
	}{
		{"interop-acme-globex-1", "interop", "wan", "", 1},
		{"edge-failover-nightly", "edge-pair", "release-qa", "", 0},
		{"interop-acme-globex-2", "interop", "datacenter", "", 2},
		{"tor-upgrade-check", "tor-pair", "wan", "r-1", 1},
		{"interop-acme-globex-3", "interop", "release-qa", "", 1},
		{"edge-failover-pr-118", "edge-pair", "wan", "", 2},
	}
	for _, j := range jobs {
		spec, err := s.FromPreset(j.name, j.preset, j.team, j.priority, j.reservation)
		if err == nil {
			_, err = s.Submit(spec)
		}
		if err != nil {
			slog.Warn("demo job", "job", j.name, "error", err)
		}
	}
}

func run(addr string, opts server.Options, seed func(*server.Service)) error {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	opts.Logger = log
	svc, err := server.Open(opts)
	if err != nil {
		return err
	}
	svc.Start()
	if seed != nil {
		seed(svc)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	hs := &http.Server{Handler: svc.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: time.Minute}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		svc.Close()
		return err
	}
	log.Info("labq serving", "url", "http://"+ln.Addr().String(), "wal", opts.WALPath, "devices", len(opts.Config.Devices),
		"github_statuses", opts.GitHub != nil, "webhook", len(opts.WebhookSecret) > 0)
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	select {
	case err = <-errc:
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err = hs.Shutdown(shutdown)
		cancel()
	}
	if cerr := svc.Close(); err == nil {
		err = cerr
	}
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}
