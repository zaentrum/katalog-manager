package library

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The second differential's generator (testdata/sdiff/sdiff.py, the review's
// harness of its third look as its start), pinned by its sha256 as the first
// one's is, and the least it must make: of every input, and of each family
// (the part of an id before its first /).
const (
	sdiffDir    = "testdata/sdiff"
	sdiffSHA256 = "7d7b95c53194da244e8b9e86c16ef9a23e0b545150d08fec02ddd632b868a965"
)

var sdiffFloor = map[string]int{"": 33261, "B": 85, "S": 32337, "T": 839}

// The gate takes no source record the contract's validator refuses. The
// second differential, input by input: sdiff.py makes the bytes of source
// records (the record the schema takes, and the example library's three,
// each fact of them varied - its type, its form, its texts and keys, its
// bytes - and the facts the schema's patterns and formats draw, at their
// edges), the gate reads each as it reads one (sourceRecordTaken: its loader,
// the forms its writers write, the contract's schema and the validator's own
// rules, the folder it names), and the vendored validator says whether it
// takes it for a source record. Any record the gate's check takes and the
// validator refuses fails the test; those the check alone refuses are
// counted by their kind, which the pull request's text names. It needs a
// python3 with jsonschema and referencing (validatorPython); CI has it.
func TestTheRecordCheckTakesNoSourceRecordTheValidatorRefuses(t *testing.T) {
	contractFiles(t)
	py := validatorPython(t)
	if py == "" {
		t.Skip("no python3 with jsonschema and referencing: the second differential is not run (" + validatorPythonEnv + ")")
	}
	b, err := os.ReadFile(filepath.Join(sdiffDir, "sdiff.py"))
	if err != nil {
		t.Fatal(err)
	}
	if got := SHA256(b); got != sdiffSHA256 {
		t.Fatalf("sdiff.py: sha256 %s; want %s, the generator as pinned", got, sdiffSHA256)
	}
	tmp := t.TempDir()
	cases, goOut, summary := filepath.Join(tmp, "cases.jsonl"), filepath.Join(tmp, "go.out"), filepath.Join(tmp, "summary.json")
	script := filepath.Join(sdiffDir, "sdiff.py")
	if out, err := exec.Command(py, "-I", "-B", script, "gen", cases).CombinedOutput(); err != nil {
		t.Fatalf("sdiff.py gen: %v\n%s", err, out)
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
	w := bufio.NewWriter(out)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	n, panics := 0, 0
	families := map[string]int{}
	for sc.Scan() {
		var c struct{ ID, B64, Folder string }
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatalf("case %d: %v", n, err)
		}
		n++
		family, _, _ := strings.Cut(c.ID, "/")
		families[family]++
		raw, err := base64.StdEncoding.DecodeString(c.B64)
		if err != nil {
			t.Fatal(err)
		}
		verdict, why := sdiffVerdict(raw, c.Folder)
		if verdict == "PANIC" {
			panics++
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", c.ID, verdict, strings.NewReplacer("\n", " ", "\t", " ", "\r", " ").Replace(why))
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	cmp, err := exec.Command(py, "-I", "-B", script, "cmp", cases, goOut, summary, tmp).CombinedOutput()
	if err != nil {
		t.Fatalf("sdiff.py cmp: %v\n%s", err, cmp)
	}
	t.Logf("%s", strings.TrimSpace(string(cmp)))
	var s struct {
		Cases         int            `json:"cases"`
		Agree         map[string]int `json:"agree"`
		Must          [][]string     `json:"must"`
		Lesser        int            `json:"lesser"`
		LesserByKind  map[string]int `json:"lesser_by_kind"`
		LesserByFamil map[string]int `json:"lesser_by_family"`
	}
	sb, err := os.ReadFile(summary)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(sb, &s); err != nil {
		t.Fatal(err)
	}
	if keep := os.Getenv("DIFFERENTIAL_KEEP"); keep != "" {
		for _, name := range []string{"cases.jsonl", "go.out", "summary.json"} {
			if b, err := os.ReadFile(filepath.Join(tmp, name)); err == nil {
				_ = os.WriteFile(filepath.Join(keep, "s-"+name), b, 0o644)
			}
		}
	}
	t.Logf("agree %v; the check alone refuses %d, by family %v", s.Agree, s.Lesser, s.LesserByFamil)
	if s.Cases != n || n < sdiffFloor[""] {
		t.Fatalf("the comparison read %d records of %d; want at least %d", s.Cases, n, sdiffFloor[""])
	}
	for family, least := range sdiffFloor {
		if family != "" && families[family] < least {
			t.Errorf("the generator made %d records of %s; want at least %d", families[family], family, least)
		}
	}
	for i, m := range s.Must {
		if i == 40 {
			break
		}
		t.Errorf("the gate's check takes a source record the validator refuses: %v", m)
	}
	if len(s.Must) > 0 {
		t.Errorf("%d source records the gate's check takes and the validator refuses", len(s.Must))
	}
	if panics > 0 {
		t.Errorf("the gate's check failed (a panic) on %d records", panics)
	}
}

// sdiffVerdict is what the gate's check says of the bytes raw of a source
// record in the folder folder: ACCEPT, REFUSE (and why), or PANIC, which the
// gate's edge would take for a hold (ReadGateWith) but no record may cause.
func sdiffVerdict(raw []byte, folder string) (verdict, why string) {
	defer func() {
		if r := recover(); r != nil {
			verdict, why = "PANIC", fmt.Sprint(r)
		}
	}()
	if _, why := sourceRecordTaken(raw, folder); why != "" {
		return "REFUSE", why
	}
	return "ACCEPT", ""
}
