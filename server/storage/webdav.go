package storage

// The `webdav` driver: gowebdav against a share, with the collection the row
// names treated as the bucket.
//
// A WebDAV share is the one destination a household already has — a NAS, a
// Nextcloud, a router with a USB disk on it — and it is the only kind whose
// "bucket" is a directory somebody made in a file manager. So the first path
// segment of the row's prefix is this driver's container: `Ping` checks that it
// exists, and a share that answers without it is `bucket missing` rather than
// a silent upload into the wrong half of the disk.
//
// One thing about the library is worth knowing before changing `Put`, because
// it is the opposite of what the RFC suggests:
//
//   - **MKCOL's 405 for an existing collection is not a failure here.** RFC 4918
//     says MKCOL on an existing resource is 405, and gowebdav's `mkcol` maps
//     that answer to 201 (`requests.go`: `if status == 405 { status = 201 }`).
//     So its streaming writes — which create the parent collections *before*
//     the PUT, unlike the s3-style "PUT, and build the parents if it 404s" —
//     are safe against a correct server, and `Put` may stream. A driver that
//     assumed otherwise would buffer every object in memory for no reason.

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"

	"nvr/server/models"

	"github.com/studio-b12/gowebdav"
)

// webdavMaxDepth bounds the walk List does. The key layout is four levels deep
// (year/month/day under nvr/<slug>), so thirty-two is a limit nobody reaches;
// it is here because a WebDAV server is somebody else's filesystem, and a loop
// or an absurd tree should cost an error rather than the process.
const webdavMaxDepth = 32

// webdavDialTimeout bounds the connection, not the transfer. A NAS asleep at
// the bottom of a shelf answers nothing, and the Settings page asked a question.
const webdavDialTimeout = 10 * time.Second

func init() {
	registerDriver(models.StorageKindWebDAV, openWebdav)
}

// webdavBackend talks WebDAV to one share.
type webdavBackend struct {
	client *gowebdav.Client

	// root is the endpoint, for the messages an operator reads.
	root string
	// rootPath is the path part of the endpoint URL. gowebdav appends object
	// paths to the root URL, so everything handed to the client is a path
	// *within* that root — pass it a URL and it is concatenated onto itself.
	rootPath string
	// prefix is the row's prefix with a trailing slash, as `joinPrefix` shapes it.
	prefix string
	// container is the prefix's first segment: this kind's stand-in for a bucket.
	container string
}

func openWebdav(_ context.Context, a *openArgs) (Backend, error) {
	row := a.Row

	endpoint, rootPath, err := webdavEndpoint(row)
	if err != nil {
		return nil, err
	}

	client := gowebdav.NewClient(endpoint, a.Secret.Username, a.Secret.Password)
	// The overall client timeout is deliberately left alone: an upload streams
	// for as long as the body takes, and a fixed ceiling would fail a large clip
	// on a slow uplink. What has to be fast is the dial.
	client.SetTransport(&http.Transport{
		DialContext:         (&net.Dialer{Timeout: webdavDialTimeout}).DialContext,
		TLSHandshakeTimeout: webdavDialTimeout,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: row.InsecureTLS},
	})

	return &webdavBackend{
		client:    client,
		root:      endpoint,
		rootPath:  rootPath,
		prefix:    joinPrefix(row.Prefix),
		container: webdavContainer(row.Prefix),
	}, nil
}

// webdavEndpoint normalises the URL the operator typed.
//
// Unlike s3, where the endpoint is a bare host and one checkbox decides TLS,
// a WebDAV endpoint is a URL and the scheme is part of it: `http://` on a LAN
// share and `https://` on a hosted one are both correct, and a bare
// `nas.local:5005` is ambiguous — it could be a host:port or a scheme-less URL
// with a path. So the scheme is required here and refused nowhere else.
func webdavEndpoint(row *models.StorageBackend) (endpoint, rootPath string, err error) {
	raw := strings.TrimSpace(row.Endpoint)
	if raw == "" {
		return "", "", errors.New("storage: webdav: endpoint is empty")
	}
	if !strings.Contains(raw, "://") {
		return "", "", fmt.Errorf("storage: webdav: endpoint %q needs a scheme — http:// for a LAN share, https:// for a hosted one", raw)
	}
	raw = strings.TrimRight(raw, "/")

	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("storage: webdav: endpoint %q: %w", row.Endpoint, err)
	}
	if u.Host == "" {
		return "", "", fmt.Errorf("storage: webdav: endpoint %q has no host", row.Endpoint)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", "", fmt.Errorf("storage: webdav: endpoint %q: scheme %q is not http or https", row.Endpoint, u.Scheme)
	}
	if u.User != nil {
		// A user in the URL would be a second place to put the username, and
		// gowebdav would ignore it while the form showed one.
		return "", "", fmt.Errorf("storage: webdav: endpoint %q carries a user; the username has its own field", row.Endpoint)
	}
	rootPath = strings.TrimRight(u.Path, "/")
	if p := strings.Trim(u.Path, "/"); p != "" && strings.HasPrefix(strings.TrimSuffix(joinPrefix(row.Prefix), "/"), p+"/") {
		return "", "", fmt.Errorf("storage: webdav: prefix %q repeats the path already in endpoint %q — use one or the other",
			row.Prefix, row.Endpoint)
	}
	return raw, rootPath, nil
}

// webdavContainer is the first segment of a prefix.
func webdavContainer(prefix string) string {
	p := strings.Trim(joinPrefix(prefix), "/")
	if p == "" {
		return ""
	}
	return strings.SplitN(p, "/", 2)[0]
}

// object is the share-side path for a key: the row's prefix, then the key,
// under the endpoint's own path.
func (b *webdavBackend) object(key string) (string, error) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(key), "/")
	if trimmed == "" {
		return "", errors.New("storage: webdav: an empty key names no object")
	}
	for _, segment := range strings.Split(trimmed, "/") {
		if segment == ".." {
			return "", fmt.Errorf("storage: webdav: key %q is not a valid object name", key)
		}
	}
	return b.rootPath + "/" + b.prefix + trimmed, nil
}

// collectionPath is where a prefix lives, as a path within the share. An empty
// endpoint path and an empty prefix is the share root, which gowebdav spells
// "/".
func (b *webdavBackend) collectionPath(prefix string) string {
	base := b.rootPath + "/" + b.prefix + strings.TrimSuffix(prefix, "/")
	base = strings.TrimRight(base, "/")
	if base == "" {
		return "/"
	}
	return base
}

// containerPath is the collection named by the prefix's first segment.
func (b *webdavBackend) containerPath() string {
	base := b.rootPath + "/" + b.container
	if base == "" {
		return "/"
	}
	return base
}

// Put streams the object to key, replacing whatever was there.
//
// The declared size is authoritative: it becomes the PUT's `Content-Length`, and
// the transport stops after that many bytes. A reader shorter than the size
// fails with an EOF from the transport; a reader longer than it is truncated
// rather than detected. The fs backend can compare the bytes it wrote against
// the size it was given because it writes to a temporary file first, and a
// share cannot. In practice the caller is the uploader (S7-08) handing over a
// finished segment whose size it just stated, so the gap is a documented one
// rather than a live hazard.
//
// No Content-Type is sent. A WebDAV server derives one from the name, and Stat
// reads back whatever it settled on; the extension is the fallback there, which
// is the same rule the fs backend follows. `contentType` is stored by the tiers
// that can store it (s3) and derived by the ones that cannot, and this kind is
// the latter.
func (b *webdavBackend) Put(_ context.Context, key string, r io.Reader, size int64, contentType string) error {
	name, err := b.object(key)
	if err != nil {
		return err
	}
	if size < 0 {
		// No length to declare. Buffer it and send a real Content-Length: a
		// chunked PUT is legal, and plenty of shares answer it badly.
		data, err := io.ReadAll(r)
		if err != nil {
			return fmt.Errorf("storage: webdav: put %s: reading the body: %w", key, err)
		}
		if err := b.client.Write(name, data, 0o644); err != nil {
			return b.mapErr("put", key, err)
		}
		return nil
	}
	if err := b.client.WriteStreamWithLength(name, r, size, 0o644); err != nil {
		return b.mapErr("put", key, err)
	}
	return nil
}

// Get opens the object for reading from byte 0.
//
// gowebdav's ReadStream answers a non-2xx GET as an error, so a missing object
// is a failure at Get rather than at the first Read — the same guarantee the s3
// driver makes with an extra HEAD.
func (b *webdavBackend) Get(_ context.Context, key string) (io.ReadCloser, error) {
	name, err := b.object(key)
	if err != nil {
		return nil, err
	}
	rc, err := b.client.ReadStream(name)
	if err != nil {
		return nil, b.mapErr("get", key, err)
	}
	return rc, nil
}

// Stat reports the object without reading it.
//
// The content type is the server's when the server reports one — a Nextcloud
// that knows a file is `video/mp4` knows better than this process — and the
// extension's when it does not.
func (b *webdavBackend) Stat(_ context.Context, key string) (ObjectInfo, error) {
	name, err := b.object(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	fi, err := b.client.Stat(name)
	if err != nil {
		return ObjectInfo{}, b.mapErr("stat", key, err)
	}
	info := ObjectInfo{Key: key, Size: fi.Size(), ModTime: fi.ModTime()}
	if ct, ok := fi.(interface{ ContentType() string }); ok {
		info.ContentType = ct.ContentType()
	}
	if info.ContentType == "" {
		info.ContentType = contentTypeFor(key)
	}
	return info, nil
}

// Delete removes the object.
//
// gowebdav answers success for a DELETE that found nothing, as S3 does, so a
// second delete is silent here too. That is the protocol saying "it is gone"
// either way, and it is why the shared contract does not require ErrNotFound
// from a second Delete.
func (b *webdavBackend) Delete(_ context.Context, key string) error {
	name, err := b.object(key)
	if err != nil {
		return err
	}
	if err := b.client.Remove(name); err != nil {
		return b.mapErr("delete", key, err)
	}
	return nil
}

// List returns the keys under a prefix, sorted, with the row's own prefix
// stripped back off.
//
// It is a walk of collections, one PROPFIND Depth 1 per directory, because
// gowebdav exposes no recursive listing — `ReadDir` is the Depth 1 PROPFIND, and
// the recursion is the driver's. A missing directory is an empty list, not a
// failure: the sweeper asks about a day before the first upload of that day has
// ever happened.
func (b *webdavBackend) List(ctx context.Context, prefix string) ([]string, error) {
	want := strings.TrimPrefix(strings.TrimSpace(prefix), "/")
	if want != "" && !strings.HasSuffix(want, "/") {
		want += "/"
	}
	var out []string
	if err := b.walk(ctx, b.collectionPath(want), want, &out, 0); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

func (b *webdavBackend) walk(ctx context.Context, dir, keyPrefix string, out *[]string, depth int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > webdavMaxDepth {
		return fmt.Errorf("storage: webdav: list %s: more than %d levels of collections", dir, webdavMaxDepth)
	}
	entries, err := b.client.ReadDir(dir)
	if err != nil {
		if gowebdav.IsErrNotFound(err) {
			return nil
		}
		return b.mapErr("list", dir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if name == "" || name == "." || name == "/" {
			continue
		}
		key := keyPrefix + name
		if e.IsDir() {
			if err := b.walk(ctx, dir+"/"+name, key+"/", out, depth+1); err != nil {
				return err
			}
			continue
		}
		*out = append(*out, key)
	}
	return nil
}

// Ping asks whether the share is there, whether this credential may use it, and
// whether the collection the row names exists on it.
//
// OPTIONS is the probe that says the thing is a WebDAV server: a share pointed
// at the wrong port answers something, and an OPTIONS that does not look like
// WebDAV is the settings mistake worth naming.
func (b *webdavBackend) Ping(ctx context.Context) error {
	if err := b.client.Connect(); err != nil {
		return b.mapErr("reaching the share", b.root, err)
	}
	if b.container == "" {
		return nil
	}
	if _, err := b.client.Stat(b.containerPath()); err != nil {
		if gowebdav.IsErrNotFound(err) {
			return fmt.Errorf("storage: webdav: the collection %q is not on %s: %w",
				b.container, b.root, ErrBucketMissing)
		}
		return b.mapErr("checking the collection", b.container, err)
	}
	return nil
}

// mapErr turns a WebDAV failure into one of the four names, by status.
//
// gowebdav reports a status as an *os.PathError wrapping a StatusError, and the
// status is the whole answer: 401 and 403 are the credential, 404 is the absent
// object, 409 is a parent collection that is not there. Anything else keeps its
// number, because a 507 from a full NAS is not a credential problem and
// inventing a name for it sends the operator to the wrong settings field.
func (b *webdavBackend) mapErr(verb, subject string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("storage: webdav: %s %s: %w", verb, subject, err)
	}

	var pe *os.PathError
	if errors.As(err, &pe) {
		var se gowebdav.StatusError
		if errors.As(pe.Err, &se) {
			switch se.Status {
			case http.StatusUnauthorized, http.StatusForbidden:
				return fmt.Errorf("storage: webdav: %s %s: the share turned us away (%d): %w",
					verb, subject, se.Status, ErrBadCredentials)
			case http.StatusNotFound:
				return fmt.Errorf("storage: webdav: %s %s: %w", verb, subject, ErrNotFound)
			case http.StatusConflict:
				return fmt.Errorf("storage: webdav: %s %s: the collection for this object is not there and could not be created: %w",
					verb, subject, ErrBucketMissing)
			}
			return fmt.Errorf("storage: webdav: %s %s: the share answered %d: %w", verb, subject, se.Status, err)
		}
	}

	if isUnreachable(err) {
		// The underlying error is kept: "discovery_unreachable" is what the
		// console draws, and "x509: certificate signed by unknown authority" is
		// the line the operator reads.
		return fmt.Errorf("storage: webdav: %s %s (%v): %w", verb, subject, err, ErrUnreachable)
	}
	return fmt.Errorf("storage: webdav: %s %s: %w", verb, subject, err)
}

// isUnreachable reports a failure in the transport rather than in an answer: a
// refused or undialable connection, a deadline, a dead mount. Every driver
// needs this test, and the fs backend's errno list is the one that also covers
// a NAS whose export went away mid-day.
func isUnreachable(err error) bool {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.ENOTCONN, syscall.EROFS, syscall.EIO, syscall.ENODEV, syscall.ESTALE, syscall.EHOSTDOWN, syscall.ENETDOWN:
			return true
		}
	}
	var netErr net.Error
	return errors.As(err, &netErr) || errors.Is(err, os.ErrDeadlineExceeded)
}
