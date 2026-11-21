// Package webchat is a self contained browser chat adapter: static client over
// HTTP, messages over WebSocket. The wire protocol is unchanged.
package webchat

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/uniu-network/uniubot/internal/adapters"
	"github.com/uniu-network/uniubot/internal/core/events"
	"github.com/uniu-network/uniubot/internal/core/message"
)

const (
	adapterID      = "webchat"
	adapterName    = "WebChat"
	defaultGroupID = 10000
	defaultPort    = 8080
	defaultHost    = "0.0.0.0"
	readLimitBytes = 1 << 20
	writeWait      = 10 * time.Second
)

//go:embed frontend
var assets embed.FS

func init() {
	adapters.Register(adapters.Meta{
		ID:   adapterID,
		Name: adapterName,
		DefaultConfig: map[string]any{
			"host": defaultHost,
			"port": defaultPort,
		},
	}, func(cfg map[string]any) adapters.Adapter {
		return New(cfg)
	})
}

type client struct {
	userID  int64
	ws      *websocket.Conn
	mode    string
	groupID int64
	mu      sync.Mutex
}

func (c *client) writeJSON(payload any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(writeWait))
	return c.ws.WriteJSON(payload)
}

type Adapter struct {
	cfg  map[string]any
	deps adapters.Deps

	mu          sync.Mutex
	clients     map[int64]*client
	server      *http.Server
	listener    net.Listener
	userCounter int64
	upgrader    websocket.Upgrader
}

func New(cfg map[string]any) *Adapter {
	return &Adapter{
		cfg:     cfg,
		clients: map[int64]*client{},
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			CheckOrigin:     func(*http.Request) bool { return true },
		},
	}
}

func (a *Adapter) ID() string { return adapterID }

func (a *Adapter) Name() string { return adapterName }

func (a *Adapter) port() int {
	switch value := a.cfg["port"].(type) {
	case int:
		if value > 0 {
			return value
		}
	case int64:
		if value > 0 {
			return int(value)
		}
	case float64:
		if value > 0 {
			return int(value)
		}
	case string:
		var parsed int
		if _, err := fmt.Sscan(value, &parsed); err == nil && parsed > 0 {
			return parsed
		}
	}
	return defaultPort
}

func (a *Adapter) host() string {
	if value, ok := a.cfg["host"].(string); ok && value != "" {
		return value
	}
	return defaultHost
}

func (a *Adapter) Start(_ context.Context, deps adapters.Deps) error {
	a.deps = deps
	address := net.JoinHostPort(a.host(), fmt.Sprint(a.port()))
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("webchat: listen on %s: %w", address, err)
	}
	a.listener = listener

	mux := http.NewServeMux()
	mux.HandleFunc("/", a.serveHTTP)
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	a.server = server
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			deps.Logger.Error("[WebChat] server error: %v", err)
		}
	}()
	deps.Logger.Info("[WebChat] Server started on http://%s:%d", a.host(), a.port())
	return nil
}

func (a *Adapter) Stop(ctx context.Context) error {
	a.mu.Lock()
	clients := make([]*client, 0, len(a.clients))
	for _, existing := range a.clients {
		clients = append(clients, existing)
	}
	a.clients = map[int64]*client{}
	a.mu.Unlock()
	for _, existing := range clients {
		existing.mu.Lock()
		_ = existing.ws.Close()
		existing.mu.Unlock()
	}
	if a.server != nil {
		shutdownCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := a.server.Shutdown(shutdownCtx)
		cancel()
		a.server = nil
		a.deps.Bot = nil
		return err
	}
	return nil
}

var contentTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".js":   "application/javascript; charset=utf-8",
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".svg":  "image/svg+xml",
	".ico":  "image/x-icon",
}

func (a *Adapter) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		a.handleWebSocket(w, r)
		return
	}
	if r.Method == http.MethodGet {
		requestPath := r.URL.Path
		if requestPath == "/" || requestPath == "/chat" || requestPath == "/index.html" {
			a.writeAsset(w, "index.html")
			return
		}
		extension := path.Ext(requestPath)
		if _, ok := contentTypes[extension]; ok {
			if a.writeAsset(w, strings.TrimPrefix(requestPath, "/")) {
				return
			}
		}
	}
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte("Not Found"))
}

func (a *Adapter) writeAsset(w http.ResponseWriter, name string) bool {
	cleaned := path.Clean("/" + name)
	if strings.Contains(cleaned, "..") {
		return false
	}
	data, err := assets.ReadFile("frontend" + cleaned)
	if err != nil {
		return false
	}
	if contentType, ok := contentTypes[path.Ext(cleaned)]; ok {
		w.Header().Set("Content-Type", contentType)
	}
	_, _ = w.Write(data)
	return true
}

type inboundMessage struct {
	Type         string          `json:"type"`
	Mode         string          `json:"mode"`
	MessageType  string          `json:"message_type"`
	GroupID      json.RawMessage `json:"groupId"`
	GroupIDSnake json.RawMessage `json:"group_id"`
	Nickname     string          `json:"nickname"`
	Content      string          `json:"content"`
	Segments     []struct {
		Type string         `json:"type"`
		Data map[string]any `json:"data"`
	} `json:"segments"`
}

func (a *Adapter) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := a.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(readLimitBytes)

	a.mu.Lock()
	a.userCounter++
	userID := a.userCounter
	instance := &client{userID: userID, ws: conn, mode: "private", groupID: defaultGroupID}
	a.clients[userID] = instance
	a.mu.Unlock()

	_ = instance.writeJSON(map[string]any{
		"type":           "connected",
		"userId":         userID,
		"defaultGroupId": defaultGroupID,
	})

	if a.deps.Logger != nil {
		a.deps.Logger.Info("[WebChat] Client %d connected", userID)
	}

	go func() {
		defer func() {
			a.mu.Lock()
			delete(a.clients, userID)
			a.mu.Unlock()
			_ = conn.Close()
			if a.deps.Logger != nil {
				a.deps.Logger.Info("[WebChat] Client %d disconnected", userID)
			}
		}()
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				if a.deps.Logger != nil && !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					a.deps.Logger.Debug("[WebChat] read error for %d: %v", userID, err)
				}
				return
			}
			var inbound inboundMessage
			if err := json.Unmarshal(data, &inbound); err != nil {
				if a.deps.Logger != nil {
					a.deps.Logger.Error("[WebChat] Error parsing message: %v", err)
				}
				continue
			}
			a.handleInbound(instance, inbound)
		}
	}()
}

func (a *Adapter) handleInbound(instance *client, inbound inboundMessage) {
	mode, groupID := resolveContext(inbound, instance)
	if inbound.Type == "context" {
		instance.setContext(mode, groupID)
		return
	}
	if inbound.Type != "message" || a.deps.Bot == nil {
		return
	}
	msg := message.Message{}
	if len(inbound.Segments) > 0 {
		for _, segment := range inbound.Segments {
			data := segment.Data
			if data == nil {
				data = map[string]any{}
			}
			switch segment.Type {
			case "text":
				msg.Add(message.Text(fmt.Sprint(data["text"])))
			case "image":
				msg.Add(message.Image(fmt.Sprint(data["file"])))
			case "at":
				msg.Add(message.At(fmt.Sprint(data["user_id"])))
			case "raw":
				rawData, _ := data["data"].(map[string]any)
				msg.Add(message.Raw(fmt.Sprint(data["platform"]), fmt.Sprint(data["type"]), rawData))
			default:
				msg.Push(message.Segment{Type: segment.Type, Data: data})
			}
		}
	} else if inbound.Content != "" {
		msg.Add(message.Text(inbound.Content))
	}
	if len(msg) == 0 {
		return
	}
	rawText := msg.PlainText()
	nickname := inbound.Nickname
	if nickname == "" {
		nickname = fmt.Sprintf("Web User %d", instance.userID)
	}
	sessionID := fmt.Sprintf("private:%d", instance.userID)
	if mode == "group" {
		sessionID = fmt.Sprintf("group:%d", groupID)
	}
	instance.setContext(mode, groupID)

	sender := events.UserInfo{
		UserID:      instance.userID,
		SkipRole:    0,
		Nickname:    events.Str(nickname),
		DisplayName: events.Str(nickname),
		SessionID:   events.Str(sessionID),
	}
	nowMillis := time.Now().UnixMilli()
	event := &events.Event{
		Time:       nowMillis / 1000,
		SelfID:     a.deps.Bot.SelfID,
		AdapterID:  adapterID,
		Adapter:    a,
		MessageID:  fmt.Sprintf("web_group_%d_%d", groupID, nowMillis),
		Message:    msg,
		RawMessage: rawText,
		Sender:     sender,
		GroupID:    groupID,
		UserID:     instance.userID,
		Kind:       events.KindGroupMessage,
	}
	if mode != "group" {
		event.Kind = events.KindPrivateMessage
		event.MessageID = fmt.Sprintf("web_private_%d_%d", instance.userID, nowMillis)
	}
	a.deps.Emit("message", event)
}

func (c *client) setContext(mode string, groupID int64) {
	c.mu.Lock()
	c.mode = mode
	c.groupID = groupID
	c.mu.Unlock()
}

func (c *client) contextSnapshot() (string, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mode, c.groupID
}

func resolveContext(inbound inboundMessage, instance *client) (string, int64) {
	_, currentGroupID := instance.contextSnapshot()
	mode := "private"
	if inbound.Mode == "group" || inbound.MessageType == "group" {
		mode = "group"
	}
	groupID := currentGroupID
	for _, raw := range []json.RawMessage{inbound.GroupID, inbound.GroupIDSnake} {
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var number float64
		if err := json.Unmarshal(raw, &number); err == nil {
			groupID = int64(number)
			break
		}
		var text string
		if err := json.Unmarshal(raw, &text); err == nil {
			var parsed int64
			if _, err := fmt.Sscan(text, &parsed); err == nil {
				groupID = parsed
				break
			}
		}
	}
	if groupID <= 0 {
		groupID = defaultGroupID
	}
	return mode, groupID
}

// SendGroup broadcasts to clients in group mode.
func (a *Adapter) SendGroup(_ context.Context, targetID string, msg message.Message) error {
	var groupID int64
	if _, err := fmt.Sscan(targetID, &groupID); err != nil {
		if a.deps.Logger != nil {
			a.deps.Logger.Warn("[WebChat] Invalid group id: %s", targetID)
		}
		return nil
	}
	segments := msg.Clone()
	payload := map[string]any{
		"type":        "reply",
		"messageType": "group",
		"groupId":     groupID,
		"content":     msg.TextContent(),
		"segments":    segments,
		"time":        float64(time.Now().UnixMilli()) / 1000,
	}
	delivered := 0
	a.mu.Lock()
	targets := make([]*client, 0, len(a.clients))
	for _, existing := range a.clients {
		if mode, clientGroupID := existing.contextSnapshot(); mode == "group" && clientGroupID == groupID {
			targets = append(targets, existing)
		}
	}
	a.mu.Unlock()
	for _, target := range targets {
		if err := target.writeJSON(payload); err == nil {
			delivered++
		}
	}
	if delivered == 0 && a.deps.Logger != nil {
		a.deps.Logger.Warn("[WebChat] No connected clients in group %d", groupID)
	}
	return nil
}

func (a *Adapter) SendPrivate(_ context.Context, targetID string, msg message.Message) error {
	var userID int64
	if _, err := fmt.Sscan(targetID, &userID); err != nil {
		return nil
	}
	a.mu.Lock()
	target, ok := a.clients[userID]
	a.mu.Unlock()
	if !ok {
		if a.deps.Logger != nil {
			a.deps.Logger.Warn("[WebChat] Client %d not found or disconnected", userID)
		}
		return nil
	}
	segments := msg.Clone()
	payload := map[string]any{
		"type":        "reply",
		"messageType": "private",
		"userId":      userID,
		"content":     msg.TextContent(),
		"segments":    segments,
		"time":        float64(time.Now().UnixMilli()) / 1000,
	}
	return target.writeJSON(payload)
}
