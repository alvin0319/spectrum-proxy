package main

import (
	"fmt"
	"github.com/c-bata/go-prompt"
	"github.com/cooldogedev/spectrum"
	"github.com/cooldogedev/spectrum/server"
	"github.com/cooldogedev/spectrum/session"
	"github.com/cooldogedev/spectrum/session/animation"
	"github.com/cooldogedev/spectrum/transport"
	"github.com/cooldogedev/spectrum/util"
	"github.com/lmittmann/tint"
	"github.com/pelletier/go-toml"
	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
	"github.com/sandertv/gophertunnel/minecraft/resource"
	"image/color"
	"log/slog"
	"os"
	"path"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

// map of server addresses to their names.
var serverMap = make(map[string]string)
var addressToName = make(map[string]string)

var lobbyServerAddress string

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
}

type Server struct {
	Name string `toml:"name"`
	Addr string `toml:"addr"`
}

// Discover returns the lobby server address for the player to connect to.
func (l LobbyDiscovery) Discover(conn *minecraft.Conn) (string, error) {
	return lobbyServerAddress, nil
}

// DiscoverFallback returns the lobby server address as a fallback for the player.
func (l LobbyDiscovery) DiscoverFallback(conn *minecraft.Conn) (string, error) {
	return lobbyServerAddress, nil
}

// LobbyProcessor implements session.Processor to handle server transfers while player is in the game.
type LobbyProcessor struct {
	// embedded session.NopProcessor to avoid implementing all methods.
	session.NopProcessor
	// s is the current session being processed.
	s *session.Session
	// log is the logger for this processor.
	log *slog.Logger
}

// ProcessServer is called when a packet is received from the server.
// Canceling it will prevent the packet from being sent to the client.
func (l *LobbyProcessor) ProcessServer(ctx *session.Context, pk *packet.Packet) {
	if t, ok := (*pk).(*packet.Transfer); ok {
		addr := t.Address
		if a, ok := serverMap[addr]; ok {
			ctx.Cancel()
			err := l.s.TransferTimeout(a, 10*time.Second)
			if err != nil {
				l.log.Error("failed to transfer", "err", err, "address", addr)
				l.s.CloseWithError(err)
			}
		}
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

	packs, err := parse(make(map[string]string)) // TODO: support content keys
	if err != nil {
		logger.Error("failed to parse resource packs", "err", err)
		return
	}

	logger.Info("Loaded resource packs", "count", len(packs))

	proxy := spectrum.NewSpectrum(server.NewStaticDiscovery(lobbyServerAddress, lobbyServerAddress), logger, &util.Opts{
		ShutdownMessage: conf.ShutdownMessage,
		Addr:            conf.BindAddr,
		AutoLogin:       true,
		LatencyInterval: 3000,
	}, transport.NewSpectral(logger))
	if err := proxy.Listen(minecraft.ListenConfig{
		StatusProvider:       util.NewStatusProvider(conf.Name, conf.Name),
		TexturePacksRequired: len(packs) > 0,
		ResourcePacks:        packs,
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

	logger.Info("Starting spectrum proxy", "addr", proxy.Opts().Addr, "mc-version", protocol.CurrentVersion, "go-version", info.GoVersion, "commit", revision)

	go processCommand(proxy, conf)

	for {
		s, err := proxy.Accept()
		if err != nil {
			continue
		}
		s.SetProcessor(&LobbyProcessor{
			s:   s,
			log: slog.Default(),
		})
		s.SetAnimation(&animation.Fade{
			Colour: color.RGBA{},
			Timing: protocol.CameraFadeTimeData{
				FadeInDuration:  0.32,
				WaitDuration:    0.84,
				FadeOutDuration: 0.23,
			},
		})
	}
}

// processCommand initializes the command prompt and handles user input commands.
func processCommand(proxy *spectrum.Spectrum, conf *ServerConfig) {
	logger := slog.Default()
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
		prompt.OptionTitle("Spectrum Proxy Console"),
		prompt.OptionPrefixTextColor(prompt.Yellow),
		prompt.OptionSuggestionBGColor(prompt.DarkGray),
		prompt.OptionDescriptionBGColor(prompt.Black),
		prompt.OptionDescriptionTextColor(prompt.White),
		prompt.OptionSelectedSuggestionBGColor(prompt.Blue),
		prompt.OptionSelectedSuggestionTextColor(prompt.White),
		prompt.OptionSelectedDescriptionBGColor(prompt.Blue),
		prompt.OptionAddKeyBind(prompt.KeyBind{
			Key: prompt.ControlC,
			Fn: func(_ *prompt.Buffer) {
				// Handle Ctrl+C to exit gracefully
				logger.Info("Exiting Spectrum Proxy Console...")
				if historyFile != nil {
					if err := historyFile.Save(); err != nil {
						logger.Error("Failed to save command history on exit", "error", err)
					}
				}
				proxy.Close()
				os.Exit(0)
			},
		}),
	}

	if historyFile != nil {
		options = append(options, prompt.OptionHistory(historyFile.GetHistory()))
	}

	p := prompt.New(
		executor,
		c.Complete,
		options...,
	)

	p.Run()
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
func parse(keys map[string]string) ([]*resource.Pack, error) {
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
		packs = append(packs, pack)
	}
	return packs, nil
}

var _ server.Discovery = &LobbyDiscovery{}
var _ session.Processor = &LobbyProcessor{}
