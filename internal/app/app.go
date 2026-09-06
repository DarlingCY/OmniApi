package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/omniapi/omni-api/internal/browser"
	"github.com/omniapi/omni-api/internal/secret"
	"github.com/omniapi/omni-api/internal/server"
	"github.com/omniapi/omni-api/internal/store"
	"github.com/omniapi/omni-api/internal/tray"
	"github.com/omniapi/omni-api/internal/webui"
)

const (
	// ModeDesktop runs the tray application bound to the loopback interface.
	ModeDesktop = "desktop"
	// ModeServer runs headless, suitable for containers.
	ModeServer = "server"
)

// Options are the resolved runtime settings.
type Options struct {
	Mode    string
	Host    string
	Port    int
	DataDir string
}

// ParseOptions reads flags with environment variable fallbacks.
func ParseOptions(arguments []string) (Options, error) {
	options := Options{}
	flags := flag.NewFlagSet("omni-api", flag.ContinueOnError)
	flags.StringVar(&options.Mode, "mode", env("OMNI_MODE", ModeDesktop), "runtime mode: desktop or server")
	flags.StringVar(&options.Host, "host", env("OMNI_HOST", ""), "listen address (defaults to 127.0.0.1 in desktop mode and 0.0.0.0 in server mode)")
	flags.IntVar(&options.Port, "port", envInt("OMNI_PORT", 47831), "listen port")
	flags.StringVar(&options.DataDir, "data-dir", env("OMNI_DATA_DIR", ""), "directory holding omni-api.db and secret.key")
	if err := flags.Parse(arguments); err != nil {
		return options, err
	}
	switch options.Mode {
	case ModeDesktop, ModeServer:
	default:
		return options, fmt.Errorf("unknown mode %q, expected desktop or server", options.Mode)
	}
	if options.Host == "" {
		if options.Mode == ModeServer {
			options.Host = "0.0.0.0"
		} else {
			options.Host = "127.0.0.1"
		}
	}
	if options.Port <= 0 || options.Port > 65535 {
		return options, fmt.Errorf("port %d is out of range", options.Port)
	}
	if options.DataDir == "" {
		resolved, err := defaultDataDir()
		if err != nil {
			return options, err
		}
		options.DataDir = resolved
	}
	return options, nil
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil {
		return fallback
	}
	return value
}

func defaultDataDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return "", homeErr
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "omni-api"), nil
}

// Run starts the gateway and blocks until the context is cancelled or, in
// desktop mode, until the tray is dismissed.
func Run(ctx context.Context, options Options) error {
	if err := os.MkdirAll(options.DataDir, 0o700); err != nil {
		return err
	}
	codec, err := secret.NewAESCodec(options.DataDir, os.Getenv("OMNI_MASTER_KEY"))
	if err != nil {
		return err
	}
	configStore := store.New(filepath.Join(options.DataDir, "omni-api.db"), codec)
	if err := configStore.Load(); err != nil {
		return err
	}
	if err := initializeAdminToken(configStore, os.Getenv("OMNI_ADMIN_TOKEN")); err != nil {
		return err
	}

	var assets fs.FS
	if directory := env("OMNI_STATIC_DIR", ""); directory != "" {
		assets = os.DirFS(directory)
	} else {
		assets = webui.Assets()
	}

	handler := server.New(server.Options{Store: configStore, Assets: assets})
	address := net.JoinHostPort(options.Host, strconv.Itoa(options.Port))
	listener, err := net.Listen("tcp", address)
	if err != nil {
		// A busy port in desktop mode means an instance is already running, so
		// behave like a second launch of a single-instance app.
		if options.Mode == ModeDesktop {
			log.Printf("omni-api is already running on %s, opening its configuration page", address)
			return browser.Open("http://" + address)
		}
		return err
	}
	httpServer := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	failures := make(chan error, 1)
	go func() {
		if serveErr := httpServer.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			failures <- serveErr
			return
		}
		failures <- nil
	}()

	consoleURL := consoleURL(options, listener.Addr())
	log.Printf("omni-api listening on http://%s (mode=%s, data-dir=%s)", listener.Addr(), options.Mode, options.DataDir)
	handler.LogRuntime("info", "", "服务开始监听", fmt.Sprintf("address=%s mode=%s", listener.Addr(), options.Mode))
	if options.Mode == ModeServer && options.Host != "127.0.0.1" && options.Host != "localhost" {
		config := configStore.Get()
		if len(config.AccessKeys) == 0 || config.Settings.AdminToken == "" {
			log.Printf("warning: the gateway is reachable from other hosts but the admin or proxy token is unset; anyone who can reach %s can use or reconfigure it", listener.Addr())
		}
	}

	if options.Mode == ModeDesktop {
		go func() {
			tray.Run(runCtx, tray.Options{
				ConsoleURL: func() string {
					token := configStore.Get().Settings.AdminToken
					if token == "" {
						return consoleURL
					}
					return consoleURL + "#token=" + url.QueryEscape(token)
				},
			})
			cancel()
		}()
	}

	select {
	case err = <-failures:
	case <-runCtx.Done():
	}

	shutdownCtx, stopShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopShutdown()
	if shutdownErr := httpServer.Shutdown(shutdownCtx); shutdownErr != nil && err == nil {
		err = shutdownErr
	}
	return err
}

func initializeAdminToken(configStore *store.Store, value string) error {
	adminToken := strings.TrimSpace(value)
	if adminToken == "" || configStore.Get().Settings.AdminToken != "" {
		return nil
	}
	if err := configStore.UpdateSettings(&adminToken, nil); err != nil {
		return fmt.Errorf("initialize admin token: %w", err)
	}
	return nil
}

func consoleURL(options Options, address net.Addr) string {
	host := options.Host
	if host == "0.0.0.0" || host == "::" || host == "" {
		host = "127.0.0.1"
	}
	port := options.Port
	if tcp, ok := address.(*net.TCPAddr); ok {
		port = tcp.Port
	}
	if override := env("OMNI_WEB_URL", ""); override != "" {
		return override
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(port))
}
