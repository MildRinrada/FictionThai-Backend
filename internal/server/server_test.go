package server_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/fictionthai/fictionthai/backend/internal/config"
	"github.com/fictionthai/fictionthai/backend/internal/server"
)

// freePort asks the OS for an unused port so these tests never collide with
// something already running on the developer's machine.
func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", loopback+":0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port
}

// loopback keeps every test listener on 127.0.0.1. Binding the wildcard
// address would make a host firewall prompt on each `go test` run, and the
// tests have no need to be reachable from the network.
const loopback = "127.0.0.1"

func testServerConfig(port int) *config.Config {
	return &config.Config{
		App: config.App{Name: "fictionthai-api", Env: config.EnvTest, LogLevel: "error"},
		HTTP: config.HTTP{
			Port:            port,
			BindAddress:     loopback,
			ReadTimeout:     5 * time.Second,
			WriteTimeout:    5 * time.Second,
			IdleTimeout:     5 * time.Second,
			ShutdownTimeout: 5 * time.Second,
			MaxRequestBytes: 1 << 20,
		},
		CORS: config.CORS{AllowedOrigins: []string{"http://localhost:3000"}},
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// docs/14 §46: a deployment must stop accepting connections, finish active
// requests, and exit - never sever a reader mid-chapter.
func TestServer_ShutsDownGracefullyOnContextCancel(t *testing.T) {
	port := freePort(t)
	cfg := testServerConfig(port)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := server.New(cfg, mux, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Run(ctx) }()

	url := "http://127.0.0.1:" + strconv.Itoa(port) + "/health"
	waitUntilServing(t, url)

	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run() returned %v, want a clean shutdown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run() did not return after its context was cancelled")
	}

	// The listener must actually be released, or a rolling deploy would fail to
	// rebind the port.
	if _, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second); err == nil {
		t.Error("the port is still accepting connections after shutdown")
	}
}

// A port that is already in use must surface as a startup error, not a server
// that silently serves nothing.
func TestServer_ReportsBindFailure(t *testing.T) {
	// Occupy the SAME address the server will ask for. A loopback listener does
	// not conflict with a wildcard bind on Windows, so both sides must use
	// loopback for the conflict to be real.
	listener, err := net.Listen("tcp", loopback+":0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	defer func() { _ = listener.Close() }()

	port := listener.Addr().(*net.TCPAddr).Port
	srv := server.New(testServerConfig(port), http.NewServeMux(), discardLogger())

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Run(context.Background()) }()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("Run() returned nil for an occupied port; startup must fail loudly")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() neither started nor reported a bind failure")
	}
}

func waitUntilServing(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the server never began serving")
}
