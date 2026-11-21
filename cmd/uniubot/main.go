// Command uniubot runs the uniubot bot host.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/uniu-network/uniubot/internal/bootstrap"
)

func main() {
	var (
		configPath = flag.String("config", "", "global configuration file (default config/config.yml)")
		pluginDir  = flag.String("plugins", "", "plugin directory (default plugins)")
		staticDir  = flag.String("static", "", "dashboard build directory (default frontend/dist)")
		host       = flag.String("host", "", "listen address override")
		port       = flag.Int("port", 0, "listen port override")
		devServer  = flag.String("dev-server", "", "proxy the dashboard to this Vite dev server URL (skips starting one)")
		frontend   = flag.String("frontend", "", "frontend directory used when starting the dev server (default frontend)")
		devPort    = flag.Int("dev-port", 0, "port for the started dev server (default dashboard.dev_port or 3000)")
		version    = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()

	if *version {
		fmt.Println(bootstrap.Version)
		return
	}

	err := bootstrap.Run(context.Background(), bootstrap.Options{
		ConfigPath:    *configPath,
		PluginDir:     *pluginDir,
		StaticDir:     *staticDir,
		ListenHost:    *host,
		ListenPort:    *port,
		DevServerURL:  *devServer,
		FrontendDir:   *frontend,
		DevServerPort: *devPort,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "uniubot: %v\n", err)
		os.Exit(1)
	}
}
