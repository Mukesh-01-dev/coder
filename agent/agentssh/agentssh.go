package agentssh

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gliderlabs/ssh"
	"github.com/google/uuid"
	"github.com/kballard/go-shellquote"
	"github.com/pkg/sftp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/afero"
	"go.uber.org/atomic"
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/xerrors"

	"cdr.dev/slog"

	"github.com/coder/coder/v2/agent/agentcontainers"
	"github.com/coder/coder/v2/agent/agentexec"
	"github.com/coder/coder/v2/agent/agentrsa"
	"github.com/coder/coder/v2/agent/usershell"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/coder/v2/pty"
)

const (
	// MagicSessionErrorCode indicates that something went wrong with the session, rather than the
	// command just returning a nonzero exit code, and is chosen as an arbitrary, high number
	// unlikely to shadow other exit codes, which are typically 1, 2, 3, etc.
	MagicSessionErrorCode = 229

	// MagicProcessCmdlineJetBrains is a string in a process's command line that
	// uniquely identifies it as JetBrains software.
	MagicProcessCmdlineJetBrains = "idea.vendor.name=JetBrains"

	// BlockedFileTransferErrorCode indicates that SSH server restricted the raw command from performing
	// the file transfer.
	BlockedFileTransferErrorCode    = 65 // Error code: host not allowed to connect
	BlockedFileTransferErrorMessage = "File transfer has been disabled."
)

// MagicSessionType is a type that represents the type of session that is being
// established.
type MagicSessionType string

const (
	// MagicSessionTypeEnvironmentVariable is used to track the purpose behind an SSH connection.
	// This is stripped from any commands being executed, and is counted towards connection stats.
	MagicSessionTypeEnvironmentVariable = "CODER_SSH_SESSION_TYPE"
	// ContainerEnvironmentVariable is used to specify the target container for an SSH connection.
	// This is stripped from any commands being executed.
	// Only available if CODER_AGENT_DEVCONTAINERS_ENABLE=true.
	ContainerEnvironmentVariable = "CODER_CONTAINER"
	// ContainerUserEnvironmentVariable is used to specify the container user for
	// an SSH connection.
	// Only available if CODER_AGENT_DEVCONTAINERS_ENABLE=true.
	ContainerUserEnvironmentVariable = "CODER_CONTAINER_USER"
)

// MagicSessionType enums.
const (
	// MagicSessionTypeUnknown means the session type could not be determined.
	MagicSessionTypeUnknown MagicSessionType = "unknown"
	// MagicSessionTypeSSH is the default session type.
	MagicSessionTypeSSH MagicSessionType = "ssh"
	// MagicSessionTypeVSCode is set in the SSH config by the VS Code extension to identify itself.
	MagicSessionTypeVSCode MagicSessionType = "vscode"
	// MagicSessionTypeJetBrains is set in the SSH config by the JetBrains
	// extension to identify itself.
	MagicSessionTypeJetBrains MagicSessionType = "jetbrains"
)

// BlockedFileTransferCommands contains a list of restricted file transfer commands.
var BlockedFileTransferCommands = []string{"nc", "rsync", "scp", "sftp"}

type reportConnectionFunc func(id uuid.UUID, sessionType MagicSessionType, ip string) (disconnected func(code int, reason string))

// Config sets configuration parameters for the agent SSH server.
type Config struct {
	// MaxTimeout sets the absolute connection timeout, none if empty. If set to
	// 3 seconds or more, keep alive will be used instead.
	MaxTimeout time.Duration
	// MOTDFile returns the path to the message of the day file. If set, the
	// file will be displayed to the user upon login.
	MOTDFile func() string
	// ServiceBanner returns the configuration for the Coder service banner.
	AnnouncementBanners func() *[]codersdk.BannerConfig
	// UpdateEnv updates the environment variables for the command to be
	// executed. It can be used to add, modify or replace environment variables.
	UpdateEnv func(current []string) (updated []string, err error)
	// WorkingDirectory sets the working directory for commands and defines
	// where users will land when they connect via SSH. Default is the home
	// directory of the user.
	WorkingDirectory func() string
	// X11DisplayOffset is the offset to add to the X11 display number.
	// Default is 10.
	X11DisplayOffset *int
	// BlockFileTransfer restricts use of file transfer applications.
	BlockFileTransfer bool
	// ReportConnection.
	ReportConnection reportConnectionFunc
	// Experimental: allow connecting to running containers via Docker exec.
	// Note that this is different from the devcontainers feature, which uses
	// subagents.
	ExperimentalContainers bool
	// X11Net allows overriding the networking implementation used for X11
	// forwarding listeners. When nil, a default implementation backed by the
	// standard library networking package is used.
	X11Net X11Network
}

type Server struct {
	mu        sync.RWMutex // Protects following.
	fs        afero.Fs
	listeners map[net.Listener]struct{}
	conns     map[net.Conn]struct{}
	sessions  map[ssh.Session]struct{}
	processes map[*os.Process]struct{}
	closing   chan struct{}
	// Wait for goroutines to exit, waited without
	// a lock on mu but protected by closing.
	wg sync.WaitGroup

	Execer       agentexec.Execer
	logger       slog.Logger
	srv          *ssh.Server
	x11Forwarder *x11Forwarder

	config *Config

	connCountVSCode     atomic.Int64
	connCountJetBrains  atomic.Int64
	connCountSSHSession atomic.Int64

	metrics *sshServerMetrics
}

func NewServer(ctx context.Context, logger slog.Logger, prometheusRegistry *prometheus.Registry, fs afero.Fs, execer agentexec.Execer, config *Config) (*Server, error) {
	if config == nil {
		config = &Config{}
	}
	if config.X11DisplayOffset == nil {
		offset := X11DefaultDisplayOffset
		config.X11DisplayOffset = &offset
	}
	if config.UpdateEnv == nil {
		config.UpdateEnv = func(current []string) ([]string, error) { return current, nil }
	}
	if config.MOTDFile == nil {
		config.MOTDFile = func() string { return "" }
	}
	if config.AnnouncementBanners == nil {
		config.AnnouncementBanners = func() *[]codersdk.BannerConfig { return &[]codersdk.BannerConfig{} }
	}
	if config.WorkingDirectory == nil {
		config.WorkingDirectory = func() string {
			home, err := userHomeDir()
			if err != nil {
				return ""
			}
			return home
		}
	}
	if config.ReportConnection == nil {
		config.ReportConnection = func(uuid.UUID, MagicSessionType, string) func(int, string) { return func(int, string) {} }
	}

	forwardHandler := &ssh.ForwardedTCPHandler{}
	unixForwardHandler := newForwardedUnixHandler(logger)

	metrics := newSSHServerMetrics(prometheusRegistry)
	s := &Server{
		Execer:    execer,
		listeners: make(map[net.Listener]struct{}),
		fs:        fs,
		conns:     make(map[net.Conn]struct{}),
		sessions:  make(map[ssh.Session]struct{}),
		processes: make(map[*os.Process]struct{}),
		logger:    logger,

		config: config,

		metrics: metrics,
		x11Forwarder: &x11Forwarder{
			logger:           logger,
			x11HandlerErrors: metrics.x11HandlerErrors,
			fs:               fs,
			displayOffset:    *config.X11DisplayOffset,
			sessions:         make(map[*x11Session]struct{}),
			connections:      make(map[net.Conn]struct{}),
			network: func() X11Network {
				if config.X11Net != nil {
					return config.X11Net
				}
				return osNet{}
			}(),
		},
	}

	srv := &ssh.Server{
		ChannelHandlers: map[string]ssh.ChannelHandler{
			"direct-tcpip": func(srv *ssh.Server, conn *gossh.ServerConn, newChan gossh.NewChannel, ctx ssh.Context) {
				// Wrapper is designed to find and track JetBrains Gateway connections.
				wrapped := NewJetbrainsChannelWatcher(ctx, s.logger, s.config.ReportConnection, newChan, &s.connCountJetBrains)
				ssh.DirectTCPIPHandler(srv, conn, wrapped, ctx)
			},
			"direct-streamlocal@openssh.com": directStreamLocalHandler,
			"session":                        ssh.DefaultSessionHandler,
		},
		ConnectionFailedCallback: func(conn net.Conn, err error) {
			s.logger.Warn(ctx, "ssh connection failed",
				slog.F("remote_addr", conn.RemoteAddr()),
				slog.F("local_addr", conn.LocalAddr()),
				slog.Error(err))
			metrics.failedConnectionsTotal.Add(1)
		},
		ConnectionCompleteCallback: func(conn *gossh.ServerConn, err error) {
			s.logger.Info(ctx, "ssh connection complete",
				slog.F("remote_addr", conn.RemoteAddr()),
				slog.F("local_addr", conn.LocalAddr()),
				slog.Error(err))
		},
		Handler: s.sessionHandler,
		// HostSigners are intentionally empty, as the host key will
		// be set before we start listening.
		HostSigners: []ssh.Signer{},
		LocalPortForwardingCallback: func(ctx ssh.Context, destinationHost string, destinationPort uint32) bool {
			// Allow local port forwarding all!
			s.logger.Debug(ctx, "local port forward",
				slog.F("destination_host", destinationHost),
				slog.F("destination_port", destinationPort))
			return true
		},
		PtyCallback: func(_ ssh.Context, _ ssh.Pty) bool {
			return true
		},
		ReversePortForwardingCallback: func(ctx ssh.Context, bindHost string, bindPort uint32) bool {
			// Allow reverse port forwarding all!
			s.logger.Debug(ctx, "reverse port forward",
				slog.F("bind_host", bindHost),
				slog.F("bind_port", bindPort))
			return true
		},
		RequestHandlers: map[string]ssh.RequestHandler{
			"tcpip-forward":                          forwardHandler.HandleSSHRequest,
			"cancel-tcpip-forward":                   forwardHandler.HandleSSHRequest,
			"streamlocal-forward@openssh.com":        unixForwardHandler.HandleSSHRequest,
			"cancel-streamlocal-forward@openssh.com": unixForwardHandler.HandleSSHRequest,
		},
		X11Callback: s.x11Callback,
		ServerConfigCallback: func(_ ssh.Context) *gossh.ServerConfig {
			return &gossh.ServerConfig{
				NoClientAuth: true,
			}
		},
		SubsystemHandlers: map[string]ssh.SubsystemHandler{
			"sftp": s.sessionHandler,
		},
	}

	// The MaxTimeout functionality has been substituted with the introduction
	// of the KeepAlive feature. In cases where very short timeouts are set, the
	// SSH server will automatically switch to the connection timeout for both
	// read and write operations.
	if config.MaxTimeout >= 3*time.Second {
		srv.ClientAliveCountMax = 3
		srv.ClientAliveInterval = config.MaxTimeout / time.Duration(srv.ClientAliveCountMax)
		srv.MaxTimeout = 0
	} else {
		srv.MaxTimeout = config.MaxTimeout
	}

	s.srv = srv
	return s, nil
}

type ConnStats struct {
	Sessions  int64
	VSCode    int64
	JetBrains int64
}

func (s *Server) ConnStats() ConnStats {
	return ConnStats{
		Sessions:  s.connCountSSHSession.Load(),
		VSCode:    s.connCountVSCode.Load(),
		JetBrains: s.connCountJetBrains.Load(),
	}
}

func extractMagicSessionType(env []string) (magicType MagicSessionType, rawType string, filteredEnv []string) {
	for _, kv := range env {
		if !strings.HasPrefix(kv, MagicSessionTypeEnvironmentVariable) {
			continue
		}

		rawType = strings.TrimPrefix(kv, MagicSessionTypeEnvironmentVariable+"=")
		// Keep going, we'll use the last instance of the env.
	}

	// Always force lowercase checking to be case-insensitive.
	switch MagicSessionType(strings.ToLower(rawType)) {
	case MagicSessionTypeVSCode:
		magicType = MagicSessionTypeVSCode
	case MagicSessionTypeJetBrains:
		magicType = MagicSessionTypeJetBrains
	case "", MagicSessionTypeSSH:
		magicType = MagicSessionTypeSSH
	default:
		magicType = MagicSessionTypeUnknown
	}

	return magicType, rawType, slices.DeleteFunc(env, func(kv string) bool {
		return strings.HasPrefix(kv, MagicSessionTypeEnvironmentVariable+"=")
	})
}

// sessionCloseTracker is a wrapper around Session that tracks the exit code.
type sessionCloseTracker struct {
	ssh.Session
	exitOnce sync.Once
	code     atomic.Int64
}

var _ ssh.Session = &sessionCloseTracker{}

func (s *sessionCloseTracker) track(code int) {
	s.exitOnce.Do(func() {
		s.code.Store(int64(code))
	})
}

func (s *sessionCloseTracker) exitCode() int {
	return int(s.code.Load())
}

func (s *sessionCloseTracker) Exit(code int) error {
	s.track(code)
	return s.Session.Exit(code)
}

func (s *sessionCloseTracker) Close() error {
	s.track(1)
	return s.Session.Close()
}

func extractContainerInfo(env []string) (container, containerUser string, filteredEnv []string) {
	for _, kv := range env {
		if strings.HasPrefix(kv, ContainerEnvironmentVariable+"=") {
			container = strings.TrimPrefix(kv, ContainerEnvironmentVariable+"=")
		}

		if strings.HasPrefix(kv, ContainerUserEnvironmentVariable+"=") {
			containerUser = strings.TrimPrefix(kv, ContainerUserEnvironmentVariable+"=")
		}
	}

	return container, containerUser, slices.DeleteFunc(env, func(kv string) bool {
		return strings.HasPrefix(kv, ContainerEnvironmentVariable+"=") || strings.HasPrefix(kv, ContainerUserEnvironmentVariable+"=")
	})
}

func (s *Server) sessionHandler(session ssh.Session) {
	ctx := session.Context()
	id := uuid.New()
	logger := s.logger.With(
		slog.F("remote_addr", session.RemoteAddr()),
		slog.F("local_addr", session.LocalAddr()),
		// Assigning a random uuid for each session is useful for tracking
		// logs for the same ssh session.
		slog.F("id", id.String()),
	)
	logger.Info(ctx, "handling ssh session")

	env := session.Environ()
	magicType, magicTypeRaw, env := extractMagicSessionType(env)

	if !s.trackSession(session, true) {
		reason := "unable to accept new session, server is closing"
		// Report connection attempt even if we couldn't accept it.
		disconnected := s.config.ReportConnection(id, magicType, session.RemoteAddr().String())
		defer disconnected(1, reason)

		logger.Info(ctx, reason)
		// See (*Server).Close() for why we call Close instead of Exit.
		_ = session.Close()
		return
	}
	defer s.trackSession(session, false)

	reportSession := true

	switch magicType {
	case MagicSessionTypeVSCode:
		s.connCountVSCode.Add(1)
		defer s.connCountVSCode.Add(-1)
	case MagicSessionTypeJetBrains:
		// Do nothing here because JetBrains launches hundreds of ssh sessions.
		// We instead track JetBrains in the single persistent tcp forwarding channel.
		reportSession = false
	case MagicSessionTypeSSH:
		s.connCountSSHSession.Add(1)
		defer s.connCountSSHSession.Add(-1)
	case MagicSessionTypeUnknown:
		logger.Warn(ctx, "invalid magic ssh session type specified", slog.F("raw_type", magicTypeRaw))
	}

	closeCause := func(string) {}
	if reportSession {
		var reason string
		closeCause = func(r string) { reason = r }

		scr := &sessionCloseTracker{Session: session}
		session = scr

		disconnected := s.config.ReportConnection(id, magicType, session.RemoteAddr().String())
		defer func() {
			disconnected(scr.exitCode(), reason)
		}()
	}

	if s.fileTransferBlocked(session) {
		s.logger.Warn(ctx, "file transfer blocked", slog.F("session_subsystem", session.Subsystem()), slog.F("raw_command", session.RawCommand()))

		if session.Subsystem() == "" { // sftp does not expect error, otherwise it fails with "package too long"
			// Response format: <status_code><message body>\n
			errorMessage := fmt.Sprintf("\x02%s\n", BlockedFileTransferErrorMessage)
			_, _ = session.Write([]byte(errorMessage))
		}
		closeCause("file transfer blocked")
		_ = session.Exit(BlockedFileTransferErrorCode)
		return
	}

	container, containerUser, env := extractContainerInfo(env)
	if container != "" {
		s.logger.Debug(ctx, "container info",
			slog.F("container", container),
			slog.F("container_user", containerUser),
		)
	}

	switch ss := session.Subsystem(); ss {
	case "":
	case "sftp":
		if s.config.ExperimentalContainers && container != "" {
			closeCause("sftp not yet supported with containers")
			_ = session.Exit(1)
			return
		}
		err := s.sftpHandler(logger, session)
		if err != nil {
			closeCause(err.Error())
		}
		return
	default:
		logger.Warn(ctx, "unsupported subsystem", slog.F("subsystem", ss))
		closeCause(fmt.Sprintf("unsupported subsystem: %s", ss))
		_ = session.Exit(1)
		return
	}

	x11, hasX11 := session.X11()
	if hasX11 {
		display, handled := s.x11Forwarder.x11Handler(ctx, session)
		if !handled {
			logger.Error(ctx, "x11 handler failed")
			closeCause("x11 handler failed")
			_ = session.Exit(1)
			return
		}
		env = append(env, fmt.Sprintf("DISPLAY=localhost:%d.%d", display, x11.ScreenNumber))
	}

	err := s.sessionStart(logger, session, env, magicType, container, containerUser)
	var exitError *exec.ExitError
	if xerrors.As(err, &exitError) {
		code := exitError.ExitCode()
		if code == -1 {
			// If we return -1 here, it will be transmitted as an
			// uint32(4294967295). This exit code is nonsense, so
			// instead we return 255 (same as OpenSSH). This is
			// also the same exit code that the shell returns for
			// -1.
			//
			// For signals, we could consider sending 128+signal
			// instead (however, OpenSSH doesn't seem to do this).
			code = 255
		}
		logger.Info(ctx, "ssh session returned",
			slog.Error(exitError),
			slog.F("process_exit_code", exitError.ExitCode()),
			slog.F("exit_code", code),
		)

		closeCause(fmt.Sprintf("process exited with error status: %d", exitError.ExitCode()))

		// TODO(mafredri): For signal exit, there's also an "exit-signal"
		// request (session.Exit sends "exit-status"), however, since it's
		// not implemented on the session interface and not used by
		// OpenSSH, we'll leave it for now.
		_ = session.Exit(code)
		return
	}
	if err != nil {
		logger.Warn(ctx, "ssh session failed", slog.Error(err))
		// This exit code is designed to be unlikely to be confused for a legit exit code
		// from the process.
		closeCause(err.Error())
		_ = session.Exit(MagicSessionErrorCode)
		return
	}
	logger.Info(ctx, "normal ssh session exit")
	_ = session.Exit(0)
}

// fileTransferBlocked method checks if the file transfer commands should be blocked.
//
// Warning: consider this mechanism as "Do not trespass" sign, as a violator can still ssh to the host,
// smuggle the `scp` binary, or just manually send files outside with `curl` or `ftp`.
// If a user needs a more sophisticated and battle-proof solution, consider full endpoint security.
func (s *Server) fileTransferBlocked(session ssh.Session) bool {
	if !s.config.BlockFileTransfer {
		return false // file transfers are permitted
	}
	// File transfers are restricted.

	if session.Subsystem() == "sftp" {
		return true
	}

	cmd := session.Command()
	if len(cmd) == 0 {
		return false // no command?
	}

	c := cmd[0]
	c = filepath.Base(c) // in case the binary is absolute path, /usr/sbin/scp

	for _, cmd := range BlockedFileTransferCommands {
		if cmd == c {
			return true
		}
	}
	return false
}

func (s *Server) sessionStart(logger slog.Logger, session ssh.Session, env []string, magicType MagicSessionType, container, containerUser string) (retErr error) {
	ctx := session.Context()

	magicTypeLabel := magicTypeMetricLabel(magicType)
	sshPty, windowSize, isPty := session.Pty()
	ptyLabel := "no"
	if isPty {
		ptyLabel = "yes"
	}

	var ei usershell.EnvInfoer
	var err error
	if s.config.ExperimentalContainers && container != "" {
		ei, err = agentcontainers.EnvInfo(ctx, s.Execer, container, containerUser)
		if err != nil {
			s.metrics.sessionErrors.WithLabelValues(magicTypeLabel, ptyLabel, "container_env_info").Add(1)
			return err
		}
	}
	cmd, err := s.CreateCommand(ctx, session.RawCommand(), env, ei)
	if err != nil {
		s.metrics.sessionErrors.WithLabelValues(magicTypeLabel, ptyLabel, "create_command").Add(1)
		return err
	}

	if ssh.AgentRequested(session) {
		l, err := ssh.NewAgentListener()
		if err != nil {
			s.metrics.sessionErrors.WithLabelValues(magicTypeLabel, ptyLabel, "listener").Add(1)
			return xerrors.Errorf("new agent listener: %w", err)
		}
		defer l.Close()
		go ssh.ForwardAgentConnections(l, session)
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", "SSH_AUTH_SOCK", l.Addr().String()))
	}

	if isPty {
		return s.startPTYSession(logger, session, magicTypeLabel, cmd, sshPty, windowSize)
	}
	return s.startNonPTYSession(logger, session, magicTypeLabel, cmd.AsExec())
}

func (s *Server) startNonPTYSession(logger slog.Logger, session ssh.Session, magicTypeLabel string, cmd *exec.Cmd) error {
	s.metrics.sessionsTotal.WithLabelValues(magicTypeLabel, "no").Add(1)

	// Create a process group and send SIGHUP to child processes,
	// otherwise context cancellation will not propagate properly
	// and SSH server close may be delayed.
	cmd.SysProcAttr = cmdSysProcAttr()

	// to match OpenSSH, we don't actually tear a non-TTY command down, even if the session ends.
	// c.f. https://github.com/coder/coder/issues/18519#issuecomment-3019118271
	cmd.Cancel = nil

	cmd.Stdout = session
	cmd.Stderr = session.Stderr()
	// This blocks forever until stdin is received if we don't
	// use StdinPipe. It's unknown what causes this.
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		s.metrics.sessionErrors.WithLabelValues(magicTypeLabel, "no", "stdin_pipe").Add(1)
		return xerrors.Errorf("create stdin pipe: %w", err)
	}
	go func() {
		_, err := io.Copy(stdinPipe, session)
		if err != nil {
			s.metrics.sessionErrors.WithLabelValues(magicTypeLabel, "no", "stdin_io_copy").Add(1)
		}
		_ = stdinPipe.Close()
	}()
	err = cmd.Start()
	if err != nil {
		s.metrics.sessionErrors.WithLabelValues(magicTypeLabel, "no", "start_command").Add(1)
		return xerrors.Errorf("start: %w", err)
	}

	// Since we don't cancel the process when the session stops, we still need to tear it down if we are closing. So
	// track it here.
	if !s.trackProcess(cmd.Process, true) {
		// must be closing
		err = cmdCancel(logger, cmd.Process)
		return xerrors.Errorf("failed to track process: %w", err)
	}
	defer s.trackProcess(cmd.Process, false)

	sigs := make(chan ssh.Signal, 1)
	session.Signals(sigs)
	defer func() {
		session.Signals(nil)
		close(sigs)
	}()
	go func() {
		for sig := range sigs {
			handleSignal(logger, sig, cmd.Process, s.metrics, magicTypeLabel)
		}
	}()
	return cmd.Wait()
}

// ptySession is the interface to the ssh.Session that startPTYSession uses
// we use an interface here so that we can fake it in tests.
type ptySession interface {
	io.ReadWriter
	Context() ssh.Context
	DisablePTYEmulation()
	RawCommand() string
	Signals(chan<- ssh.Signal)
}

func (s *Server) startPTYSession(logger slog.Logger, session ptySession, magicTypeLabel string, cmd *pty.Cmd, sshPty ssh.Pty, windowSize <-chan ssh.Window) (retErr error) {
	s.metrics.sessionsTotal.WithLabelValues(magicTypeLabel, "yes").Add(1)

	ctx := session.Context()
	// Disable minimal PTY emulation set by gliderlabs/ssh (NL-to-CRNL).
	// See https://github.com/coder/coder/issues/3371.
	session.DisablePTYEmulation()

	if isLoginShell(session.RawCommand()) {
		banners := s.config.AnnouncementBanners()
		if banners != nil {
			for _, banner := range *banners {
				err := showAnnouncementBanner(session, banner)
				if err != nil {
					logger.Error(ctx, "agent failed to show announcement banner", slog.Error(err))
					s.metrics.sessionErrors.WithLabelValues(magicTypeLabel, "yes", "announcement_banner").Add(1)
					break
				}
			}
		}
	}

	if !isQuietLogin(s.fs, session.RawCommand()) {
		err := showMOTD(s.fs, session, s.config.MOTDFile())
		if err != nil {
			logger.Error(ctx, "agent failed to show MOTD", slog.Error(err))
			s.metrics.sessionErrors.WithLabelValues(magicTypeLabel, "yes", "motd").Add(1)
		}
	}

	cmd.Env = append(cmd.Env, fmt.Sprintf("TERM=%s", sshPty.Term))

	// The pty package sets `SSH_TTY` on supported platforms.
	ptty, process, err := pty.Start(cmd, pty.WithPTYOption(
		pty.WithSSHRequest(sshPty),
		pty.WithLogger(slog.Stdlib(ctx, logger, slog.LevelInfo)),
	))
	if err != nil {
		s.metrics.sessionErrors.WithLabelValues(magicTypeLabel, "yes", "start_command").Add(1)
		return xerrors.Errorf("start command: %w", err)
	}
	defer func() {
		closeErr := ptty.Close()
		if closeErr != nil {
			logger.Warn(ctx, "failed to close tty", slog.Error(closeErr))
			s.metrics.sessionErrors.WithLabelValues(magicTypeLabel, "yes", "close").Add(1)
			if retErr == nil {
				retErr = closeErr
			}
		}
	}()
	sigs := make(chan ssh.Signal, 1)
	session.Signals(sigs)
	defer func() {
		session.Signals(nil)
		close(sigs)
	}()
	go func() {
		for {
			if sigs == nil && windowSize == nil {
				return
			}

			select {
			case sig, ok := <-sigs:
				if !ok {
					sigs = nil
					continue
				}
				handleSignal(logger, sig, process, s.metrics, magicTypeLabel)
			case win, ok := <-windowSize:
				if !ok {
					windowSize = nil
					continue
				}
				// #nosec G115 - Safe conversions for terminal dimensions which are expected to be within uint16 range
				resizeErr := ptty.Resize(uint16(win.Height), uint16(win.Width))
				// If the pty is closed, then command has exited, no need to log.
				if resizeErr != nil && !errors.Is(resizeErr, pty.ErrClosed) {
					logger.Warn(ctx, "failed to resize tty", slog.Error(resizeErr))
					s.metrics.sessionErrors.WithLabelValues(magicTypeLabel, "yes", "resize").Add(1)
				}
			}
		}
	}()

	go func() {
		_, err := io.Copy(ptty.InputWriter(), session)
		if err != nil {
			s.metrics.sessionErrors.WithLabelValues(magicTypeLabel, "yes", "input_io_copy").Add(1)
		}
	}()

	// We need to wait for the command output to finish copying.  It's safe to
	// just do this copy on the main handler goroutine because one of two things
	// will happen:
	//
	// 1. The command completes & closes the TTY, which then triggers an error
	//    after we've Read() all the buffered data from the PTY.
	// 2. The client hangs up, which cancels the command's Context, and go will
	//    kill the command's process.  This then has the same effect as (1).
	n, err := io.Copy(session, ptty.OutputReader())
	logger.Debug(ctx, "copy output done", slog.F("bytes", n), slog.Error(err))
	if err != nil {
		s.metrics.sessionErrors.WithLabelValues(magicTypeLabel, "yes", "output_io_copy").Add(1)
		return xerrors.Errorf("copy error: %w", err)
	}
	// We've gotten all the output, but we need to wait for the process to
	// complete so that we can get the exit code.  This returns
	// immediately if the TTY was closed as part of the command exiting.
	err = process.Wait()
	var exitErr *exec.ExitError
	// ExitErrors just mean the command we run returned a non-zero exit code, which is normal
	// and not something to be concerned about.  But, if it's something else, we should log it.
	if err != nil && !xerrors.As(err, &exitErr) {
		logger.Warn(ctx, "process wait exited with error", slog.Error(err))
		s.metrics.sessionErrors.WithLabelValues(magicTypeLabel, "yes", "wait").Add(1)
	}
	if err != nil {
		return xerrors.Errorf("process wait: %w", err)
	}
	return nil
}

func handleSignal(logger slog.Logger, ssig ssh.Signal, signaler interface{ Signal(os.Signal) error }, metrics *sshServerMetrics, magicTypeLabel string) {
	ctx := context.Background()
	sig := osSignalFrom(ssig)
	logger = logger.With(slog.F("ssh_signal", ssig), slog.F("signal", sig.String()))
	logger.Info(ctx, "received signal from client")
	err := signaler.Signal(sig)
	if err != nil {
		logger.Warn(ctx, "signaling the process failed", slog.Error(err))
		metrics.sessionErrors.WithLabelValues(magicTypeLabel, "yes", "signal").Add(1)
	}
}

func (s *Server) sftpHandler(logger slog.Logger, session ssh.Session) error {
	s.metrics.sftpConnectionsTotal.Add(1)

	ctx := session.Context()

	// Typically sftp sessions don't request a TTY, but if they do,
	// we must ensure the gliderlabs/ssh CRLF emulation is disabled.
	// Otherwise sftp will be broken. This can happen if a user sets
	// `RequestTTY force` in their SSH config.
	session.DisablePTYEmulation()

	var opts []sftp.ServerOption
	// Change current working directory to the users home
	// directory so that SFTP connections land there.
	homedir, err := userHomeDir()
	if err != nil {
		logger.Warn(ctx, "get sftp working directory failed, unable to get home dir", slog.Error(err))
	} else {
		opts = append(opts, sftp.WithServerWorkingDirectory(homedir))
	}

	server, err := sftp.NewServer(session, opts...)
	if err != nil {
		logger.Debug(ctx, "initialize sftp server", slog.Error(err))
		return xerrors.Errorf("initialize sftp server: %w", err)
	}
	defer server.Close()

	err = server.Serve()
	if err == nil || errors.Is(err, io.EOF) {
		// Unless we call `session.Exit(0)` here, the client won't
		// receive `exit-status` because `(*sftp.Server).Close()`
		// calls `Close()` on the underlying connection (session),
		// which actually calls `channel.Close()` because it isn't
		// wrapped. This causes sftp clients to receive a non-zero
		// exit code. Typically sftp clients don't echo this exit
		// code but `scp` on macOS does (when using the default
		// SFTP backend).
		_ = session.Exit(0)
		return nil
	}
	logger.Warn(ctx, "sftp server closed with error", slog.Error(err))
	s.metrics.sftpServerErrors.Add(1)
	_ = session.Exit(1)
	return xerrors.Errorf("sftp server closed with error: %w", err)
}

func (s *Server) CommandEnv(ei usershell.EnvInfoer, addEnv []string) (shell, dir string, env []string, err error) {
	if ei == nil {
		ei = &usershell.SystemEnvInfo{}
	}

	currentUser, err := ei.User()
	if err != nil {
		return "", "", nil, xerrors.Errorf("get current user: %w", err)
	}
	username := currentUser.Username

	shell, err = ei.Shell(username)
	if err != nil {
		return "", "", nil, xerrors.Errorf("get user shell: %w", err)
	}

	dir = s.config.WorkingDirectory()

	// If the metadata directory doesn't exist, we run the command
	// in the users home directory.
	_, err = os.Stat(dir)
	if dir == "" || err != nil {
		// Default to user home if a directory is not set.
		homedir, err := ei.HomeDir()
		if err != nil {
			return "", "", nil, xerrors.Errorf("get home dir: %w", err)
		}
		dir = homedir
	}
	env = append(ei.Environ(), addEnv...)
	// Set login variables (see `man login`).
	env = append(env, fmt.Sprintf("USER=%s", username))
	env = append(env, fmt.Sprintf("LOGNAME=%s", username))
	env = append(env, fmt.Sprintf("SHELL=%s", shell))

	env, err = s.config.UpdateEnv(env)
	if err != nil {
		return "", "", nil, xerrors.Errorf("apply env: %w", err)
	}

	return shell, dir, env, nil
}

// CreateCommand processes raw command input with OpenSSH-like behavior.
// If the script provided is empty, it will default to the users shell.
// This injects environment variables specified by the user at launch too.
// The final argument is an interface that allows the caller to provide
// alternative implementations for the dependencies of CreateCommand.
// This is useful when creating a command to be run in a separate environment
// (for example, a Docker container). Pass in nil to use the default.
func (s *Server) CreateCommand(ctx context.Context, script string, env []string, ei usershell.EnvInfoer) (*pty.Cmd, error) {
	if ei == nil {
		ei = &usershell.SystemEnvInfo{}
	}

	shell, dir, env, err := s.CommandEnv(ei, env)
	if err != nil {
		return nil, xerrors.Errorf("prepare command env: %w", err)
	}

	// OpenSSH executes all commands with the users current shell.
	// We replicate that behavior for IDE support.
	caller := "-c"
	if runtime.GOOS == "windows" {
		caller = "/c"
	}
	name := shell
	args := []string{caller, script}

	// A preceding space is generally not idiomatic for a shebang,
	// but in Terraform it's quite standard to use <<EOF for a multi-line
	// string which would indent with spaces, so we accept it for user-ease.
	if strings.HasPrefix(strings.TrimSpace(script), "#!") {
		// If the script starts with a shebang, we should
		// execute it directly. This is useful for running
		// scripts that aren't executable.
		shebang := strings.SplitN(strings.TrimSpace(script), "\n", 2)[0]
		shebang = strings.TrimSpace(shebang)
		shebang = strings.TrimPrefix(shebang, "#!")
		words, err := shellquote.Split(shebang)
		if err != nil {
			return nil, xerrors.Errorf("split shebang: %w", err)
		}
		name = words[0]
		if len(words) > 1 {
			args = words[1:]
		} else {
			args = []string{}
		}
		args = append(args, caller, script)
	}

	// gliderlabs/ssh returns a command slice of zero
	// when a shell is requested.
	if len(script) == 0 {
		args = []string{}
		if runtime.GOOS != "windows" {
			// On Linux and macOS, we should start a login
			// shell to consume juicy environment variables!
			args = append(args, "-l")
		}
	}

	// Modify command prior to execution. This will usually be a no-op, but not
	// always. For example, to run a command in a Docker container, we need to
	// modify the command to be `docker exec -it <container> <command>`.
	modifiedName, modifiedArgs := ei.ModifyCommand(name, args...)
	// Log if the command was modified.
	if modifiedName != name && slices.Compare(modifiedArgs, args) != 0 {
		s.logger.Debug(ctx, "modified command",
			slog.F("before", append([]string{name}, args...)),
			slog.F("after", append([]string{modifiedName}, modifiedArgs...)),
		)
	}
	cmd := s.Execer.PTYCommandContext(ctx, modifiedName, modifiedArgs...)
	cmd.Dir = dir
	cmd.Env = env

	// Set SSH connection environment variables (these are also set by OpenSSH
	// and thus expected to be present by SSH clients). Since the agent does
	// networking in-memory, trying to provide accurate values here would be
	// nonsensical. For now, we hard code these values so that they're present.
	srcAddr, srcPort := "0.0.0.0", "0"
	dstAddr, dstPort := "0.0.0.0", "0"
	cmd.Env = append(cmd.Env, fmt.Sprintf("SSH_CLIENT=%s %s %s", srcAddr, srcPort, dstPort))
	cmd.Env = append(cmd.Env, fmt.Sprintf("SSH_CONNECTION=%s %s %s %s", srcAddr, srcPort, dstAddr, dstPort))

	return cmd, nil
}

// Serve starts the server to handle incoming connections on the provided listener.
// It returns an error if no host keys are set or if there is an issue accepting connections.
func (s *Server) Serve(l net.Listener) (retErr error) {
	// Ensure we're not mutating HostSigners as we're reading it.
	s.mu.RLock()
	noHostKeys := len(s.srv.HostSigners) == 0
	s.mu.RUnlock()

	if noHostKeys {
		return xerrors.New("no host keys set")
	}

	s.logger.Info(context.Background(), "started serving listener", slog.F("listen_addr", l.Addr()))
	defer func() {
		s.logger.Info(context.Background(), "stopped serving listener",
			slog.F("listen_addr", l.Addr()), slog.Error(retErr))
	}()
	defer l.Close()

	s.trackListener(l, true)
	defer s.trackListener(l, false)
	for {
		conn, err := l.Accept()
		if err != nil {
			return err
		}
		go s.handleConn(l, conn)
	}
}

func (s *Server) handleConn(l net.Listener, c net.Conn) {
	logger := s.logger.With(
		slog.F("remote_addr", c.RemoteAddr()),
		slog.F("local_addr", c.LocalAddr()),
		slog.F("listen_addr", l.Addr()))
	defer c.Close()

	if !s.trackConn(l, c, true) {
		// Server is closed or we no longer want
		// connections from this listener.
		logger.Info(context.Background(), "received connection after server closed")
		return
	}
	defer s.trackConn(l, c, false)
	logger.Info(context.Background(), "started serving ssh connection")
	// note: srv.ConnectionCompleteCallback logs completion of the connection
	s.srv.HandleConn(c)
}

// trackListener registers the listener with the server. If the server is
// closing, the function will block until the server is closed.
//
//nolint:revive
func (s *Server) trackListener(l net.Listener, add bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if add {
		for s.closing != nil {
			closing := s.closing
			// Wait until close is complete before
			// serving a new listener.
			s.mu.Unlock()
			<-closing
			s.mu.Lock()
		}
		s.wg.Add(1)
		s.listeners[l] = struct{}{}
		return
	}
	s.wg.Done()
	delete(s.listeners, l)
}

// trackConn registers the connection with the server. If the server is
// closed or the listener is closed, the connection is not registered
// and should be closed.
//
//nolint:revive
func (s *Server) trackConn(l net.Listener, c net.Conn, add bool) (ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if add {
		found := false
		for ll := range s.listeners {
			if l == ll {
				found = true
				break
			}
		}
		if s.closing != nil || !found {
			// Server or listener closed.
			return false
		}
		s.wg.Add(1)
		s.conns[c] = struct{}{}
		return true
	}
	s.wg.Done()
	delete(s.conns, c)
	return true
}

// trackSession registers the session with the server. If the server is
// closing, the session is not registered and should be closed.
//
//nolint:revive
func (s *Server) trackSession(ss ssh.Session, add bool) (ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if add {
		if s.closing != nil {
			// Server closed.
			return false
		}
		s.wg.Add(1)
		s.sessions[ss] = struct{}{}
		return true
	}
	s.wg.Done()
	delete(s.sessions, ss)
	return true
}

// trackCommand registers the process with the server. If the server is
// closing, the process is not registered and should be closed.
//
//nolint:revive
func (s *Server) trackProcess(p *os.Process, add bool) (ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if add {
		if s.closing != nil {
			// Server closed.
			return false
		}
		s.wg.Add(1)
		s.processes[p] = struct{}{}
		return true
	}
	s.wg.Done()
	delete(s.processes, p)
	return true
}

// Close the server and all active connections. Server can be re-used
// after Close is done.
func (s *Server) Close() error {
	s.mu.Lock()

	// Guard against multiple calls to Close and
	// accepting new connections during close.
	if s.closing != nil {
		closing := s.closing
		s.mu.Unlock()
		<-closing
		return xerrors.New("server is closed")
	}
	s.closing = make(chan struct{})

	ctx := context.Background()

	s.logger.Debug(ctx, "closing server")

	// Stop accepting new connections.
	s.logger.Debug(ctx, "closing all active listeners", slog.F("count", len(s.listeners)))
	for l := range s.listeners {
		_ = l.Close()
	}

	// Close all active sessions to gracefully
	// terminate client connections.
	s.logger.Debug(ctx, "closing all active sessions", slog.F("count", len(s.sessions)))
	for ss := range s.sessions {
		// We call Close on the underlying channel here because we don't
		// want to send an exit status to the client (via Exit()).
		// Typically OpenSSH clients will return 255 as the exit status.
		_ = ss.Close()
	}
	s.logger.Debug(ctx, "closing all active connections", slog.F("count", len(s.conns)))
	for c := range s.conns {
		_ = c.Close()
	}

	for p := range s.processes {
		_ = cmdCancel(s.logger, p)
	}

	s.logger.Debug(ctx, "closing SSH server")
	err := s.srv.Close()

	s.mu.Unlock()

	s.logger.Debug(ctx, "closing X11 forwarding")
	_ = s.x11Forwarder.Close()

	s.logger.Debug(ctx, "waiting for all goroutines to exit")
	s.wg.Wait() // Wait for all goroutines to exit.

	s.mu.Lock()
	close(s.closing)
	s.closing = nil
	s.mu.Unlock()

	s.logger.Debug(ctx, "closing server done")

	return err
}

// Shutdown stops accepting new connections. The current implementation
// calls Close() for simplicity instead of waiting for existing
// connections to close. If the context times out, Shutdown will return
// but Close() may not have completed.
func (s *Server) Shutdown(ctx context.Context) error {
	ch := make(chan error, 1)
	go func() {
		// TODO(mafredri): Implement shutdown, SIGHUP running commands, etc.
		// For now we just close the server.
		ch <- s.Close()
	}()
	var err error
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case err = <-ch:
	}
	// Re-check for context cancellation precedence.
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		return xerrors.Errorf("close server: %w", err)
	}
	return nil
}

func isLoginShell(rawCommand string) bool {
	return len(rawCommand) == 0
}

// isQuietLogin checks if the SSH server should perform a quiet login or not.
//
// https://github.com/openssh/openssh-portable/blob/25bd659cc72268f2858c5415740c442ee950049f/session.c#L816
func isQuietLogin(fs afero.Fs, rawCommand string) bool {
	// We are always quiet unless this is a login shell.
	if !isLoginShell(rawCommand) {
		return true
	}

	// Best effort, if we can't get the home directory,
	// we can't lookup .hushlogin.
	homedir, err := userHomeDir()
	if err != nil {
		return false
	}

	_, err = fs.Stat(filepath.Join(homedir, ".hushlogin"))
	return err == nil
}

// showAnnouncementBanner will write the service banner if enabled and not blank
// along with a blank line for spacing.
func showAnnouncementBanner(session io.Writer, banner codersdk.BannerConfig) error {
	if banner.Enabled && banner.Message != "" {
		// The banner supports Markdown so we might want to parse it but Markdown is
		// still fairly readable in its raw form.
		message := strings.TrimSpace(banner.Message) + "\n\n"
		return writeWithCarriageReturn(strings.NewReader(message), session)
	}
	return nil
}

// showMOTD will output the message of the day from
// the given filename to dest, if the file exists.
//
// https://github.com/openssh/openssh-portable/blob/25bd659cc72268f2858c5415740c442ee950049f/session.c#L784
func showMOTD(fs afero.Fs, dest io.Writer, filename string) error {
	if filename == "" {
		return nil
	}

	f, err := fs.Open(filename)
	if err != nil {
		if xerrors.Is(err, os.ErrNotExist) {
			// This is not an error, there simply isn't a MOTD to show.
			return nil
		}
		return xerrors.Errorf("open MOTD: %w", err)
	}
	defer f.Close()

	return writeWithCarriageReturn(f, dest)
}

// writeWithCarriageReturn writes each line with a carriage return to ensure
// that each line starts at the beginning of the terminal.
func writeWithCarriageReturn(src io.Reader, dest io.Writer) error {
	s := bufio.NewScanner(src)
	for s.Scan() {
		_, err := fmt.Fprint(dest, s.Text()+"\r\n")
		if err != nil {
			return xerrors.Errorf("write line: %w", err)
		}
	}
	if err := s.Err(); err != nil {
		return xerrors.Errorf("read line: %w", err)
	}
	return nil
}

// userHomeDir returns the home directory of the current user, giving
// priority to the $HOME environment variable.
func userHomeDir() (string, error) {
	// First we check the environment.
	homedir, err := os.UserHomeDir()
	if err == nil {
		return homedir, nil
	}

	// As a fallback, we try the user information.
	u, err := user.Current()
	if err != nil {
		return "", xerrors.Errorf("current user: %w", err)
	}
	return u.HomeDir, nil
}

// UpdateHostSigner updates the host signer with a new key generated from the provided seed.
// If an existing host key exists with the same algorithm, it is overwritten
func (s *Server) UpdateHostSigner(seed int64) error {
	key, err := CoderSigner(seed)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.srv.AddHostKey(key)

	return nil
}

// CoderSigner generates a deterministic SSH signer based on the provided seed.
// It uses RSA with a key size of 2048 bits.
func CoderSigner(seed int64) (gossh.Signer, error) {
	// Clients should ignore the host key when connecting.
	// The agent needs to authenticate with coderd to SSH,
	// so SSH authentication doesn't improve security.
	coderHostKey := agentrsa.GenerateDeterministicKey(seed)

	coderSigner, err := gossh.NewSignerFromKey(coderHostKey)
	return coderSigner, err
}
