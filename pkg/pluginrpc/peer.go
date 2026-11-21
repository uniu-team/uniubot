package pluginrpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

type Logger interface {
	Debug(format string, args ...any)
	Warn(format string, args ...any)
	Error(format string, args ...any)
}

type HandlerFunc func(ctx context.Context, params json.RawMessage) (any, error)

type Options struct {
	MaxMessageSize int
	// WriteTimeout bounds one frame write (default 10s); a timeout closes the peer
	// because the stream may be out of sync.
	WriteTimeout    time.Duration
	MaxInflight     int
	Logger          Logger
	OnProtocolError func(error)
	OnClose         func(error)
	// OnReady runs synchronously before the read loop starts, so the first inbound
	// frame never arrives without handlers.
	OnReady func(*Peer)
	Name    string
}

type call struct {
	ch chan *Message
}

type Peer struct {
	reader  *bufio.Reader
	writer  io.Writer
	opts    Options
	name    string
	writeMu sync.Mutex

	handlerMu sync.RWMutex
	handlers  map[string]HandlerFunc

	pendingMu sync.Mutex
	pending   map[string]*call
	nextID    atomic.Uint64

	inflightMu sync.Mutex
	inflight   map[string]context.CancelFunc

	closed     chan struct{}
	closeOnce  sync.Once
	closeErr   error
	sem        chan struct{}
	startedAt  time.Time
	handlersWG sync.WaitGroup
}

// NewPeer panics on a nil reader or writer: inside the read loop it could only
// crash the process.
func NewPeer(reader io.Reader, writer io.Writer, opts Options) *Peer {
	if reader == nil || writer == nil {
		panic("pluginrpc: NewPeer requires a non-nil reader and writer")
	}
	if opts.MaxMessageSize <= 0 {
		opts.MaxMessageSize = DefaultMaxMessageSize
	}
	if opts.MaxInflight <= 0 {
		opts.MaxInflight = DefaultMaxInflight
	}
	if opts.WriteTimeout <= 0 {
		opts.WriteTimeout = defaultWriteTimeout
	}
	if opts.Logger == nil {
		opts.Logger = nopLogger{}
	}
	p := &Peer{
		reader:    bufio.NewReaderSize(reader, 64*1024),
		writer:    writer,
		opts:      opts,
		name:      opts.Name,
		handlers:  map[string]HandlerFunc{},
		pending:   map[string]*call{},
		inflight:  map[string]context.CancelFunc{},
		closed:    make(chan struct{}),
		sem:       make(chan struct{}, opts.MaxInflight),
		startedAt: time.Now(),
	}
	if opts.OnReady != nil {
		opts.OnReady(p)
	}
	go p.readLoop()
	return p
}

func (p *Peer) Uptime() time.Duration { return time.Since(p.startedAt) }

func (p *Peer) Handle(method string, handler HandlerFunc) {
	p.handlerMu.Lock()
	p.handlers[method] = handler
	p.handlerMu.Unlock()
}

func (p *Peer) HandleFunc(method string, handler HandlerFunc) { p.Handle(method, handler) }

func (p *Peer) lookup(method string) (HandlerFunc, bool) {
	p.handlerMu.RLock()
	defer p.handlerMu.RUnlock()
	handler, ok := p.handlers[method]
	return handler, ok
}

func (p *Peer) Closed() bool {
	select {
	case <-p.closed:
		return true
	default:
		return false
	}
}

// WaitClosed reports whether the peer closed within the timeout.
func (p *Peer) WaitClosed(timeout time.Duration) bool {
	if p.Closed() {
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.closed:
		return true
	case <-timer.C:
		return false
	}
}

// Close fails every pending call.
func (p *Peer) Close(err error) {
	p.closeOnce.Do(func() {
		p.closeErr = err
		close(p.closed)
		p.pendingMu.Lock()
		pending := p.pending
		p.pending = map[string]*call{}
		p.pendingMu.Unlock()
		reason := "plugin connection closed"
		if err != nil {
			reason = err.Error()
		}
		for _, entry := range pending {
			select {
			case entry.ch <- &Message{Error: NewError(CodeNotReady, reason)}:
			default:
			}
		}
		// Cancel running handlers so a dead plugin does not block goroutines.
		p.inflightMu.Lock()
		cancels := make([]context.CancelFunc, 0, len(p.inflight))
		for _, cancel := range p.inflight {
			cancels = append(cancels, cancel)
		}
		p.inflightMu.Unlock()
		for _, cancel := range cancels {
			cancel()
		}
		if p.opts.OnClose != nil {
			p.opts.OnClose(err)
		}
	})
}

func (p *Peer) readLoop() {
	for {
		frame, err := p.readFrame()
		if err != nil {
			if errors.Is(err, errFrameTooLarge) {
				p.opts.Logger.Error("%s: oversized RPC frame discarded (limit %d bytes)", p.name, p.opts.MaxMessageSize)
				_ = p.writeMessage(&Message{
					JSONRPC: "2.0",
					ID:      json.RawMessage("null"),
					Error:   NewError(CodePayloadTooLarge, "message exceeds the maximum frame size"),
				})
				if p.opts.OnProtocolError != nil {
					p.opts.OnProtocolError(err)
				}
				continue
			}
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
				// Let running handlers flush their response before the pipe goes away.
				p.waitHandlers(2 * time.Second)
				p.Close(nil)
				return
			}
			p.Close(err)
			return
		}
		if len(bytes.TrimSpace(frame)) == 0 {
			continue
		}
		var message Message
		if err := json.Unmarshal(frame, &message); err != nil {
			p.opts.Logger.Error("%s: invalid JSON-RPC message: %v", p.name, err)
			_ = p.writeMessage(&Message{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: NewError(CodeParseError, "parse error")})
			if p.opts.OnProtocolError != nil {
				p.opts.OnProtocolError(err)
			}
			continue
		}
		if message.JSONRPC != "" && message.JSONRPC != "2.0" {
			p.opts.Logger.Error("%s: unsupported jsonrpc version %q", p.name, message.JSONRPC)
			if message.IsRequest() && message.HasID() {
				_ = p.writeMessage(&Message{JSONRPC: "2.0", ID: message.ID, Error: NewError(CodeInvalidRequest, "unsupported jsonrpc version")})
			}
			if p.opts.OnProtocolError != nil {
				p.opts.OnProtocolError(fmt.Errorf("unsupported jsonrpc version %q", message.JSONRPC))
			}
			continue
		}
		p.dispatch(&message)
	}
}

func (p *Peer) dispatch(message *Message) {
	if !message.IsRequest() {
		p.deliver(message)
		return
	}
	if message.Method == NotificationCancel {
		p.cancelInflight(message.Params)
		return
	}
	handler, ok := p.lookup(message.Method)
	if !ok {
		if message.HasID() {
			_ = p.writeMessage(&Message{JSONRPC: "2.0", ID: message.ID, Error: NewError(CodeMethodNotFound, "method not found: "+message.Method)})
		}
		return
	}
	select {
	case p.sem <- struct{}{}:
	default:
		if message.HasID() {
			_ = p.writeMessage(&Message{JSONRPC: "2.0", ID: message.ID, Error: NewError(CodePluginError, "too many in-flight requests")})
		} else {
			p.opts.Logger.Warn("%s: dropping notification %s, too many in-flight requests", p.name, message.Method)
		}
		return
	}
	if message.IsNotification() {
		p.handlersWG.Add(1)
		go p.runHandler(context.Background(), nil, message, handler)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	key := string(message.ID)
	p.inflightMu.Lock()
	p.inflight[key] = cancel
	p.inflightMu.Unlock()
	p.handlersWG.Add(1)
	go p.runHandler(ctx, cancel, message, handler)
}

// waitHandlers reports once all handlers finished or the timeout expired.
func (p *Peer) waitHandlers(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		p.handlersWG.Wait()
		close(done)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

func (p *Peer) runHandler(ctx context.Context, cancel context.CancelFunc, message *Message, handler HandlerFunc) {
	defer p.handlersWG.Done()
	defer func() { <-p.sem }()
	if cancel != nil {
		defer cancel()
		defer func() {
			p.inflightMu.Lock()
			delete(p.inflight, string(message.ID))
			p.inflightMu.Unlock()
		}()
	}
	result, err := safeInvoke(ctx, handler, message.Params)
	if !message.HasID() {
		if err != nil {
			p.opts.Logger.Error("%s: notification %s failed: %v", p.name, message.Method, err)
		}
		return
	}
	if err != nil {
		rpcErr := AsError(err)
		if rpcErr == nil {
			if errors.Is(err, context.Canceled) {
				rpcErr = NewError(CodeCancelled, "request cancelled")
			} else if errors.Is(err, context.DeadlineExceeded) {
				rpcErr = NewError(CodeTimeout, "request timed out")
			} else {
				rpcErr = NewError(CodePluginError, err.Error())
			}
		}
		_ = p.writeMessage(&Message{JSONRPC: "2.0", ID: message.ID, Error: rpcErr})
		return
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		_ = p.writeMessage(&Message{JSONRPC: "2.0", ID: message.ID, Error: NewError(CodeInternalError, "cannot encode result: "+err.Error())})
		return
	}
	_ = p.writeMessage(&Message{JSONRPC: "2.0", ID: message.ID, Result: encoded})
}

func safeInvoke(ctx context.Context, handler HandlerFunc, params json.RawMessage) (result any, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = NewError(CodeInternalError, fmt.Sprintf("handler panic: %v", recovered))
		}
	}()
	return handler(ctx, params)
}

func (p *Peer) cancelInflight(params json.RawMessage) {
	var cancel CancelParams
	if err := json.Unmarshal(params, &cancel); err != nil {
		return
	}
	key := string(cancel.ID)
	p.inflightMu.Lock()
	stop, ok := p.inflight[key]
	p.inflightMu.Unlock()
	if ok {
		stop()
	}
}

func (p *Peer) deliver(message *Message) {
	if !message.HasID() {
		p.opts.Logger.Warn("%s: response without id ignored", p.name)
		return
	}
	key := string(message.ID)
	p.pendingMu.Lock()
	entry, ok := p.pending[key]
	if ok {
		delete(p.pending, key)
	}
	p.pendingMu.Unlock()
	if !ok {
		p.opts.Logger.Debug("%s: response for unknown request %s ignored", p.name, key)
		return
	}
	select {
	case entry.ch <- message:
	default:
	}
}

func (p *Peer) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if p.Closed() {
		return nil, NewError(CodeNotReady, "plugin connection is closed")
	}
	id := p.nextID.Add(1)
	idRaw, err := json.Marshal(id)
	if err != nil {
		return nil, err
	}
	encoded, err := marshalParams(params)
	if err != nil {
		return nil, err
	}
	entry := &call{ch: make(chan *Message, 1)}
	key := string(idRaw)
	p.pendingMu.Lock()
	select {
	case <-p.closed:
		p.pendingMu.Unlock()
		reason := "plugin connection is closed"
		if p.closeErr != nil {
			reason = p.closeErr.Error()
		}
		return nil, NewError(CodeNotReady, reason)
	default:
	}
	p.pending[key] = entry
	p.pendingMu.Unlock()
	defer func() {
		p.pendingMu.Lock()
		delete(p.pending, key)
		p.pendingMu.Unlock()
	}()

	if err := p.writeMessage(&Message{JSONRPC: "2.0", ID: idRaw, Method: method, Params: encoded}); err != nil {
		return nil, err
	}
	select {
	case response := <-entry.ch:
		if response.Error != nil {
			return nil, response.Error
		}
		return response.Result, nil
	case <-ctx.Done():
		_ = p.writeMessage(&Message{JSONRPC: "2.0", Method: NotificationCancel, Params: mustJSON(CancelParams{ID: idRaw})})
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, NewError(CodeTimeout, "request timed out: "+method)
		}
		return nil, NewError(CodeCancelled, "request cancelled: "+method)
	case <-p.closed:
		reason := "plugin connection closed"
		if p.closeErr != nil {
			reason = p.closeErr.Error()
		}
		return nil, NewError(CodeNotReady, reason)
	}
}

func (p *Peer) Notify(method string, params any) error {
	encoded, err := marshalParams(params)
	if err != nil {
		return err
	}
	return p.writeMessage(&Message{JSONRPC: "2.0", Method: method, Params: encoded})
}

func (p *Peer) CallJSON(ctx context.Context, method string, params, out any) error {
	result, err := p.Call(ctx, method, params)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if len(result) == 0 || string(result) == "null" {
		return nil
	}
	return json.Unmarshal(result, out)
}

func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	if raw, ok := params.(json.RawMessage); ok {
		return raw, nil
	}
	if raw, ok := params.([]byte); ok {
		return json.RawMessage(raw), nil
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

func mustJSON(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return encoded
}

func (p *Peer) writeMessage(message *Message) error {
	encoded, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if len(encoded) > p.opts.MaxMessageSize {
		return NewError(CodePayloadTooLarge, "outbound message exceeds the maximum frame size")
	}
	encoded = append(encoded, '\n')

	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	select {
	case <-p.closed:
		return NewError(CodeNotReady, "plugin connection is closed")
	default:
	}
	// A child that stops draining stdin must not block the host forever: child
	// stdio usually rejects write deadlines, so the write runs in a goroutine and is
	// abandoned after writeTimeout; killing the process unblocks it.
	if deadlineSetter, ok := p.writer.(interface{ SetWriteDeadline(time.Time) error }); ok {
		if err := deadlineSetter.SetWriteDeadline(time.Now().Add(p.opts.WriteTimeout)); err == nil {
			defer func() { _ = deadlineSetter.SetWriteDeadline(time.Time{}) }()
			if _, err := p.writer.Write(encoded); err != nil {
				return NewError(CodeNotReady, "write failed: "+err.Error())
			}
			return nil
		}
	}
	result := make(chan error, 1)
	go func() {
		_, err := p.writer.Write(encoded)
		result <- err
	}()
	timer := time.NewTimer(p.opts.WriteTimeout)
	defer timer.Stop()
	select {
	case err := <-result:
		if err != nil {
			return NewError(CodeNotReady, "write failed: "+err.Error())
		}
		return nil
	case <-timer.C:
		// The stream can no longer stay frame aligned, so mark the peer dead instead of
		// letting the next write interleave; the supervisor restarts the plugin.
		err := NewError(CodeTimeout, "write to the peer timed out; connection closed")
		p.Close(err)
		return err
	}
}

const defaultWriteTimeout = 10 * time.Second

var errFrameTooLarge = errors.New("rpc frame exceeds the maximum size")

func (p *Peer) readFrame() ([]byte, error) {
	var frame []byte
	for {
		chunk, err := p.reader.ReadSlice('\n')
		if len(frame)+len(chunk) > p.opts.MaxMessageSize {
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = p.reader.ReadSlice('\n')
			}
			return nil, errFrameTooLarge
		}
		frame = append(frame, chunk...)
		switch {
		case err == nil:
			return bytes.TrimRight(frame, "\r\n"), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if len(frame) > 0 {
				return bytes.TrimRight(frame, "\r\n"), nil
			}
			return nil, io.EOF
		default:
			return nil, err
		}
	}
}

type nopLogger struct{}

func (nopLogger) Debug(string, ...any) {}
func (nopLogger) Warn(string, ...any)  {}
func (nopLogger) Error(string, ...any) {}
