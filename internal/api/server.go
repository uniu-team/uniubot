package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/uniu-network/uniubot/internal/adapters"
	"github.com/uniu-network/uniubot/internal/app"
	"github.com/uniu-network/uniubot/internal/core/config"
	"github.com/uniu-network/uniubot/internal/core/logging"
	"github.com/uniu-network/uniubot/internal/storage"
)

type Server struct {
	bots       *app.Manager
	global     *config.Store
	storage    *storage.Manager
	logger     *logging.Logger
	jwt        *JWT
	version    string
	staticDir  string
	devProxy   func() (string, bool) // dashboard dev server target + readiness
	httpServer *http.Server
	mux        *http.ServeMux
}

type Options struct {
	Bots      *app.Manager
	Global    *config.Store
	Storage   *storage.Manager
	Logger    *logging.Logger
	Version   string
	StaticDir string
	// DevProxyURL proxies unknown paths to an already running dev server.
	DevProxyURL string
	// DevProxy returns the current target and readiness; requests get 503 until
	// ready, matching the original "Dashboard dev server is starting" response.
	DevProxy func() (string, bool)
}

func NewServer(opts Options) (*Server, error) {
	jwtHelper, err := LoadOrCreateJWT("data/jwt")
	if err != nil {
		return nil, fmt.Errorf("cannot initialise JWT keys: %w", err)
	}
	staticDir := opts.StaticDir
	if staticDir == "" {
		staticDir = "frontend/dist"
	}
	s := &Server{
		bots:      opts.Bots,
		global:    opts.Global,
		storage:   opts.Storage,
		logger:    opts.Logger,
		jwt:       jwtHelper,
		version:   opts.Version,
		staticDir: staticDir,
		devProxy:  resolveDevProxy(opts),
		mux:       http.NewServeMux(),
	}
	s.routes()
	return s, nil
}

func resolveDevProxy(opts Options) func() (string, bool) {
	if opts.DevProxy != nil {
		return opts.DevProxy
	}
	if opts.DevProxyURL != "" {
		target := opts.DevProxyURL
		return func() (string, bool) { return target, true }
	}
	return nil
}

func (s *Server) routes() {
	// Authentication.
	s.mux.HandleFunc("POST /auth/login", s.handleLogin)

	s.mux.HandleFunc("POST /message", s.handleMessageWebhook)
	s.mux.HandleFunc("POST /message/{botId}", s.handleMessageWebhook)

	s.mux.HandleFunc("GET /api/bots/list", s.authed(s.handleBotsList))
	s.mux.HandleFunc("GET /api/config", s.authed(s.handleConfigGet))
	s.mux.HandleFunc("POST /api/config", s.authed(s.handleConfigSet))
	s.mux.HandleFunc("POST /api/system/restart", s.authed(s.handleSystemRestart))
	s.mux.HandleFunc("POST /api/bots/create", s.authed(s.handleBotsCreate))
	s.mux.HandleFunc("POST /api/bots/delete", s.authed(s.handleBotsDelete))
	s.mux.HandleFunc("POST /api/bots/start", s.authed(s.handleBotsStart))
	s.mux.HandleFunc("POST /api/bots/stop", s.authed(s.handleBotsStop))
	s.mux.HandleFunc("GET /api/bots/config", s.authed(s.handleBotsConfig))
	s.mux.HandleFunc("POST /api/bots/setConfig", s.authed(s.handleBotsSetConfig))
	s.mux.HandleFunc("POST /api/bots/reload", s.authed(s.handleBotsReload))
	s.mux.HandleFunc("GET /api/status", s.authed(s.handleStatus))
	s.mux.HandleFunc("GET /api/plugins/list", s.authed(s.handlePluginsList))
	s.mux.HandleFunc("POST /api/plugins/getConfig", s.authed(s.handlePluginsGetConfig))
	s.mux.HandleFunc("POST /api/plugins/setConfig", s.authed(s.handlePluginsSetConfig))
	s.mux.HandleFunc("POST /api/plugins/reload", s.authed(s.handlePluginsReload))
	s.mux.HandleFunc("POST /api/plugins/unload", s.authed(s.handlePluginsUnload))
	s.mux.HandleFunc("POST /api/plugins/load", s.authed(s.handlePluginsLoad))
	s.mux.HandleFunc("POST /api/plugins/toggle", s.authed(s.handlePluginsToggle))
	s.mux.HandleFunc("GET /api/adapters/list", s.authed(s.handleAdaptersList))
	s.mux.HandleFunc("POST /api/adapters/setConfig", s.authed(s.handleAdaptersSetConfig))
	s.mux.HandleFunc("POST /api/adapters/reload", s.authed(s.handleAdaptersReload))
	s.mux.HandleFunc("GET /api/adapters/available", s.authed(s.handleAdaptersAvailable))
	s.mux.HandleFunc("POST /api/adapters/add", s.authed(s.handleAdaptersAdd))
	s.mux.HandleFunc("POST /api/adapters/remove", s.authed(s.handleAdaptersRemove))
	s.mux.HandleFunc("GET /api/messages", s.authed(s.handleMessages))
	s.mux.HandleFunc("GET /api/commands/list", s.authed(s.handleCommandsList))
	s.mux.HandleFunc("GET /api/db/tables", s.authed(s.handleDBTables))
	s.mux.HandleFunc("GET /api/db/table", s.authed(s.handleDBTable))
	s.mux.HandleFunc("GET /api/db/table/data", s.authed(s.handleDBTableData))
	s.mux.HandleFunc("POST /api/db/query", s.authed(s.handleDBQuery))
	s.mux.HandleFunc("POST /api/db/table/row", s.authed(s.handleDBInsertRow))
	s.mux.HandleFunc("PUT /api/db/table/row", s.authed(s.handleDBUpdateRow))
	s.mux.HandleFunc("POST /api/db/table/row/delete", s.authed(s.handleDBDeleteRow))
	s.mux.HandleFunc("POST /api/db/table/create", s.authed(s.handleDBCreateTable))
	s.mux.HandleFunc("POST /api/db/table/drop", s.authed(s.handleDBDropTable))
	s.mux.HandleFunc("GET /api/logs", s.authed(s.handleLogs))
	s.mux.HandleFunc("GET /api/logs/stream", s.authed(s.handleLogsStream))

	s.mux.HandleFunc("/", s.handleFrontend)
}

func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.logger.IsLevelEnabled(logging.LevelDebug) {
			s.logDebugRequest(r)
		}
		defer func() {
			if recovered := recover(); recovered != nil {
				s.logger.Error("HTTP error: panic while handling %s %s: %v", r.Method, r.URL.Path, recovered)
				writeJSON(w, http.StatusInternalServerError, map[string]any{"code": 500, "message": "internal error"})
			}
		}()
		s.mux.ServeHTTP(w, r)
	})
}

// sensitiveHeaders are redacted from the debug request log.
var sensitiveHeaders = map[string]bool{
	"cookie":        true,
	"authorization": true,
	"x-signature":   true,
	"token":         true,
}

func (s *Server) logDebugRequest(r *http.Request) {
	body := ""
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)); err == nil {
			body = string(raw)
			r.Body = io.NopCloser(strings.NewReader(body))
		}
	}
	headers := make(map[string]string, len(r.Header))
	for key, values := range r.Header {
		if sensitiveHeaders[strings.ToLower(key)] {
			headers[key] = "[redacted]"
			continue
		}
		headers[key] = strings.Join(values, ", ")
	}
	encoded, _ := json.Marshal(headers)
	s.logger.Debug("HTTP %s %s?%s headers=%s body=%s", r.Method, r.URL.Path, r.URL.RawQuery, string(encoded), body)
}

func (s *Server) ListenAndServe(host string, port int) error {
	address := net.JoinHostPort(host, fmt.Sprint(port))
	server := &http.Server{
		Addr:              address,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}
	s.httpServer = server
	s.logger.Info("Server is running on http://%s:%d", host, port)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpServer == nil {
		return nil
	}
	return s.httpServer.Shutdown(ctx)
}

func (s *Server) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("token")
		if err != nil || cookie.Value == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"code": 401, "message": "no token found"})
			return
		}
		if _, err := s.jwt.Verify(cookie.Value); err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"code": 401, "message": "token invalid"})
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		return
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"code": status, "message": message})
}

func writeText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// readBody keeps the raw text: the OneBot adapter verifies a signature over it.
func readBody(r *http.Request) (string, any, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return "", nil, err
	}
	text := string(raw)
	if strings.TrimSpace(text) == "" {
		return text, nil, nil
	}
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var body any
	if err := decoder.Decode(&body); err != nil {
		return text, nil, err
	}
	return text, normalizeJSON(body), nil
}

// normalizeJSON turns json.Number into float64/int64.
func normalizeJSON(value any) any {
	switch typed := value.(type) {
	case json.Number:
		if parsed, err := typed.Int64(); err == nil {
			return parsed
		}
		if parsed, err := typed.Float64(); err == nil {
			return parsed
		}
		return typed.String()
	case map[string]any:
		for key, item := range typed {
			typed[key] = normalizeJSON(item)
		}
		return typed
	case []any:
		for i, item := range typed {
			typed[i] = normalizeJSON(item)
		}
		return typed
	default:
		return value
	}
}

func bodyMap(body any) map[string]any {
	if typed, ok := body.(map[string]any); ok {
		return typed
	}
	return map[string]any{}
}

func queryInt(r *http.Request, name string, def int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def
	}
	var parsed int
	if _, err := fmt.Sscan(raw, &parsed); err != nil {
		return def
	}
	return parsed
}

func clamp(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	_, body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	payload := bodyMap(body)
	username, _ := payload["username"].(string)
	password, _ := payload["password"].(string)
	if username != s.global.String("web.username", "") || password != s.global.String("web.password", "") {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"code": 401, "message": "username or password error"})
		return
	}
	token, err := s.jwt.Issue(map[string]any{"username": username, "password": password}, TokenTTLSeconds)
	if err != nil {
		s.logger.Error("failed to issue token: %v", err)
		writeError(w, http.StatusInternalServerError, "cannot issue token")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "token",
		Value:    token,
		Path:     "/",
		MaxAge:   TokenTTLSeconds,
		HttpOnly: false,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"code": 0})
}

func (s *Server) handleMessageWebhook(w http.ResponseWriter, r *http.Request) {
	rawBody, body, err := readBody(r)
	if err != nil {
		writeText(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	botID := r.PathValue("botId")
	var instance *app.BotInstance
	var ok bool
	if botID == "" {
		instance, ok = s.bots.FirstInstance()
		if !ok {
			writeText(w, http.StatusNotFound, "No bot instances available")
			return
		}
	} else {
		instance, ok = s.bots.GetInstance(botID)
		if !ok {
			writeText(w, http.StatusNotFound, fmt.Sprintf("Bot %q not found", botID))
			return
		}
	}
	headers := make(map[string]string, len(r.Header))
	for key, values := range r.Header {
		if len(values) > 0 {
			headers[key] = values[0]
		}
	}
	response, handled := instance.Adapters.HandleHTTPRequest(r.Context(), &adapters.HTTPRequest{
		Body:    body,
		RawBody: rawBody,
		Headers: headers,
	})
	if !handled {
		writeText(w, http.StatusNotFound, "No adapter handled this request")
		return
	}
	if response.Status == 0 {
		response.Status = http.StatusOK
	}
	switch typed := response.Body.(type) {
	case nil:
		w.WriteHeader(response.Status)
	case string:
		writeText(w, response.Status, typed)
	default:
		writeJSON(w, response.Status, typed)
	}
}

var assetContentTypes = map[string]string{
	".css":   "text/css; charset=utf-8",
	".html":  "text/html; charset=utf-8",
	".ico":   "image/x-icon",
	".js":    "text/javascript; charset=utf-8",
	".json":  "application/json; charset=utf-8",
	".map":   "application/json; charset=utf-8",
	".png":   "image/png",
	".svg":   "image/svg+xml",
	".wasm":  "application/wasm",
	".webp":  "image/webp",
	".woff":  "font/woff",
	".woff2": "font/woff2",
}

func (s *Server) handleFrontend(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeJSON(w, http.StatusNotFound, map[string]any{"code": 404, "message": "not found"})
		return
	}
	if s.devProxy != nil {
		target, ready := s.devProxy()
		if !ready || target == "" {
			writeText(w, http.StatusServiceUnavailable, "Dashboard dev server is starting")
			return
		}
		s.proxyDashboard(w, r, target)
		return
	}
	path := r.URL.Path
	if strings.HasPrefix(path, "/assets/") {
		s.serveStatic(w, r, filepath.Join(s.staticDir, "assets", strings.TrimPrefix(path, "/assets/")))
		return
	}
	if path == "/favicon.ico" {
		candidates := []string{
			filepath.Join(s.staticDir, "favicon.ico"),
			filepath.Join("frontend/public/favicon.ico"),
		}
		for _, candidate := range candidates {
			if _, err := os.Stat(candidate); err == nil {
				s.serveStatic(w, r, candidate)
				return
			}
		}
		writeText(w, http.StatusNotFound, "Not Found")
		return
	}
	if accept := r.Header.Get("Accept"); strings.Contains(accept, "text/html") {
		s.serveStatic(w, r, filepath.Join(s.staticDir, "index.html"))
		return
	}
	writeText(w, http.StatusNotFound, "Not Found")
}

func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request, filePath string) {
	absoluteRoot, err := filepath.Abs(s.staticDir)
	if err != nil {
		writeText(w, http.StatusInternalServerError, "Internal error")
		return
	}
	absolutePath, err := filepath.Abs(filePath)
	if err != nil || (absolutePath != absoluteRoot && !strings.HasPrefix(absolutePath, absoluteRoot+string(os.PathSeparator))) {
		writeText(w, http.StatusNotFound, "Not Found")
		return
	}
	info, err := os.Stat(absolutePath)
	if err != nil || info.IsDir() {
		writeText(w, http.StatusNotFound, "Not Found")
		return
	}
	if contentType, ok := assetContentTypes[strings.ToLower(filepath.Ext(absolutePath))]; ok {
		w.Header().Set("Content-Type", contentType)
	}
	http.ServeFile(w, r, absolutePath)
}

// proxyDashboard forwards to the dev server, including the HMR WebSocket upgrade.
func (s *Server) proxyDashboard(w http.ResponseWriter, r *http.Request, targetURL string) {
	target, err := url.Parse(targetURL)
	if err != nil {
		writeText(w, http.StatusBadGateway, "Dashboard dev server URL is invalid")
		return
	}
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		s.proxyWebSocket(w, r, target)
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		s.logger.Error("dashboard dev server proxy error: %v", err)
		writeText(w, http.StatusServiceUnavailable, "Dashboard dev server is unreachable")
	}
	proxy.ServeHTTP(w, r)
}

func (s *Server) proxyWebSocket(w http.ResponseWriter, r *http.Request, target *url.URL) {
	// Vite 5 rejects any Origin on a loopback listener (DNS-rebinding protection),
	// but browsers always send one: drop it here. The dev server is only reachable
	// through this proxy, so the protection still holds for direct connections.
	r.Header.Del("Origin")
	r.Header.Del("Sec-WebSocket-Origin")
	r.Host = target.Host

	backend := target.Host
	if !strings.Contains(backend, ":") {
		backend += ":80"
	}
	upstream, err := net.DialTimeout("tcp", backend, 5*time.Second)
	if err != nil {
		s.logger.Error("dashboard websocket proxy error: %v", err)
		writeText(w, http.StatusBadGateway, "Dashboard dev server is unreachable")
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		_ = upstream.Close()
		writeText(w, http.StatusInternalServerError, "WebSocket proxying is not supported")
		return
	}
	client, _, err := hijacker.Hijack()
	if err != nil {
		_ = upstream.Close()
		return
	}
	if err := r.Write(upstream); err != nil {
		_ = client.Close()
		_ = upstream.Close()
		return
	}
	go func() {
		_, _ = io.Copy(upstream, client)
		_ = upstream.Close()
	}()
	_, _ = io.Copy(client, upstream)
	_ = client.Close()
}
