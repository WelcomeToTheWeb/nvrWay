// Package deps pins the dependency set the v1 plan commits to, from Step 1.
//
// `go mod tidy` drops every require no package imports, and the plan's pinned
// versions are a decision, not a starting point: the storage drivers arrive in
// Step 7, the go2rtc config renderer in Step 3, JWT/WS in Step 2. Blank imports
// here keep those exact pins in go.mod and go.sum from the first commit, so a
// Step 7 commit changes code — not a dependency set resolved under time
// pressure, and not a version that drifted in the meantime.
//
// Nothing in this package is imported by anything that is built: the server
// binary never links a storage driver it does not use.
package deps

import (
	// Step 2 — auth: HS256 JWTs, argon2id password hashes, WS hub.
	_ "github.com/golang-jwt/jwt/v5"
	_ "golang.org/x/crypto/argon2"
	_ "nhooyr.io/websocket"

	// Step 3 — rendering go2rtc.yaml from camera rows.
	_ "gopkg.in/yaml.v3"

	// Step 7 — storage drivers (S3, WebDAV, SFTP, local/NFS) and the sealing
	// key derivation (SHA-256), plus ssh for the SFTP driver.
	_ "github.com/minio/minio-go/v7"
	_ "github.com/pkg/sftp"
	_ "github.com/studio-b12/gowebdav"
	_ "golang.org/x/crypto/ssh"

	// Step 11 — the in-test S3 server storage/s3_test.go talks to. A test-only
	// dependency, so it is pinned here rather than imported from a _test.go file
	// that does not exist yet.
	_ "github.com/johannesboyne/gofakes3"
)
