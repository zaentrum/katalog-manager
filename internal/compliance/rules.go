package compliance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// The workers that report the rules they run with (PUT
// /api/analyze/rules/{worker}).
const (
	Transcoder = "transcoder"
	Packager   = "packager"
)

// ErrUnknownWorker refuses a report of a worker that has no rules here.
var ErrUnknownWorker = errors.New("no such worker: the transcoder and the packager report their rules")

// BadReport refuses a report whose body is no report of the worker's rules.
type BadReport struct{ Reason string }

func (e *BadReport) Error() string { return e.Reason }

// ErrNotReady says the catalog keeps no rules and no check: migration 051 is
// not in place.
var ErrNotReady = errors.New("migration 051 (db/migrations/051_library_compliance.sql) is not applied")

// TranscoderRules is what the check reads of the transcoder's report: its
// caps, Mbit/s of the video and GiB of a movie's file by kind and bucket (0
// switches a rule off), its 2160 bucket (from minHeight lines tall or
// minWidth wide), its HEVC encoder with the maxrate overrides of a
// source-size encode by bucket, and its ladder. A value it did not report is
// nil: the rules that read it are not checked.
type TranscoderRules struct {
	Caps struct {
		Movie1080Mbps   *float64 `json:"movie1080Mbps"`
		Movie2160Mbps   *float64 `json:"movie2160Mbps"`
		MovieMaxGiB     *float64 `json:"movieMaxGiB"`
		Episode1080Mbps *float64 `json:"episode1080Mbps"`
		Episode2160Mbps *float64 `json:"episode2160Mbps"`
	} `json:"caps"`
	Bucket2160 struct {
		MinHeight *float64 `json:"minHeight"`
		MinWidth  *float64 `json:"minWidth"`
	} `json:"bucket2160"`
	Encoder struct {
		Codec           *string  `json:"codec"`
		Preset          *string  `json:"preset"`
		CQ              *float64 `json:"cq"`
		CRF             *float64 `json:"crf"`
		Maxrate1080Mbps *float64 `json:"maxrate1080Mbps"`
		Maxrate2160Mbps *float64 `json:"maxrate2160Mbps"`
	} `json:"encoder"`
	Ladder      *string `json:"ladder"`
	ExtraLadder *string `json:"extraLadder"`
}

// PackagerRules is what the check reads of the packager's report: the codec
// of its 5.1 companion ("off": none), the bit rates of an encoded companion
// and of the stereo rendition, and the forms of image subtitles it carries.
type PackagerRules struct {
	SurroundAudio   *string  `json:"surroundAudio"`
	SurroundBitrate *string  `json:"surroundBitrate"`
	StereoBitrate   *string  `json:"stereoBitrate"`
	ImageSubtitles  []string `json:"imageSubtitles"`
}

// checked are the values of a worker's rules the check reads, which its rules
// hash covers: the same rules reported again keep it, whatever else the
// report carries (its version, the extras' ladder, the encoder's quality).
type checkedTranscoder struct {
	Caps       any
	Bucket2160 any
	Codec      *string
	Maxrate    [2]*float64
	Ladder     *string
}

type checkedPackager struct {
	SurroundAudio, SurroundBitrate, StereoBitrate *string
	ImageSubtitles                                []string
}

// hashOf is the hex SHA-256 of v as JSON.
func hashOf(v any) string {
	b, _ := json.Marshal(v)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func (t *TranscoderRules) hash() string {
	return hashOf(checkedTranscoder{Caps: t.Caps, Bucket2160: t.Bucket2160, Codec: t.Encoder.Codec,
		Maxrate: [2]*float64{t.Encoder.Maxrate1080Mbps, t.Encoder.Maxrate2160Mbps}, Ladder: t.Ladder})
}

func (p *PackagerRules) hash() string {
	forms := append([]string(nil), p.ImageSubtitles...)
	sort.Strings(forms)
	if p.ImageSubtitles == nil {
		forms = nil
	}
	return hashOf(checkedPackager{SurroundAudio: p.SurroundAudio, SurroundBitrate: p.SurroundBitrate,
		StereoBitrate: p.StereoBitrate, ImageSubtitles: forms})
}

// report is a worker's report as it is sent: its version and its rules.
type report struct {
	Version *string         `json:"version"`
	Rules   json.RawMessage `json:"rules"`
}

// maxReport is the most a report's body may hold.
const maxReport = 64 << 10

// bitrateRE is a bit rate as the packager writes one: 448k, 192000, 1.5M.
var bitrateRE = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*([kKmM]?)$`)

// parseBitrate is a bit rate written as bitrateRE takes it, in bit/s.
func parseBitrate(s string) (float64, bool) {
	m := bitrateRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil || n <= 0 || math.IsInf(n, 0) {
		return 0, false
	}
	switch strings.ToLower(m[2]) {
	case "k":
		n *= 1000
	case "m":
		n *= 1000000
	}
	return n, true
}

// parseReport reads the body of a worker's report: its version (at most 255
// characters, or none) and its rules, a JSON object whose values the check
// reads are of their kind, numbers never below 0. It answers the rules as
// they came, the version, and the hash of what the check reads of them.
func parseReport(worker string, body []byte) (rules []byte, version *string, hash string, err error) {
	if worker != Transcoder && worker != Packager {
		return nil, nil, "", ErrUnknownWorker
	}
	var r report
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&r); err != nil {
		return nil, nil, "", &BadReport{"the body is no report of rules: " + err.Error()}
	}
	if r.Version != nil {
		v := strings.TrimSpace(*r.Version)
		switch {
		case v == "":
			r.Version = nil
		case len(v) > 255:
			return nil, nil, "", &BadReport{"version is longer than 255 characters"}
		default:
			r.Version = &v
		}
	}
	raw := bytes.TrimSpace(r.Rules)
	if len(raw) == 0 || raw[0] != '{' {
		return nil, nil, "", &BadReport{`rules is no JSON object: {"version": "...", "rules": {...}}`}
	}
	switch worker {
	case Transcoder:
		var t TranscoderRules
		if err := json.Unmarshal(raw, &t); err != nil {
			return nil, nil, "", &BadReport{"the transcoder's rules: " + typeError(err)}
		}
		for key, v := range map[string]*float64{
			"caps.movie1080Mbps": t.Caps.Movie1080Mbps, "caps.movie2160Mbps": t.Caps.Movie2160Mbps,
			"caps.movieMaxGiB": t.Caps.MovieMaxGiB, "caps.episode1080Mbps": t.Caps.Episode1080Mbps,
			"caps.episode2160Mbps": t.Caps.Episode2160Mbps, "bucket2160.minHeight": t.Bucket2160.MinHeight,
			"bucket2160.minWidth": t.Bucket2160.MinWidth, "encoder.maxrate1080Mbps": t.Encoder.Maxrate1080Mbps,
			"encoder.maxrate2160Mbps": t.Encoder.Maxrate2160Mbps,
		} {
			if v != nil && (*v < 0 || math.IsInf(*v, 0) || math.IsNaN(*v)) {
				return nil, nil, "", &BadReport{fmt.Sprintf("the transcoder's rules: %s is %v, and it is 0 or above", key, *v)}
			}
		}
		hash = t.hash()
	case Packager:
		var p PackagerRules
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, nil, "", &BadReport{"the packager's rules: " + typeError(err)}
		}
		for key, v := range map[string]*string{"surroundBitrate": p.SurroundBitrate, "stereoBitrate": p.StereoBitrate} {
			if v != nil {
				if _, ok := parseBitrate(*v); !ok {
					return nil, nil, "", &BadReport{fmt.Sprintf("the packager's rules: %s is %q, which is no bit rate (448k, 192000)", key, *v)}
				}
			}
		}
		hash = p.hash()
	}
	return raw, r.Version, hash, nil
}

// typeError says what a value of the wrong kind is, in the report's terms.
func typeError(err error) string {
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) {
		return fmt.Sprintf("%s is a JSON %s, and it is a %s", te.Field, te.Value, kindOf(te.Type.Kind().String()))
	}
	return err.Error()
}

func kindOf(goKind string) string {
	switch goKind {
	case "float64":
		return "number"
	case "slice":
		return "list"
	case "struct":
		return "object"
	}
	return goKind
}

// Reported is what the catalog keeps of a worker's report.
type Reported struct {
	Worker     string    `json:"worker"`
	Version    *string   `json:"version"`
	ReportedAt time.Time `json:"reportedAt"`
	// Changed: its rules are not those it reported last.
	Changed bool `json:"changed"`
}

// ReportRules keeps the rules worker reports it runs with, the body of PUT
// /api/analyze/rules/{worker}: {"version": "...", "rules": {...}}. Rules
// that changed have the titles checked again against them.
func (s *Service) ReportRules(ctx context.Context, worker string, body []byte) (Reported, error) {
	out := Reported{Worker: worker}
	if len(body) > maxReport {
		return out, &BadReport{fmt.Sprintf("the body holds %d bytes, more than a report of rules does (%d)", len(body), maxReport)}
	}
	rules, version, hash, err := parseReport(worker, body)
	if err != nil {
		return out, err
	}
	if why := s.ready(ctx); why != nil {
		return out, why
	}
	var changedAt time.Time
	err = s.pool.QueryRow(ctx, `INSERT INTO `+store.WorkerRulesTable+` AS w (worker, version, rules, ruleshash, reportedat, changedat)
		VALUES ($1, $2, $3::jsonb, $4, now(), now())
		ON CONFLICT (worker) DO UPDATE SET version = EXCLUDED.version, rules = EXCLUDED.rules, reportedat = now(),
			changedat = CASE WHEN w.ruleshash = EXCLUDED.ruleshash THEN w.changedat ELSE now() END,
			ruleshash = EXCLUDED.ruleshash
		RETURNING reportedat, changedat`, worker, version, string(rules), hash).Scan(&out.ReportedAt, &changedAt)
	if err != nil {
		return out, err
	}
	out.Version, out.Changed = version, changedAt.Equal(out.ReportedAt)
	if out.Changed {
		s.poke()
	}
	return out, nil
}

// workerReport is a worker's rules as the catalog keeps them.
type workerReport struct {
	version    *string
	raw        []byte
	hash       string
	reportedAt time.Time
	t          *TranscoderRules
	p          *PackagerRules
}

// rulesState is what the check holds the titles against: each worker's
// report (nil for one that never reported) and the library's layout.
type rulesState struct {
	transcoder, packager *workerReport
	v2                   bool
}

// version is the check's own: a check that reads its findings otherwise
// gets a new one, and every title is checked again.
const version = "compliance/1"

// hash is the hash a title checked against s is kept under: both workers'
// rules, the layout and the check's version.
func (s rulesState) hash() string {
	parts := []string{version, "legacy", "-", "-"}
	if s.v2 {
		parts[1] = "v2"
	}
	if s.transcoder != nil {
		parts[2] = s.transcoder.hash
	}
	if s.packager != nil {
		parts[3] = s.packager.hash
	}
	return hashOf(parts)
}

// loadRules reads the workers' reports; a report that cannot be read any
// more (written by a later service, say) is taken for none.
func (s *Service) loadRules(ctx context.Context) (rulesState, error) {
	var st rulesState
	rows, err := s.pool.Query(ctx, `SELECT worker, version, rules::text, ruleshash, reportedat FROM `+store.WorkerRulesTable)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var worker, raw string
		w := &workerReport{}
		if err := rows.Scan(&worker, &w.version, &raw, &w.hash, &w.reportedAt); err != nil {
			return st, err
		}
		w.raw = []byte(raw)
		switch worker {
		case Transcoder:
			var t TranscoderRules
			if json.Unmarshal(w.raw, &t) == nil {
				w.t, st.transcoder = &t, w
			}
		case Packager:
			var p PackagerRules
			if json.Unmarshal(w.raw, &p) == nil {
				w.p, st.packager = &p, w
			}
		}
	}
	return st, rows.Err()
}

// PipelineRules are the rules the workers last reported, each written out.
func (s *Service) PipelineRules(ctx context.Context) (graph.PipelineRules, error) {
	if why := s.ready(ctx); why != nil {
		return graph.PipelineRules{}, why
	}
	st, err := s.loadRules(ctx)
	if err != nil {
		return graph.PipelineRules{}, err
	}
	return pipelineRulesOf(st), nil
}

func pipelineRulesOf(st rulesState) graph.PipelineRules {
	var out graph.PipelineRules
	for _, w := range []struct {
		name string
		r    *workerReport
		to   **graph.WorkerRules
	}{{Transcoder, st.transcoder, &out.Transcoder}, {Packager, st.packager, &out.Packager}} {
		if w.r == nil {
			continue
		}
		*w.to = &graph.WorkerRules{Worker: w.name, Version: w.r.version, ReportedAt: w.r.reportedAt, Rules: settingsOf(w.r.raw)}
	}
	return out
}

// A rule's label and how its value is written, by its key in a report.
type setting struct {
	key, label string
	unit       func(v any) string
}

func mbps(v any) string {
	if f, ok := v.(float64); ok {
		if f == 0 {
			return "off"
		}
		return number(f) + " Mbit/s"
	}
	return plain(v)
}

func gibValue(v any) string {
	if f, ok := v.(float64); ok {
		if f == 0 {
			return "off"
		}
		return number(f) + " GiB"
	}
	return plain(v)
}

func unitOf(unit string) func(v any) string {
	return func(v any) string {
		if f, ok := v.(float64); ok {
			return number(f) + " " + unit
		}
		return plain(v)
	}
}

func override(v any) string {
	if v == nil {
		return "none: the cap"
	}
	return mbps(v)
}

func ladder(v any) string {
	if s, ok := v.(string); ok && strings.TrimSpace(s) == "" {
		return "one rendition"
	}
	return plain(v)
}

// settings are the rules a report holds that the console names, in the
// order it lists them; any other key follows, by name.
var settings = []setting{
	{"caps.movie1080Mbps", "Movie cap (1080)", mbps},
	{"caps.movie2160Mbps", "Movie cap (2160)", mbps},
	{"caps.movieMaxGiB", "Movie file size cap", gibValue},
	{"caps.episode1080Mbps", "Episode cap (1080)", mbps},
	{"caps.episode2160Mbps", "Episode cap (2160)", mbps},
	{"bucket2160.minHeight", "2160 bucket from a height of", unitOf("lines")},
	{"bucket2160.minWidth", "2160 bucket from a width of", unitOf("pixels")},
	{"encoder.codec", "HEVC encoder", plain},
	{"encoder.preset", "Encoder preset", plain},
	{"encoder.cq", "Constant quality", plain},
	{"encoder.crf", "Constant rate factor", plain},
	{"encoder.maxrate1080Mbps", "Maxrate (1080)", override},
	{"encoder.maxrate2160Mbps", "Maxrate (2160)", override},
	{"ladder", "Ladder", ladder},
	{"extraLadder", "Extras ladder", ladder},
	{"surroundAudio", "5.1 companion", plain},
	{"surroundBitrate", "5.1 bit rate", plain},
	{"stereoBitrate", "Stereo bit rate", plain},
	{"imageSubtitles", "Image subtitles carried", plain},
}

// skipped are keys a report carries that the console does not list: the
// transcoder's ladder in parts, which ladder says.
var skipped = map[string]bool{"ladderRungs": true}

// settingsOf writes out each rule of a report's rules raw: one a value,
// nested objects by their dotted key.
func settingsOf(raw []byte) []graph.RuleSetting {
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		return []graph.RuleSetting{}
	}
	leaves := map[string]any{}
	var walk func(prefix string, m map[string]any)
	walk = func(prefix string, m map[string]any) {
		for k, v := range m {
			key := prefix + k
			if skipped[key] {
				continue
			}
			if sub, ok := v.(map[string]any); ok {
				walk(key+".", sub)
				continue
			}
			leaves[key] = v
		}
	}
	walk("", doc)
	out := []graph.RuleSetting{}
	for _, s := range settings {
		if v, ok := leaves[s.key]; ok {
			out = append(out, graph.RuleSetting{Key: s.key, Label: s.label, Value: s.unit(v)})
			delete(leaves, s.key)
		}
	}
	rest := make([]string, 0, len(leaves))
	for k := range leaves {
		rest = append(rest, k)
	}
	sort.Strings(rest)
	for _, k := range rest {
		out = append(out, graph.RuleSetting{Key: k, Label: k, Value: plain(leaves[k])})
	}
	return out
}

// plain writes a JSON value: a number as short as it reads, a list of words
// joined, null as not set.
func plain(v any) string {
	switch x := v.(type) {
	case nil:
		return "not set"
	case string:
		return x
	case float64:
		return number(x)
	case bool:
		if x {
			return "yes"
		}
		return "no"
	case []any:
		if len(x) == 0 {
			return "none"
		}
		parts := make([]string, 0, len(x))
		for _, e := range x {
			if _, ok := e.(map[string]any); ok {
				b, _ := json.Marshal(e)
				parts = append(parts, string(b))
				continue
			}
			parts = append(parts, plain(e))
		}
		return strings.Join(parts, ", ")
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// number writes f as short as it reads: 8, 14, 7.5, 0.25.
func number(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}
