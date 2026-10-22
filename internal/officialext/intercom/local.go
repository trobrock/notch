package intercom

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/trobrock/notch/internal/workspace"
)

type peerRecord struct {
	Version   int       `json:"version"`
	Project   string    `json:"project"`
	SessionID string    `json:"session_id"`
	Name      string    `json:"name"`
	Mode      string    `json:"mode,omitempty"`
	Endpoint  string    `json:"endpoint"`
	Token     string    `json:"token"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
}

func runtimeRoot(cwd string) (string, string, error) {
	canonical, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		canonical, err = filepath.Abs(cwd)
	}
	if err != nil {
		return "", "", fmt.Errorf("resolve project directory: %w", err)
	}
	// Repository identity is shared by linked Git worktrees. Separate clones
	// stay separate; a future remote route must explicitly map project identity.
	info, err := workspace.Resolve(canonical)
	if err != nil {
		return "", "", err
	}
	hash := sha256.Sum256([]byte(filepath.Clean(info.TrustKey)))
	project := hex.EncodeToString(hash[:8])
	base := os.Getenv("NOTCH_INTERCOM_DIR")
	if base == "" {
		if runtime := os.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(runtime) {
			base = filepath.Join(runtime, "notch-intercom")
		} else if cache, cacheErr := os.UserCacheDir(); cacheErr == nil {
			base = filepath.Join(cache, "notch", "intercom")
		} else {
			return "", "", errors.New("cannot find a private directory for intercom")
		}
	}
	if !filepath.IsAbs(base) {
		return "", "", errors.New("NOTCH_INTERCOM_DIR must be absolute")
	}
	root := filepath.Join(base, project)
	return root, project, nil
}

func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	if err := validateDirectoryAncestors(filepath.Dir(path), 0); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 || !directoryOwnedByUser(info) {
		return fmt.Errorf("intercom directory %q must be a private directory owned by the current user (mode 0700, not a symlink)", path)
	}
	return nil
}

// Follow symlink targets explicitly instead of checking only EvalSymlinks'
// final result: an intermediate target can itself be controlled by another user.
func validateDirectoryAncestors(path string, links int) error {
	if links > 40 {
		return errors.New("too many intercom directory symlinks")
	}
	for {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !safeDirectoryAncestor(info) {
			return fmt.Errorf("intercom directory has an unsafe ancestor %q", path)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			// Lexically cleaning '..' across another symlink can validate a
			// different directory than the kernel traverses. Refuse that alias
			// rather than implementing a second filesystem path resolver.
			for _, component := range strings.Split(target, string(filepath.Separator)) {
				if component == ".." {
					return fmt.Errorf("intercom ancestor symlink %q contains an unsupported parent traversal; use a canonical runtime path", path)
				}
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(path), target)
			}
			if err := validateDirectoryAncestors(filepath.Clean(target), links+1); err != nil {
				return err
			}
		}
		parent := filepath.Dir(path)
		if parent == path {
			return nil
		}
		path = parent
	}
}

func (s *service) openEndpoint() error {
	s.mu.Lock()
	root, project, sessionID, name, mode := s.root, s.project, s.sessionID, s.name, s.mode
	s.mu.Unlock()
	stem := safeStem(sessionID)
	if root == "" {
		return errors.New("intercom project is not configured")
	}
	// Validate the runtime base before publishing project-owned sockets. Do
	// not chmod an existing shared or attacker-owned directory into seeming safe.
	if err := ensurePrivateDirectory(filepath.Dir(root)); err != nil {
		return err
	}
	if err := ensurePrivateDirectory(root); err != nil {
		return err
	}

	endpoint := filepath.Join(root, stem+".sock")
	recordPath := filepath.Join(root, stem+".json")
	ownerLock, err := acquireOwnerLock(endpoint + ".lock")
	if err != nil {
		return fmt.Errorf("intercom session %q is active or starting: %w", sessionID, err)
	}
	keepLock := false
	defer func() {
		if !keepLock {
			releaseOwnerLock(ownerLock)
		}
	}()
	listener, err := net.Listen("unix", endpoint)
	if err != nil {
		// The process lock proves no live or starting owner can use this path.
		// Unix leaves socket files after a crash, so replace that stale path.
		if removeErr := os.Remove(endpoint); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return fmt.Errorf("remove stale intercom endpoint: %w", removeErr)
		}
		listener, err = net.Listen("unix", endpoint)
	}
	if err != nil {
		return fmt.Errorf("listen for intercom: %w", err)
	}
	_ = os.Remove(recordPath)
	if err := os.Chmod(endpoint, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(endpoint)
		return fmt.Errorf("protect intercom endpoint: %w", err)
	}
	token, err := randomID()
	if err != nil {
		_ = listener.Close()
		_ = os.Remove(endpoint)
		return err
	}
	record := peerRecord{Version: protocolVersion, Project: project, SessionID: sessionID, Name: name, Mode: mode, Endpoint: endpoint, Token: token, PID: os.Getpid(), StartedAt: time.Now().UTC()}
	if err := writeRecord(recordPath, record); err != nil {
		_ = listener.Close()
		_ = os.Remove(endpoint)
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.listener, s.endpoint, s.record, s.token, s.ownerLock, s.cancel = listener, endpoint, recordPath, token, ownerLock, cancel
	s.mu.Unlock()
	keepLock = true
	go s.acceptLoop(ctx, listener)
	return nil
}

func writeRecord(path string, record peerRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	tmp := path + fmt.Sprintf(".%d.tmp", os.Getpid())
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write intercom identity: %w", err)
	}
	_ = os.Chmod(tmp, 0o600)
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("publish intercom identity: %w", err)
	}
	return nil
}

func (s *service) closeEndpoint() {
	s.mu.Lock()
	listener, cancel, endpoint, record, token, ownerLock := s.listener, s.cancel, s.endpoint, s.record, s.token, s.ownerLock
	s.listener, s.cancel, s.endpoint, s.record, s.token, s.ownerLock = nil, nil, "", "", "", nil
	s.generation++
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if listener != nil {
		_ = listener.Close()
	}
	owned := false
	if data, err := os.ReadFile(record); err == nil {
		var current peerRecord
		owned = json.Unmarshal(data, &current) == nil && token != "" && current.Token == token
	}
	// Remove the record while our socket path still blocks replacement. Once
	// the socket is unlinked, a successor may safely publish its own record.
	if owned && record != "" {
		_ = os.Remove(record)
	}
	if owned && endpoint != "" {
		_ = os.Remove(endpoint)
	}
	releaseOwnerLock(ownerLock)
}

func (s *service) acceptLoop(ctx context.Context, listener net.Listener) {
	slots := make(chan struct{}, 16)
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		select {
		case slots <- struct{}{}:
			go func() { defer func() { <-slots }(); s.handleConn(ctx, conn) }()
		default:
			_ = conn.Close()
		}
	}
}

func (s *service) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	line, err := readBoundedLine(conn)
	if err != nil {
		_ = json.NewEncoder(conn).Encode(response{Version: protocolVersion, Error: err.Error()})
		return
	}
	var request message
	if err := json.Unmarshal(line, &request); err != nil {
		_ = json.NewEncoder(conn).Encode(response{Version: protocolVersion, Error: "malformed message: " + err.Error()})
		return
	}
	var result response
	if request.Type == "ping" {
		s.mu.Lock()
		matches := request.Project == s.project && request.ToSession == s.sessionID
		s.mu.Unlock()
		if !matches {
			result = response{Version: protocolVersion, ID: request.ID, Error: "intercom endpoint identity mismatch"}
		} else {
			result = response{Version: protocolVersion, ID: request.ID, Delivered: true}
		}
	} else {
		result = s.receive(ctx, request)
	}
	_ = json.NewEncoder(conn).Encode(result)
}

func (s *service) peers(ctx context.Context) ([]peerRecord, error) {
	s.mu.Lock()
	root, project, own, ready := s.root, s.project, s.sessionID, s.listener != nil
	s.mu.Unlock()
	if root == "" || !ready {
		return nil, errors.New("intercom session is not ready")
	}
	items, err := filepath.Glob(filepath.Join(root, "*.json"))
	if err != nil {
		return nil, err
	}
	peers := make([]peerRecord, 0, len(items))
	for _, path := range items {
		data, readErr := os.ReadFile(path)
		var peer peerRecord
		if readErr != nil || json.Unmarshal(data, &peer) != nil || peer.Version != protocolVersion || peer.Project != project || peer.SessionID == "" || peer.Endpoint == "" {
			continue
		}
		expectedRecord := filepath.Join(root, safeStem(peer.SessionID)+".json")
		expectedEndpoint := filepath.Join(root, safeStem(peer.SessionID)+".sock")
		if filepath.Clean(path) != expectedRecord || filepath.Clean(peer.Endpoint) != expectedEndpoint {
			continue
		}
		if peer.SessionID == own {
			continue
		}
		if _, statErr := os.Stat(peer.Endpoint); statErr != nil {
			continue
		}
		pingCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
		result, pingErr := exchange(pingCtx, peer.Endpoint, message{Version: protocolVersion, Type: "ping", ID: "ping", Project: project, ToSession: peer.SessionID})
		cancel()
		if pingErr != nil || result.Error != "" {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			// A timeout can be a busy live peer. Stale files are ignored here
			// and replaced safely by the next owner during endpoint startup.
			continue
		}
		peers = append(peers, peer)
	}
	sort.Slice(peers, func(i, j int) bool {
		if peers[i].Name == peers[j].Name {
			return peers[i].SessionID < peers[j].SessionID
		}
		return peers[i].Name < peers[j].Name
	})
	return peers, nil
}

func exchange(ctx context.Context, endpoint string, request message) (response, error) {
	dialer := net.Dialer{Timeout: time.Second}
	conn, err := dialer.DialContext(ctx, "unix", endpoint)
	if err != nil {
		return response{}, err
	}
	defer conn.Close()
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancel()
	deadline := time.Now().Add(3 * time.Second)
	if value, ok := ctx.Deadline(); ok && value.Before(deadline) {
		deadline = value
	}
	_ = conn.SetDeadline(deadline)
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return response{}, err
	}
	line, err := readBoundedLine(conn)
	if err != nil {
		return response{}, err
	}
	var result response
	if err := json.Unmarshal(line, &result); err != nil {
		return response{}, fmt.Errorf("malformed delivery response: %w", err)
	}
	if result.Version != protocolVersion {
		return response{}, fmt.Errorf("unsupported response version %d", result.Version)
	}
	if result.ID != request.ID {
		return response{}, fmt.Errorf("delivery response ID %q does not match request %q", result.ID, request.ID)
	}
	return result, nil
}

func safeStem(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:12])
}
