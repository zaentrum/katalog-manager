package library

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// The schemas' validator and what it reads, vendored in testdata/contract at
// 71999959 and pinned file by file (SHA256SUMS, its own sha256 pinned
// here): tools/validate-library-v2.py, the library's schemas
// (library/v2/*.schema.json) and the example library's series and people,
// which the record logic wrote.
const (
	contractDir         = "testdata/contract"
	contractSumsSHA256  = "4b663b41ff0e893881d0d55e3cc7cdf48ea31ed0943cb697182047d76971feb0"
	validatorSHA256     = "351434af70627b79a06983eb8ad07ab9bd78fd8bcc17a6745db593c1b624bdb6"
	exampleSeries       = "8dae7353-070a-5ff5-8ee3-a2d54615f984"
	exampleEpisode      = "fb7dca21-b87b-5a41-af9f-a837fc5f0d1c"
	exampleVersion      = "d40ee9b9-0cf6-5e72-881e-84eb858082b7"
	exampleSource       = "9c9e9620-1e82-58df-8460-bf8272b35982"
	examplePackage      = "da8cf340-e17f-557b-8a56-0ffee7232b4a"
	validatorPythonEnv  = "KATALOG_TEST_PYTHON"
	exampleEpisodeTitle = "Example Episode"
)

// contractFiles checks the vendored files against SHA256SUMS, and it
// against its pin.
func contractFiles(t *testing.T) {
	t.Helper()
	sums := filepath.Join(contractDir, "SHA256SUMS")
	b, err := os.ReadFile(sums)
	if err != nil {
		t.Fatal(err)
	}
	if got := SHA256(b); got != contractSumsSHA256 {
		t.Fatalf("%s: sha256 %s; want %s, the vendored files as listed", sums, got, contractSumsSHA256)
	}
	listed := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		sum, name, ok := strings.Cut(sc.Text(), "  ")
		if !ok {
			t.Fatalf("%s: %q", sums, sc.Text())
		}
		listed[name] = true
		got, _, err := SHA256File(filepath.Join(contractDir, name))
		if err != nil || got != sum {
			t.Fatalf("%s: sha256 %s, %v; want %s", name, got, err, sum)
		}
		if name == "tools/validate-library-v2.py" && sum != validatorSHA256 {
			t.Fatalf("the validator: sha256 %s; want %s", sum, validatorSHA256)
		}
	}
	_ = filepath.WalkDir(contractDir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() != "SHA256SUMS" {
			rel, _ := filepath.Rel(contractDir, p)
			if !listed[filepath.ToSlash(rel)] {
				t.Errorf("%s is not listed in SHA256SUMS", rel)
			}
		}
		return nil
	})
}

// validatorPython is the python3 that runs the validator: one that imports
// jsonschema and referencing ("" where none does; CI has one).
func validatorPython(t *testing.T) string {
	t.Helper()
	py := os.Getenv(validatorPythonEnv)
	if py == "" {
		py = "python3"
	}
	if err := exec.Command(py, "-I", "-B", "-c", "import jsonschema, referencing").Run(); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("the validator needs python3 with jsonschema and referencing (%s): %v", py, err)
		}
		return ""
	}
	return py
}

// rewriteJSON rewrites the record at path as change leaves it, written as the
// record logic writes one (Encode).
func rewriteJSON(t *testing.T, path string, change func(d Doc) Doc) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	d, err := DecodeDoc(b)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Encode(change(d))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
	return out
}

// resum writes the line of name in the checksums file sums anew, of its
// content now.
func resum(t *testing.T, sums, name string, content []byte) {
	t.Helper()
	b, err := os.ReadFile(sums)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if _, n, ok := strings.Cut(l, "  "); ok && n == name {
			l = SHA256(content) + "  " + name
		}
		lines = append(lines, l)
	}
	if err := os.WriteFile(sums, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A library katalog-manager retired an original in passes the schemas'
// validator at 71999959. The library is the example the record logic wrote
// (a series, its episodes, its people), its one episode whose original lies
// in its version's folder as the record logic recorded it once its picture
// was read: its packets read whole for a caption signature (none; HEVC),
// its static HDR metadata as its picture states it (none), and its
// package's video rendition saying the transcoder's encoder. The catalog
// holds it as the adopt does, with a read of its frames the analyzer sent
// (its six windows, none showing captions); the retire job lets it go and
// writes its original-deleted event, the read it decided on in it
// (captionsRead). The validator, run on the library as it is then, finds
// nothing wrong: not the event, nor its read, nor what it accepted.
func TestALibraryARetireWroteInPassesTheValidator(t *testing.T) {
	contractFiles(t)
	py := validatorPython(t)
	if py == "" {
		t.Skip("no python3 with jsonschema and referencing: the validator is not run (" + validatorPythonEnv + ")")
	}
	st := storetest.Open(t)
	ctx := context.Background()
	root := t.TempDir()
	if out, err := exec.Command("cp", "-R", filepath.Join(contractDir, "library", "v2", "examples", "series"),
		filepath.Join(contractDir, "library", "v2", "examples", "people"), root).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	cfg := config.Config{LibraryRoot: root, WorkRoot: filepath.Join(t.TempDir(), "work"),
		PackagesRoot: filepath.Join(t.TempDir(), "packages"), NFSRoot: filepath.Join(t.TempDir(), "media")}
	p := PathsOf(cfg)
	itemDir := p.EpisodeDir(exampleSeries, exampleEpisode)
	versionDir, sourceDir := VersionDir(itemDir, exampleVersion), SourceDir(itemDir, exampleSource)
	// as the record logic records the original once its picture was read
	record := rewriteJSON(t, filepath.Join(sourceDir, "source.json"), func(d Doc) Doc {
		streams, _ := listOf(d, "streams")
		streams[0] = streams[0].(Doc).With("captionSignature", false)
		e, _ := d.Get("essence")
		return d.With("streams", streams).With("essence", e.(Doc).With("hdr10Metadata", false))
	})
	resum(t, filepath.Join(sourceDir, SumsFile), "source.json", record)
	pkg := rewriteJSON(t, filepath.Join(versionDir, PackageFile), func(d Doc) Doc {
		r, _ := d.Get("renditions")
		video, _ := listOf(r.(Doc), "video")
		video[0] = video[0].(Doc).With("encoder", "hevc_nvenc")
		return d.With("renditions", r.(Doc).With("video", video))
	})
	if err := os.WriteFile(filepath.Join(versionDir, ".complete"), []byte("sha256:"+SHA256(pkg)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(versionDir, "original.mkv")
	size, qh1, err := QH1(original)
	if err != nil {
		t.Fatal(err)
	}
	// the catalog as the adopt leaves it
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES
		('s1', 'library.layout', 'v2'), ('s2', 'library.originals', 'delete-after-package')`)
	storetest.AddItem(t, st, exampleSeries, "series", "Example Series", "")
	storetest.AddItem(t, st, exampleEpisode, "episode", exampleEpisodeTitle, exampleSeries)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, arrivalpath, sizebytes, qh1,
			state, recordedat, recorddir) VALUES ($1, $2, 'original.mkv', $3, $4, $5, 'present', now() - interval '1 hour', $6)`,
		exampleSource, exampleEpisode, original, size, qh1, sourceDir)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state, packageid, dir, completedat)
		VALUES ($1, $2, ARRAY[$3::varchar], 'complete', $4, $5, now() - interval '1 hour')`,
		exampleVersion, exampleEpisode, exampleSource, examplePackage, versionDir)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, codec, resolution, sizebytes,
			isprimary, kind, sourceid) VALUES ('orig-ep', $1, $2, 'hevc', '3840x2160', $3, true, 'primary', $4),
			('pkg-ep', $1, $5, 'hevc', '3840x2160', 1000, false, 'packaged', NULL)`,
		exampleEpisode, original, size, exampleSource, filepath.Join(versionDir, PackageFile))
	steps := processing.New(st.Pool())
	for _, step := range processing.OriginalSteps {
		if err := steps.Upsert(ctx, exampleEpisode, step, processing.StatusDone, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	// a packager that makes stereo alone, as the example's package is
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_workerrules (worker, version, rules, ruleshash, reportedat, changedat)
		VALUES ('packager', '1', '{"surroundAudio": "off"}'::jsonb, 'h', now(), now())`)
	f := &retireFixture{st: st, cfg: cfg, p: p}
	f.settle(t, exampleEpisode, exampleSource, exampleVersion)
	// the read the analyzer sent of it: its six windows over the 2700 s its
	// record gives, at 24 frames a second, none showing captions, as the
	// catalog keeps it (its report as sent beside)
	var ws []string
	for _, w := range []struct{ at, start float64 }{{0, 0}, {0.1, 270}, {0.3, 810}, {0.5, 1350}, {0.7, 1890}, {0.9, 2430}} {
		ws = append(ws, fmt.Sprintf(`{"at": %g, "start": %g, "end": %g, "frames": 48, "captions": false, "interlacedFrames": 0, `+
			`"repeatFrames": 0}`, w.at, w.start, w.start+2))
	}
	report := `{"captionWindows": [` + strings.Join(ws, ", ") + `], "duration": 2700, "span": {"start": 0, "end": 2700}, ` +
		`"gop": 2, "sizeBytes": ` + fmt.Sprint(size) + `, "ffprobe": "7.1.5"}`
	var parsed []CaptionWindow
	if err := json.Unmarshal([]byte(`[`+strings.Join(ws, ", ")+`]`), &parsed); err != nil {
		t.Fatal(err)
	}
	windows, _ := json.Marshal(parsed)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_sourcecaptions SET captionwindows = $2::jsonb, duration = 2700,
		readspanstart = 0, readspanend = 2700, readgop = 2, readreport = $3, ffprobeversion = '7.1.5',
		readat = now() - interval '10 minutes' WHERE source_id = $1`, exampleSource, string(windows), report)
	f.r = NewRetirer(st.Pool(), cfg, processing.New(st.Pool()))
	rep := f.pass(t)
	s := f.source(t, exampleSource)
	if rep.Originals != 1 || s.State != SourceDeleted {
		t.Fatalf("the pass: %+v; the source %s (%s); want the original deleted", rep, s.State, deref(s.Error))
	}
	dir, err := FindEvent(itemDir, *s.RetireEventID, EventOriginalDeleted)
	if err != nil || dir == "" {
		t.Fatalf("the event: %q, %v", dir, err)
	}
	ev, err := readRecordDoc(filepath.Join(dir, EventFile))
	if err != nil {
		t.Fatal(err)
	}
	if read, ok := ev.Get("captionsRead"); !ok || read == nil {
		t.Errorf("the event: %v; want the read it decided on", ev)
	}
	out, err := exec.Command(py, "-I", "-B", filepath.Join(contractDir, "tools", "validate-library-v2.py"), root).CombinedOutput()
	if err != nil || !strings.HasSuffix(strings.TrimSpace(string(out)), "OK") {
		t.Errorf("the validator: %v\n%s", err, out)
	}
}
