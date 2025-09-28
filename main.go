package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"image/color"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/cooldogedev/spectrum"
	"github.com/cooldogedev/spectrum/api"
	"github.com/cooldogedev/spectrum/server"
	"github.com/cooldogedev/spectrum/session"
	"github.com/cooldogedev/spectrum/session/animation"
	"github.com/cooldogedev/spectrum/transport"
	"github.com/cooldogedev/spectrum/util"
	"github.com/elk-language/go-prompt"
	"github.com/lmittmann/tint"
	"github.com/oomph-ac/oconfig"
	"github.com/oomph-ac/oomph"
	"github.com/oomph-ac/oomph/player"
	"github.com/pelletier/go-toml"
	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
	"github.com/sandertv/gophertunnel/minecraft/resource"
)

// map of server addresses to their names.
var serverMap = make(map[string]string)
var addressToName = make(map[string]string)

var lobbyServerAddress string
var resourcePackServer *ResourcePackServer

// LobbyDiscovery implements server.Discovery to discover the lobby server address.
type LobbyDiscovery struct {
}

type ServerConfig struct {
	// Name is the name of this server, used for MOTD.
	Name string `toml:"name"`
	// BindAddr is the address to bind the proxy server to.
	BindAddr string `toml:"bind_addr"`
	// DefaultServer is the name of the default server to connect to.
	DefaultServer string `toml:"default_server"`
	// Servers is a list of servers to connect to.
	Servers []Server `toml:"servers"`
	// ShutdownMessage is the message sent to players when the proxy is shutting down.
	ShutdownMessage string `toml:"shutdown_message"`
	// Debug enables debug mode, which logs more information.
	Debug bool `toml:"debug"`
	// CdnConfig contains CDN configuration.
	CdnConfig CdnConfig `toml:"cdn_config"`
	// OomphEnabled indicates whether to enable Oomph Anticheat proxy.
	OomphEnabled bool `toml:"oomph_enabled"`

	APIServer APIServer `toml:"api_server"`
}

type Server struct {
	Name string `toml:"name"`
	Addr string `toml:"addr"`
}

type CdnConfig struct {
	Enabled bool   `toml:"enabled"`
	Ip      string `toml:"ip"`
	Port    int    `toml:"port"`
}

type APIServer struct {
	BindAddr string `toml:"bind_addr"`
	Token    string `toml:"token"`
}

// Discover returns the lobby server address for the player to connect to.
func (l LobbyDiscovery) Discover(conn *minecraft.Conn) (string, error) {
	return lobbyServerAddress, nil
}

// DiscoverFallback returns the lobby server address as a fallback for the player.
func (l LobbyDiscovery) DiscoverFallback(conn *minecraft.Conn) (string, error) {
	return lobbyServerAddress, nil
}

// TransferProcessor implements session.Processor to handle server transfers while player is in the game.
type TransferProcessor struct {
	session.NopProcessor
	// s is the current session being processed.
	s *session.Session
	// log is the logger for this processor.
	log *slog.Logger
}

// ProcessServer is called when a packet is received from the server.
// Canceling it will prevent the packet from being sent to the client.
func (p *TransferProcessor) ProcessServer(ctx *session.Context, pk *packet.Packet) {
	if t, ok := (*pk).(*packet.Transfer); ok {
		addr := t.Address
		if a, ok := serverMap[addr]; ok {
			ctx.Cancel()
			err := p.s.TransferTimeout(a, 10*time.Second)
			if err != nil {
				p.log.Error("failed to transfer", "err", err, "address", addr)
				p.s.CloseWithError(err)
			}
		}
		return
	}
}

func main() {
	conf, err := readConfig()
	if err != nil {
		panic(fmt.Errorf("read config: %w", err))
	}

	var logLevel slog.Level
	if conf.Debug {
		logLevel = slog.LevelDebug
	} else {
		logLevel = slog.LevelInfo
	}

	slog.SetLogLoggerLevel(logLevel)

	w := os.Stderr
	logger := slog.New(
		tint.NewHandler(w, &tint.Options{
			Level:      logLevel,
			TimeFormat: time.TimeOnly,
		}),
	)
	slog.SetDefault(logger)

	for _, srv := range conf.Servers {
		serverMap[srv.Name] = srv.Addr
		addressToName[srv.Addr] = srv.Name
		if srv.Name == conf.DefaultServer {
			lobbyServerAddress = srv.Addr
		}
		logger.Info("Loaded server", "name", srv.Name, "address", srv.Addr)
	}

	if lobbyServerAddress == "" {
		logger.Error("No default server found")
		return
	}

	packs, err := parse(make(map[string]string), logger) // TODO: support content keys
	if err != nil {
		logger.Error("failed to parse resource packs", "err", err)
		return
	}

	logger.Info("Loaded resource packs", "count", len(packs))

	// Start the HTTP resource pack server if CDN is enabled
	if conf.CdnConfig.Enabled && len(packs) > 0 {
		baseURL := fmt.Sprintf("http://%s:%d", conf.CdnConfig.Ip, conf.CdnConfig.Port)

		// Create and start the resource pack HTTP server
		resourcePackServer, err = NewResourcePackServer(packs, conf.CdnConfig.Port, logger)
		if err != nil {
			logger.Error("Failed to create resource pack HTTP server", "error", err)
			return
		}

		// Start the HTTP server in a goroutine
		go func() {
			if err := resourcePackServer.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("Resource pack HTTP server error", "error", err)
			}
		}()

		// Wait for the server to be ready before modifying resource packs
		resourcePackServer.WaitForReady()
		logger.Info("Resource pack HTTP server is ready", "baseURL", baseURL)

		// Modify resource packs to use HTTP URLs
		packs = ModifyResourcePackForCDN(packs, baseURL)
		for _, pack := range packs {
			logger.Debug("Loaded resource pack", "name", pack.Name(), "uuid", pack.UUID(), "url", pack.DownloadURL())
		}
		logger.Info("Modified resource packs to use HTTP URLs")
	}

	var flushRate time.Duration
	if conf.OomphEnabled {
		flushRate = -1
	} else {
		flushRate = 20 / time.Second
	}

	var autoLogin = true
	if conf.OomphEnabled {
		autoLogin = false
	}

	oconfig.Global = oconfig.DefaultConfig
	//oconfig.Global.Network.Transport = oconfig.NetworkTransportSpectral

	oconfig.Global.Movement.AcceptClientPosition = false
	oconfig.Global.Movement.PositionAcceptanceThreshold = 0.003
	oconfig.Global.Movement.AcceptClientVelocity = false
	oconfig.Global.Movement.VelocityAcceptanceThreshold = 0.077

	oconfig.Global.Movement.PersuasionThreshold = 0.002
	oconfig.Global.Movement.CorrectionThreshold = 0.003

	oconfig.Global.Combat.MaximumAttackAngle = 90
	oconfig.Global.Combat.EnableClientEntityTracking = true

	oconfig.Global.Network.GlobalMovementCutoffThreshold = -1
	oconfig.Global.Network.MaxEntityRewind = 6
	oconfig.Global.Network.MaxGhostBlockChain = 7
	oconfig.Global.Network.MaxKnockbackDelay = -1
	oconfig.Global.Network.MaxBlockUpdateDelay = -1

	proxy := spectrum.NewSpectrum(server.NewStaticDiscovery(lobbyServerAddress, lobbyServerAddress), logger, &util.Opts{
		ShutdownMessage: conf.ShutdownMessage,
		Addr:            conf.BindAddr,
		AutoLogin:       autoLogin,
		LatencyInterval: 1000,
		ClientDecode:    player.ClientDecode,
		SyncProtocol:    false,
	}, transport.NewSpectral(logger))
	if err := proxy.Listen(minecraft.ListenConfig{
		StatusProvider:       util.NewStatusProvider(conf.Name, conf.Name),
		TexturePacksRequired: len(packs) > 0,
		ResourcePacks:        packs,
		FlushRate:            flushRate,
	}); err != nil {
		return
	}

	info, _ := debug.ReadBuildInfo()
	if info == nil {
		info = &debug.BuildInfo{GoVersion: "N/A", Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "N/A"}}}
	}
	revision := ""
	for _, set := range info.Settings {
		if set.Key == "vcs.revision" {
			revision = set.Value
		}
	}

	logger.Info("Starting spectrum proxy", "oomph-enabled", conf.OomphEnabled, "addr", proxy.Opts().Addr, "mc-version", protocol.CurrentVersion, "go-version", info.GoVersion, "commit", revision)

	go processCommand(proxy, conf)

	go func() {
		a := api.NewAPI(proxy.Registry(), logger, api.NewSecretBasedAuthentication(conf.APIServer.Token))
		if err := a.Listen(conf.APIServer.BindAddr); err != nil {
			logger.Error("Error starting API server", "err", err)
			return
		}
		logger.Info("Started API server", "bind-addr", conf.APIServer.BindAddr, "token", conf.APIServer.Token)
		for {
			_ = a.Accept()
		}
	}()

	for {
		s, err := proxy.Accept()
		if err != nil {
			continue
		}
		s.SetAnimation(&animation.Fade{
			Colour: color.RGBA{},
			Timing: protocol.CameraFadeTimeData{
				FadeInDuration:  0.32,
				WaitDuration:    0.84,
				FadeOutDuration: 0.23,
			},
		})
		if conf.OomphEnabled {
			go func(s *session.Session) {
				// Disable auto-login so that Oomph's processor can modify the StartGame data to allow server-authoritative movement.
				f, err := os.OpenFile(fmt.Sprintf("./logs/%s.log", s.Client().IdentityData().DisplayName), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0744)
				if err != nil {
					s.Disconnect("failed to create log file")
					return
				}
				playerLogHandler := slog.NewTextHandler(f, &slog.HandlerOptions{
					Level: slog.LevelDebug,
				})
				playerLog := slog.New(playerLogHandler)
				proc := oomph.NewProcessor(s, proxy.Registry(), proxy.Listener(), playerLog)
				proc.Player().SetCloser(func() {
					f.Close()
				})
				proc.Player().SetRecoverFunc(func(p *player.Player, err any) {
					logger.Error("Error during processing player packet", "player", p.Name(), "err", err)
					debug.PrintStack()
				})
				proc.Player().AddPerm(player.PermissionDebug)
				proc.Player().AddPerm(player.PermissionAlerts)
				proc.Player().AddPerm(player.PermissionLogs)
				proc.Player().HandleEvents(player.NewExampleEventHandler())
				s.SetProcessor(proc)

				if err := s.LoginTimeout(10 * time.Second); err != nil {
					s.Disconnect(err.Error())
					f.Close()
					if !errors.Is(err, context.Canceled) {
						logger.Error("failed to login session", "err", err)
					}
					return
				}

				proc.Player().SetServerConn(s.Server())
			}(s)
		}
	}
}

// isInContainer returns if the application is running in the container.
func isInContainer() bool {
	file, err := os.Open("/proc/1/cgroup")
	if err != nil {
		return false
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "docker") || strings.Contains(line, "kubepods") {
			return true
		}
	}
	return false
}

// processCommand initializes the command prompt and handles user input commands.
func processCommand(proxy *spectrum.Spectrum, conf *ServerConfig) {
	logger := slog.Default()
	if runtime.GOOS == "linux" {
		if isInContainer() {
			logger.Info("Not using console due to in container environment")
			handleTermination(proxy)
			return
		}

		_, err := syscall.Open("/dev/tty", syscall.O_RDONLY, 0)
		if err != nil {
			logger.Info("Not using console due to /dev/tty not exists")
			handleTermination(proxy)
			return
		}
	}
	c := NewCompleter(proxy)

	historyFile, err := NewHistoryFile("command_history.txt", 100)
	if err != nil {
		logger.Error("Failed to initialize command history", "error", err)
	}

	executor := func(in string) {
		if in = strings.TrimSpace(in); in != "" {
			if historyFile != nil {
				if err := historyFile.Append(in); err != nil {
					logger.Error("Failed to save command to history", "error", err)
				}
			}
			handleCommand(in, proxy, conf)
		}
	}

	options := []prompt.Option{
		prompt.WithTitle("Spectrum Proxy Console"),
		prompt.WithPrefixTextColor(prompt.Yellow),
		prompt.WithSuggestionBGColor(prompt.DarkGray),
		prompt.WithDescriptionBGColor(prompt.Black),
		prompt.WithDescriptionTextColor(prompt.White),
		prompt.WithSelectedSuggestionBGColor(prompt.Blue),
		prompt.WithSelectedSuggestionTextColor(prompt.White),
		prompt.WithSelectedDescriptionBGColor(prompt.Blue),
		prompt.WithCompleter(c.Complete),
		prompt.WithKeyBind(prompt.KeyBind{
			Key: prompt.ControlC,
			Fn: func(_ *prompt.Prompt) bool {
				// Handle Ctrl+C to exit gracefully
				logger.Info("Exiting Spectrum Proxy Console...")
				if historyFile != nil {
					if err := historyFile.Save(); err != nil {
						logger.Error("Failed to save command history on exit", "error", err)
					}
				}
				proxy.Close()
				os.Exit(0)
				return false
			},
		}),
	}

	if historyFile != nil {
		options = append(options, prompt.WithHistory(historyFile.GetHistory()))
	}

	p := prompt.New(
		executor,
		options...,
	)

	p.Run()
}

func handleTermination(proxy *spectrum.Spectrum) {
	go func() {
		var interrupt = make(chan os.Signal, 1)
		signal.Notify(interrupt, os.Interrupt)
		<-interrupt
		for _, s := range proxy.Registry().GetSessions() {
			s.Server().WritePacket(&packet.Disconnect{})
			s.Disconnect("Proxy restarting...")
		}
		time.Sleep(time.Second)
		os.Exit(0)
	}()
}

// handleCommand processes the input command
func handleCommand(command string, proxy *spectrum.Spectrum, conf *ServerConfig) {
	args := strings.Fields(command)
	if len(args) == 0 {
		return
	}

	logger := slog.Default()

	switch args[0] {
	case "players":
		sessions := proxy.Registry().GetSessions()
		if len(sessions) == 0 {
			logger.Info("No players online")
			return
		}

		logger.Info(fmt.Sprintf("Players online (%d)", len(sessions)))
		for _, s := range sessions {
			playerName := s.Client().IdentityData().DisplayName
			logger.Info(fmt.Sprintf("- %s", playerName))
		}

	case "transfer":
		if len(args) < 3 {
			logger.Info("Usage: transfer <player> <server>")
			return
		}

		playerName := args[1]
		serverName := args[2]

		var targetSession *session.Session
		for _, s := range proxy.Registry().GetSessions() {
			if s.Client().IdentityData().DisplayName == playerName {
				targetSession = s
				break
			}
		}

		if targetSession == nil {
			logger.Info(fmt.Sprintf("Player '%s' not found", playerName))
			return
		}

		var serverAddr string
		for name, addr := range serverMap {
			if name == serverName {
				serverAddr = addr
				break
			}
		}

		if serverAddr == "" {
			logger.Info(fmt.Sprintf("Server '%s' not found", serverName))
			return
		}

		err := targetSession.TransferTimeout(serverAddr, 10*time.Second)
		if err != nil {
			logger.Error("Failed to transfer player", "player", playerName, "server", serverName, "error", err)
			return
		}

		logger.Info(fmt.Sprintf("Transferred %s to %s", playerName, serverName))

	case "info":
		logger.Info("Spectrum Proxy Information")
		logger.Info(fmt.Sprintf("- Bind Address: %s", proxy.Opts().Addr))
		logger.Info(fmt.Sprintf("- Default Server: %s", conf.DefaultServer))
		logger.Info(fmt.Sprintf("- Connected Players: %d", len(proxy.Registry().GetSessions())))
		logger.Info("Available Servers:")

		for addr, name := range serverMap {
			logger.Info(fmt.Sprintf("- %s (%s)", name, addr))
		}
		logger.Info(fmt.Sprintf("Goroutines: %d", runtime.NumGoroutine()))
		logger.Info(fmt.Sprintf("Go Version: %s", runtime.Version()))
		var memStats runtime.MemStats
		runtime.ReadMemStats(&memStats)
		logger.Info(fmt.Sprintf("Total Allocated Memory: %.2f MB", float64(memStats.TotalAlloc)/1024/1024))

	case "stop", "end":
		if resourcePackServer != nil {
			if err := resourcePackServer.Close(); err != nil {
				logger.Error("Failed to close resource pack HTTP server", "error", err)
			}
		}
		proxy.Close()
		logger.Info("Stopped proxy")
		os.Exit(0)

	default:
		logger.Info(fmt.Sprintf("Unknown command: %s", args[0]))
		logger.Info("Available commands: players, transfer, info")
	}
}

// readConfig reads the configuration from config.toml or creates a default one if it doesn't exist.
func readConfig() (*ServerConfig, error) {
	conf := &ServerConfig{
		Name:          "Spectrum Proxy",
		BindAddr:      "0.0.0.0:19132",
		DefaultServer: "lobby",
		Servers: []Server{
			{
				Name: "lobby",
				Addr: "127.0.0.1:19133",
			},
			{
				Name: "island1",
				Addr: "127.0.0.1:19134",
			},
		},
		ShutdownMessage: "Proxy shutdown",
		CdnConfig: CdnConfig{
			Enabled: false,
			Ip:      "0.0.0.0",
			Port:    8080,
		},
		OomphEnabled: false,
		APIServer: APIServer{
			BindAddr: "127.0.0.1:19132",
			Token:    "",
		},
	}
	if _, err := os.Stat("config.toml"); err == nil {
		data, err := os.ReadFile("config.toml")
		if err != nil {
			return nil, err
		}
		if err := toml.Unmarshal(data, conf); err != nil {
			return nil, err
		}
		return conf, nil
	}
	b, err := toml.Marshal(conf)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile("config.toml", b, 0644); err != nil {
		return nil, err
	}
	return conf, nil
}

// parse reads resource packs from the "resource_packs" directory and applies content keys if provided.
func parse(keys map[string]string, logger *slog.Logger) ([]*resource.Pack, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	dir := path.Join(wd, "resource_packs")
	if _, err := os.Stat(dir); err != nil && os.IsNotExist(err) {
		if err := os.Mkdir(dir, os.ModePerm); err != nil {
			return nil, err
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var packs []*resource.Pack
	for _, entry := range entries {
		pack, err := resource.ReadPath(path.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}

		if key, ok := keys[pack.UUID().String()]; ok {
			pack = pack.WithContentKey(key)
		}
		sizeInMB := float64(pack.Len()) / (1024 * 1024)
		logger.Debug("Loaded pack", "name", pack.Name(), "size", fmt.Sprintf("%.2fMB", sizeInMB), "uuid", pack.UUID(), "version", pack.Version())
		packs = append(packs, pack)
	}
	return packs, nil
}

var _ server.Discovery = &LobbyDiscovery{}
var _ session.Processor = &TransferProcessor{}
