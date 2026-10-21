package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/samjingwen/tandem/core"
	"github.com/samjingwen/tandem/internal/api"
	"github.com/samjingwen/tandem/internal/bootstrap"
	"github.com/samjingwen/tandem/internal/cli"
	"github.com/samjingwen/tandem/internal/opencode"
	"github.com/samjingwen/tandem/internal/store"
	"github.com/samjingwen/tandem/internal/workspace"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "tandem:", err)
		os.Exit(1)
	}
}

func defaultState() string {
	root, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(root, "tandem", "default")
}

func run(ctx context.Context, args []string, out, errOut io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "--version":
			_, err := fmt.Fprintln(out, core.Version)
			return err
		case "serve":
			return serve(ctx, args[1:], out, errOut)
		case "runtime-connect":
			return connect(ctx, args[1:], out, errOut)
		case "--help", "help", "-h":
			if _, err := fmt.Fprintln(out, "Tandem: persistent coordination for coding agents\n\nService: tandem serve --state-dir DIR [--bind 127.0.0.1:7433]\nRuntime setup: tandem runtime-connect --state-dir DIR\nLocal executable version: tandem --version\n\nClient commands:"); err != nil {
				return err
			}
			return cli.Run(ctx, []string{"--help"}, out, errOut)
		}
	}
	return cli.Run(ctx, args, out, errOut)
}

func connect(ctx context.Context, args []string, out, errOut io.Writer) error {
	flags := flag.NewFlagSet("runtime-connect", flag.ContinueOnError)
	flags.SetOutput(errOut)
	directory := flags.String("state-dir", defaultState(), "dedicated Tandem instance directory")
	binary := flags.String("opencode", "opencode", "supported OpenCode 2.1.0 CLI executable")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || *directory == "" {
		return errors.New("runtime-connect requires a state directory and no positional arguments")
	}
	setupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	connection, err := bootstrap.Pair(setupCtx, *binary, *directory)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Connected OpenCode %s at %s. Pairing credentials were saved owner-only; no provider credentials were copied.\n", connection.Version, connection.URL)
	return err
}

func validateBind(bind string, allowRemote bool, token, cert, key string, proxy bool) error {
	host, _, err := net.SplitHostPort(bind)
	if err != nil {
		return errors.New("bind must include an explicit host and port")
	}
	ip := net.ParseIP(host)
	local := ip != nil && ip.IsLoopback()
	if !local && !allowRemote {
		return errors.New("non-loopback binding requires --allow-remote and explicit authentication/TLS configuration")
	}
	if (cert == "") != (key == "") {
		return errors.New("tls-cert and tls-key must be supplied together")
	}
	if !local && (len(token) < 32 || (cert == "" && !proxy)) {
		return errors.New("remote access requires TANDEM_TOKEN of at least 32 characters and TLS or --behind-tls-proxy")
	}
	return nil
}

func serve(ctx context.Context, args []string, out, errOut io.Writer) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(errOut)
	directory := flags.String("state-dir", defaultState(), "dedicated local instance directory; never share over a network filesystem")
	bind := flags.String("bind", "127.0.0.1:7433", "HTTP bind address")
	workers := flags.Int("concurrency", 2, "maximum concurrent worker executions")
	maxTasks := flags.Int("max-tasks", 128, "maximum retained tasks in this MVP instance")
	allocator := flags.String("allocator", "worktrunk", "checkout allocator: worktrunk or git")
	modelReference := flags.String("model", "", "optional existing OpenCode provider/model#variant; omitted uses native configuration")
	agent := flags.String("agent", "", "optional existing native agent ID; logical Tandem roles remain independent")
	allowRemote := flags.Bool("allow-remote", false, "explicitly enable authenticated non-loopback listening")
	cert := flags.String("tls-cert", "", "TLS certificate path")
	key := flags.String("tls-key", "", "TLS private key path")
	proxy := flags.Bool("behind-tls-proxy", false, "operator asserts TLS is enforced by a trusted reverse proxy")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || *directory == "" || *workers < 1 || *workers > 32 || *maxTasks < 1 {
		return errors.New("invalid service arguments or concurrency bound")
	}
	if *allocator != "git" && *allocator != "worktrunk" {
		return errors.New("allocator must be git or worktrunk")
	}
	if _, err := exec.LookPath("git"); err != nil {
		return errors.New("missing Git executable for project/workspace ownership checks")
	}
	if *allocator == "worktrunk" {
		if _, err := exec.LookPath("wt"); err != nil {
			return errors.New("selected allocator requires Worktrunk; install the documented version or explicitly choose --allocator git where project policy permits")
		}
	}
	token := os.Getenv("TANDEM_TOKEN")
	if err := validateBind(*bind, *allowRemote, token, *cert, *key, *proxy); err != nil {
		return err
	}
	s, err := store.Open(*directory)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	if token == "" {
		token, err = bootstrap.ClientToken(*directory)
		if err != nil {
			return err
		}
	}
	if len(token) < 32 {
		return errors.New("TANDEM_TOKEN must contain at least 32 characters")
	}
	connection := bootstrap.Connection{URL: os.Getenv("OPENCODE_URL"), Password: os.Getenv("OPENCODE_PASSWORD"), Version: "2.1.0"}
	if connection.URL == "" {
		connection, err = bootstrap.Load(*directory)
		if err != nil {
			return errors.New("configure an OpenCode connection with runtime-connect, or set OPENCODE_URL and OPENCODE_PASSWORD")
		}
	}
	if connection.Password == "" {
		return errors.New("an authenticated OpenCode connection is required")
	}
	model, err := opencode.ParseModelReference(*modelReference)
	if err != nil {
		return err
	}
	runtimeClient, err := opencode.New(opencode.Options{URL: connection.URL, Password: connection.Password, ExpectedVersion: "2.1.0", Model: model, Agent: *agent})
	if err != nil {
		return err
	}
	hash := sha256.Sum256([]byte(connection.URL + "\x002.1.0\x00" + *modelReference + "\x00" + *agent))
	runtimeID := "opencode:" + hex.EncodeToString(hash[:16])
	root, err := filepath.Abs(*directory)
	if err != nil {
		return err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	w := workspace.Git{Root: filepath.Join(root, "worktrees"), UseWorktrunk: *allocator == "worktrunk"}
	engine := core.New(s, runtimeClient, w, core.Options{Concurrency: *workers, MaxTasks: *maxTasks, RuntimeID: runtimeID, RegisterProject: workspace.Register})
	listener, err := net.Listen("tcp", *bind)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: api.New(engine, api.Options{Token: token}), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 35 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 * 1024}
	if *cert != "" {
		certificate, err := tls.LoadX509KeyPair(*cert, *key)
		if err != nil {
			_ = listener.Close()
			return errors.New("TLS certificate/key could not be loaded")
		}
		server.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}
	}
	serveErr := make(chan error, 1)
	go func() {
		if *cert != "" {
			serveErr <- server.ServeTLS(listener, "", "")
		} else {
			serveErr <- server.Serve(listener)
		}
	}()
	schedulerCtx, stopScheduler := context.WithCancel(ctx)
	defer stopScheduler()
	schedulerDone := make(chan error, 1)
	go func() { schedulerDone <- engine.Run(schedulerCtx, time.Second) }()
	state, err := engine.Snapshot(ctx)
	if err != nil {
		_ = server.Close()
		stopScheduler()
		<-schedulerDone
		return err
	}
	scheme := "http"
	if *cert != "" {
		scheme = "https"
	}
	receipt := struct {
		URL        string `json:"url"`
		InstanceID string `json:"instanceId"`
		Version    string `json:"version"`
		TokenFile  string `json:"tokenFile,omitempty"`
	}{URL: scheme + "://" + listener.Addr().String(), InstanceID: state.InstanceID, Version: core.Version}
	if os.Getenv("TANDEM_TOKEN") == "" {
		receipt.TokenFile = filepath.Join(root, "client.token")
	}
	if err = json.NewEncoder(out).Encode(receipt); err != nil {
		_ = server.Close()
		stopScheduler()
		<-schedulerDone
		return err
	}
	logger := slog.New(slog.NewTextHandler(errOut, nil))
	logger.Info("Tandem service owns scheduling", "instance", state.InstanceID, "bind", listener.Addr().String())
	select {
	case <-ctx.Done():
	case err = <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			stopScheduler()
			<-schedulerDone
			return err
		}
	}
	stopScheduler()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = server.Shutdown(shutdownCtx); err != nil {
		_ = server.Close()
	}
	<-schedulerDone
	logger.Info("Tandem stopped; runtime workers were not cancelled")
	return nil
}
