// Package devserver supervises the Vite dev server used by dashboard.debug: it
// starts it, waits for the port, exposes the URL and terminates it on shutdown.
package devserver

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/uniu-network/uniubot/internal/core/logging"
)

type Options struct {
	Dir     string
	Command string
	// Args defaults to npm run dev -- --host <host> --port <port> --strictPort.
	Args         []string
	Host         string
	Port         int
	ReadyTimeout time.Duration
	Logger       *logging.Logger
}

type Server struct {
	url    string
	cmd    *exec.Cmd
	logger *logging.Logger

	mu      sync.RWMutex
	started bool
	ready   bool
	exitErr error

	done     chan struct{}
	stopOnce sync.Once
}

// URL reports the base URL and whether the server is ready to serve.
func (s *Server) URL() (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.url, s.ready
}

func (s *Server) Alive() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

func Start(ctx context.Context, opts Options) (*Server, error) {
	dir := opts.Dir
	if dir == "" {
		dir = "frontend"
	}
	host := opts.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := opts.Port
	if port <= 0 {
		port = 3000
	}
	timeout := opts.ReadyTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	logger := opts.Logger
	if logger == nil {
		logger = logging.Default()
	}

	viteConfig := filepath.Join(dir, "vite.config.js")
	if _, err := os.Stat(viteConfig); err != nil {
		return nil, fmt.Errorf("cannot find %s: dashboard.debug requires a Vite project in %s", viteConfig, dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules")); err != nil {
		return nil, fmt.Errorf("%s/node_modules is missing: run `make frontend` (or `cd %s && npm install`) once", dir, dir)
	}

	command := opts.Command
	if command == "" {
		command = "npm"
	}
	args := opts.Args
	if len(args) == 0 {
		args = []string{"run", "dev", "--", "--host", host, "--port", fmt.Sprint(port), "--strictPort"}
	}

	cmd := exec.Command(command, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "BROWSER=none", "FORCE_COLOR=0")
	configureProcessGroup(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("cannot start %s: %w", command, err)
	}

	server := &Server{
		url:    fmt.Sprintf("http://%s:%d", host, port),
		cmd:    cmd,
		logger: logger,
		ready:  false,
		done:   make(chan struct{}),
	}
	// Vite output is logged at DEBUG level.
	go server.pump(stdout)
	go server.pump(stderr)
	go func() {
		waitErr := cmd.Wait()
		server.mu.Lock()
		server.exitErr = waitErr
		server.ready = false
		server.mu.Unlock()
		close(server.done)
	}()

	address := net.JoinHostPort(host, fmt.Sprint(port))
	deadline := time.Now().Add(timeout)
	for {
		if !server.Alive() {
			server.mu.RLock()
			exitErr := server.exitErr
			server.mu.RUnlock()
			if exitErr == nil {
				exitErr = errors.New("dev server exited during startup")
			}
			return nil, fmt.Errorf("vite dev server failed to start: %w (is port %d already in use?)", exitErr, port)
		}
		if conn, err := net.DialTimeout("tcp", address, 300*time.Millisecond); err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			_ = server.Stop(context.Background())
			return nil, fmt.Errorf("vite dev server did not accept connections on %s within %s", address, timeout)
		}
		select {
		case <-ctx.Done():
			_ = server.Stop(context.Background())
			return nil, ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}

	server.mu.Lock()
	server.started = true
	server.ready = true
	server.mu.Unlock()
	logger.Info("Dashboard dev server is running on %s", server.url)
	return server, nil
}

func (s *Server) pump(stream interface{ Read([]byte) (int, error) }) {
	reader := bufio.NewReaderSize(stream, 16*1024)
	for {
		line, err := reader.ReadString('\n')
		if trimmed := strings.TrimRight(line, "\r\n"); trimmed != "" {
			s.logger.Debug("[vite] %s", trimmed)
		}
		if err != nil {
			return
		}
	}
}

// Stop: SIGINT to the process group, then SIGKILL.
func (s *Server) Stop(ctx context.Context) error {
	var err error
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.ready = false
		s.mu.Unlock()
		if !s.Alive() {
			return
		}
		if signalErr := signalProcessGroup(s.cmd, os.Interrupt); signalErr != nil {
			err = signalErr
		}
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		select {
		case <-s.done:
			return
		case <-timer.C:
		}
		if killErr := signalProcessGroup(s.cmd, os.Kill); killErr != nil && err == nil {
			err = killErr
		}
		select {
		case <-s.done:
		case <-time.After(2 * time.Second):
			if err == nil {
				err = errors.New("vite dev server did not exit after SIGKILL")
			}
		}
	})
	if err != nil {
		s.logger.Warn("failed to stop the dashboard dev server cleanly: %v", err)
	}
	return err
}
