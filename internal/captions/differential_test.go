package captions

import (
	"bufio"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/library"
)

// The record logic the differential holds the gate to: the schemas'
// tools/libv2_records.py at 71999959, vendored in testdata/differential and
// pinned by its sha256, as the plan's file is. A copy that differs fails
// the test.
const (
	differentialDir      = "testdata/differential"
	recordLogicSHA256    = "5109334f5645d99a90ce728d541b64dcc0dd7802312d0771bc25b104b5b0d3db"
	differentialMaxShown = 40
)

// differentialCase is one input of the differential: the files of an item
// folder as text, and a read's report (its "original") as text; nil: none.
type differentialCase struct {
	ID      string  `json:"id"`
	Source  *string `json:"source"`
	Probe   *string `json:"probe"`
	Package *string `json:"package"`
	Version *string `json:"version"`
	Others  []struct {
		ID      string  `json:"id"`
		Package *string `json:"package"`
		Version *string `json:"version"`
	} `json:"others"`
	Read *string `json:"read"`
}

func putText(t *testing.T, path string, text *string) {
	t.Helper()
	if text == nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(*text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// loaded is the record text as the record logic's loaders read one (nil:
// none, or one that can't be read).
func loaded(text *string) any {
	if text == nil {
		return nil
	}
	d, err := library.LoadRecord([]byte(*text))
	if err != nil {
		return nil
	}
	return d
}

// The gate holds what the record logic holds. The differential of the
// review, input by input: differential.py makes the inputs (a title's
// records, its stored probe, its packages, and a read's report, each varied
// at every edge the contract draws), the gate says what it does with each
// (the read taken the way the PUT takes one, kept the way the catalog keeps
// one), its port of the record logic's gate (library.RecordLogicHolds) what
// that says of the same records, and the vendored record logic what the
// contract says. Where the record logic holds an original, neither the gate
// nor its port lets it go, and the port holds each key it holds (its closed
// captions, its fields; a raise holds both): any such input fails the test.
// The inputs the gate holds and the record logic lets go are counted by their
// kind, which the pull request's text names, each with why. It needs python3 (3.11 or
// later); CI has it, and fails without it.
func TestTheGateHoldsWhatTheRecordLogicHolds(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("the differential needs python3, which CI has: none is on the PATH")
		}
		t.Skip("no python3 on the PATH: the differential is not run")
	}
	b, err := os.ReadFile(filepath.Join(differentialDir, "libv2_records.py"))
	if err != nil {
		t.Fatal(err)
	}
	if got := library.SHA256(b); got != recordLogicSHA256 {
		t.Fatalf("libv2_records.py: sha256 %s; want %s, the record logic as vendored", got, recordLogicSHA256)
	}
	tmp := t.TempDir()
	cases, goOut, summary := filepath.Join(tmp, "cases.jsonl"), filepath.Join(tmp, "go.out"), filepath.Join(tmp, "summary.json")
	script := filepath.Join(differentialDir, "differential.py")
	if out, err := exec.Command(py, "-I", "-B", script, "gen", cases).CombinedOutput(); err != nil {
		t.Fatalf("differential.py gen: %v\n%s", err, out)
	}
	f, err := os.Open(cases)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out, err := os.Create(goOut)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	base := t.TempDir()
	n := 0
	for sc.Scan() {
		var c differentialCase
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatalf("case %d: %v", n, err)
		}
		n++
		verdict, note, why := gateVerdict(t, filepath.Join(base, fmt.Sprint(n)), c)
		port := portVerdict(c)
		fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\n", c.ID, verdict, clean(note), clean(why), port)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	cmp, err := exec.Command(py, "-I", "-B", script, "cmp", cases, goOut, summary).CombinedOutput()
	if err != nil {
		t.Fatalf("differential.py cmp: %v\n%s", err, cmp)
	}
	var s struct {
		Cases      int            `json:"cases"`
		Agree      map[string]int `json:"agree"`
		Must       [][]string     `json:"must"`
		Lesser     map[string]int `json:"lesser"`
		PortMust   [][]string     `json:"port_must"`
		PortLesser map[string]int `json:"port_lesser"`
		PortHeld   []string       `json:"port_held"`
	}
	sb, err := os.ReadFile(summary)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(sb, &s); err != nil {
		t.Fatal(err)
	}
	t.Logf("%s", strings.TrimSpace(string(cmp)))
	if keep := os.Getenv("DIFFERENTIAL_KEEP"); keep != "" {
		// the inputs, the gate's verdicts and the comparison, kept to read
		for _, name := range []string{"cases.jsonl", "go.out", "summary.json"} {
			if b, err := os.ReadFile(filepath.Join(tmp, name)); err == nil {
				_ = os.WriteFile(filepath.Join(keep, name), b, 0o644)
			}
		}
	}
	t.Logf("agree %v; the gate holds and the record logic lets go, by kind: %v; the port: %v", s.Agree, s.Lesser, s.PortLesser)
	if s.Cases != n || n == 0 {
		t.Fatalf("the comparison read %d cases of %d", s.Cases, n)
	}
	for i, m := range s.Must {
		if i == differentialMaxShown {
			break
		}
		t.Errorf("the gate lets go where the record logic holds: %v", m)
	}
	for i, m := range s.PortMust {
		if i == differentialMaxShown {
			break
		}
		t.Errorf("the port lets go what the record logic holds: %v", m)
	}
	if len(s.Must) > 0 || len(s.PortMust) > 0 {
		t.Errorf("%d inputs the gate lets go where the record logic holds, %d the port lets go a key of", len(s.Must),
			len(s.PortMust))
	}
	// and the port holds a key the record logic doesn't only of a time
	// written as Python's float() alone reads it (its digits split by
	// underscores, of another width, or set off by a separator Python
	// strips): no plan, which holds
	for _, id := range s.PortHeld {
		if !portHoldsAlone(id) {
			t.Errorf("the port holds what the record logic lets go: %s", id)
		}
	}
}

// portHoldsAlone reports whether the case id is of a kind the port holds and
// the record logic lets go: a time of the plan written as float() alone
// reads it.
func portHoldsAlone(id string) bool {
	return strings.HasPrefix(id, "P/") && (strings.HasSuffix(id, "/underscore") || strings.HasSuffix(id, "/fullwidth") ||
		strings.HasSuffix(id, "/fs"))
}

func clean(s string) string {
	s = strings.NewReplacer("\n", " ", "\t", " ").Replace(s)
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// gateVerdict is what the gate does with the case c, its files written in
// the item folder dir: GOES, KEPT or ERR, how the PUT took its read, and why.
func gateVerdict(t *testing.T, dir string, c differentialCase) (verdict, note, why string) {
	t.Helper()
	putText(t, filepath.Join(dir, "sources", "a", "source.json"), c.Source)
	putText(t, filepath.Join(dir, "sources", "a", "ffprobe.json"), c.Probe)
	putText(t, filepath.Join(dir, "versions", "v-a", "package.json"), c.Package)
	putText(t, filepath.Join(dir, "versions", "v-a", "version.json"), c.Version)
	for _, o := range c.Others {
		putText(t, filepath.Join(dir, "versions", o.ID, "package.json"), o.Package)
		putText(t, filepath.Join(dir, "versions", o.ID, "version.json"), o.Version)
		if o.Package == nil && o.Version == nil {
			_ = os.MkdirAll(filepath.Join(dir, "versions", o.ID), 0o775)
		}
	}
	facts := library.GateFacts{}
	note = "no read"
	if c.Read != nil {
		body := `{"token": "t", "original": ` + *c.Read + `}`
		var rep Report
		switch k := RepeatedKey([]byte(body)); {
		case k != "":
			note = "PUT 400: key twice " + k
		default:
			if err := json.Unmarshal([]byte(body), &rep); err != nil || rep.Original == nil {
				note = fmt.Sprintf("PUT 400: %v", err)
				break
			}
			o, err := rep.Original.check()
			switch {
			case err != nil:
				note = "PUT 400: " + err.Error()
			case !o.read && !o.found:
				note = "PUT kept as failed: " + o.why
				facts.CaptionsFailed = o.why
			default:
				var ws []library.CaptionWindow
				if err := json.Unmarshal(o.windows, &ws); err != nil {
					t.Fatalf("case %s: stored windows: %v", c.ID, err)
				}
				facts.Captions = &library.CaptionsRead{Windows: ws, Duration: o.duration, Span: o.span, Gop: o.gop,
					CaptionStreams: o.streams, OffList: rep.Original.OffList, Report: o.report}
				note = "read"
			}
		}
	}
	g, err := library.ReadGateWith(dir, "v-a", facts, "a")
	switch {
	case err != nil:
		return "ERR", note, err.Error()
	case g.Unknown != "":
		return "KEPT", note, "unknown: " + g.Unknown
	case len(g.Beyond()) > 0:
		return "KEPT", note, "beyond: " + strings.Join(g.Beyond(), ",") + " | " + g.Scan + " | " + g.Captions
	}
	return "GOES", note, ""
}

// portVerdict is what the port of the record logic's gate says of the case
// c's records and read, as the record logic reads them: HOLD and the keys it
// holds ("HOLD closedCaptions,interlaced"), or GO.
func portVerdict(c differentialCase) string {
	packages := []any{}
	var pkgEssence any
	if c.Package != nil {
		own := loaded(c.Package)
		packages = append(packages, own)
		if d, ok := own.(library.Doc); ok {
			pkgEssence, _ = d.Get("essence")
		}
	}
	for _, o := range c.Others {
		if o.Package != nil {
			packages = append(packages, loaded(o.Package))
		}
	}
	var reads []any
	if c.Read != nil {
		reads = []any{library.RecordLogicRead([]byte(*c.Read))}
	}
	held := library.RecordLogicHolds(loaded(c.Source), loaded(c.Probe), packages, pkgEssence, reads)
	if len(held) == 0 {
		return "GO"
	}
	keys := slices.Sorted(maps.Keys(held))
	return "HOLD " + strings.Join(keys, ",")
}
