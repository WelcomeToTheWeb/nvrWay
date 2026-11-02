package go2rtc

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"nvr/server/config"
	"nvr/server/models"
)

// The renderer: camera rows in, the whole go2rtc.yaml out, as a pure function.
//
// The file is generated, never edited. go2rtc's own web UI has a YAML editor
// pointed at the same file, and every stream it adds there is a stream the
// database has never heard of — recording skips it, detection never sees it, and
// the sweeper cannot account for it. So the banner below travels in the bytes
// (S3-05: no code path but this one writes go2rtc's config).
//
// Determinism is the other reason it is a function and not a template: the file
// is rewritten on every camera write, so a renderer that ordered streams by map
// iteration would turn "I added a camera" into a full-file shuffle, and the
// question every operator asks — "did my change land?" — would be unanswerable
// from the diff. Streams are sorted by slug, and nothing else is emitted.
//
// The shape is the plan's (Step 3) and docker/go2rtc.yaml.template's:
//
//	api:    { listen: ":1984", origin: "*" }
//	rtsp:   { listen: ":8554" }
//	webrtc: { listen: ":8555" }
//	streams:
//	  <slug>: ["<source_uri>"]
//	  <slug>_det: ["ffmpeg:<slug>#video=mjpeg#width=640#height=360#raw=-r 5"]
//
// `<slug>_det` is the detection tap: go2rtc transcodes the camera to mjpeg at
// the row's size and rate and the worker reads frames from it. The slug is the
// stream name, verbatim — there is no mapping table, which is why the slug
// pattern is validated here rather than escaped.

// DetectionSuffix names the detection tap of a camera: <slug>_det. It is the
// name the worker is told to read (S5-09) and the name the probe reports, so it
// is part of the wire contract, not a rendering detail.
const DetectionSuffix = "_det"

// DemoSlug is the one slug with a demo-only rendering rule, and DemoDetSource is
// that rule. DEMO-ONLY: it exists so `scripts/dev.sh --demo` proves the whole
// pipeline with no physical camera (S10-02), and `/fixtures` is a dev-only mount
// (`./testdata:/fixtures:ro` in the dev compose) that a production installation
// does not have. It is not a feature: a camera an operator slugs
// `demo-doorbell` in production gets a source naming a file that is not there.
//
// The rule replaces the *_det source only, for a reason worth stating: the demo
// source is a looped JPEG, so tapping it through `ffmpeg:<slug>#video=mjpeg`
// would decode H.264 and re-encode mjpeg on every frame to produce a picture
// that never changes. Reading the fixture directly costs nothing and keeps the
// detection rate honest.
const (
	DemoSlug      = "demo-doorbell"
	DemoDetSource = "exec:ffmpeg -re -loop 1 -i /fixtures/bus.jpg -f mjpeg -r 5 -"
)

// The listeners are the plan's, not configuration. compose publishes these
// ports; GO2RTC_URL / GO2RTC_RTSP_URL / GO2RTC_PUBLIC_URL say where the server
// and the browsers reach them, which is a different question from where go2rtc
// binds. origin is "*" because the console talks to go2rtc directly from the
// browser (S8-05): an origin-restricted API is a live view that only works on
// the sidecar's own host.
const (
	apiListen    = ":1984"
	rtspListen   = ":8554"
	webrtcListen = ":8555"
)

// Header is the banner every generated file carries.
const header = `# Rendered by the nvrWay server from the camera rows. Do not hand-edit.
# A stream added here is a stream the database has never heard of: the recorder
# will not record it, detection will not see it, and retention cannot account for
# it. Every camera write re-renders this file (S3-05), so a hand edit is gone by
# the next save.
`

// RenderConfig renders the whole go2rtc.yaml for these camera rows.
//
// It renders exactly what it is given — the caller decides which rows are in
// scope (S3-05 feeds it the active cameras), and a renderer that filtered rows
// on its own would be a second policy nobody can find.
//
// The errors are input rejections, not go2rtc answers: a bad slug, a missing
// source, a size of zero. They reach the API as a 422 naming the camera (S3-03),
// because "camera back-patio: source_uri is required" is fixable by the operator
// and "go2rtc rejected the request" is not.
func RenderConfig(cameras []models.Camera, cfg *config.Config) ([]byte, error) {
	order := make([]int, len(cameras))
	for i := range cameras {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return cameras[order[a]].Slug < cameras[order[b]].Slug })

	seen := map[string]int{}
	var out strings.Builder
	out.WriteString(header)
	out.WriteString(renderListeners(cfg))

	if len(cameras) == 0 {
		// `streams: {}` is the honest empty file: go2rtc starts with no streams,
		// and an empty `streams:` block is the one shape its loader refuses.
		out.WriteString("streams: {}\n")
		return []byte(out.String()), nil
	}

	out.WriteString("streams:\n")
	for _, i := range order {
		camera := cameras[i]
		slug := strings.TrimSpace(camera.Slug)
		if !models.ValidSlug(slug) {
			return nil, fmt.Errorf("camera %s: slug %q is not a usable stream name (^[a-z0-9][a-z0-9-]{1,31}$)",
				camera.ID, camera.Slug)
		}
		if first, taken := seen[slug]; taken {
			return nil, fmt.Errorf("camera %s: slug %q is already used by camera %s — the slug is the stream name, so two rows cannot share it",
				camera.ID, slug, cameras[first].ID)
		}
		seen[slug] = i

		source := strings.TrimSpace(camera.SourceURI)
		if source == "" {
			return nil, fmt.Errorf("camera %s: source_uri is required (slug %q has nothing to read)", camera.ID, slug)
		}
		out.WriteString(fmt.Sprintf("  %s: [%s]\n", slug, yamlString(source)))

		if !camera.DetectionEnabled {
			continue
		}
		detSource, err := detectionSource(camera, slug, cfg)
		if err != nil {
			return nil, err
		}
		out.WriteString(fmt.Sprintf("  %s%s: [%s]\n", slug, DetectionSuffix, yamlString(detSource)))
	}

	return []byte(out.String()), nil
}

// renderListeners writes the three sections the sidecar binds, plus the API
// credentials when they are configured.
//
// The credentials belong in the file: GO2RTC_USERNAME / GO2RTC_PASSWORD is the
// pair the server's client signs requests with (S3-01), and go2rtc only checks a
// password it was given. Rendering an API with no username is a sidecar whose
// "authenticated" API answers anyone on the network — and the pair in .env would
// be a knob with no effect.
func renderListeners(cfg *config.Config) string {
	var out strings.Builder
	out.WriteString("api: { listen: " + yamlString(apiListen) + ", origin: " + yamlString("*"))
	if cfg != nil && strings.TrimSpace(cfg.Go2rtcUsername) != "" && strings.TrimSpace(cfg.Go2rtcPassword) != "" {
		out.WriteString(", username: " + yamlString(cfg.Go2rtcUsername) +
			", password: " + yamlString(cfg.Go2rtcPassword))
	}
	out.WriteString(" }\n")
	out.WriteString("rtsp: { listen: " + yamlString(rtspListen) + " }\n")
	out.WriteString("webrtc: { listen: " + yamlString(webrtcListen) + " }\n")
	return out.String()
}

// detectionSource builds the `<slug>_det` source: the transcode request, with the
// camera row's size and rate and the GO2RTC_MJPEG_* defaults underneath.
//
// The `raw=-r N` tail is where the rate is handed to the encoder, and the whole
// string is what fails when it is wrong: a source go2rtc cannot open is a camera
// whose detection silently never runs, so the numbers are validated here.
//
// A zero on the row means "use the installation default". A zero after the
// fallback is not a request at all — `-r 0` is a stream with no frames — and
// GO2RTC_MJPEG_FPS=0 is a legal read (config_test.go asserts it stays 0), so the
// renderer is the place that stops it.
func detectionSource(camera models.Camera, slug string, cfg *config.Config) (string, error) {
	// DEMO-ONLY rule, the one in this file (see DemoSlug): the demo camera's tap
	// reads the fixture JPEG directly.
	if slug == DemoSlug {
		return DemoDetSource, nil
	}

	width, height, fps := camera.DetectionWidth, camera.DetectionHeight, camera.DetectionFPS
	if cfg != nil {
		if width <= 0 {
			width = cfg.Go2rtcMjpegWidth
		}
		if height <= 0 {
			height = cfg.Go2rtcMjpegHeight
		}
		if fps <= 0 {
			fps = cfg.Go2rtcMjpegFPS
		}
	}
	if width <= 0 {
		return "", fmt.Errorf("camera %s: no detection width (set detection_width, or GO2RTC_MJPEG_WIDTH)", camera.ID)
	}
	if height <= 0 {
		return "", fmt.Errorf("camera %s: no detection height (set detection_height, or GO2RTC_MJPEG_HEIGHT)", camera.ID)
	}
	if fps <= 0 {
		return "", fmt.Errorf("camera %s: no detection frame rate (set detection_fps, or GO2RTC_MJPEG_FPS; 0 asks go2rtc for a stream with no frames)", camera.ID)
	}

	return fmt.Sprintf("ffmpeg:%s#video=mjpeg#width=%d#height=%d#raw=-r %d", slug, width, height, fps), nil
}

// yamlString writes one YAML double-quoted scalar.
//
// Double quotes, always: a source URI is full of characters YAML reads as
// structure — `:`, `#`, `*`, `&`, `?`, spaces — and a stream written unquoted is
// a config file that parses as something other than what the camera row said.
// strconv.Quote's escapes are a subset of YAML's double-quoted escapes, so the
// bytes round-trip.
func yamlString(s string) string {
	return strconv.Quote(s)
}
