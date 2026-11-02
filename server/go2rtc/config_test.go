package go2rtc

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"nvr/server/config"
	"nvr/server/models"
)

// The golden file is the contract. It is asserted as exact bytes on purpose:
// a stream name, a quoting change, a `raw=` spelling or a listener port that
// moves is a config the sidecar applies and the recorder's RTSP URL no longer
// matches, and none of that is visible in a test that only checks "it parsed".

// goldenTwoCameras: one detection-off camera, one demo slug with detection on.
const goldenTwoCameras = `# Rendered by the nvrWay server from the camera rows. Do not hand-edit.
# A stream added here is a stream the database has never heard of: the recorder
# will not record it, detection will not see it, and retention cannot account for
# it. Every camera write re-renders this file (S3-05), so a hand edit is gone by
# the next save.
api: { listen: ":1984", origin: "*" }
rtsp: { listen: ":8554" }
webrtc: { listen: ":8555" }
streams:
  back-patio: ["rtsp://cam2:554/live"]
  demo-doorbell: ["exec:ffmpeg -re -loop 1 -i /fixtures/bus.jpg -pix_fmt yuv420p -c:v libx264 -preset veryfast -f h264 -"]
  demo-doorbell_det: ["exec:ffmpeg -re -loop 1 -i /fixtures/bus.jpg -f mjpeg -r 5 -"]
`

const goldenWithCredentials = `# Rendered by the nvrWay server from the camera rows. Do not hand-edit.
# A stream added here is a stream the database has never heard of: the recorder
# will not record it, detection will not see it, and retention cannot account for
# it. Every camera write re-renders this file (S3-05), so a hand edit is gone by
# the next save.
api: { listen: ":1984", origin: "*", username: "admin", password: "s3cret" }
rtsp: { listen: ":8554" }
webrtc: { listen: ":8555" }
streams:
  demo-doorbell: ["exec:ffmpeg -re -loop 1 -i /fixtures/bus.jpg -pix_fmt yuv420p -c:v libx264 -preset veryfast -f h264 -"]
  demo-doorbell_det: ["exec:ffmpeg -re -loop 1 -i /fixtures/bus.jpg -f mjpeg -r 5 -"]
`

func demoCamera() models.Camera {
	return models.Camera{
		ID:               "cm-demo",
		Slug:             "demo-doorbell",
		Name:             "Demo Doorbell",
		SourceURI:        "exec:ffmpeg -re -loop 1 -i /fixtures/bus.jpg -pix_fmt yuv420p -c:v libx264 -preset veryfast -f h264 -",
		RecordMode:       models.RecordModeContinuous,
		DetectionEnabled: true,
		DetectionFPS:     5,
		DetectionWidth:   640,
		DetectionHeight:  360,
	}
}

func patioCamera() models.Camera {
	return models.Camera{
		ID:               "cm-patio",
		Slug:             "back-patio",
		Name:             "Back Patio",
		SourceURI:        "rtsp://cam2:554/live",
		RecordMode:       models.RecordModeContinuous,
		DetectionEnabled: false,
	}
}

func mjpegDefaults() *config.Config {
	return &config.Config{Go2rtcMjpegWidth: 640, Go2rtcMjpegHeight: 360, Go2rtcMjpegFPS: 5}
}

func TestConfigGoldenTwoCameras(t *testing.T) {
	got, err := RenderConfig([]models.Camera{demoCamera(), patioCamera()}, mjpegDefaults())
	if err != nil {
		t.Fatalf("RenderConfig: %v", err)
	}
	if string(got) != goldenTwoCameras {
		t.Fatalf("golden bytes changed:\n--- got ---\n%s\n--- want ---\n%s", got, goldenTwoCameras)
	}
}

func TestConfigGoldenWithCredentials(t *testing.T) {
	cfg := mjpegDefaults()
	cfg.Go2rtcUsername = "admin"
	cfg.Go2rtcPassword = "s3cret"

	got, err := RenderConfig([]models.Camera{demoCamera()}, cfg)
	if err != nil {
		t.Fatalf("RenderConfig: %v", err)
	}
	if string(got) != goldenWithCredentials {
		t.Fatalf("golden bytes changed:\n--- got ---\n%s\n--- want ---\n%s", got, goldenWithCredentials)
	}
}

func TestConfigHeaderSaysTheFileIsGenerated(t *testing.T) {
	// go2rtc's own web UI edits this file. The rule has to be visible where the
	// operator would look at it, not only in a card.
	got, err := RenderConfig(nil, mjpegDefaults())
	if err != nil {
		t.Fatalf("RenderConfig: %v", err)
	}
	if !strings.HasPrefix(string(got), "# Rendered by the nvrWay server") {
		t.Fatalf("the banner is the first thing in the file: %q", got)
	}
	if !strings.Contains(string(got), "Do not hand-edit") {
		t.Fatalf("say it plainly: %s", got)
	}
}

func TestConfigEmptyRowsRenderAnEmptyStreamMap(t *testing.T) {
	got, err := RenderConfig([]models.Camera{}, mjpegDefaults())
	if err != nil {
		t.Fatalf("RenderConfig: %v", err)
	}
	if !strings.HasSuffix(string(got), "streams: {}\n") {
		t.Fatalf("an installation with no cameras is a sidecar with no streams: %s", got)
	}

	// The empty file must load: go2rtc refuses an `streams:` block with nothing
	// in it, and a config the sidecar cannot load at boot is an installation that
	// starts with no media and no error the operator can see.
	var parsed map[string]any
	if err := yaml.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("the empty render is not loadable YAML: %v", err)
	}
	streams, ok := parsed["streams"].(map[string]any)
	if !ok || len(streams) != 0 {
		t.Fatalf("streams must parse as an empty mapping: %+v", parsed["streams"])
	}
}

func TestConfigIsSortedBySlugWhateverTheRowOrder(t *testing.T) {
	// The file is rewritten on every camera write. Map-iteration order would turn
	// "I added a camera" into a full-file shuffle, so the diff cannot answer
	// "did my change land?".
	forward, err := RenderConfig([]models.Camera{demoCamera(), patioCamera()}, mjpegDefaults())
	if err != nil {
		t.Fatalf("RenderConfig: %v", err)
	}
	backward, err := RenderConfig([]models.Camera{patioCamera(), demoCamera()}, mjpegDefaults())
	if err != nil {
		t.Fatalf("RenderConfig: %v", err)
	}
	if string(forward) != string(backward) {
		t.Fatal("row order must not move bytes")
	}

	lines := strings.Split(string(forward), "\n")
	var slugs []string
	for _, line := range lines {
		if strings.HasPrefix(line, "  ") && strings.Contains(line, ": [") {
			slugs = append(slugs, strings.TrimSpace(strings.Split(line, ":")[0]))
		}
	}
	if len(slugs) != 3 {
		t.Fatalf("three streams (2 cameras, 1 detection tap): %v", slugs)
	}
	for i := 1; i < len(slugs); i++ {
		if slugs[i-1] >= slugs[i] {
			t.Fatalf("slugs are emitted sorted: %v", slugs)
		}
	}
}

func TestConfigDoesNotReorderTheRowsItWasGiven(t *testing.T) {
	// A pure function: the caller's slice is the caller's.
	rows := []models.Camera{demoCamera(), patioCamera()}
	if _, err := RenderConfig(rows, mjpegDefaults()); err != nil {
		t.Fatalf("RenderConfig: %v", err)
	}
	if rows[0].Slug != "demo-doorbell" || rows[1].Slug != "back-patio" {
		t.Fatalf("sorting must not touch the caller's rows: %s, %s", rows[0].Slug, rows[1].Slug)
	}
}

func TestConfigCarriesTheRowWidthHeightAndRateExactly(t *testing.T) {
	camera := demoCamera()
	camera.Slug = "garden"
	camera.DetectionWidth = 1280
	camera.DetectionHeight = 720
	camera.DetectionFPS = 3

	got, err := RenderConfig([]models.Camera{camera}, mjpegDefaults())
	if err != nil {
		t.Fatalf("RenderConfig: %v", err)
	}
	want := `garden_det: ["ffmpeg:garden#video=mjpeg#width=1280#height=720#raw=-r 3"]`
	if !strings.Contains(string(got), want) {
		t.Fatalf("the transcode request carries the row's numbers exactly:\n%s", got)
	}
}

func TestConfigFallsBackToTheMjpegDefaults(t *testing.T) {
	camera := demoCamera()
	camera.Slug = "garden"
	camera.DetectionWidth = 0
	camera.DetectionHeight = 0
	camera.DetectionFPS = 0

	got, err := RenderConfig([]models.Camera{camera}, mjpegDefaults())
	if err != nil {
		t.Fatalf("RenderConfig: %v", err)
	}
	want := `garden_det: ["ffmpeg:garden#video=mjpeg#width=640#height=360#raw=-r 5"]`
	if !strings.Contains(string(got), want) {
		t.Fatalf("a zero on the row means 'use the installation default':\n%s", got)
	}
}

func TestConfigOmitsTheDetectionTapWhenDetectionIsOff(t *testing.T) {
	got, err := RenderConfig([]models.Camera{patioCamera()}, mjpegDefaults())
	if err != nil {
		t.Fatalf("RenderConfig: %v", err)
	}
	if strings.Contains(string(got), "_det") {
		t.Fatalf("detection off emits no tap: the sidecar must not be transcoding frames nobody reads:\n%s", got)
	}
	if !strings.Contains(string(got), `back-patio: ["rtsp://cam2:554/live"]`) {
		t.Fatalf("the camera itself is still streamed (the recorder reads it): %s", got)
	}
}

func TestConfigDemoSlugTapsTheFixtureNotTheTranscode(t *testing.T) {
	// DEMO-ONLY (S10-02): the demo source is a looped JPEG, so tapping it through
	// the ffmpeg transcode decodes H.264 and re-encodes mjpeg every frame to
	// produce a picture that never changes.
	got, err := RenderConfig([]models.Camera{demoCamera()}, mjpegDefaults())
	if err != nil {
		t.Fatalf("RenderConfig: %v", err)
	}
	want := `demo-doorbell_det: ["exec:ffmpeg -re -loop 1 -i /fixtures/bus.jpg -f mjpeg -r 5 -"]`
	if !strings.Contains(string(got), want) {
		t.Fatalf("the demo tap reads the fixture directly:\n%s", got)
	}
	if strings.Contains(string(got), "demo-doorbell#video=mjpeg") {
		t.Fatal("the demo slug must not get the generic transcode tap")
	}
}

func TestConfigDemoRuleIsDocumentedAsDemoOnly(t *testing.T) {
	// The card's exit criterion: the rule is documented as demo-only in the file,
	// so a reader of config.go cannot mistake /fixtures for a production path.
	source, err := detectionSource(models.Camera{ID: "x"}, DemoSlug, mjpegDefaults())
	if err != nil {
		t.Fatalf("detectionSource: %v", err)
	}
	if source != DemoDetSource {
		t.Fatalf("the demo rule: %q", source)
	}

	file, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("reading config.go: %v", err)
	}
	if !strings.Contains(string(file), "DEMO-ONLY") {
		t.Fatal("the demo rule must be labelled as demo-only in the source it lives in")
	}
	if !strings.Contains(string(file), "/fixtures") {
		t.Fatal("the file must name the dev-only mount the rule depends on")
	}
}

func TestConfigRejectsSlugsThatAreNotStreamNames(t *testing.T) {
	for _, slug := range []string{"", " Front-Door ", "Front-Door", "-door", "a", strings.Repeat("d", 33), "door door", "door/../x"} {
		camera := patioCamera()
		camera.Slug = slug
		_, err := RenderConfig([]models.Camera{camera}, mjpegDefaults())
		if err == nil {
			t.Fatalf("slug %q is not a stream name and must not render", slug)
		}
		if !strings.Contains(err.Error(), "usable stream name") {
			t.Fatalf("slug %q: the message names the pattern, got %v", slug, err)
		}
		if !strings.Contains(err.Error(), "^[a-z0-9][a-z0-9-]{1,31}$") {
			t.Fatalf("slug %q: the message quotes the pattern, got %v", slug, err)
		}
	}
}

func TestConfigAcceptsTheSmallestAndLongestLegalSlugs(t *testing.T) {
	for _, slug := range []string{"ab", "0door", "a1", strings.Repeat("d", 32), "front-door-2"} {
		camera := patioCamera()
		camera.Slug = slug
		if _, err := RenderConfig([]models.Camera{camera}, mjpegDefaults()); err != nil {
			t.Fatalf("slug %q is legal: %v", slug, err)
		}
	}
}

func TestConfigRejectsTwoRowsOnOneSlug(t *testing.T) {
	first, second := patioCamera(), patioCamera()
	second.ID = "cm-patio-2"

	_, err := RenderConfig([]models.Camera{first, second}, mjpegDefaults())
	if err == nil {
		t.Fatal("two rows cannot share a stream name")
	}
	if !strings.Contains(err.Error(), "back-patio") || !strings.Contains(err.Error(), "cm-patio-2") {
		t.Fatalf("say which slug and which camera: %v", err)
	}
}

func TestConfigRejectsACameraWithNoSource(t *testing.T) {
	camera := patioCamera()
	camera.SourceURI = "   "

	_, err := RenderConfig([]models.Camera{camera}, mjpegDefaults())
	if err == nil {
		t.Fatal("a camera with no source is not a stream")
	}
	if !strings.Contains(err.Error(), "source_uri is required") {
		t.Fatalf("%v", err)
	}
}

func TestConfigRefusesAFrameRateOfZero(t *testing.T) {
	// GO2RTC_MJPEG_FPS=0 is a legal read (config_test asserts an explicit 0 stays
	// 0), and `raw=-r 0` is a stream with no frames: the tap would open, produce
	// nothing, and detection would silently never run.
	camera := demoCamera()
	camera.Slug = "garden"
	camera.DetectionFPS = 0

	_, err := RenderConfig([]models.Camera{camera}, &config.Config{
		Go2rtcMjpegWidth: 640, Go2rtcMjpegHeight: 360, Go2rtcMjpegFPS: 0,
	})
	if err == nil {
		t.Fatal("a detection rate of zero is not a request")
	}
	if !strings.Contains(err.Error(), "GO2RTC_MJPEG_FPS") {
		t.Fatalf("name the knob the operator turns: %v", err)
	}
}

func TestConfigRefusesAFrameSizeOfZero(t *testing.T) {
	camera := demoCamera()
	camera.Slug = "garden"
	camera.DetectionWidth = 0
	camera.DetectionHeight = 0

	_, err := RenderConfig([]models.Camera{camera}, &config.Config{
		Go2rtcMjpegWidth: 0, Go2rtcMjpegHeight: 0, Go2rtcMjpegFPS: 5,
	})
	if err == nil {
		t.Fatal("a detection frame of zero is not a request")
	}
	if !strings.Contains(err.Error(), "GO2RTC_MJPEG_WIDTH") {
		t.Fatalf("name the knob: %v", err)
	}
}

func TestConfigParsesAsThePlannedShape(t *testing.T) {
	got, err := RenderConfig([]models.Camera{demoCamera(), patioCamera()}, mjpegDefaults())
	if err != nil {
		t.Fatalf("RenderConfig: %v", err)
	}

	var doc struct {
		API struct {
			Listen string `yaml:"listen"`
			Origin string `yaml:"origin"`
		} `yaml:"api"`
		RTSP struct {
			Listen string `yaml:"listen"`
		} `yaml:"rtsp"`
		WebRTC struct {
			Listen string `yaml:"listen"`
		} `yaml:"webrtc"`
		Streams map[string][]string `yaml:"streams"`
	}
	if err := yaml.Unmarshal(got, &doc); err != nil {
		t.Fatalf("the render is not loadable YAML: %v\n%s", err, got)
	}
	if doc.API.Listen != ":1984" || doc.API.Origin != "*" {
		t.Fatalf("api section: %+v", doc.API)
	}
	if doc.RTSP.Listen != ":8554" {
		t.Fatalf("rtsp section: %+v", doc.RTSP)
	}
	if doc.WebRTC.Listen != ":8555" {
		t.Fatalf("webrtc section: %+v", doc.WebRTC)
	}
	if len(doc.Streams) != 3 {
		t.Fatalf("streams: %+v", doc.Streams)
	}
	if got := doc.Streams["back-patio"]; len(got) != 1 || got[0] != "rtsp://cam2:554/live" {
		t.Fatalf("the camera stream is the source verbatim: %v", got)
	}
	if got := doc.Streams["demo-doorbell_det"]; len(got) != 1 || got[0] != DemoDetSource {
		t.Fatalf("the demo tap: %v", got)
	}
}

func TestConfigQuotesASourceThatIsFullOfYAMLStructure(t *testing.T) {
	// A source URI is dense with characters YAML reads as structure: `:`, `#`,
	// `*`, spaces, and an `&` query separator. Quoting must round-trip it exactly.
	camera := patioCamera()
	camera.SourceURI = `rtsp://user:pa"ss@cam:554/live?channel=1&subtype=0 #1`

	got, err := RenderConfig([]models.Camera{camera}, mjpegDefaults())
	if err != nil {
		t.Fatalf("RenderConfig: %v", err)
	}

	var doc struct {
		Streams map[string][]string `yaml:"streams"`
	}
	if err := yaml.Unmarshal(got, &doc); err != nil {
		t.Fatalf("render not loadable: %v\n%s", err, got)
	}
	if got := doc.Streams["back-patio"][0]; got != camera.SourceURI {
		t.Fatalf("the source must survive quoting verbatim:\n got %q\nwant %q", got, camera.SourceURI)
	}
}

func TestConfigEmitsOneEntryPerCameraAndOnePerDetectionTap(t *testing.T) {
	rows := []models.Camera{
		demoCamera(),
		patioCamera(),
	}
	second := demoCamera()
	second.ID = "cm-garden"
	second.Slug = "garden"
	rows = append(rows, second)

	got, err := RenderConfig(rows, mjpegDefaults())
	if err != nil {
		t.Fatalf("RenderConfig: %v", err)
	}

	var doc struct {
		Streams map[string][]string `yaml:"streams"`
	}
	if err := yaml.Unmarshal(got, &doc); err != nil {
		t.Fatalf("render not loadable: %v", err)
	}
	if len(doc.Streams) != 5 {
		t.Fatalf("3 cameras + 2 detection taps: %+v", doc.Streams)
	}
	for _, slug := range []string{"back-patio", "demo-doorbell", "demo-doorbell_det", "garden", "garden_det"} {
		if _, ok := doc.Streams[slug]; !ok {
			t.Fatalf("stream %s missing: %v", slug, doc.Streams)
		}
	}
	if _, ok := doc.Streams["back-patio_det"]; ok {
		t.Fatal("a detection-off camera has no tap")
	}
}
