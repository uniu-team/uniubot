// Package bootstrap assembles the application and owns the shutdown sequence.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/uniu-network/uniubot/internal/adapters/all" // registers the built-in adapters

	"github.com/uniu-network/uniubot/internal/api"
	"github.com/uniu-network/uniubot/internal/app"
	"github.com/uniu-network/uniubot/internal/core/config"
	"github.com/uniu-network/uniubot/internal/core/logging"
	"github.com/uniu-network/uniubot/internal/devserver"
	"github.com/uniu-network/uniubot/internal/storage"
)

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// Version can be overridden with -ldflags -X .../bootstrap.Version=...
var Version = "1.0.0"

type Options struct {
	ConfigPath string
	PluginDir  string
	StaticDir  string
	ListenHost string
	ListenPort int
	// DevServerURL reuses a running Vite; when empty and dashboard.debug is set, the
	// host starts one.
	DevServerURL  string
	FrontendDir   string
	DevServerPort int
}

type Runtime struct {
	Config    *config.Store
	Storage   *storage.Manager
	Bots      *app.Manager
	Server    *api.Server
	DevServer *devserver.Server
	Logger    *logging.Logger
	Version   string
}

func Run(ctx context.Context, opts Options) error {
	runtime, err := New(ctx, opts)
	if err != nil {
		return err
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)

	serverErr := make(chan error, 1)
	host := opts.ListenHost
	if host == "" {
		host = runtime.Config.String("web.host", "0.0.0.0")
	}
	port := opts.ListenPort
	if port == 0 {
		port = runtime.Config.Int("web.port", 3000)
	}
	go func() {
		serverErr <- runtime.Server.ListenAndServe(host, port)
	}()

	select {
	case <-ctx.Done():
		runtime.Logger.Info("context cancelled, shutting down")
	case sig := <-signals:
		runtime.Logger.Info("%s received. Shutting down...", sig)
	case err := <-serverErr:
		if err != nil {
			runtime.Logger.Error("HTTP server failed: %v", err)
			_ = runtime.Shutdown(context.Background())
			return err
		}
	}

	return runtime.Shutdown(context.Background())
}

func New(ctx context.Context, opts Options) (*Runtime, error) {
	configPath := opts.ConfigPath
	if configPath == "" {
		configPath = config.GlobalConfigPath
	}
	global, err := config.Open(configPath)
	if err != nil {
		return nil, fmt.Errorf("cannot load %s: %w", configPath, err)
	}

	logger := logging.Default()
	logger.ApplyConfigLevel(global.String("log.level", "INFO"))

	runtime := &Runtime{Config: global, Logger: logger, Version: Version}
	logger.Info("booting uniubot v%s", Version)

	storageManager := storage.NewManager(logger)
	if err := storageManager.Init(storage.OptionsFromConfig(global)); err != nil {
		return nil, fmt.Errorf("database initialization failed: %w", err)
	}
	runtime.Storage = storageManager

	botManager := app.NewManager(global, storageManager, Version, logger)
	if opts.PluginDir != "" {
		botManager.SetPluginDir(opts.PluginDir)
	}
	runtime.Bots = botManager

	// Debug mode: either reuse --dev-server/UNIUBOT_DEV_SERVER or start Vite here.
	var devProxy func() (string, bool)
	if staticURL := firstNonEmpty(opts.DevServerURL, os.Getenv("UNIUBOT_DEV_SERVER")); staticURL != "" {
		target := staticURL
		devProxy = func() (string, bool) { return target, true }
		logger.Warn("dashboard debug mode enabled: proxying the dashboard to %s", target)
	} else if global.Bool("dashboard.debug", false) {
		logger.Warn("dashboard debug mode enabled: starting the Vite dev server")
		devPort := opts.DevServerPort
		if devPort == 0 {
			devPort = global.Int("dashboard.dev_port", 3000)
		}
		dev, err := devserver.Start(ctx, devserver.Options{
			Dir:    opts.FrontendDir,
			Port:   devPort,
			Logger: logger,
		})
		if err != nil {
			logger.Error("failed to start the dashboard dev server: %v", err)
			logger.Warn("dashboard debug mode is falling back to the built static files; run `make frontend` or set --dev-server")
		} else {
			runtime.DevServer = dev
			devProxy = dev.URL
		}
	}

	server, err := api.NewServer(api.Options{
		Bots:      botManager,
		Global:    global,
		Storage:   storageManager,
		Logger:    logger,
		Version:   Version,
		StaticDir: opts.StaticDir,
		DevProxy:  devProxy,
	})
	if err != nil {
		_ = storageManager.Close()
		return nil, err
	}
	runtime.Server = server

	if err := botManager.LoadAll(ctx); err != nil {
		logger.Error("failed to load bots: %v", err)
	}
	return runtime, nil
}

// Shutdown stops the dashboard dev server, bots, the HTTP server and the database.
func (r *Runtime) Shutdown(ctx context.Context) error {
	shutdownCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var firstErr error
	record := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if r.DevServer != nil {
		if err := r.DevServer.Stop(shutdownCtx); err != nil {
			r.Logger.Warn("failed to stop the dashboard dev server: %v", err)
		}
	}
	if err := r.Server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.Canceled) {
		r.Logger.Error("failed to stop HTTP server: %v", err)
		record(err)
	}
	r.Bots.StopAll(shutdownCtx)
	if err := r.Storage.Close(); err != nil {
		r.Logger.Error("failed to close database: %v", err)
		record(err)
	}
	r.Logger.Info("shutdown complete")
	return firstErr
}
