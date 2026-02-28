package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	gliderssh "github.com/gliderlabs/ssh"
	"github.com/pkg/sftp"
	gossh "golang.org/x/crypto/ssh"
)

func (s *Service) startSFTPServer() error {
	hostKey, err := ensureSFTPHostKey(s.cfg.SFTP.HostKeyPath)
	if err != nil {
		return err
	}

	addr := fmt.Sprintf("%s:%d", s.cfg.SFTP.Host, s.cfg.SFTP.Port)
	server := &gliderssh.Server{
		Addr: addr,
		PasswordHandler: func(ctx gliderssh.Context, password string) bool {
			authData, err := s.authenticateSFTP(ctx.User(), password)
			if err != nil {
				bootWarn("sftp auth failed username=%s error=%v", ctx.User(), err)
				return false
			}
			s.sftpAuthSessions.Store(ctx.SessionID(), authData)
			return true
		},
		SubsystemHandlers: map[string]gliderssh.SubsystemHandler{
			"sftp": func(sess gliderssh.Session) {
				defer s.sftpAuthSessions.Delete(sess.Context().SessionID())
				value, ok := s.sftpAuthSessions.Load(sess.Context().SessionID())
				if !ok {
					_ = sess.Close()
					return
				}

				authData, ok := value.(*SFTPAuthResponse)
				if !ok {
					_ = sess.Close()
					return
				}

				vfs := newVirtualSFTPFS(s.volumesPath, authData)
				handlers := sftp.Handlers{
					FileGet:  vfs,
					FilePut:  vfs,
					FileCmd:  vfs,
					FileList: vfs,
				}
				reqServer := sftp.NewRequestServer(sess, handlers)
				if err := reqServer.Serve(); err != nil && !errors.Is(err, io.EOF) {
					bootWarn("sftp serve error session=%s error=%v", sess.Context().SessionID(), err)
				}
				_ = reqServer.Close()
			},
		},
	}
	server.AddHostKey(hostKey)

	go func() {
		bootInfo("sftp server listening bind=%s", addr)
		if err := server.ListenAndServe(); err != nil {
			bootFatal("sftp server stopped: %v", err)
		}
	}()

	return nil
}

func (s *Service) authenticateSFTP(username, password string) (*SFTPAuthResponse, error) {
	payload := SFTPAuthRequest{
		ConnectorID: s.cfg.Connector.ID,
		Token:       s.cfg.Connector.Token,
		Username:    strings.TrimSpace(username),
		Password:    password,
	}

	response, err := s.panelPostJSON(panelSFTPAuthPath, payload, int(sftpAuthTimeout.Seconds()))
	if err != nil {
		return nil, err
	}

	raw, _ := json.Marshal(response)
	var auth SFTPAuthResponse
	if err := json.Unmarshal(raw, &auth); err != nil {
		return nil, err
	}
	if !auth.Success {
		if strings.TrimSpace(auth.Error) != "" {
			return nil, errors.New(auth.Error)
		}
		return nil, errors.New("invalid sftp credentials")
	}

	return &auth, nil
}

func ensureSFTPHostKey(path string) (gossh.Signer, error) {
	if raw, err := os.ReadFile(path); err == nil {
		return gossh.ParsePrivateKey(raw)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	pemBlock := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	if err := os.WriteFile(path, pemBlock, 0o600); err != nil {
		return nil, err
	}
	return gossh.ParsePrivateKey(pemBlock)
}

type virtualSFTPFS struct {
	volumesPath string
	allowed     map[string]int // containerId -> serverId
}

func newVirtualSFTPFS(volumesPath string, auth *SFTPAuthResponse) *virtualSFTPFS {
	allowed := map[string]int{}
	for _, server := range auth.Servers {
		containerID := strings.TrimSpace(server.ContainerID)
		if containerID == "" || server.ID <= 0 {
			continue
		}
		allowed[containerID] = server.ID
	}
	return &virtualSFTPFS{volumesPath: volumesPath, allowed: allowed}
}

type resolvedVirtualPath struct {
	IsRoot         bool
	IsContainerDir bool
	ContainerID    string
	ServerID       int
	RealPath       string
}

func (v *virtualSFTPFS) resolve(virtualPath string) (resolvedVirtualPath, error) {
	cleanPath := path.Clean("/" + strings.ReplaceAll(strings.TrimSpace(virtualPath), "\\", "/"))
	if cleanPath == "/" {
		return resolvedVirtualPath{IsRoot: true}, nil
	}

	parts := strings.Split(strings.TrimPrefix(cleanPath, "/"), "/")
	containerID := strings.TrimSpace(parts[0])
	serverID, ok := v.allowed[containerID]
	if !ok {
		return resolvedVirtualPath{}, os.ErrPermission
	}

	serverRoot := filepath.Clean(filepath.Join(v.volumesPath, strconv.Itoa(serverID)))
	if len(parts) == 1 {
		return resolvedVirtualPath{
			ContainerID:    containerID,
			ServerID:       serverID,
			IsContainerDir: true,
			RealPath:       serverRoot,
		}, nil
	}

	rel := filepath.Join(parts[1:]...)
	real := filepath.Clean(filepath.Join(serverRoot, rel))
	if real != serverRoot && !strings.HasPrefix(real, serverRoot+string(filepath.Separator)) {
		return resolvedVirtualPath{}, os.ErrPermission
	}
	return resolvedVirtualPath{ContainerID: containerID, ServerID: serverID, RealPath: real}, nil
}

func (v *virtualSFTPFS) Fileread(req *sftp.Request) (io.ReaderAt, error) {
	resolved, err := v.resolve(req.Filepath)
	if err != nil {
		return nil, err
	}
	if resolved.IsRoot || resolved.IsContainerDir {
		return nil, os.ErrPermission
	}
	return os.Open(resolved.RealPath)
}

func (v *virtualSFTPFS) Filewrite(req *sftp.Request) (io.WriterAt, error) {
	resolved, err := v.resolve(req.Filepath)
	if err != nil {
		return nil, err
	}
	if resolved.IsRoot || resolved.IsContainerDir {
		return nil, os.ErrPermission
	}
	if err := os.MkdirAll(filepath.Dir(resolved.RealPath), 0o755); err != nil {
		return nil, err
	}
	flags := os.O_CREATE | os.O_WRONLY
	if req.Method == "Put" {
		flags |= os.O_TRUNC
	}
	return os.OpenFile(resolved.RealPath, flags, 0o644)
}

func (v *virtualSFTPFS) OpenFile(req *sftp.Request) (sftp.WriterAtReaderAt, error) {
	resolved, err := v.resolve(req.Filepath)
	if err != nil {
		return nil, err
	}
	if resolved.IsRoot || resolved.IsContainerDir {
		return nil, os.ErrPermission
	}
	if err := os.MkdirAll(filepath.Dir(resolved.RealPath), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(resolved.RealPath, os.O_CREATE|os.O_RDWR, 0o644)
}

func (v *virtualSFTPFS) Filecmd(req *sftp.Request) error {
	switch req.Method {
	case "Rename":
		source, err := v.resolve(req.Filepath)
		if err != nil {
			return err
		}
		target, err := v.resolve(req.Target)
		if err != nil {
			return err
		}
		if source.IsRoot || source.IsContainerDir || target.IsRoot || target.IsContainerDir {
			return os.ErrPermission
		}
		if source.ContainerID != target.ContainerID {
			return os.ErrPermission
		}
		return os.Rename(source.RealPath, target.RealPath)
	case "Remove":
		resolved, err := v.resolve(req.Filepath)
		if err != nil {
			return err
		}
		if resolved.IsRoot || resolved.IsContainerDir {
			return os.ErrPermission
		}
		return os.RemoveAll(resolved.RealPath)
	case "Rmdir":
		resolved, err := v.resolve(req.Filepath)
		if err != nil {
			return err
		}
		if resolved.IsRoot || resolved.IsContainerDir {
			return os.ErrPermission
		}
		return os.Remove(resolved.RealPath)
	case "Mkdir":
		resolved, err := v.resolve(req.Filepath)
		if err != nil {
			return err
		}
		if resolved.IsRoot {
			return os.ErrPermission
		}
		return os.MkdirAll(resolved.RealPath, 0o755)
	case "Setstat":
		resolved, err := v.resolve(req.Filepath)
		if err != nil {
			return err
		}
		if resolved.IsRoot || resolved.IsContainerDir {
			return os.ErrPermission
		}
		// Kept intentionally permissive and minimal for broad client compatibility.
		return nil
	default:
		return errors.New("unsupported command")
	}
}

func (v *virtualSFTPFS) Filelist(req *sftp.Request) (sftp.ListerAt, error) {
	switch req.Method {
	case "List":
		resolved, err := v.resolve(req.Filepath)
		if err != nil {
			return nil, err
		}
		if resolved.IsRoot {
			keys := make([]string, 0, len(v.allowed))
			for containerID := range v.allowed {
				keys = append(keys, containerID)
			}
			sort.Strings(keys)
			items := make([]os.FileInfo, 0, len(keys))
			for _, key := range keys {
				items = append(items, virtualDirectoryInfo{name: key})
			}
			return sftpListerAt(items), nil
		}
		entries, err := os.ReadDir(resolved.RealPath)
		if err != nil {
			return nil, err
		}
		items := make([]os.FileInfo, 0, len(entries))
		for _, entry := range entries {
			info, err := entry.Info()
			if err != nil {
				continue
			}
			items = append(items, info)
		}
		return sftpListerAt(items), nil
	case "Stat":
		resolved, err := v.resolve(req.Filepath)
		if err != nil {
			return nil, err
		}
		if resolved.IsRoot {
			return sftpListerAt([]os.FileInfo{virtualDirectoryInfo{name: "/"}}), nil
		}
		if resolved.IsContainerDir {
			return sftpListerAt([]os.FileInfo{virtualDirectoryInfo{name: resolved.ContainerID}}), nil
		}
		info, err := os.Stat(resolved.RealPath)
		if err != nil {
			return nil, err
		}
		return sftpListerAt([]os.FileInfo{info}), nil
	default:
		return nil, errors.New("unsupported list method")
	}
}

type sftpListerAt []os.FileInfo

func (l sftpListerAt) ListAt(target []os.FileInfo, offset int64) (int, error) {
	if offset >= int64(len(l)) {
		return 0, io.EOF
	}
	n := copy(target, l[offset:])
	if n < len(target) {
		return n, io.EOF
	}
	return n, nil
}

type virtualDirectoryInfo struct {
	name string
}

func (v virtualDirectoryInfo) Name() string       { return v.name }
func (v virtualDirectoryInfo) Size() int64        { return 0 }
func (v virtualDirectoryInfo) Mode() os.FileMode  { return os.ModeDir | 0o755 }
func (v virtualDirectoryInfo) ModTime() time.Time { return time.Now() }
func (v virtualDirectoryInfo) IsDir() bool        { return true }
func (v virtualDirectoryInfo) Sys() interface{}   { return nil }
