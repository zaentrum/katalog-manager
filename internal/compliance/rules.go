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
	// LadderRungs are the item ladder's rungs in parts, and RungMaxrateMbps
	// the maxrate of a rung it sets none for, Mbit/s, by the height of its
	// box and its codec (the planner's table): what an estimate counts
	// every rung at.
	LadderRungs     []ladderRung                  `json:"ladderRungs"`
	RungMaxrateMbps map[string]map[string]float64 `json:"rungMaxrateMbps"`
	// DolbyVision is what the transcoder does with an original of each
	// Dolby Vision profile it names, as it can at runtime ("profile5":
	// "reshaped" on its Vulkan device, "refused" without one; "profile7":
	// "refused"); nil from a transcoder that doesn't report it.
	DolbyVision map[string]string `json:"dolbyVision"`
}

// PackagerRules is what the check reads of the packager's report: the codec
// of its surround companion ("off": none) and its layout ("5.1"), the bit
// rates of an encoded companion and of the stereo rendition, and the forms of
// image subtitles it carries.
type PackagerRules struct {
	SurroundAudio   *string  `json:"surroundAudio"`
	SurroundLayout  *string  `json:"surroundLayout"`
	SurroundBitrate *string  `json:"surroundBitrate"`
	StereoBitrate   *string  `json:"stereoBitrate"`
	ImageSubtitles  []string `json:"imageSubtitles"`
}

// checked are the values of a worker's rules the check reads, which its rules
// hash covers: the same rules reported again keep it, whatever else the
// report carries (its version, the extras' ladder, the encoder's quality).
type checkedTranscoder struct {
	Caps        any
	Bucket2160  any
	Codec       *string
	Maxrate     [2]*float64
	Ladder      *string
	DolbyVision map[string]string `json:",omitempty"`
	// the rungs and their table, which the estimate counts
	LadderRungs     []ladderRung                  `json:",omitempty"`
	RungMaxrateMbps map[string]map[string]float64 `json:",omitempty"`
}

type checkedPackager struct {
	SurroundAudio, SurroundLayout, SurroundBitrate, StereoBitrate *string
	ImageSubtitles                                                []string
}

// hashOf is the hex SHA-256 of v as JSON.
func hashOf(v any) string {
	b, _ := json.Marshal(v)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func (t *TranscoderRules) hash() string {
	return hashOf(checkedTranscoder{Caps: t.Caps, Bucket2160: t.Bucket2160, Codec: t.Encoder.Codec,
		Maxrate: [2]*float64{t.Encoder.Maxrate1080Mbps, t.Encoder.Maxrate2160Mbps}, Ladder: t.Ladder,
		DolbyVision: t.DolbyVision, LadderRungs: t.LadderRungs, RungMaxrateMbps: t.RungMaxrateMbps})
}

func (p *PackagerRules) hash() string {
	forms := append([]string(nil), p.ImageSubtitles...)
	sort.Strings(forms)
	if p.ImageSubtitles == nil {
		forms = nil
	}
	return hashOf(checkedPackager{SurroundAudio: p.SurroundAudio, SurroundLayout: p.SurroundLayout,
		SurroundBitrate: p.SurroundBitrate, StereoBitrate: p.StereoBitrate, ImageSubtitles: forms})
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
	// retries says the sweep that retries a failed step runs (the
	// service's KATALOG_RETRY_INTERVAL): a step's next retry is one.
	retries bool
}

// version is the check's own: a check that reads its findings otherwise, or
// writes them in other words, gets a new one, and every title is checked
// again.
const version = "compliance/8"

// hash is the hash a title checked against s is kept under: both workers'
// rules, the layout, whether failed steps are retried, and the check's
// version.
func (s rulesState) hash() string {
	parts := []string{version, "legacy", "-", "-", "no retries"}
	if s.v2 {
		parts[1] = "v2"
	}
	if s.retries {
		parts[4] = "retries"
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

// A row of the rules panel: the key of the report it is listed by, its
// label, the other keys of the report it says too (with), and its value in
// words, with the configuration as reported for its detail where the words
// aren't it (nil where they are). The value gets every value of the report
// by its key (got).
type setting struct {
	key, label string
	with       []string
	words      func(got func(key string) (any, bool)) (string, *string)
	// unlessNull leaves the row out when its key is reported as null: a
	// knob of another encoder than the one reported (NVENC's cq on
	// libx265).
	unlessNull bool
}

// one is a row of one key whose value is unit's words of it, no detail.
func one(key, label string, unit func(v any) string) setting {
	return setting{key: key, label: label, words: func(got func(string) (any, bool)) (string, *string) {
		v, _ := got(key)
		return unit(v), nil
	}}
}

// unlessNull is s, left out when its key is reported as null.
func unlessNull(s setting) setting {
	s.unlessNull = true
	return s
}

// allowedRate writes a bit rate a worker allows, given in Mbit/s, as a
// bound: "at most 8 Mbit/s"; 0 allows any.
func allowedRate(v any) string {
	if f, ok := v.(float64); ok {
		if f == 0 {
			return "any"
		}
		return "at most " + rateText(f*1e6)
	}
	return plain(v)
}

// allowedSize writes a file size the transcoder allows, given in GiB, as a
// bound in the clients' decimal units with the GiB as set: "at most 16.1 GB
// (15 GiB)"; 0 allows any.
func allowedSize(v any) string {
	if f, ok := v.(float64); ok {
		if f == 0 {
			return "any"
		}
		return "at most " + sizeText(f*gib) + " (" + number(f) + nbsp + "GiB)"
	}
	return plain(v)
}

// pixels writes a count of pixels: "2,000 pixels".
func pixels(v any) string {
	if f, ok := v.(float64); ok && f == float64(int(f)) {
		return countText(int(f)) + nbsp + plural(int(f), "pixel", "pixels")
	}
	return plain(v)
}

// encodeRate writes the bit rate an encode is made at: up to the caps the
// panel lists above it, unless the transcoder sets one of its own.
func encodeRate(v any) string {
	if v == nil {
		return "up to the caps above"
	}
	return allowedRate(v)
}

// renditions is a row of a ladder in words, its spec the detail: "one, at
// the original’s size, in HEVC" of source:hevc; a spec the words can't read
// is said as it is. On a CPU encoder, a rendition at the original's size
// that leaves its codec to the default keeps an H.264 original as it is
// when browsers can play it (the transcoder's CPU rule).
func renditions(key, label string) setting {
	return setting{key: key, label: label, words: func(got func(string) (any, bool)) (string, *string) {
		v, _ := got(key)
		spec, ok := v.(string)
		if !ok {
			return plain(v), nil
		}
		if strings.TrimSpace(spec) == "" {
			spec = "source"
		}
		codec, _ := got("encoder.codec")
		cpu := false
		if c, ok := codec.(string); ok && strings.EqualFold(strings.TrimSpace(c), "libx265") {
			cpu = true
		}
		if words, ok := ladderWords(spec, cpu); ok {
			return words, strp(spec)
		}
		return spec, nil
	}}
}

// rung is a rendition a ladder plans: at the original's size or a height,
// in a codec (named, or the default), at a maxrate of its own (0: none).
type rung struct {
	size, codec   string
	named         bool
	maxrate       float64
	originalsSize bool
}

// ladderWords says a ladder spec (<source|NNNp>[:<hevc|h264>][:<maxrate>],
// comma-separated) in words; ok false for one it can't read.
func ladderWords(spec string, cpu bool) (string, bool) {
	var rungs []rung
	for _, tok := range strings.Split(spec, ",") {
		parts := strings.Split(strings.TrimSpace(tok), ":")
		name := strings.ToLower(strings.TrimSpace(parts[0]))
		r := rung{}
		switch {
		case name == "source":
			r.size, r.codec, r.originalsSize = "at the original’s size", "HEVC", true
		case heightRE.MatchString(name):
			r.size, r.codec = name, "H.264"
		default:
			return "", false
		}
		for _, p := range parts[1:] {
			switch p = strings.ToLower(strings.TrimSpace(p)); {
			case p == "hevc":
				r.codec, r.named = "HEVC", true
			case p == "h264":
				r.codec, r.named = "H.264", true
			default:
				bps, ok := parseBitrate(p)
				if !ok || bps <= 0 {
					return "", false
				}
				r.maxrate = bps
			}
		}
		rungs = append(rungs, r)
	}
	if len(rungs) == 0 {
		return "", false
	}
	codecOf := func(r rung) string {
		if cpu && r.originalsSize && !r.named && r.codec == "HEVC" {
			return "HEVC (or H.264 as it is when browsers can play it)"
		}
		return r.codec
	}
	same, sizes := true, make([]string, 0, len(rungs))
	for _, r := range rungs {
		sizes = append(sizes, r.size)
		if codecOf(r) != codecOf(rungs[0]) || r.maxrate > 0 {
			same = false
		}
	}
	count := countWord(len(rungs))
	if same {
		return count + ", " + joinAnd(sizes) + ", in " + codecOf(rungs[0]), true
	}
	var each []string
	for _, r := range rungs {
		w := r.size + " in " + codecOf(r)
		if r.maxrate > 0 {
			w += " (at most " + rateText(r.maxrate) + ")"
		}
		each = append(each, w)
	}
	return count + ": " + joinAnd(each), true
}

// heightRE is a rung's height: "720p".
var heightRE = regexp.MustCompile(`^[0-9]{3,4}p$`)

// countWord is a count in words to ten: "one", "two"; above, in digits.
func countWord(n int) string {
	if n >= 1 && n <= 10 {
		return []string{"one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten"}[n-1]
	}
	return countText(n)
}

// surroundTrackRow is the packager's surround track in words: "5.1 E-AC-3,
// 448 kbit/s" (its layout, its codec, the bit rate it's encoded at), none
// when it makes none; the report's values its detail.
func surroundTrackRow(got func(string) (any, bool)) (string, *string) {
	codec, _ := got("surroundAudio")
	c, ok := codec.(string)
	if !ok {
		return plain(codec), nil
	}
	detail := rawOf(got, "surroundAudio", "surroundLayout", "surroundBitrate")
	if strings.EqualFold(strings.TrimSpace(c), "off") || strings.TrimSpace(c) == "" {
		return "none", detail
	}
	words := audioCodecName(c)
	if layout, ok := got("surroundLayout"); ok {
		if l, ok := layout.(string); ok && strings.TrimSpace(l) != "" {
			words = strings.TrimSpace(l) + " " + words
		}
	}
	if rate, ok := got("surroundBitrate"); ok {
		words += ", " + bitrate(rate)
	}
	return words, detail
}

// stereoTrackRow is the packager's stereo track in words: its codec where it
// reports it, and its bit rate ("AAC, 192 kbit/s"); the report's values its
// detail.
func stereoTrackRow(got func(string) (any, bool)) (string, *string) {
	var words []string
	if codec, ok := got("stereoAudio"); ok {
		if c, ok := codec.(string); ok && strings.TrimSpace(c) != "" {
			words = append(words, audioCodecName(c))
		}
	}
	if rate, ok := got("stereoBitrate"); ok {
		words = append(words, bitrate(rate))
	}
	return strings.Join(words, ", "), rawOf(got, "stereoAudio", "stereoBitrate")
}

// rawOf is the values of keys as the report gives them, "key=value" each.
func rawOf(got func(string) (any, bool), keys ...string) *string {
	var parts []string
	for _, k := range keys {
		if v, ok := got(k); ok {
			parts = append(parts, k+"="+plain(v))
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return strp(strings.Join(parts, ", "))
}

// bitrate writes a bit rate the packager writes as 448k.
func bitrate(v any) string {
	if s, ok := v.(string); ok {
		if bps, ok := parseBitrate(s); ok {
			return rateText(bps)
		}
	}
	return plain(v)
}

// forms writes the forms of image subtitles the packager keeps.
func forms(v any) string {
	list, ok := v.([]any)
	if !ok {
		return plain(v)
	}
	if len(list) == 0 {
		return "none"
	}
	names := make([]string, 0, len(list))
	for _, f := range list {
		if s, ok := f.(string); ok {
			names = append(names, formName(s))
		}
	}
	return strings.Join(names, ", ")
}

// settings are the rows of the rules panel, in the order it lists them; any
// other key a report holds follows, by name, as it is.
var settings = []setting{
	one("caps.movie1080Mbps", "Video bit rate, 1080p movies", allowedRate),
	one("caps.movie2160Mbps", "Video bit rate, 2160p movies", allowedRate),
	one("caps.movieMaxGiB", "File size, movies", allowedSize),
	one("caps.episode1080Mbps", "Video bit rate, 1080p episodes", allowedRate),
	one("caps.episode2160Mbps", "Video bit rate, 2160p episodes", allowedRate),
	one("bucket2160.minHeight", "2160p from a height of", pixels),
	one("bucket2160.minWidth", "2160p from a width of", pixels),
	one("encoder.codec", "Encoder", plain),
	one("encoder.preset", "Encoder preset", plain),
	unlessNull(one("encoder.cq", "Constant quality", plain)),
	unlessNull(one("encoder.crf", "Constant rate factor", plain)),
	one("encoder.maxrate1080Mbps", "Encode bit rate, 1080p", encodeRate),
	one("encoder.maxrate2160Mbps", "Encode bit rate, 2160p", encodeRate),
	renditions("ladder", "Renditions"),
	renditions("extraLadder", "Renditions of extras"),
	{key: "surroundAudio", label: "Surround track", with: []string{"surroundLayout", "surroundBitrate"}, words: surroundTrackRow},
	{key: "stereoAudio", label: "Stereo track", with: []string{"stereoBitrate"}, words: stereoTrackRow},
	one("imageSubtitles", "Image subtitles kept", forms),
}

// dvProfileRE is a key of the transcoder's Dolby Vision, a profile's.
var dvProfileRE = regexp.MustCompile(`^dolbyVision\.profile([0-9]+)$`)

// dolbyVisionWords is what the transcoder does with an original of a Dolby
// Vision profile, in words: "reshaped to HDR10", "left as it is".
func dolbyVisionWords(v any) string {
	switch s, _ := v.(string); s {
	case "reshaped":
		return "reshaped to HDR10"
	case dvRefused:
		return "left as it is"
	}
	return plain(v)
}

// skipped are keys a report carries that the panel does not list: the
// transcoder's ladder in parts, which its spec says.
var skipped = map[string]bool{"ladderRungs": true}

// settingsOf writes out the rules of a report's rules raw, a row each, in
// the panel's order: a row is listed when the report gives any of its keys,
// by the first it gives; nested objects by their dotted key.
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
	got := func(key string) (any, bool) {
		v, ok := leaves[key]
		return v, ok
	}
	out := []graph.RuleSetting{}
	said := map[string]bool{}
	for _, s := range settings {
		keys := append([]string{s.key}, s.with...)
		first := ""
		for _, k := range keys {
			if _, ok := leaves[k]; ok {
				first = k
				break
			}
		}
		if first == "" || s.unlessNull && leaves[s.key] == nil {
			for _, k := range keys {
				said[k] = true
			}
			continue
		}
		value, detail := s.words(got)
		out = append(out, graph.RuleSetting{Key: first, Label: s.label, Value: value, Detail: detail})
		for _, k := range keys {
			said[k] = true
		}
	}
	// the transcoder's Dolby Vision, a row each profile it names, by profile
	var profiles []string
	for k := range leaves {
		if !said[k] && dvProfileRE.MatchString(k) {
			profiles = append(profiles, k)
		}
	}
	sort.Slice(profiles, func(i, j int) bool {
		a, _ := strconv.Atoi(dvProfileRE.FindStringSubmatch(profiles[i])[1])
		b, _ := strconv.Atoi(dvProfileRE.FindStringSubmatch(profiles[j])[1])
		return a < b
	})
	for _, k := range profiles {
		v := leaves[k]
		out = append(out, graph.RuleSetting{Key: k, Label: "Dolby Vision profile " + dvProfileRE.FindStringSubmatch(k)[1],
			Value: dolbyVisionWords(v), Detail: strp(plain(v))})
		said[k] = true
	}
	rest := make([]string, 0, len(leaves))
	for k := range leaves {
		if !said[k] {
			rest = append(rest, k)
		}
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
