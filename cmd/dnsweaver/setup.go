package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"gopkg.in/natefinch/lumberjack.v2"

	"gitlab.bluewillows.net/root/dnsweaver/internal/config"
	"gitlab.bluewillows.net/root/dnsweaver/internal/docker"
	k8s "gitlab.bluewillows.net/root/dnsweaver/internal/kubernetes"
	"gitlab.bluewillows.net/root/dnsweaver/pkg/provider"
	"gitlab.bluewillows.net/root/dnsweaver/pkg/source"
	"gitlab.bluewillows.net/root/dnsweaver/providers/adguard"
	"gitlab.bluewillows.net/root/dnsweaver/providers/cloudflare"
	"gitlab.bluewillows.net/root/dnsweaver/providers/dnsmasq"
	"gitlab.bluewillows.net/root/dnsweaver/providers/pihole"
	"gitlab.bluewillows.net/root/dnsweaver/providers/rfc2136"
	"gitlab.bluewillows.net/root/dnsweaver/providers/technitium"
	"gitlab.bluewillows.net/root/dnsweaver/providers/webhook"
	dnsweaversource "gitlab.bluewillows.net/root/dnsweaver/sources/dnsweaver"
	k8ssource "gitlab.bluewillows.net/root/dnsweaver/sources/kubernetes"
	"gitlab.bluewillows.net/root/dnsweaver/sources/traefik"
)

// parseBoolEnv reads an environment variable and returns true if it's a truthy value.
func parseBoolEnv(key string) bool {
	v := os.Getenv(key)
	switch strings.ToLower(v) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// boolPtr returns a pointer to a bool value.
func boolPtr(b bool) *bool {
	return &b
}

// setupLogger configures structured logging based on application config.
func setupLogger(cfg *config.Config) *slog.Logger {
	logLevel := parseLogLevel(cfg.LogLevel())

	var output io.Writer = os.Stdout

	if logFile := cfg.LogFile(); logFile != "" {
		output = &lumberjack.Logger{
			Filename:   logFile,
			MaxSize:    cfg.LogMaxSize(),
			MaxBackups: cfg.LogMaxBackups(),
			MaxAge:     cfg.LogMaxAge(),
			Compress:   cfg.LogCompress(),
		}
	}

	opts := &slog.HandlerOptions{Level: logLevel}

	var handler slog.Handler
	if cfg.LogFormat() == "text" {
		handler = slog.NewTextHandler(output, opts)
	} else {
		handler = slog.NewJSONHandler(output, opts)
	}

	return slog.New(handler)
}

// parseLogLevel converts a string log level to slog.Level.
func parseLogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// parseDockerMode converts a string Docker mode to the typed constant.
func parseDockerMode(mode string) docker.Mode {
	switch mode {
	case "swarm":
		return docker.ModeSwarm
	case "standalone":
		return docker.ModeStandalone
	case "both":
		return docker.ModeBoth
	default:
		return docker.ModeAuto
	}
}

// registerSources registers hostname extraction sources based on configuration.
func registerSources(registry *source.Registry, cfg *config.Config, logger *slog.Logger) error {
	for _, name := range cfg.SourceNames() {
		switch name {
		case "traefik":
			src := createTraefikSource(cfg, logger)
			if err := registry.Register(src); err != nil {
				return fmt.Errorf("registering traefik source: %w", err)
			}
			logger.Info("registered source",
				slog.String("name", name),
				slog.Bool("file_discovery", src.SupportsDiscovery()),
			)
		case "dnsweaver":
			src := dnsweaversource.New(dnsweaversource.WithLogger(logger))
			if err := registry.Register(src); err != nil {
				return fmt.Errorf("registering dnsweaver source: %w", err)
			}
			logger.Info("registered source",
				slog.String("name", name),
				slog.Bool("file_discovery", src.SupportsDiscovery()),
			)
		case "kubernetes":
			src := k8ssource.New(k8ssource.WithLogger(logger))
			if err := registry.Register(src); err != nil {
				return fmt.Errorf("registering kubernetes source: %w", err)
			}
			logger.Info("registered source",
				slog.String("name", name),
			)
		default:
			logger.Warn("unknown source, skipping", slog.String("source", name))
		}
	}

	// Auto-register kubernetes source when K8s platform is enabled.
	// This source is always needed for K8s workloads (reads pre-extracted
	// hostnames from resource converters). It doesn't need to be explicitly
	// listed in DNSWEAVER_SOURCES — it's platform-implied.
	if cfg.UseKubernetes() {
		if registry.Get("kubernetes") == nil {
			src := k8ssource.New(k8ssource.WithLogger(logger))
			if err := registry.Register(src); err != nil {
				return fmt.Errorf("registering kubernetes source: %w", err)
			}
			logger.Info("auto-registered kubernetes source for K8s platform")
		}
	}

	return nil
}

// createTraefikSource creates a Traefik label parser with optional file discovery.
func createTraefikSource(cfg *config.Config, logger *slog.Logger) *traefik.Traefik {
	opts := []traefik.Option{
		traefik.WithLogger(logger),
	}

	// Configure file discovery if paths are set
	srcCfg := cfg.GetSourceInstance("traefik")
	if srcCfg != nil && srcCfg.FileDiscovery.IsEnabled() {
		opts = append(opts, traefik.WithFileDiscovery(srcCfg.FileDiscovery))
		logger.Debug("traefik file discovery configured",
			slog.Any("paths", srcCfg.FileDiscovery.FilePaths),
			slog.String("pattern", srcCfg.FileDiscovery.FilePattern),
		)
	}

	return traefik.New(opts...)
}

// registerProviderFactories registers all available DNS provider factories.
func registerProviderFactories(registry *provider.Registry) {
	// Register Technitium provider factory (private DNS)
	registry.RegisterFactory("technitium", technitium.Factory())

	// Register Cloudflare provider factory (public DNS)
	registry.RegisterFactory("cloudflare", cloudflare.Factory())

	// Register Webhook provider factory (custom integrations)
	registry.RegisterFactory("webhook", webhook.Factory())

	// Register dnsmasq provider factory (local DNS, Pi-hole backend)
	registry.RegisterFactory("dnsmasq", dnsmasq.Factory())

	// Register Pi-hole provider factory (local DNS via Pi-hole API or file mode)
	registry.RegisterFactory("pihole", pihole.Factory())

	// Register RFC 2136 provider factory (BIND, Windows DNS, PowerDNS, etc.)
	registry.RegisterFactory("rfc2136", rfc2136.Factory())

	// Register AdGuard Home provider factory (local DNS via AdGuard Home API)
	registry.RegisterFactory("adguard", adguard.Factory())
}

// initializeProviders initializes all configured providers using the manager.
// Unlike createProviderInstances, this method does not fail fatally if a provider
// is temporarily unavailable - it queues it for retry instead.
func initializeProviders(manager *provider.Manager, cfg *config.Config) error {
	for _, inst := range cfg.ProviderInstances {
		providerCfg := inst.ToProviderConfig()
		if err := manager.InitializeProvider(providerCfg); err != nil {
			// Only returns error for invalid configuration (not connection failures)
			return fmt.Errorf("invalid provider config %s: %w", inst.Name, err)
		}
	}
	return nil
}

// buildK8sConfig converts application config into Kubernetes watcher config.
func buildK8sConfig(cfg *config.Config) k8s.Config {
	k8sCfg := k8s.DefaultConfig()
	k8sCfg.Kubeconfig = cfg.K8sKubeconfig()
	k8sCfg.WatchIngress = cfg.K8sWatchIngress()
	k8sCfg.WatchIngressRoute = cfg.K8sWatchIngressRoute()
	k8sCfg.WatchHTTPRoute = cfg.K8sWatchHTTPRoute()
	k8sCfg.WatchServices = cfg.K8sWatchServices()
	k8sCfg.LabelSelector = cfg.K8sLabelSelector()
	k8sCfg.AnnotationFilter = cfg.K8sAnnotationFilter()

	if ns := cfg.K8sNamespaces(); ns != "" {
		k8sCfg.Namespaces = strings.Split(ns, ",")
		for i := range k8sCfg.Namespaces {
			k8sCfg.Namespaces[i] = strings.TrimSpace(k8sCfg.Namespaces[i])
		}
	}

	return k8sCfg
}
