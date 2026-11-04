package storage

// The `s3` driver: minio-go, path-style, always.
//
// Path-style is not a preference, it is the plan's decision (Step 7): MinIO,
// Cloudflare R2, Backblaze B2, Wasabi and every self-hosted endpoint work
// path-style, while virtual-host style needs a wildcard DNS record and a
// TLS certificate covering it — which is exactly the thing a media tier on a
// LAN does not have. So `BucketLookupPath` is set unconditionally and the
// request URL is `https://host/<bucket>/<key>`, which is also what makes the
// Settings page's "verify this path" answerable from a single endpoint.
//
// Two behaviours are not the tier's and are made the driver's:
//
//   - **`Get` answers a missing object at `Get`, not at the first `Read`.**
//     minio's `GetObject` returns an object whose error only surfaces when you
//     read it, and the playback proxy has already written the 200 headers by
//     then. So `Get` stats the object first and closes the handle if it is not
//     there, which is one extra HEAD per playback request and a named 404
//     instead of a truncated download.
//   - **A credential failure is `bad credentials`, not a stack trace.** The
//     mapping is by the S3 error code and HTTP status, so a MinIO release that
//     rewords its message cannot break the console.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"nvr/server/models"
)

// s3DefaultRegion is the region assumed when the row leaves it empty. Every S3
// implementation accepts it, and naming it avoids the client's location
// lookup — a round trip to a host the operator has not told us the region of,
// on the request path.
const s3DefaultRegion = "us-east-1"

// s3MaxRetries bounds minio's own retry loop. The default is ten, which is
// right for a transfer and wrong for a connection test: the Settings page
// asked a question and the answer is "nothing answered", not a thirty-second
// spinner. The uploader's backoff (S7-08) is the retry that matters.
const s3MaxRetries = 2

// s3Backend is a bucket addressed path-style.
type s3Backend struct {
	client *minio.Client
	bucket string
	// prefix is the row's `prefix`, prepended to every key. The key layout
	// (S7-07) already starts with `nvr/`, so an empty prefix is the normal
	// case and a non-empty one is a bucket shared with something else.
	prefix string
}

func init() {
	registerDriver(models.StorageKindS3, openS3)
}

// openS3 builds the client from the row, with the credentials unsealed by
// `Open` (S7-01, S7-02).
func openS3(ctx context.Context, a *openArgs) (Backend, error) {
	row := a.Row

	endpoint, err := s3Endpoint(row.Endpoint)
	if err != nil {
		return nil, err
	}
	bucket := strings.TrimSpace(row.Bucket)
	if bucket == "" {
		return nil, fmt.Errorf("bucket is empty: %w", ErrBucketMissing)
	}

	region := strings.TrimSpace(row.Region)
	if region == "" {
		region = s3DefaultRegion
	}

	opts := &minio.Options{
		// The static credentials are the unsealed pair. An empty access key is
		// not turned into nil: a bucket that needs no credential is the row
		// with empty keys, and sending an unsigned request is a different
		// behaviour from sending one signed with an empty key.
		Creds:        credentials.NewStaticV4(a.Secret.AccessKey, a.Secret.SecretKey, ""),
		Secure:       !row.InsecureTLS,
		Region:       region,
		BucketLookup: minio.BucketLookupPath,
		MaxRetries:   s3MaxRetries,
	}

	client, err := minio.New(endpoint, opts)
	if err != nil {
		// A malformed endpoint is a settings error, and it is refused at open
		// rather than at the first upload tick.
		return nil, fmt.Errorf("endpoint %q: %w", row.Endpoint, err)
	}

	return &s3Backend{
		client: client,
		bucket: bucket,
		prefix: joinPrefix(row.Prefix),
	}, nil
}

// joinPrefix normalises a row prefix into something that can be concatenated
// with a key: no leading slash, trailing slash if non-empty.
func joinPrefix(p string) string {
	p = strings.TrimPrefix(strings.TrimSpace(p), "/")
	if p == "" {
		return ""
	}
	p = strings.TrimSuffix(p, "/")
	if p == "" {
		return ""
	}
	return p + "/"
}

// s3Endpoint strips the scheme and the trailing slash from the row's endpoint.
//
// `insecure_tls` is what decides whether TLS is used (the plan's rule,
// `EnableSSL = !InsecureTLS`), so an `http://` written in the endpoint field
// is decoration and is removed rather than allowed to contradict the checkbox.
// Leaving the scheme in would also produce a client that dials
// `http://http://host:9000/…`, which is the failure this function exists to
// prevent.
func s3Endpoint(raw string) (string, error) {
	endpoint := strings.TrimSpace(raw)
	if endpoint == "" {
		return "", errors.New("storage: s3: endpoint is empty")
	}
	for _, scheme := range []string{"https://", "http://", "s3://"} {
		endpoint = strings.TrimPrefix(endpoint, scheme)
	}
	endpoint = strings.TrimRight(endpoint, "/")
	if endpoint == "" {
		return "", errors.New("storage: s3: endpoint is empty")
	}
	if strings.Contains(endpoint, "/") {
		// A path in the endpoint is the mistake an operator makes after
		// copying a browser URL. Path-style puts the bucket in the path, so a
		// path here would be sent twice.
		return "", fmt.Errorf("storage: s3: endpoint %q must be host or host:port; the bucket goes in its own field", raw)
	}
	return endpoint, nil
}

// object is the bucket-side name for a key.
func (b *s3Backend) object(key string) (string, error) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(key), "/")
	if trimmed == "" {
		return "", errors.New("storage: s3: an empty key names no object")
	}
	for _, segment := range strings.Split(trimmed, "/") {
		if segment == ".." {
			return "", fmt.Errorf("storage: s3: key %q is not a valid object name", key)
		}
	}
	return b.prefix + trimmed, nil
}

// Put stores the object with the content type the caller named.
//
// The type is stored, not guessed: the playback proxy answers it, and a bucket
// that returns `application/octet-stream` for a clip makes the browser download
// the file instead of playing it.
func (b *s3Backend) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	name, err := b.object(key)
	if err != nil {
		return err
	}
	opts := minio.PutObjectOptions{ContentType: contentType}
	if opts.ContentType == "" {
		opts.ContentType = "application/octet-stream"
	}
	if _, err := b.client.PutObject(ctx, b.bucket, name, r, size, opts); err != nil {
		return b.mapErr("put", key, err)
	}
	return nil
}

// Get opens the object for reading from byte 0, answering ErrNotFound for an
// object that is not there.
//
// The stat is not waste: minio's GetObject returns a handle whose error only
// appears on the first Read, and by then the clip route has written its 200
// headers. One HEAD turns "a truncated download that plays as silence" into
// the named 404 the console draws.
func (b *s3Backend) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	name, err := b.object(key)
	if err != nil {
		return nil, err
	}
	obj, err := b.client.GetObject(ctx, b.bucket, name, minio.GetObjectOptions{})
	if err != nil {
		return nil, b.mapErr("get", key, err)
	}
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		return nil, b.mapErr("get", key, err)
	}
	return obj, nil
}

// Stat reports the object without reading it.
func (b *s3Backend) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	name, err := b.object(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	info, err := b.client.StatObject(ctx, b.bucket, name, minio.StatObjectOptions{})
	if err != nil {
		return ObjectInfo{}, b.mapErr("stat", key, err)
	}
	return ObjectInfo{
		Key:         key,
		Size:        info.Size,
		ModTime:     info.LastModified.UTC(),
		ContentType: info.ContentType,
	}, nil
}

// Delete removes the object.
//
// S3's DELETE is idempotent: removing an object that is already gone answers
// 204 and no error. That is the protocol's answer, not a driver bug, and it is
// why the shared contract does not require a second Delete to report
// `ErrNotFound` — the sweeper clears the row's `remote_key` either way.
func (b *s3Backend) Delete(ctx context.Context, key string) error {
	name, err := b.object(key)
	if err != nil {
		return err
	}
	if err := b.client.RemoveObject(ctx, b.bucket, name, minio.RemoveObjectOptions{}); err != nil {
		return b.mapErr("delete", key, err)
	}
	return nil
}

// List returns the keys under a prefix, sorted, with the row's own prefix
// stripped back off so the caller sees the keys it addressed.
func (b *s3Backend) List(ctx context.Context, prefix string) ([]string, error) {
	want := b.prefix + strings.TrimPrefix(strings.TrimSpace(prefix), "/")
	out := make([]string, 0, 32)
	for item := range b.client.ListObjects(ctx, b.bucket, minio.ListObjectsOptions{
		Prefix:    want,
		Recursive: true,
	}) {
		if item.Err != nil {
			return nil, b.mapErr("list", prefix, item.Err)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		key := strings.TrimPrefix(item.Key, b.prefix)
		// A directory marker — a zero-length key ending in `/` that some
		// implementations create to stand in for a folder — is not an object
		// the queue shelved, and listing it would offer the console a file with
		// no bytes and no type.
		if strings.HasSuffix(key, "/") {
			continue
		}
		out = append(out, key)
	}
	sort.Strings(out)
	return out, nil
}

// Ping asks whether the bucket is there and this credential may use it.
//
// The three answers are the three the Settings page draws (S7-10): nothing
// answered, the credential was turned away, and the bucket named in the
// settings does not exist.
func (b *s3Backend) Ping(ctx context.Context) error {
	exists, err := b.client.BucketExists(ctx, b.bucket)
	if err != nil {
		return b.mapErr("reaching the bucket", b.bucket, err)
	}
	if !exists {
		return fmt.Errorf("the bucket %q is not there: %w", b.bucket, ErrBucketMissing)
	}
	return nil
}

// mapErr turns a minio failure into one of the four names, by code and status.
//
// The credential is never in the message: minio's ErrorResponse carries the
// code, the message and the resource, and none of them is the secret. The
// access key can appear in a server's message ("access key `AKIA…` does not
// exist"), which is the server's choice and not a leak by this process, but
// the secret never travels anywhere but the signature.
func (b *s3Backend) mapErr(verb, subject string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		// A cancelled request is not a tier failure: the caller walked away.
		return fmt.Errorf("storage: s3: %s %s: %w", verb, subject, err)
	}

	resp := minio.ToErrorResponse(err)
	switch {
	case resp.Code == "NoSuchBucket":
		return fmt.Errorf("storage: s3: %s %s: the bucket %q is not there: %w", verb, subject, b.bucket, ErrBucketMissing)

	case resp.Code == "NoSuchKey" || resp.StatusCode == http.StatusNotFound:
		// An object operation's 404 is the absence of the object, which is not
		// one of the four connection names — the caller answers a named 404 of
		// its own.
		return fmt.Errorf("storage: s3: %s %s: %w", verb, subject, ErrNotFound)

	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized,
		resp.Code == "AccessDenied", resp.Code == "SignatureDoesNotMatch",
		resp.Code == "InvalidAccessKeyId", resp.Code == "NoSuchEntity",
		resp.Code == "InvalidToken", resp.Code == "ExpiredToken":
		return fmt.Errorf("storage: s3: %s %s: %w", verb, subject, ErrBadCredentials)

	case resp.StatusCode == 0 && resp.Code == "":
		// No S3 error document at all: the dial failed, TLS did not verify, the
		// name did not resolve, or the deadline passed mid-request. The
		// underlying error is kept in the message — "discovery_unreachable" is
		// the answer the console draws, but "x509: certificate signed by unknown
		// authority" is the line the operator reads, and dropping it turns a
		// certificate problem into a mystery.
		return fmt.Errorf("storage: s3: %s %s (%v): %w", verb, subject, err, ErrUnreachable)

	default:
		return fmt.Errorf("storage: s3: %s %s (%s %d): %w", verb, subject, resp.Code, resp.StatusCode, err)
	}
}
