package plugins

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/uniu-network/uniubot/internal/core/logging"
	"github.com/uniu-network/uniubot/pkg/pluginrpc"
)

// maxProtocolErrors is the number of malformed stdout frames tolerated before the
// supervisor stops the plugin.
const maxProtocolErrors = 50

type ProcessOptions struct {
	Dir      string
	Manifest *Manifest
	Logger   *logging.Logger
	OnExit   func(err error)
	// RegisterHandlers runs before the RPC read loop, so host services are available
	// from the very first child frame.
	RegisterHandlers func(*pluginrpc.Peer)
}

// Process is a plugin child process: stdout carries the protocol, stderr goes to
// the host log.
type Process struct {
	manifest *Manifest
	dir      string
	logger   *logging.Logger
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	peer     *pluginrpc.Peer
	onExit   func(error)

	done     chan struct{}
	exitErr  error
	stopping atomic.Bool
	protocol atomic.Int64

	mu     sync.Mutex
	stopMu sync.Mutex
}

func StartProcess(ctx context.Context, opts ProcessOptions) (*Process, error) {
	manifest := opts.Manifest
	dir := opts.Dir
	if manifest.WorkDir != "" {
		if filepath.IsAbs(manifest.WorkDir) {
			dir = manifest.WorkDir
		} else {
			dir = filepath.Join(opts.Dir, manifest.WorkDir)
		}
	}
	// Resolve relative executables here: cmd.Dir would otherwise resolve them twice
	// (plugins/x/plugins/x/plugin).
	command := manifest.Command
	if strings.ContainsAny(command, `/\`) {
		if !filepath.IsAbs(command) {
			command = filepath.Join(opts.Dir, command)
		}
		if absolute, err := filepath.Abs(command); err == nil {
			command = absolute
		}
	}

	cmd := exec.Command(command, manifest.Args...)
	cmd.Dir = dir
	env := os.Environ()
	for key, value := range manifest.Env {
		env = append(env, key+"="+value)
	}
	cmd.Env = env
	configureProcessGroup(cmd)

	// Pipes are created explicitly: Wait closes Std*Pipe handles, which can truncate
	// the last protocol frame (e.g. the plugin.shutdown response).
	stdinRead, stdinWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		_ = stdinRead.Close()
		_ = stdinWrite.Close()
		return nil, err
	}
	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		_ = stdinRead.Close()
		_ = stdinWrite.Close()
		_ = stdoutRead.Close()
		_ = stdoutWrite.Close()
		return nil, err
	}
	cmd.Stdin = stdinRead
	cmd.Stdout = stdoutWrite
	cmd.Stderr = stderrWrite

	if err := cmd.Start(); err != nil {
		_ = stdinRead.Close()
		_ = stdinWrite.Close()
		_ = stdoutRead.Close()
		_ = stdoutWrite.Close()
		_ = stderrRead.Close()
		_ = stderrWrite.Close()
		return nil, fmt.Errorf("start plugin %s (%s): %w", manifest.ID, command, err)
	}
	// The parent closes its copies of the child ends, so EOF stays observable.
	_ = stdinRead.Close()
	_ = stdoutWrite.Close()
	_ = stderrWrite.Close()

	proc := &Process{
		manifest: manifest,
		dir:      opts.Dir,
		logger:   opts.Logger,
		cmd:      cmd,
		stdin:    stdinWrite,
		onExit:   opts.OnExit,
		done:     make(chan struct{}),
	}
	proc.peer = pluginrpc.NewPeer(stdoutRead, stdinWrite, pluginrpc.Options{
		Name:    "plugin:" + manifest.ID,
		Logger:  proc.logger,
		OnReady: opts.RegisterHandlers,
		OnProtocolError: func(error) {
			count := proc.protocol.Add(1)
			if count == maxProtocolErrors {
				proc.logger.Error("[plugin:%s] too many protocol errors, stopping the plugin", manifest.ID)
				_ = proc.Signal(os.Kill)
			}
		},
	})
	go func() {
		proc.pumpStderr(stderrRead)
		_ = stderrRead.Close()
	}()
	go func() {
		waitErr := cmd.Wait()
		proc.mu.Lock()
		proc.exitErr = waitErr
		proc.mu.Unlock()
		// Drain the frames written before exit: Wait does not close the *os.File.
		if !proc.peer.WaitClosed(2 * time.Second) {
			proc.peer.Close(waitErr)
		}
		_ = stdoutRead.Close()
		close(proc.done)
		if opts.OnExit != nil {
			opts.OnExit(waitErr)
		}
	}()
	return proc, nil
}

func (p *Process) pumpStderr(stream io.Reader) {
	reader := bufio.NewReaderSize(stream, 32*1024)
	for {
		line, err := reader.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed != "" {
			if strings.HasPrefix(trimmed, "[plugin:") {
				p.logger.Info("%s", trimmed)
			} else {
				p.logger.Info("[plugin:%s] %s", p.manifest.ID, trimmed)
			}
		}
		if err != nil {
			return
		}
	}
}

func (p *Process) Peer() *pluginrpc.Peer { return p.peer }

func (p *Process) PID() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *Process) Done() <-chan struct{} { return p.done }

func (p *Process) Exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *Process) ExitError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitErr
}

func (p *Process) ProtocolErrors() int64 { return p.protocol.Load() }

func (p *Process) Signal(sig os.Signal) error {
	if p.cmd.Process == nil {
		return nil
	}
	return signalProcess(p.cmd.Process.Pid, sig)
}

// Stop: plugin.shutdown, close stdin, wait, then SIGTERM and finally SIGKILL.
func (p *Process) Stop(ctx context.Context) error {
	p.stopMu.Lock()
	defer p.stopMu.Unlock()
	p.stopping.Store(true)
	if p.Exited() {
		return nil
	}
	shutdownCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	_, err := p.peer.Call(shutdownCtx, pluginrpc.MethodShutdown, map[string]any{})
	cancel()
	_ = p.stdin.Close()

	grace := time.NewTimer(1500 * time.Millisecond)
	defer grace.Stop()
	select {
	case <-p.done:
		return nil
	case <-grace.C:
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		p.logger.Warn("[plugin:%s] graceful shutdown failed: %v", p.manifest.ID, err)
	}
	_ = p.Signal(os.Interrupt)
	termTimer := time.NewTimer(2 * time.Second)
	defer termTimer.Stop()
	select {
	case <-p.done:
		return nil
	case <-termTimer.C:
	}
	_ = p.Signal(os.Kill)
	killTimer := time.NewTimer(2 * time.Second)
	defer killTimer.Stop()
	select {
	case <-p.done:
	case <-killTimer.C:
		return fmt.Errorf("plugin %s did not exit after SIGKILL", p.manifest.ID)
	}
	return nil
}

func (p *Process) Kill() error {
	p.stopping.Store(true)
	return p.Signal(os.Kill)
}

func (p *Process) Stopping() bool { return p.stopping.Load() }
