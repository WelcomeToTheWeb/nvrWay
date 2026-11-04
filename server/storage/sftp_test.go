package storage

// The `sftp` driver's tests run against an in-process SSH server: x/crypto's own
// `ssh.NewServerConn` with `pkg/sftp`'s own `sftp.NewServer` over a temp
// directory on the real filesystem. The driver's client, its transport,
// x/crypto's host-key negotiation and pkg/sftp's packet layer all run, so this
// is the protocol, not a mock of the driver.
//
// The server's host key can be swapped between connections while the address,
// the port, the backend row and the credentials all stay the same. That is the
// only honest way to test the pin: a changed key at the same address with
// working credentials is the interception case, and it must fail.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"nvr/server/models"
	"nvr/server/store"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

const (
	sftpTestUser = "nvr"
	sftpTestPass = "a-share-secret"
	sftpTestBad  = "not-the-password"
)

// sftpTestServer is the in-test SSH server.
type sftpTestServer struct {
	ln   net.Listener
	addr string
	dir  string // the directory the sftp subsystem serves, on the real filesystem

	user string
	pass string

	// authorized is the public key accepted for publickey auth, when the test
	// wants key auth rather than password auth.
	authorized ssh.PublicKey

	mu       sync.Mutex
	hostKey  ssh.Signer
	sessions int
}

func newSFTPTestServer(t *testing.T, user, pass string) *sftpTestServer {
	t.Helper()

	dir, err := os.MkdirTemp("", "nvr-sftp-test")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	s := &sftpTestServer{user: user, pass: pass, dir: dir}
	s.setHostKey(mustSFTPSigner(t))

	s.ln, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s.addr = s.ln.Addr().String()
	go s.accept()
	t.Cleanup(func() { _ = s.ln.Close() })
	return s
}

func mustSFTPSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	return signer
}

// setHostKey swaps the key the server presents to the *next* connection. A
// fresh ssh.ServerConfig is built per connection, so the library never reads
// the key while another goroutine is writing it.
func (s *sftpTestServer) setHostKey(signer ssh.Signer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hostKey = signer
}

func (s *sftpTestServer) fingerprint() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ssh.FingerprintSHA256(s.hostKey.PublicKey())
}

func (s *sftpTestServer) serverConfig() *ssh.ServerConfig {
	s.mu.Lock()
	hostKey := s.hostKey
	authorized := s.authorized
	user, pass := s.user, s.pass
	s.mu.Unlock()

	cfg := &ssh.ServerConfig{
		PasswordCallback: func(conn ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			if conn.User() == user && string(password) == pass {
				return &ssh.Permissions{}, nil
			}
			return nil, fmt.Errorf("password rejected for %q", conn.User())
		},
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if authorized != nil && conn.User() == user && bytes.Equal(key.Marshal(), authorized.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, fmt.Errorf("public key rejected for %q", conn.User())
		},
	}
	cfg.AddHostKey(hostKey)
	return cfg
}

func (s *sftpTestServer) accept() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.serve(conn)
	}
}

func (s *sftpTestServer) serve(conn net.Conn) {
	sshConn, chans, reqs, err := ssh.NewServerConn(conn, s.serverConfig())
	if err != nil {
		_ = conn.Close()
		return
	}
	s.mu.Lock()
	s.sessions++
	s.mu.Unlock()

	go func() {
		for req := range reqs {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}()
	for newChan := range chans {
		s.handleChannel(sshConn, newChan)
	}
	_ = sshConn.Wait()
}

func (s *sftpTestServer) handleChannel(sshConn *ssh.ServerConn, newChan ssh.NewChannel) {
	if newChan.ChannelType() != "session" {
		_ = newChan.Reject(ssh.UnknownChannelType, "only session channels")
		return
	}
	ch, reqs, err := newChan.Accept()
	if err != nil {
		return
	}
	go func() {
		defer func() { _ = ch.Close() }()
		for req := range reqs {
			if req.Type != "subsystem" {
				if req.WantReply {
					_ = req.Reply(false, nil)
				}
				continue
			}
			// DiscardRequests answers every request with a failure, which would
			// refuse the sftp subsystem, so the reply is made here.
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
			srv, err := sftp.NewServer(ch)
			if err != nil {
				return
			}
			srv.Serve()
			_ = srv.Close()
			return
		}
	}()
}

// writeTree makes a directory under the served root, the way a share would
// already have it before the recorder ever connects.
func (s *sftpTestServer) mkdir(rel string) string {
	full := filepath.Join(s.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(full, 0o755); err != nil {
		panic(err)
	}
	return full
}

// listFiles walks the served directory and returns the paths it holds, relative
// to it. This is how a test checks that the driver never wrote outside RootPath.
func (s *sftpTestServer) listFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(s.dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(s.dir, p)
		if relErr != nil {
			return relErr
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walking the served directory: %v", err)
	}
	return out
}

// sftpRow is a backend row for the in-test server. The password and the key
// path are sealed with `key`, because that is what a row in the database holds.
func sftpRow(t *testing.T, st *store.Store, name, endpoint, rootPath, prefix, user, password, keyPath string, key [32]byte) *models.StorageBackend {
	t.Helper()
	sealed := func(plain string) string {
		if plain == "" {
			return ""
		}
		return mustSeal(t, plain, key)
	}
	row := &models.StorageBackend{
		Name:           name,
		Kind:           models.StorageKindSFTP,
		Endpoint:       endpoint,
		RootPath:       rootPath,
		Prefix:         prefix,
		Username:       user,
		Password:       sealed(password),
		PrivateKeyPath: sealed(keyPath),
		Enabled:        true,
	}
	if st != nil {
		if err := st.StorageBackends.Create(row); err != nil {
			t.Fatalf("creating backend %s: %v", name, err)
		}
	}
	return row
}

// newSFTPBackend opens a backend against the in-test server, with a real
// settings store for the pin.
func newSFTPBackend(t *testing.T, s *sftpTestServer, st *store.Store, rootPath, prefix, password string) (Backend, *models.StorageBackend, [32]byte) {
	t.Helper()
	key := sealKey(t)
	if password == "" {
		password = sftpTestPass
	}
	row := sftpRow(t, st, "sftp-"+s.addr, s.addr, rootPath, prefix, sftpTestUser, password, "", key)
	b, err := OpenWithSettings(context.Background(), row, key, st.Settings)
	if err != nil {
		t.Fatalf("OpenWithSettings: %v", err)
	}
	return b, row, key
}

// -- the shared contract --------------------------------------------------

func TestSFTPContractAgainstAnSSHServer(t *testing.T) {
	s := newSFTPTestServer(t, sftpTestUser, sftpTestPass)
	st := newStorageTestStore(t)
	root := s.mkdir("media")

	b, _, _ := newSFTPBackend(t, s, st, root, "nvr/", "")
	assertBackendContract(t, b, "nvr/")

	if err := b.Ping(context.Background()); err != nil {
		t.Fatalf("Ping against a healthy server: %v", err)
	}

	// The contract proves the five operations behave; this proves *where* they
	// put the bytes. The served directory is the real filesystem, so the
	// assertion is a plain os.Stat of root_path + prefix + key.
	ctx := context.Background()
	if err := b.Put(ctx, "events/7/clip.mp4", strings.NewReader("v"), 1, "video/mp4"); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "nvr", "events", "7", "clip.mp4")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("the object is not where root_path + prefix + key says it is (%s): %v", want, err)
	}
	if err := b.Delete(ctx, "events/7/clip.mp4"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(want); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Delete left %s behind: %v", want, err)
	}
}

// -- host-key pinning -----------------------------------------------------

func TestSFTPHostKeyIsPinnedOnFirstUse(t *testing.T) {
	s := newSFTPTestServer(t, sftpTestUser, sftpTestPass)
	st := newStorageTestStore(t)
	root := s.mkdir("media")

	b, row, _ := newSFTPBackend(t, s, st, root, "nvr/", "")
	if err := b.Ping(context.Background()); err != nil {
		t.Fatalf("first Ping: %v", err)
	}

	pinned, err := st.Settings.Get(sftpPinPrefix + row.ID)
	if err != nil {
		t.Fatalf("reading the pin after first use: %v", err)
	}
	if pinned != s.fingerprint() {
		t.Fatalf("the pin is %q, want the key the server presented (%q)", pinned, s.fingerprint())
	}
	// The key is the plan's, verbatim: the namespace plus the backend's id.
	if !strings.HasPrefix(sftpPinPrefix+row.ID, "sftp_hostkey_") {
		t.Fatalf("the pin key is %q, want it under sftp_hostkey_", sftpPinPrefix+row.ID)
	}

	// A second Ping with the same key does not rewrite the pin, and does not
	// fail: the pin is a check, not a nonce.
	if err := b.Ping(context.Background()); err != nil {
		t.Fatalf("second Ping with the pinned key: %v", err)
	}
}

// TestSFTPChangedHostKeyIsTheNamedError is the whole reason the pin exists: the
// same address, the same backend, the same working credentials, and a different
// key. That is an interception, and it must fail every operation, not just the
// connection test.
func TestSFTPChangedHostKeyIsTheNamedError(t *testing.T) {
	s := newSFTPTestServer(t, sftpTestUser, sftpTestPass)
	st := newStorageTestStore(t)
	root := s.mkdir("media")

	b, _, _ := newSFTPBackend(t, s, st, root, "nvr/", "")
	if err := b.Ping(context.Background()); err != nil {
		t.Fatalf("Ping before the change: %v", err)
	}
	pinned := s.fingerprint()

	s.setHostKey(mustSFTPSigner(t))

	err := b.Ping(context.Background())
	if err == nil {
		t.Fatal("a different host key was accepted")
	}
	if ErrorCode(err) != ErrHostKeyChanged.Error() {
		t.Fatalf("a changed host key maps to %q (%v), want %q", ErrorCode(err), err, ErrHostKeyChanged.Error())
	}
	if !strings.Contains(err.Error(), pinned) || !strings.Contains(err.Error(), s.fingerprint()) {
		t.Fatalf("the failure does not name both fingerprints, so an operator cannot tell a rotation from an attack: %v", err)
	}
	// The credential is not blamed: it is still correct, and sending the
	// operator to the password field would hide the real problem.
	if strings.Contains(err.Error(), sftpTestPass) {
		t.Fatalf("the failure names the password: %v", err)
	}

	// And it is not the connection test that is strict — every operation dials,
	// so every operation refuses the new key.
	if _, err := b.Get(context.Background(), "nvr/anything.mp4"); ErrorCode(err) != ErrHostKeyChanged.Error() {
		t.Fatalf("Get with a changed host key maps to %q, want %q", ErrorCode(err), ErrHostKeyChanged.Error())
	}
	if err := b.Put(context.Background(), "nvr/x.mp4", strings.NewReader("v"), 1, "video/mp4"); ErrorCode(err) != ErrHostKeyChanged.Error() {
		t.Fatalf("Put with a changed host key maps to %q, want %q", ErrorCode(err), ErrHostKeyChanged.Error())
	}
}

// TestSFTPNoUnsignedHostKeyIsEverAccepted states the rule the other way round:
// the only key a connection ever trusts is one it has already recorded. There
// is no fallback path in the driver, and a backend that has never been
// connected cannot be pointed at a second server and quietly work.
func TestSFTPNoUnsignedHostKeyIsEverAccepted(t *testing.T) {
	sA := newSFTPTestServer(t, sftpTestUser, sftpTestPass)
	sB := newSFTPTestServer(t, sftpTestUser, sftpTestPass)
	st := newStorageTestStore(t)
	rootA := sA.mkdir("media")

	key := sealKey(t)
	// One backend row, pinned to server A.
	row := sftpRow(t, st, "pinned", sA.addr, rootA, "nvr/", sftpTestUser, sftpTestPass, "", key)
	b, err := OpenWithSettings(context.Background(), row, key, st.Settings)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := b.Ping(context.Background()); err != nil {
		t.Fatalf("Ping against the pinned server: %v", err)
	}

	// The same row, now pointed at a different server with a different key and
	// the same credentials: refused, not silently re-trusted.
	row.Endpoint = sB.addr
	b2, err := OpenWithSettings(context.Background(), row, key, st.Settings)
	if err != nil {
		t.Fatalf("Open against the second server: %v", err)
	}
	if err := b2.Ping(context.Background()); ErrorCode(err) != ErrHostKeyChanged.Error() {
		t.Fatalf("a second server with a different key maps to %q, want %q", ErrorCode(err), ErrHostKeyChanged.Error())
	}
}

func TestSFTPOpenWithoutPinStoreIsNamed(t *testing.T) {
	s := newSFTPTestServer(t, sftpTestUser, sftpTestPass)
	key := sealKey(t)
	row := sftpRow(t, nil, "no-store", s.addr, s.mkdir("media"), "nvr/", sftpTestUser, sftpTestPass, "", key)

	_, err := Open(context.Background(), row, key)
	if !errors.Is(err, ErrNoPinStore) {
		t.Fatalf("an sftp backend opened without a pin store: %v, want %v", err, ErrNoPinStore)
	}
	if ErrorCode(err) != "" {
		t.Fatalf("a missing pin store maps to %q, want no connection name", ErrorCode(err))
	}
}

// -- credentials ----------------------------------------------------------

func TestSFTPBadCredentialsIsTheNamedErrorNotAPanic(t *testing.T) {
	s := newSFTPTestServer(t, sftpTestUser, sftpTestPass)
	st := newStorageTestStore(t)
	root := s.mkdir("media")

	t.Run("the wrong password", func(t *testing.T) {
		b, _, _ := newSFTPBackend(t, s, st, root, "nvr/", sftpTestBad)
		err := b.Ping(context.Background())
		if err == nil {
			t.Fatal("a wrong password was accepted")
		}
		if ErrorCode(err) != ErrBadCredentials.Error() {
			t.Fatalf("a wrong password maps to %q (%v), want %q", ErrorCode(err), err, ErrBadCredentials.Error())
		}
		if strings.Contains(err.Error(), sftpTestBad) {
			t.Fatalf("the failure names the password: %v", err)
		}
	})

	t.Run("the wrong user", func(t *testing.T) {
		key := sealKey(t)
		row := sftpRow(t, st, "wrong-user", s.addr, root, "nvr/", "someoneelse", sftpTestPass, "", key)
		b, err := OpenWithSettings(context.Background(), row, key, st.Settings)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if err := b.Ping(context.Background()); ErrorCode(err) != ErrBadCredentials.Error() {
			t.Fatalf("a wrong user maps to %q, want %q", ErrorCode(err), ErrBadCredentials.Error())
		}
	})

	t.Run("neither a password nor a key is refused at open", func(t *testing.T) {
		key := sealKey(t)
		row := sftpRow(t, st, "anonymous", s.addr, root, "nvr/", sftpTestUser, "", "", key)
		_, err := OpenWithSettings(context.Background(), row, key, st.Settings)
		if err == nil || !strings.Contains(err.Error(), "password or a private key") {
			t.Fatalf("a row with no credential opened as %v, want a settings error", err)
		}
	})

	t.Run("a sealed password this process cannot open is not bad credentials", func(t *testing.T) {
		row := sftpRow(t, st, "other-key", s.addr, root, "nvr/", sftpTestUser, sftpTestPass, "", sealKey(t))
		_, err := OpenWithSettings(context.Background(), row, webdavOtherKey(), st.Settings)
		if !errors.Is(err, ErrBadKey) {
			t.Fatalf("an unopenable credential reported as %v, want %v", err, ErrBadKey)
		}
	})
}

func TestSFTPPrivateKeyAuthWorks(t *testing.T) {
	s := newSFTPTestServer(t, sftpTestUser, sftpTestPass)
	st := newStorageTestStore(t)
	root := s.mkdir("media")

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "nvr test key")
	if err != nil {
		t.Fatalf("marshalling the test key: %v", err)
	}
	keyFile := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.authorized = signer.PublicKey()
	s.mu.Unlock()

	key := sealKey(t)
	// No password: the key alone has to work.
	row := sftpRow(t, st, "by-key", s.addr, root, "nvr/", sftpTestUser, "", keyFile, key)
	b, err := OpenWithSettings(context.Background(), row, key, st.Settings)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := b.Ping(context.Background()); err != nil {
		t.Fatalf("Ping with a private key: %v", err)
	}
	if err := b.Put(context.Background(), "nvr/2026/11/04/cam-a-000001.mp4", strings.NewReader("v"), 1, "video/mp4"); err != nil {
		t.Fatalf("Put with a private key: %v", err)
	}
}

func TestSFTPPrivateKeyProblemsAreNamedAndNeverPanic(t *testing.T) {
	s := newSFTPTestServer(t, sftpTestUser, sftpTestPass)
	st := newStorageTestStore(t)
	root := s.mkdir("media")
	key := sealKey(t)

	t.Run("a key file that is not there", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "absent")
		row := sftpRow(t, st, "absent-key", s.addr, root, "nvr/", sftpTestUser, "", missing, key)
		b, err := OpenWithSettings(context.Background(), row, key, st.Settings)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		err = b.Ping(context.Background())
		if err == nil || !strings.Contains(err.Error(), missing) {
			t.Fatalf("a missing key file failed as %v, want a message naming the path", err)
		}
		if strings.Contains(err.Error(), sftpTestPass) {
			t.Fatalf("the failure names a credential: %v", err)
		}
	})

	t.Run("a key file that is not a key", func(t *testing.T) {
		junk := filepath.Join(t.TempDir(), "junk")
		if err := os.WriteFile(junk, []byte("this is not a PEM block\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		row := sftpRow(t, st, "junk-key", s.addr, root, "nvr/", sftpTestUser, "", junk, key)
		b, err := OpenWithSettings(context.Background(), row, key, st.Settings)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if err := b.Ping(context.Background()); err == nil {
			t.Fatal("an unparseable key was accepted")
		}
	})

	t.Run("an encrypted key says so", func(t *testing.T) {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		block, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "nvr test key", []byte("hunter2"))
		if err != nil {
			t.Fatalf("marshalling an encrypted test key: %v", err)
		}
		enc := filepath.Join(t.TempDir(), "id_encrypted")
		if err := os.WriteFile(enc, pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatal(err)
		}
		row := sftpRow(t, st, "enc-key", s.addr, root, "nvr/", sftpTestUser, "", enc, key)
		b, err := OpenWithSettings(context.Background(), row, key, st.Settings)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		err = b.Ping(context.Background())
		if err == nil || !strings.Contains(err.Error(), "passphrase") {
			t.Fatalf("an encrypted key failed as %v, want a message naming the missing passphrase", err)
		}
		// The passphrase itself is not in the message, and neither is the key.
		if strings.Contains(err.Error(), "hunter2") {
			t.Fatalf("the failure names the passphrase: %v", err)
		}
	})
}

// -- the four names, and the shape of the tier ----------------------------

func TestSFTPRootPathMissingIsBucketMissing(t *testing.T) {
	s := newSFTPTestServer(t, sftpTestUser, sftpTestPass)
	st := newStorageTestStore(t)

	// The server answers and the credential works; the directory is not there.
	b, _, _ := newSFTPBackend(t, s, st, filepath.Join(s.dir, "not-there"), "nvr/", "")
	err := b.Ping(context.Background())
	if ErrorCode(err) != ErrBucketMissing.Error() {
		t.Fatalf("a missing root_path maps to %q (%v), want %q", ErrorCode(err), err, ErrBucketMissing.Error())
	}
	if !strings.Contains(err.Error(), "not-there") {
		t.Fatalf("the failure does not name the directory: %v", err)
	}
}

func TestSFTPRootPathThatIsAFileIsBucketMissing(t *testing.T) {
	s := newSFTPTestServer(t, sftpTestUser, sftpTestPass)
	st := newStorageTestStore(t)
	file := filepath.Join(s.dir, "afile")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	b, _, _ := newSFTPBackend(t, s, st, file, "nvr/", "")
	if err := b.Ping(context.Background()); ErrorCode(err) != ErrBucketMissing.Error() {
		t.Fatalf("a root_path that is a file maps to %q, want %q", ErrorCode(err), ErrBucketMissing.Error())
	}
}

func TestSFTPRootPathMustBeAbsolute(t *testing.T) {
	s := newSFTPTestServer(t, sftpTestUser, sftpTestPass)
	st := newStorageTestStore(t)
	key := sealKey(t)

	row := sftpRow(t, st, "relative", s.addr, "media", "nvr/", sftpTestUser, sftpTestPass, "", key)
	_, err := OpenWithSettings(context.Background(), row, key, st.Settings)
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("a relative root_path opened as %v, want a refusal", err)
	}
}

func TestSFTPNothingAnsweringIsDiscoveryUnreachable(t *testing.T) {
	s := newSFTPTestServer(t, sftpTestUser, sftpTestPass)
	st := newStorageTestStore(t)
	root := s.mkdir("media")
	b, _, _ := newSFTPBackend(t, s, st, root, "nvr/", "")

	if err := s.ln.Close(); err != nil {
		t.Fatalf("closing the listener: %v", err)
	}
	err := b.Ping(context.Background())
	if ErrorCode(err) != ErrUnreachable.Error() {
		t.Fatalf("a dead server maps to %q (%v), want %q", ErrorCode(err), err, ErrUnreachable.Error())
	}
}

func TestSFTPKeyCannotEscapeRootPath(t *testing.T) {
	s := newSFTPTestServer(t, sftpTestUser, sftpTestPass)
	st := newStorageTestStore(t)
	root := s.mkdir("media")
	s.mkdir("outside")

	b, _, _ := newSFTPBackend(t, s, st, root, "nvr/", "")
	ctx := context.Background()

	for _, key := range []string{"../secret.txt", "nvr/../../secret.txt", "/../secret.txt", ""} {
		if _, err := b.Get(ctx, key); err == nil {
			t.Errorf("Get(%q) was accepted", key)
		} else if ErrorCode(err) != "" {
			t.Errorf("Get(%q) reported %q, want a settings-shaped error rather than a named one", key, ErrorCode(err))
		}
		if err := b.Put(ctx, key, strings.NewReader("x"), 1, ""); err == nil {
			t.Errorf("Put(%q) was accepted", key)
		}
	}

	// And the server's filesystem says so too: nothing was written outside the
	// backend's own directory.
	if err := b.Put(ctx, "nvr/2026/11/04/cam-a-000001.mp4", strings.NewReader("v"), 1, "video/mp4"); err != nil {
		t.Fatal(err)
	}
	for _, f := range s.listFiles(t) {
		if !strings.HasPrefix(f, "media/") {
			t.Errorf("the server holds %q, which is outside root_path", f)
		}
	}
}

func TestSFTPSecondDeleteNamesTheAbsence(t *testing.T) {
	s := newSFTPTestServer(t, sftpTestUser, sftpTestPass)
	st := newStorageTestStore(t)
	root := s.mkdir("media")
	b, _, _ := newSFTPBackend(t, s, st, root, "nvr/", "")
	ctx := context.Background()

	if err := b.Put(ctx, "gone.mp4", strings.NewReader("x"), 1, "video/mp4"); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete(ctx, "gone.mp4"); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	// A filesystem says "not there", which is what the fs backend says too, and
	// what s3 and WebDAV do not.
	if err := b.Delete(ctx, "gone.mp4"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v, want %v", err, ErrNotFound)
	}
}

func TestSFTPPutLeavesNoPartFileAndListSkipsOne(t *testing.T) {
	s := newSFTPTestServer(t, sftpTestUser, sftpTestPass)
	st := newStorageTestStore(t)
	root := s.mkdir("media")
	b, _, _ := newSFTPBackend(t, s, st, root, "nvr/", "")
	ctx := context.Background()

	if err := b.Put(ctx, "2026/11/04/cam-a-000001.mp4", strings.NewReader("body"), 4, "video/mp4"); err != nil {
		t.Fatal(err)
	}
	for _, f := range s.listFiles(t) {
		if strings.HasSuffix(f, ".part") {
			t.Errorf("a successful Put left %q behind", f)
		}
	}

	// An upload in flight is not an object the queue shelved: a playback proxy
	// that listed a `.part` would offer a file that is still growing.
	if err := os.WriteFile(filepath.Join(root, "nvr/2026/11/04/cam-a-000002.mp4.part"), []byte("half"), 0o644); err != nil {
		t.Fatal(err)
	}
	keys, err := b.List(ctx, "2026/11/04/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, k := range keys {
		if strings.HasSuffix(k, ".part") {
			t.Errorf("List returned %q, which is an upload in flight", k)
		}
	}
	if len(keys) != 1 || keys[0] != "2026/11/04/cam-a-000001.mp4" {
		t.Fatalf("List returned %v, want the one shelved object", keys)
	}
}

func TestSFTPGetReadsWholeAndClosesTheConnection(t *testing.T) {
	s := newSFTPTestServer(t, sftpTestUser, sftpTestPass)
	st := newStorageTestStore(t)
	root := s.mkdir("media")
	b, _, _ := newSFTPBackend(t, s, st, root, "nvr/", "")
	ctx := context.Background()

	body := "a short stand-in for an mp4"
	if err := b.Put(ctx, "clip.mp4", strings.NewReader(body), int64(len(body)), "video/mp4"); err != nil {
		t.Fatal(err)
	}
	rc, err := b.Get(ctx, "clip.mp4")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if string(got) != body {
		t.Fatalf("Get read %q, want %q", got, body)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("closing the object: %v", err)
	}
	// The reader owns the connection, so closing it twice must not turn a
	// finished transfer into an error.
	if err := rc.Close(); err != nil {
		t.Fatalf("closing the object twice: %v", err)
	}

	s.mu.Lock()
	sessions := s.sessions
	s.mu.Unlock()
	if sessions < 2 {
		t.Fatalf("the server saw %d sessions, want one per operation", sessions)
	}
}

func TestSFTPSecretNeverAppearsInALineThisProcessWrites(t *testing.T) {
	s := newSFTPTestServer(t, sftpTestUser, sftpTestPass)
	st := newStorageTestStore(t)
	// The credential is right and the directory is not there: the failure is the
	// missing container, and the password belongs nowhere in it.
	b, _, _ := newSFTPBackend(t, s, st, filepath.Join(s.dir, "not-there"), "nvr/", "")

	var logged bytes.Buffer
	out := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(out)

	err := b.Ping(context.Background())
	if err == nil {
		t.Fatal("a missing root_path produced no error")
	}
	if strings.Contains(err.Error(), sftpTestPass) {
		t.Fatalf("the error carries the password: %v", err)
	}
	if strings.Contains(logged.String(), sftpTestPass) {
		t.Fatalf("the log carries the password: %s", logged.String())
	}
}

// -- the endpoint, and dispatch -------------------------------------------

func TestSFTPEndpointBecomesADialableAddress(t *testing.T) {
	cases := []struct {
		in   string
		want string
		bad  bool
	}{
		{"nas.local", "nas.local:22", false},
		{"nas.local:2222", "nas.local:2222", false},
		{"sftp://nas.local", "nas.local:22", false},
		{"ssh://nas.local:2222", "nas.local:2222", false},
		{"nas.local/", "nas.local:22", false},
		{"fe80::1", "[fe80::1]:22", false},
		{"[::1]:2222", "[::1]:2222", false},
		{"nas.local/srv/media", "", true},
		{"", "", true},
	}
	for _, tc := range cases {
		got, err := sftpHostPort(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("endpoint %q opened as %q, want a refusal", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("endpoint %q: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("endpoint %q became %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSFTPOpenDispatchesWithSettingsAndUnsealedCredentials(t *testing.T) {
	s := newSFTPTestServer(t, sftpTestUser, sftpTestPass)
	st := newStorageTestStore(t)
	root := s.mkdir("media")
	key := sealKey(t)

	row := sftpRow(t, st, "dispatched", s.addr, root, "nvr/", sftpTestUser, sftpTestPass, "", key)
	row.IsDefault = true
	b, err := OpenDefault(context.Background(), st, key)
	if err != nil {
		t.Fatalf("OpenDefault: %v", err)
	}
	if err := b.Ping(context.Background()); err != nil {
		t.Fatalf("Ping through the dispatched backend: %v", err)
	}
}

func TestSFTPDriverIsRegisteredForTheRealKind(t *testing.T) {
	kinds := RegisteredKinds()
	var found bool
	for _, k := range kinds {
		if k == models.StorageKindSFTP {
			found = true
		}
	}
	if !found {
		t.Fatalf("registered kinds are %v, want %q among them", kinds, models.StorageKindSFTP)
	}
}
