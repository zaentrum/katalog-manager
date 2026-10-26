package graph

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// complianceContract is the library compliance's GraphQL contract, as the
// console is built against it, with what was added to it since (the totals
// of the whole filter and the queue's counts; each title a request skipped,
// and why; the totals of the titles a request queues): the schema may add to
// it, and may rename or remove nothing of it.
const complianceContract = `
enum ComplianceRule { NOT_PACKAGED PACKAGING_FAILED OVER_BITRATE_CAP OVER_SIZE_CAP NO_SURROUND IMAGE_SUBTITLES_MISSING VIDEO_NOT_HEVC }
enum ComplianceKind { MOVIE EPISODE }
type RuleSetting { key: String! label: String! value: String! }
type WorkerRules { worker: String! version: String reportedAt: String! rules: [RuleSetting!]! }
type PipelineRules { transcoder: WorkerRules packager: WorkerRules }
type ComplianceFinding { rule: ComplianceRule! text: String! measured: String limit: String }
type ComplianceRow {
  itemId: ID! title: String! year: Int kind: ComplianceKind!
  seriesId: ID seriesTitle: String season: Int episode: Int
  findings: [ComplianceFinding!]!
  originalBytes: Float packageBytes: Float estimatedBytes: Float
  queue: String
  evaluatedAt: String!
}
type ComplianceSummary { rule: ComplianceRule! titles: Int! packageBytes: Float! estimatedBytes: Float }
type ComplianceReport {
  rules: PipelineRules!
  notEvaluated: [String!]!
  evaluatedAt: String
  evaluating: Boolean!
  total: Int!
  summary: [ComplianceSummary!]!
  rows: [ComplianceRow!]!
  nextCursor: String
}
type ComplianceQueued { queued: Int! alreadyQueued: Int! skipped: Int! message: String! }
extend type Query {
  pipelineRules: PipelineRules!
  libraryCompliance(kind: ComplianceKind, rules: [ComplianceRule!], search: String, first: Int = 100, after: String): ComplianceReport!
}
extend type Mutation {
  queueRework(items: [ID!]!): ComplianceQueued!
  queueReworkMatching(kind: ComplianceKind, rules: [ComplianceRule!], search: String): ComplianceQueued!
  refreshCompliance(kind: ComplianceKind): Boolean!
}
type ComplianceTotals { titles: Int! originalBytes: Float! packageBytes: Float! estimatedBytes: Float }
type ComplianceQueueCounts { queued: Int! processing: Int! processed: Int! failed: Int! }   # the re-encode queue, all titles
extend type ComplianceReport { matching: ComplianceTotals!  queue: ComplianceQueueCounts! }
type ComplianceSkip { itemId: ID! title: String! reason: String! }
extend type ComplianceQueued { skippedTitles: [ComplianceSkip!]! }
extend type ComplianceReport { queueable: ComplianceTotals! }   # the matching titles NOT queued or processing: what "select all N matching" queues
extend type ComplianceFinding { detail: String }   # tool output or ids, e.g. "ffmpeg exited 1", "avc1.640028"; never in the text
extend type ComplianceRow { queueDetail: String }   # the queue reason's technical part
extend type RuleSetting { detail: String }   # raw config, e.g. "source:hevc", "libx265"
extend type ComplianceQueued {
  queuedText: String      # toast, no full stop; null when nothing was queued
  alreadyText: String     # second plain toast; null when none
  notQueuedText: String   # warning banner line, titles under skippedTitles; null when none
}
extend type ComplianceRow {
  resolution: String            # the package's video, else the original's
  videoCodec: String
  videoBitrate: Float           # bit/s
  videoBitrateEstimated: Boolean!
  bitrateCap: Float             # bit/s
  sizeCap: Float                # bytes
  durationSeconds: Float
  packageAudio: String
  originalAudio: String
  outcome: ComplianceOutcome
}
enum ComplianceOutcome { REENCODE REPACKAGE FIRST_PACKAGE BLOCKED }
`

var (
	contractBlockRE = regexp.MustCompile(`(enum|type|extend type)\s+(\w+)\s*\{([^}]*)\}`)
	contractFieldRE = regexp.MustCompile(`(\w+)\s*(?:\(([^)]*)\))?\s*:\s*([\[\]\w!]+)`)
)

// introspected is a type as introspection gives it.
type introspected struct {
	Name   string
	Kind   string
	Fields []struct {
		Name string
		Type typeRef
		Args []struct {
			Name         string
			Type         typeRef
			DefaultValue *string
		}
	}
	EnumValues []struct{ Name string }
}

// sdlOf writes a type as the SDL writes it: [ComplianceRule!]!.
func sdlOf(t typeRef) string {
	switch t.Kind {
	case "NON_NULL":
		return sdlOf(*t.OfType) + "!"
	case "LIST":
		return "[" + sdlOf(*t.OfType) + "]"
	}
	return t.Name
}

// The schema holds the library compliance's contract to the letter: every
// enum with its values, every type with each of its fields of its type, and
// the root fields with their arguments and the default of first. A field it
// adds is no breach; one renamed, removed or of another type is.
func TestTheComplianceContract(t *testing.T) {
	resp := MustSchema(NewResolver(nil, testConfig, Services{})).Exec(context.Background(),
		`{ __schema { types { name kind enumValues { name }
			fields { name type { `+typeFields+` } args { name defaultValue type { `+typeFields+` } } } } } }`, "", nil)
	if len(resp.Errors) > 0 {
		t.Fatal(resp.Errors)
	}
	var s struct {
		Schema struct{ Types []introspected } `json:"__schema"`
	}
	if err := json.Unmarshal(resp.Data, &s); err != nil {
		t.Fatal(err)
	}
	types := map[string]introspected{}
	for _, ty := range s.Schema.Types {
		types[ty.Name] = ty
	}
	blocks := contractBlockRE.FindAllStringSubmatch(complianceContract, -1)
	if len(blocks) != 24 {
		t.Fatalf("the contract reads as %d blocks, want 24", len(blocks))
	}
	for _, b := range blocks {
		kind, name, body := b[1], b[2], b[3]
		ty, ok := types[name]
		if !ok {
			t.Errorf("%s is not in the schema", name)
			continue
		}
		if kind == "enum" {
			want := strings.Fields(body)
			var got []string
			for _, v := range ty.EnumValues {
				got = append(got, v.Name)
			}
			if strings.Join(got, " ") != strings.Join(want, " ") {
				t.Errorf("enum %s: %v, want %v", name, got, want)
			}
			continue
		}
		fields := map[string]int{}
		for i, f := range ty.Fields {
			fields[f.Name] = i
		}
		for _, m := range contractFieldRE.FindAllStringSubmatch(body, -1) {
			field, args, typ := m[1], m[2], m[3]
			i, ok := fields[field]
			if !ok {
				t.Errorf("%s.%s is not in the schema", name, field)
				continue
			}
			f := ty.Fields[i]
			if got := sdlOf(f.Type); got != typ {
				t.Errorf("%s.%s is %s, want %s", name, field, got, typ)
			}
			var want, got []string
			for _, a := range strings.Split(args, ",") {
				if a = strings.TrimSpace(a); a != "" {
					want = append(want, strings.Join(strings.Fields(a), " "))
				}
			}
			for _, a := range f.Args {
				s := a.Name + ": " + sdlOf(a.Type)
				if a.DefaultValue != nil {
					s += " = " + *a.DefaultValue
				}
				got = append(got, s)
			}
			sort.Strings(want)
			sort.Strings(got)
			if strings.Join(got, ", ") != strings.Join(want, ", ") {
				t.Errorf("%s.%s takes (%s), want (%s)", name, field, strings.Join(got, ", "), strings.Join(want, ", "))
			}
		}
	}
}
