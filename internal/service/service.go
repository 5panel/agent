// Package service wraps github.com/kardianos/service so the same binary
// installs itself as a systemd unit on Linux and a Windows service, and
// runs correctly under either. The unit is generated from the template in
// this file (Restart=always, RestartSec=5) rather than the library's
// default one, whose RestartSec is two minutes.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"

	"github.com/kardianos/service"
)

// Name is the service name on both platforms.
const Name = "fivepanel-agent"

// Description shows in systemctl and the Windows Services panel.
const Description = "Connects your MySQL/MariaDB or MongoDB database to FivePanel over an outbound WebSocket"

// systemdUnit is rendered by kardianos' mini template engine; the
// functions ({{Path}}, {{Arguments}}, ...) are the ones its own template
// uses.
const systemdUnit = `[Unit]
Description={{Description}}
ConditionFileIsExecutable={{Path | cmdEscape}}
After=network-online.target mysql.service mariadb.service mongod.service
Wants=network-online.target
{{range Dependencies}}{{.}}
{{end}}
[Service]
StartLimitInterval=5
StartLimitBurst=10
ExecStart={{Path | cmdEscape}}{{range Arguments}} {{. | cmd}}{{end}}
{{if WorkingDirectory}}WorkingDirectory={{WorkingDirectory | cmdEscape}}
{{end}}{{if UserName}}User={{UserName}}
{{end}}{{if LimitNOFILE}}LimitNOFILE={{LimitNOFILE}}
{{end}}Restart=always
RestartSec=5
EnvironmentFile=-/etc/sysconfig/{{Name}}
EnvironmentFile=-/etc/fivepanel/environment

{{range EnvVars}}{{.}}
{{end}}[Install]
WantedBy=multi-user.target
`

// config builds the kardianos description for the given config path.
func config(configPath string) (*service.Config, error) {
	abs, err := filepath.Abs(configPath)
	if err != nil {
		return nil, err
	}
	c := &service.Config{
		Name:        Name,
		DisplayName: "FivePanel Agent",
		Description: Description,
		Arguments:   []string{"run", "--config", abs},
		Option: service.KeyValue{
			"SystemdScript": systemdUnit,
			"LimitNOFILE":   65536,
			// Windows: restart 5 s after a crash, reset the counter daily.
			"OnFailure":              "restart",
			"OnFailureDelayDuration": "5s",
			"OnFailureResetPeriod":   86400,
		},
	}
	if runtime.GOOS != "windows" {
		c.WorkingDirectory = filepath.Dir(abs)
	}
	return c, nil
}

// noop satisfies service.Interface for control actions, which never start
// the program.
type noop struct{}

func (noop) Start(service.Service) error { return nil }
func (noop) Stop(service.Service) error  { return nil }

// Control runs install/uninstall/start/stop/status. install requires the
// config file to exist, since that path is baked into the unit.
func Control(action, configPath string) (string, error) {
	if action == "install" {
		if st, err := os.Stat(configPath); err != nil || st.IsDir() {
			return "", fmt.Errorf("config file %s not found; run `fivepanel-agent setup` first", configPath)
		}
	}
	c, err := config(configPath)
	if err != nil {
		return "", err
	}
	s, err := service.New(noop{}, c)
	if err != nil {
		return "", err
	}
	switch action {
	case "install", "uninstall", "start", "stop", "restart":
		if err := service.Control(s, action); err != nil {
			return "", err
		}
		return fmt.Sprintf("%s: %s done", Name, action), nil
	case "status":
		st, err := s.Status()
		if err != nil {
			if errors.Is(err, service.ErrNotInstalled) {
				return Name + ": not installed", nil
			}
			return "", err
		}
		switch st {
		case service.StatusRunning:
			return Name + ": running", nil
		case service.StatusStopped:
			return Name + ": stopped", nil
		}
		return Name + ": unknown", nil
	}
	return "", fmt.Errorf("unknown service action %q (install, uninstall, start, stop, restart, status)", action)
}

// Interactive reports whether the process runs in a terminal rather than
// under a service manager (on Windows: not started by the SCM).
func Interactive() bool { return service.Interactive() }

// program adapts a run function to service.Interface.
type program struct {
	run    func(ctx context.Context) error
	cancel context.CancelFunc
	done   chan struct{}
	err    error
	once   sync.Once
}

func (p *program) Start(s service.Service) error {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	go func() {
		defer close(p.done)
		p.err = p.run(ctx)
		// The run function returned on its own (bad token, replaced):
		// ask the service manager to stop us so the exit is orderly.
		if ctx.Err() == nil {
			p.once.Do(func() { go func() { _ = s.Stop() }() })
		}
	}()
	return nil
}

func (p *program) Stop(service.Service) error {
	p.cancel()
	<-p.done
	return nil
}

// Run executes fn until it returns or the process is asked to stop. In a
// terminal that is Ctrl-C / SIGTERM; under a service manager it is the
// manager's stop request. The error fn returned is passed back.
func Run(fn func(ctx context.Context) error) error {
	if Interactive() {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return fn(ctx)
	}
	c, err := config("config.yaml")
	if err != nil {
		return err
	}
	p := &program{run: fn}
	s, err := service.New(p, c)
	if err != nil {
		return err
	}
	if err := s.Run(); err != nil {
		return err
	}
	return p.err
}

// Logger returns a slog handler that writes to the platform's service log
// (Windows event log, syslog) when running as a service, or nil in a
// terminal.
func Logger(level slog.Leveler) slog.Handler {
	if Interactive() {
		return nil
	}
	c, err := config("config.yaml")
	if err != nil {
		return nil
	}
	s, err := service.New(noop{}, c)
	if err != nil {
		return nil
	}
	l, err := s.Logger(nil)
	if err != nil {
		return nil
	}
	return &eventHandler{logger: l, level: level}
}

// eventHandler routes slog records into service.Logger.
type eventHandler struct {
	logger service.Logger
	level  slog.Leveler
	attrs  []slog.Attr
}

func (h *eventHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level.Level()
}

func (h *eventHandler) Handle(_ context.Context, r slog.Record) error {
	msg := r.Message
	for _, a := range h.attrs {
		msg += " " + a.String()
	}
	r.Attrs(func(a slog.Attr) bool {
		msg += " " + a.String()
		return true
	})
	switch {
	case r.Level >= slog.LevelError:
		return h.logger.Error(msg)
	case r.Level >= slog.LevelWarn:
		return h.logger.Warning(msg)
	default:
		return h.logger.Info(msg)
	}
}

func (h *eventHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &eventHandler{logger: h.logger, level: h.level, attrs: append(append([]slog.Attr{}, h.attrs...), attrs...)}
}

func (h *eventHandler) WithGroup(string) slog.Handler { return h }
