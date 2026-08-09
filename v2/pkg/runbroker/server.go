package runbroker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// PeerCredentials is the kernel-captured Unix peer identity. It is attached
// to the handler context for audit and optional UID/GID policy checks.
type PeerCredentials struct {
	PID int32
	UID uint32
	GID uint32
}

// PeerCredentialsFromContext reads the identity captured with SO_PEERCRED.
func PeerCredentialsFromContext(ctx context.Context) (PeerCredentials, bool) {
	if ctx == nil {
		return PeerCredentials{}, false
	}
	credentials, ok := ctx.Value(contextPeerCredentialsKey{}).(PeerCredentials)
	return credentials, ok
}

// SessionFromContext returns the session that authorized the current request.
func SessionFromContext(ctx context.Context) (Session, bool) {
	if ctx == nil {
		return Session{}, false
	}
	session, ok := ctx.Value(contextSessionKey{}).(Session)
	return session, ok
}

// UnixServer owns one private Unix listener and composes the supplied handler
// only after session, bearer, policy, model, and peer authorization succeed.
type UnixServer struct {
	manager    *Manager
	config     UnixServerConfig
	listener   net.Listener
	httpServer *http.Server
	lockFile   *os.File
	socketInfo os.FileInfo
	lockPath   string
	lockInfo   os.FileInfo

	serveOnce sync.Once
	serveErr  error
	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
}

// NewUnixServer creates and binds a per-run Unix HTTP socket. The parent
// directory must already exist, be owned by the current user, have no group or
// other permission bits, and contain no symlink components. A sibling lock
// file is held for the server lifetime to prevent two owners from racing over
// stale socket cleanup.
func NewUnixServer(config UnixServerConfig) (*UnixServer, error) {
	if config.Sessions == nil || config.Handler == nil {
		return nil, fmt.Errorf("%w: sessions and handler are required", ErrInvalidInput)
	}
	if err := validateSessionID(config.SessionID); err != nil {
		return nil, err
	}
	if err := validateBinding(config.Binding); err != nil {
		return nil, err
	}
	if err := validatePolicy(config.PolicyDigest); err != nil {
		return nil, err
	}
	if err := validateSocketPath(config.SocketPath); err != nil {
		return nil, err
	}
	if err := validatePeerPolicy(config.Peer); err != nil {
		return nil, err
	}
	config.SocketPath = filepath.Clean(config.SocketPath)
	config.MaxHeaderBytes = defaultInt(config.MaxHeaderBytes, defaultMaxHeaderBytes)
	config.MaxBodyBytes = defaultInt64(config.MaxBodyBytes, defaultMaxBodyBytes)
	config.ReadHeaderTimeout = defaultDuration(config.ReadHeaderTimeout, defaultReadHeader)
	config.ReadTimeout = defaultDuration(config.ReadTimeout, defaultReadTimeout)
	config.WriteTimeout = defaultDuration(config.WriteTimeout, defaultWriteTimeout)
	config.IdleTimeout = defaultDuration(config.IdleTimeout, defaultIdleTimeout)
	config.ShutdownTimeout = defaultDuration(config.ShutdownTimeout, defaultShutdownTimeout)
	if config.MaxHeaderBytes < 1024 || config.MaxHeaderBytes > 16<<20 || config.MaxBodyBytes <= 0 || config.MaxBodyBytes > 1<<30 {
		return nil, fmt.Errorf("%w: limits are outside supported range", ErrInvalidInput)
	}
	if config.ReadHeaderTimeout <= 0 || config.ReadTimeout <= 0 || config.WriteTimeout < 0 || config.IdleTimeout <= 0 || config.ShutdownTimeout <= 0 {
		return nil, fmt.Errorf("%w: server timeouts are outside the supported range", ErrInvalidInput)
	}

	lockPath := config.SocketPath + ".lock"
	lockFile, err := openSocketLock(lockPath)
	if err != nil {
		return nil, err
	}
	cleanupLock := func() {
		_ = unix.Flock(int(lockFile.Fd()), unix.LOCK_UN)
		_ = lockFile.Close()
	}
	if err := removeStaleSocket(config.SocketPath); err != nil {
		cleanupLock()
		return nil, err
	}
	lockInfo, err := os.Lstat(lockPath)
	if err != nil || lockInfo.Mode()&os.ModeSymlink != 0 || !lockInfo.Mode().IsRegular() {
		cleanupLock()
		if err == nil {
			err = ErrSocketUnsafe
		}
		return nil, fmt.Errorf("verify Unix socket lock: %w", err)
	}
	listener, err := net.Listen("unix", config.SocketPath)
	if err != nil {
		cleanupLock()
		if errors.Is(err, os.ErrExist) {
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("listen on Unix socket: %w", err)
	}
	if err := os.Chmod(config.SocketPath, 0600); err != nil {
		_ = listener.Close()
		_ = os.Remove(config.SocketPath)
		cleanupLock()
		return nil, fmt.Errorf("set Unix socket permissions: %w", err)
	}
	socketInfo, err := os.Lstat(config.SocketPath)
	if err != nil || socketInfo.Mode()&os.ModeSymlink != 0 || socketInfo.Mode()&os.ModeSocket == 0 {
		_ = listener.Close()
		_ = os.Remove(config.SocketPath)
		cleanupLock()
		if err == nil {
			err = ErrNotUnixSocket
		}
		return nil, fmt.Errorf("verify Unix socket: %w", err)
	}

	server := &UnixServer{
		manager:    config.Sessions,
		config:     config,
		listener:   listener,
		lockFile:   lockFile,
		socketInfo: socketInfo,
		lockPath:   lockPath,
		lockInfo:   lockInfo,
		closeDone:  make(chan struct{}),
	}
	server.httpServer = &http.Server{
		Handler:           server.authorizedHandler(),
		MaxHeaderBytes:    config.MaxHeaderBytes,
		ReadHeaderTimeout: config.ReadHeaderTimeout,
		ReadTimeout:       config.ReadTimeout,
		WriteTimeout:      config.WriteTimeout,
		IdleTimeout:       config.IdleTimeout,
		ConnContext: func(ctx context.Context, connection net.Conn) context.Context {
			credentials, captureErr := capturePeerCredentials(connection)
			if captureErr != nil {
				return withPeerError(ctx, captureErr)
			}
			return withPeerCredentials(ctx, credentials)
		},
	}
	return server, nil
}

// SocketPath returns the bound socket path.
func (s *UnixServer) SocketPath() string { return s.config.SocketPath }

// Serve runs the server until ctx is canceled or Close is called. Only one
// Serve call is allowed. Context cancellation triggers bounded graceful
// shutdown followed by forceful connection close if needed.
func (s *UnixServer) Serve(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if s.isClosed() {
		return ErrClosed
	}
	started := false
	s.serveOnce.Do(func() {
		started = true
		serveContext, cancel := context.WithCancel(ctx)
		go func() {
			<-serveContext.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), s.config.ShutdownTimeout)
			defer cancel()
			_ = s.Close(shutdownCtx)
		}()
		err := s.httpServer.Serve(s.listener)
		if errors.Is(err, http.ErrServerClosed) {
			s.serveErr = nil
		} else {
			s.serveErr = err
		}
		if !s.isClosed() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), s.config.ShutdownTimeout)
			_ = s.Close(shutdownCtx)
			cancel()
		}
		cancel()
	})
	if !started {
		return ErrAlreadyServing
	}
	return s.serveErr
}

// Close gracefully stops the server and removes only the exact socket inode
// this instance created. It is safe for concurrent callers and repeated use.
func (s *UnixServer) Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.closeOnce.Do(func() {
		shutdownErr := s.httpServer.Shutdown(ctx)
		_ = s.listener.Close()
		if shutdownErr != nil {
			_ = s.httpServer.Close()
		}
		cleanupErr := s.cleanupSocket()
		lockCleanupErr := s.cleanupLock()
		unlockErr := unix.Flock(int(s.lockFile.Fd()), unix.LOCK_UN)
		closeFileErr := s.lockFile.Close()
		if shutdownErr != nil {
			s.closeErr = shutdownErr
		} else if cleanupErr != nil {
			s.closeErr = cleanupErr
		} else if lockCleanupErr != nil {
			s.closeErr = lockCleanupErr
		} else if unlockErr != nil {
			s.closeErr = unlockErr
		} else {
			s.closeErr = closeFileErr
		}
		close(s.closeDone)
	})
	<-s.closeDone
	return s.closeErr
}

func (s *UnixServer) isClosed() bool {
	select {
	case <-s.closeDone:
		return true
	default:
		return false
	}
}

func (s *UnixServer) cleanupSocket() error {
	info, err := os.Lstat(s.config.SocketPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect Unix socket during cleanup: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 {
		return ErrNotUnixSocket
	}
	if !os.SameFile(s.socketInfo, info) {
		return nil
	}
	if err := os.Remove(s.config.SocketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove Unix socket: %w", err)
	}
	return nil
}

func (s *UnixServer) cleanupLock() error {
	info, err := os.Lstat(s.lockPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect Unix socket lock during cleanup: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return ErrSocketUnsafe
	}
	if !os.SameFile(s.lockInfo, info) {
		return nil
	}
	if err := os.Remove(s.lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove Unix socket lock: %w", err)
	}
	return nil
}

func (s *UnixServer) authorizedHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		credentials, ok := PeerCredentialsFromContext(request.Context())
		if !ok {
			writeUnauthorized(writer, http.StatusForbidden)
			return
		}
		if peerErr, exists := request.Context().Value(contextPeerErrorKey{}).(error); exists || peerErr != nil {
			writeUnauthorized(writer, http.StatusForbidden)
			return
		}
		if !s.config.Peer.allows(credentials) {
			writeUnauthorized(writer, http.StatusForbidden)
			return
		}
		sessionHeader, sessionHeaderOK := singleHeader(request.Header.Values(HeaderSessionID), maxIdentifierBytes)
		policyHeader, policyHeaderOK := singleHeader(request.Header.Values(HeaderPolicyDigest), maxPolicyBytes)
		if !sessionHeaderOK || !policyHeaderOK || sessionHeader != string(s.config.SessionID) || policyHeader != s.config.PolicyDigest {
			writeUnauthorized(writer, http.StatusUnauthorized)
			return
		}
		token, ok := parseBearerHeader(request.Header.Values(HeaderAuthorization))
		if !ok {
			writeUnauthorized(writer, http.StatusUnauthorized)
			return
		}
		model, modelOK := singleHeader(request.Header.Values(HeaderModel), maxModelBytes)
		if !modelOK {
			writeUnauthorized(writer, http.StatusUnauthorized)
			return
		}
		authorized, err := s.manager.Authorize(request.Context(), AuthorizeRequest{
			SessionID:    s.config.SessionID,
			Token:        BearerToken(token),
			Binding:      s.config.Binding,
			Model:        model,
			PolicyDigest: s.config.PolicyDigest,
		})
		if err != nil {
			writeUnauthorized(writer, http.StatusUnauthorized)
			return
		}
		if request.ContentLength > s.config.MaxBodyBytes {
			writeUnauthorized(writer, http.StatusRequestEntityTooLarge)
			return
		}
		request.Body = http.MaxBytesReader(writer, request.Body, s.config.MaxBodyBytes)
		ctx := context.WithValue(request.Context(), contextSessionKey{}, authorized)
		s.config.Handler.ServeHTTP(writer, request.WithContext(ctx))
	})
}

func parseBearerHeader(values []string) (string, bool) {
	if len(values) != 1 || len(values[0]) > maxTokenBytes+16 {
		return "", false
	}
	value := values[0]
	space := strings.IndexByte(value, ' ')
	if space <= 0 || !strings.EqualFold(value[:space], "Bearer") || space+1 >= len(value) || value[space+1] == ' ' {
		return "", false
	}
	token := value[space+1:]
	if strings.IndexAny(token, " \t\r\n") >= 0 {
		return "", false
	}
	return token, true
}

func singleHeader(values []string, maxBytes int) (string, bool) {
	if len(values) != 1 || values[0] == "" || len(values[0]) > maxBytes {
		return "", false
	}
	return values[0], true
}

func writeUnauthorized(writer http.ResponseWriter, status int) {
	writer.Header().Set("Cache-Control", "no-store")
	http.Error(writer, http.StatusText(status), status)
}

func (policy PeerPolicy) allows(credentials PeerCredentials) bool {
	return (policy.UID == nil || *policy.UID == credentials.UID) && (policy.GID == nil || *policy.GID == credentials.GID)
}

func validatePeerPolicy(policy PeerPolicy) error {
	return nil
}

func defaultInt(value, fallback int) int {
	if value == 0 {
		return fallback
	}
	return value
}

func defaultInt64(value, fallback int64) int64 {
	if value == 0 {
		return fallback
	}
	return value
}

func defaultDuration(value, fallback time.Duration) time.Duration {
	if value == 0 {
		return fallback
	}
	return value
}

func validateSocketPath(path string) error {
	if path == "" || !filepath.IsAbs(path) || strings.IndexByte(path, 0) >= 0 {
		return fmt.Errorf("%w: socket path must be absolute", ErrInvalidInput)
	}
	clean := filepath.Clean(path)
	if clean == string(filepath.Separator) || clean != path || len([]byte(clean)) >= 108 {
		return fmt.Errorf("%w: socket path is not canonical or exceeds Unix socket limits", ErrInvalidInput)
	}
	parent := filepath.Dir(clean)
	if err := verifyPrivateDirectory(parent); err != nil {
		return err
	}
	return nil
}

func verifyPrivateDirectory(path string) error {
	if path == "" || !filepath.IsAbs(path) {
		return fmt.Errorf("%w: socket directory must be absolute", ErrSocketUnsafe)
	}
	current := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("%w: inspect %s: %v", ErrSocketUnsafe, current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: symlink path component %s", ErrSocketUnsafe, current)
		}
		if !info.IsDir() {
			return fmt.Errorf("%w: %s is not a directory", ErrSocketUnsafe, current)
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%w: inspect private directory: %v", ErrSocketUnsafe, err)
	}
	if info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("%w: directory permissions must not grant group/other access", ErrSocketUnsafe)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && uint32(stat.Uid) != uint32(os.Geteuid()) {
		return fmt.Errorf("%w: directory is not owned by the current user", ErrSocketUnsafe)
	}
	return nil
}

func openSocketLock(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("open Unix socket lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("runbroker: create lock file handle")
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("lock Unix socket: %w", err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		_ = unix.Flock(fd, unix.LOCK_UN)
		_ = file.Close()
		return nil, fmt.Errorf("set Unix socket lock permissions: %w", err)
	}
	return file, nil
}

func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect existing Unix socket: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: refusing to remove symlink at socket path", ErrSocketUnsafe)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%w: existing path is not a socket", ErrNotUnixSocket)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale Unix socket: %w", err)
	}
	return nil
}
