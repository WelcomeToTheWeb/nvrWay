package storage

// The `sftp` driver: x/crypto/ssh + pkg/sftp, with the host key pinned rather
// than trusted.
//
// SFTP is the only kind here that can be silently man-in-the-middled, because
// the thing being authenticated the first time is the *server*. So the host key
// is checked against a pin stored per backend (`sftp_hostkey_<backend id>`),
// pinned on first use and required to match every time after that. There is no
// `HostKeyFallback` and no `InsecureIgnoreHostKey` anywhere in this file: the
// first connection trusts a key it has never seen and records it, and every
// connection after that must present the same one. A changed pin is
// `sftp host key changed`, one of the four names the Settings page draws, and
// it is reported even when the username and password are perfect — because a
// server that answers with a different key while your credentials work is the
// interception case, not the typo case.
//
// Two behaviours are the driver's, not the protocol's:
//
//   - **The connection is dialled per operation and closed at the end of it.**
//     `Backend` has no `Close`, and `Open` runs on every upload tick and every
//     playback request, so a connection memoised on the backend would be one
//     leaked SSH session per tick. The cost is a handshake per operation, which
//     on a LAN is tens of milliseconds against a transfer that moves a video
//     segment. `Get` hands its connection to the returned reader, because the
//     caller already has to close that.
//   - **`Put` writes `<key>.part` and renames**, as the fs backend does. A
//     playback proxy that streams an object while the uploader is still writing
//     it would otherwise serve a truncated mp4 as if it were finished.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"nvr/server/models"
	"nvr/server/store"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"gorm.io/gorm"
)

// sftpPinPrefix is the settings key namespace for host-key pins. The suffix is
// the backend's id, not its name: a rename of the backend in the Settings page
// must not silently re-trust a key the operator had already pinned.
const sftpPinPrefix = "sftp_hostkey_"

// sftpDefaultPort is the port an endpoint without one means.
const sftpDefaultPort = "22"

// sftpDialTimeout bounds the TCP connect and the SSH handshake. An SFTP server
// behind a half-dead network should answer "nothing answered" quickly; the
// transfer itself is not bounded, because a large clip on a slow uplink takes
// the time it takes.
const sftpDialTimeout = 10 * time.Second

// sftpMaxDepth bounds the walk List does, for the reason the webdav driver
// gives: a remote tree is somebody else's filesystem.
const sftpMaxDepth = 32

// ErrNoPinStore is an sftp backend opened without somewhere to keep its host-key
// pin. It is not one of the four connection names: the server is fine, the
// credential is fine, and the process is missing the store that remembers which
// key it trusted. `OpenWithSettings` and `OpenDefault` both supply it; a caller
// that reaches for `Open` and gets this is a caller that would otherwise accept
// any key on every boot.
var ErrNoPinStore = errors.New("the host-key pin store is unavailable")

func init() {
	registerDriver(models.StorageKindSFTP, openSFTP)
}

// sftpBackend is one SFTP server, a user on it, and a directory on that server
// this backend owns.
type sftpBackend struct {
	host     string // host:port
	user     string
	password string
	keyPath  string // a file on *this* host, already unsealed by Open

	root   string // RootPath on the server: this kind's container
	prefix string // the row's prefix, inside the container

	settings *store.SettingStore
	pinKey   string
}

func openSFTP(_ context.Context, a *openArgs) (Backend, error) {
	row := a.Row

	host, err := sftpHostPort(row.Endpoint)
	if err != nil {
		return nil, err
	}
	root := strings.TrimSpace(row.RootPath)
	if root == "" {
		// The container the settings name is not there, which is the same
		// answer a missing bucket is.
		return nil, fmt.Errorf("root_path is empty: %w", ErrBucketMissing)
	}
	if !strings.HasPrefix(root, "/") {
		// A relative root resolves against the server's idea of the user's
		// home directory, which is a path this process cannot reason about and
		// the confinement check cannot bound.
		return nil, fmt.Errorf("root_path %q must be absolute on the server", row.RootPath)
	}
	if a.Secret.Password == "" && a.Secret.PrivateKeyPath == "" {
		// x/crypto has no way to offer "none" as an authentication method, so a
		// row with neither is a settings error rather than an anonymous server.
		return nil, errors.New("an sftp backend needs a password or a private key")
	}
	if a.Settings == nil {
		return nil, fmt.Errorf("sftp backend %q: %w", row.Name, ErrNoPinStore)
	}
	if row.ID == "" {
		// The pin is keyed by id, so a row without one has no pin to check.
		return nil, fmt.Errorf("sftp backend %q has no id to pin a host key under: %w", row.Name, ErrNoPinStore)
	}

	return &sftpBackend{
		host:     host,
		user:     a.Secret.Username,
		password: a.Secret.Password,
		keyPath:  a.Secret.PrivateKeyPath,
		root:     strings.TrimRight(root, "/"),
		prefix:   strings.TrimSuffix(joinPrefix(row.Prefix), "/"),
		settings: a.Settings,
		pinKey:   sftpPinPrefix + row.ID,
	}, nil
}

// sftpHostPort normalises the endpoint into the address x/crypto dials.
//
// The scheme is decoration here, as it is on s3: an SFTP endpoint is a host,
// and the port is either in the field or the protocol's default. A path is
// refused, because the directory belongs in root_path.
func sftpHostPort(raw string) (string, error) {
	endpoint := strings.TrimSpace(raw)
	if endpoint == "" {
		return "", errors.New("storage: sftp: endpoint is empty")
	}
	for _, scheme := range []string{"sftp://", "ssh://"} {
		endpoint = strings.TrimPrefix(endpoint, scheme)
	}
	endpoint = strings.TrimRight(endpoint, "/")
	if i := strings.Index(endpoint, "/"); i >= 0 {
		return "", fmt.Errorf("storage: sftp: endpoint %q must be host or host:port; the directory goes in root_path", raw)
	}
	if endpoint == "" {
		return "", errors.New("storage: sftp: endpoint is empty")
	}
	// A bare IPv6 address needs brackets before a port can be appended; a
	// bracketed one is already dialable as-is.
	if _, port, splitErr := net.SplitHostPort(endpoint); splitErr != nil {
		if strings.Count(endpoint, ":") > 1 {
			// No port and more than one colon: an unbracketed IPv6 address.
			return "[" + endpoint + "]" + ":" + sftpDefaultPort, nil
		}
		return endpoint + ":" + sftpDefaultPort, nil
	} else if port == "" {
		return endpoint[:len(endpoint)-1] + sftpDefaultPort, nil
	}
	return endpoint, nil
}

// base is the directory every key of this backend lives under, on the server.
func (b *sftpBackend) base() string {
	if b.prefix == "" {
		return b.root
	}
	return b.root + "/" + b.prefix
}

// path turns a key into the remote file that holds it, refusing anything that
// would leave the backend's directory.
//
// Keys are slash-separated on every driver (the interface says so), and the
// SFTP protocol takes forward slashes whatever the server's own platform, so
// this uses `path` and not `path/filepath`.
func (b *sftpBackend) remotePath(key string) (string, error) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(key), "/")
	if trimmed == "" {
		return "", errors.New("storage: sftp: an empty key names no object")
	}
	if strings.ContainsRune(trimmed, 0) {
		return "", errors.New("storage: sftp: a key may not contain a NUL")
	}
	cleaned := path.Clean(trimmed)
	for _, segment := range strings.Split(cleaned, "/") {
		if segment == ".." {
			return "", fmt.Errorf("storage: sftp: key %q escapes %s", key, b.root)
		}
	}
	if cleaned == "." {
		return "", fmt.Errorf("storage: sftp: key %q names the tier root %s, not an object", key, b.root)
	}
	base := b.base()
	full := base + "/" + cleaned
	if !strings.HasPrefix(full, base+"/") {
		return "", fmt.Errorf("storage: sftp: key %q escapes %s", key, base)
	}
	return full, nil
}

// dial opens a connection, authenticates, and verifies the host key against the
// pin. The caller closes what it gets back.
func (b *sftpBackend) dial() (*sftp.Client, *ssh.Client, error) {
	cfg := &ssh.ClientConfig{
		User:            b.user,
		Timeout:         sftpDialTimeout,
		HostKeyCallback: b.verifyHostKey,
	}
	if b.keyPath != "" {
		signer, err := sftpLoadSigner(b.keyPath)
		if err != nil {
			return nil, nil, err
		}
		cfg.Auth = append(cfg.Auth, ssh.PublicKeys(signer))
	}
	if b.password != "" {
		cfg.Auth = append(cfg.Auth, ssh.Password(b.password))
	}

	sshConn, err := ssh.Dial("tcp", b.host, cfg)
	if err != nil {
		return nil, nil, b.mapDialErr(err)
	}
	sftpConn, err := sftp.NewClient(sshConn)
	if err != nil {
		_ = sshConn.Close()
		return nil, nil, fmt.Errorf("storage: sftp: the server accepted the connection but not the sftp subsystem: %w", err)
	}
	return sftpConn, sshConn, nil
}

// sftpLoadSigner reads a private key file from *this* host.
//
// The path itself is sealed in the row (which key file a backend uses says
// something about the machine), and the file's contents never leave here: they
// are parsed into a signer and the PEM is dropped.
func sftpLoadSigner(pemPath string) (ssh.Signer, error) {
	pem, err := os.ReadFile(pemPath)
	if err != nil {
		// The path is unsealed by the time we get here, so naming it is safe;
		// the key material never is.
		return nil, fmt.Errorf("storage: sftp: reading the private key %s: %w", pemPath, err)
	}
	signer, err := ssh.ParsePrivateKey(pem)
	if err != nil {
		var needPassphrase *ssh.PassphraseMissingError
		if errors.As(err, &needPassphrase) {
			return nil, fmt.Errorf("storage: sftp: the private key %s is encrypted and v1 has no passphrase field: %w", pemPath, err)
		}
		return nil, fmt.Errorf("storage: sftp: parsing the private key %s: %w", pemPath, err)
	}
	return signer, nil
}

// verifyHostKey is the pin check, and the only HostKeyCallback this driver has.
//
// The first connection to a backend records the key it saw; every connection
// after that must present the same fingerprint. This runs during the handshake,
// before authentication, so a key change is reported even when the credentials
// would have worked — which is the interception case the pin exists for.
func (b *sftpBackend) verifyHostKey(_ string, _ net.Addr, key ssh.PublicKey) error {
	fingerprint := ssh.FingerprintSHA256(key)

	pinned, err := b.settings.Get(b.pinKey)
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("storage: sftp: reading the host-key pin for %s: %w", b.pinKey, err)
		}
		// First use. Record it before the connection is used for anything, so a
		// crash mid-transfer does not leave the next boot with no pin and a
		// second chance to trust a stranger.
		if err := b.settings.Set(b.pinKey, fingerprint); err != nil {
			return fmt.Errorf("storage: sftp: storing the host-key pin for %s: %w", b.pinKey, err)
		}
		return nil
	}
	if pinned != fingerprint {
		return fmt.Errorf("%w: %s presents %s and this backend pinned %s",
			ErrHostKeyChanged, b.host, fingerprint, pinned)
	}
	return nil
}

// mapDialErr turns a dial failure into one of the names.
//
// The host-key error is already wrapped by `verifyHostKey` and x/crypto passes
// it through (`ssh: handshake failed: %w`), so `errors.Is` finds it. An
// authentication failure has no sentinel in x/crypto — the library reports it as
// a sentence naming the methods it tried — so it is recognised by that prefix
// and nothing else, and the sentence is kept in the message.
func (b *sftpBackend) mapDialErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrHostKeyChanged) {
		return err
	}
	if isUnreachable(err) {
		return fmt.Errorf("storage: sftp: dialling %s (%v): %w", b.host, err, ErrUnreachable)
	}
	if isSSHAuthFailure(err) {
		return fmt.Errorf("storage: sftp: %s refused %q (%v): %w", b.host, b.user, err, ErrBadCredentials)
	}
	return fmt.Errorf("storage: sftp: dialling %s: %w", b.host, err)
}

// isSSHAuthFailure recognises x/crypto's authentication refusal. The library
// exports no sentinel for it, so this matches the message it builds
// (`client_auth.go`: "ssh: unable to authenticate, attempted methods %v, no
// supported methods remain") and the per-method refusals it can return instead.
func isSSHAuthFailure(err error) bool {
	msg := err.Error()
	for _, marker := range []string{
		"unable to authenticate",
		"ssh: must specify a auth method",
		"no supported methods remain",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// mapSFTPErr turns a filesystem-level failure into the tier's own error.
//
// A missing path is `ErrNotFound`, which is not one of the four connection
// names — the caller answers a named 404 of its own. A permission denial keeps
// its own message here: it is the credential failing on one object, not the
// connection test, and `Ping` is where it becomes `bad credentials`.
func (b *sftpBackend) mapSFTPErr(verb, key string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("storage: sftp: %s %s: %w", verb, key, ErrNotFound)
	}
	if isUnreachable(err) || isSSHAuthFailure(err) {
		// The connection died mid-operation: the next operation dials again, and
		// this one is reported as unreachable rather than as a filesystem fault.
		return fmt.Errorf("storage: sftp: %s %s (%v): %w", verb, key, err, ErrUnreachable)
	}
	return fmt.Errorf("storage: sftp: %s %s: %w", verb, key, err)
}

// Put writes the object, atomically: the bytes go to `<key>.part`, are flushed,
// and only then become the object by rename. The reason is the playback proxy —
// an sftp server answers a GET for a file that is still being written, and a
// truncated mp4 is served as a finished one.
func (b *sftpBackend) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	name, err := b.remotePath(key)
	if err != nil {
		return err
	}
	sftpConn, sshConn, err := b.dial()
	if err != nil {
		return err
	}
	defer func() { _ = sftpConn.Close(); _ = sshConn.Close() }()

	if err := sftpConn.MkdirAll(path.Dir(name)); err != nil {
		return b.mapSFTPErr("put", key, err)
	}
	tmp := name + ".part"
	f, err := sftpConn.Create(tmp)
	if err != nil {
		return b.mapSFTPErr("put", key, err)
	}
	written, copyErr := copyWhileChecking(ctx, f, r)
	closeErr := f.Close()
	if copyErr != nil {
		_ = sftpConn.Remove(tmp)
		if size >= 0 {
			return fmt.Errorf("storage: sftp: put %s: wrote %d of %d bytes: %w", key, written, size, copyErr)
		}
		return b.mapSFTPErr("put", key, copyErr)
	}
	if closeErr != nil {
		_ = sftpConn.Remove(tmp)
		return b.mapSFTPErr("put", key, closeErr)
	}
	if size >= 0 && written != size {
		_ = sftpConn.Remove(tmp)
		return fmt.Errorf("storage: sftp: put %s: wrote %d bytes for a size of %d", key, written, size)
	}
	// Rename over an existing object replaces it on every POSIX server, which is
	// what "Put replaces whatever was there" means.
	if err := sftpConn.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = sftpConn.Remove(tmp)
		return b.mapSFTPErr("put", key, err)
	}
	if err := sftpConn.Rename(tmp, name); err != nil {
		_ = sftpConn.Remove(tmp)
		return b.mapSFTPErr("put", key, err)
	}
	return nil
}

// Get opens the object for reading from byte 0.
//
// The returned reader owns the connection: closing it closes the file, the sftp
// session and the SSH transport. That is the only place the connection can go,
// because the caller already has to close the reader.
func (b *sftpBackend) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	name, err := b.remotePath(key)
	if err != nil {
		return nil, err
	}
	sftpConn, sshConn, err := b.dial()
	if err != nil {
		return nil, err
	}
	f, err := sftpConn.Open(name)
	if err != nil {
		_ = sftpConn.Close()
		_ = sshConn.Close()
		return nil, b.mapSFTPErr("get", key, err)
	}
	return &sftpObject{file: f, sftp: sftpConn, ssh: sshConn}, nil
}

// sftpObject is an open remote file plus the two connections that carry it.
type sftpObject struct {
	file   *sftp.File
	sftp   *sftp.Client
	ssh    *ssh.Client
	closed bool
}

func (o *sftpObject) Read(p []byte) (int, error) { return o.file.Read(p) }

// Close is idempotent: the playback proxy closes the reader when it finishes
// writing the response, and a deferred close in the same handler must not turn
// a 200 into a spurious error.
func (o *sftpObject) Close() error {
	if o.closed {
		return nil
	}
	o.closed = true
	err := o.file.Close()
	_ = o.sftp.Close()
	_ = o.ssh.Close()
	return err
}

// Stat reports the object without reading it.
//
// The content type comes from the extension, as it does on the fs backend: a
// remote filesystem has no type for a file either, and the two kinds that do
// (s3, and a WebDAV share that reports one) answer it from the tier.
func (b *sftpBackend) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	name, err := b.remotePath(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	sftpConn, sshConn, err := b.dial()
	if err != nil {
		return ObjectInfo{}, err
	}
	defer func() { _ = sftpConn.Close(); _ = sshConn.Close() }()

	fi, err := sftpConn.Stat(name)
	if err != nil {
		return ObjectInfo{}, b.mapSFTPErr("stat", key, err)
	}
	if fi.IsDir() {
		return ObjectInfo{}, fmt.Errorf("storage: sftp: key %q names the directory %s, not an object", key, name)
	}
	return ObjectInfo{
		Key:         key,
		Size:        fi.Size(),
		ModTime:     fi.ModTime().UTC(),
		ContentType: contentTypeFor(key),
	}, nil
}

// Delete removes the object.
//
// A second delete reports `ErrNotFound`, as it does on the fs backend and unlike
// s3 and WebDAV: this is a filesystem, and a filesystem says "not there". The
// sweeper clears the row's `remote_key` either way, which is why the shared
// contract does not require one answer of all four kinds.
func (b *sftpBackend) Delete(ctx context.Context, key string) error {
	name, err := b.remotePath(key)
	if err != nil {
		return err
	}
	sftpConn, sshConn, err := b.dial()
	if err != nil {
		return err
	}
	defer func() { _ = sftpConn.Close(); _ = sshConn.Close() }()

	if err := sftpConn.Remove(name); err != nil {
		return b.mapSFTPErr("delete", key, err)
	}
	return nil
}

// List returns the keys under a prefix, sorted, as keys the caller can hand
// back to Get: relative to the backend, prefix included.
//
// One connection serves the whole walk. A tree four levels deep is one
// handshake and five directory reads, not five handshakes, and the difference
// is the difference between a sweep that finishes and one that spends its tick
// on transport.
func (b *sftpBackend) List(ctx context.Context, prefix string) ([]string, error) {
	want := strings.TrimPrefix(strings.TrimSpace(prefix), "/")
	if want != "" && !strings.HasSuffix(want, "/") {
		want += "/"
	}
	dir := b.base()
	if want != "" {
		dir += "/" + strings.TrimSuffix(want, "/")
	}

	sftpConn, sshConn, err := b.dial()
	if err != nil {
		return nil, err
	}
	defer func() { _ = sftpConn.Close(); _ = sshConn.Close() }()

	var out []string
	if err := b.walk(ctx, sftpConn, dir, want, &out, 0); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// walk descends one directory. A directory that is not there is an empty list,
// not a failure — the sweeper asks about a day before the first upload of that
// day has ever happened.
func (b *sftpBackend) walk(ctx context.Context, sftpConn *sftp.Client, dir, keyPrefix string, out *[]string, depth int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > sftpMaxDepth {
		return fmt.Errorf("storage: sftp: list %s: more than %d levels of directories", dir, sftpMaxDepth)
	}
	entries, err := sftpConn.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return b.mapSFTPErr("list", dir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if name == "" || name == "." || name == "/" {
			continue
		}
		// A `.part` is an upload in flight, not an object the queue shelved.
		if strings.HasSuffix(name, ".part") {
			continue
		}
		key := keyPrefix + name
		if e.IsDir() {
			if err := b.walk(ctx, sftpConn, dir+"/"+name, key+"/", out, depth+1); err != nil {
				return err
			}
			continue
		}
		*out = append(*out, key)
	}
	return nil
}

// Ping is the connection test: dial, verify the host key against the pin,
// authenticate, and check that the directory this backend owns is there.
//
// The three answers are the three the Settings page draws, and the order is the
// order they happen in: a key that changed is reported before the credential is
// tried, and a credential that works is still reported as `bucket missing` when
// the directory is not on the server.
func (b *sftpBackend) Ping(ctx context.Context) error {
	sftpConn, sshConn, err := b.dial()
	if err != nil {
		return err
	}
	defer func() { _ = sftpConn.Close(); _ = sshConn.Close() }()

	fi, err := sftpConn.Stat(b.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("storage: sftp: the directory %q is not on %s: %w", b.root, b.host, ErrBucketMissing)
		}
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("storage: sftp: %s will not let %q read %q: %w", b.host, b.user, b.root, ErrBadCredentials)
		}
		return b.mapSFTPErr("checking the root", b.root, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("storage: sftp: root_path %q on %s is not a directory: %w", b.root, b.host, ErrBucketMissing)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}
