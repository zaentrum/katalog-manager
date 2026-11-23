package library

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// The contract's schema of a source record: source.schema.json and the
// definitions it refers to, as the contract's tests vendor them at 71999959
// (testdata/contract, pinned by SHA256SUMS). The schemas' validator takes a
// source record only where it validates against it and names its folder
// (its sourceId), and its gate keeps the original of any other; so does this
// gate (sourceRecordRefused).
var (
	//go:embed testdata/contract/library/v2/source.schema.json
	sourceSchemaText []byte
	//go:embed testdata/contract/library/v2/defs.schema.json
	defsSchemaText []byte
)

// schemaBase is where the schemas' $id places them.
const schemaBase = "https://zaentrum.github.io/schemas/library/v2/"

// sourceSchema is the contract's schema of a source record, compiled once as
// the validator reads it (draft 2020-12, its formats asserted): of each
// pattern Python's re would match no less than (schemaPattern), of a
// date-time no less strict than its checker (schemaDateTime).
var sourceSchema = sync.OnceValues(func() (*jsonschema.Schema, error) {
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	c.RegisterFormat(&jsonschema.Format{Name: "date-time", Validate: schemaDateTime})
	c.UseRegexpEngine(schemaPattern)
	for name, text := range map[string][]byte{"source.schema.json": sourceSchemaText, "defs.schema.json": defsSchemaText} {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(text))
		if err != nil {
			return nil, err
		}
		if err := c.AddResource(schemaBase+name, doc); err != nil {
			return nil, err
		}
	}
	return c.Compile(schemaBase + "source.schema.json")
})

// sourceRecordRefused says why the contract's validator takes the source
// record rec, read from the folder sources/<folder>/, for no source record:
// its schema refuses it, or it names another folder (sourceId); "" when it
// takes it. A schema that can't be read takes none.
func sourceRecordRefused(rec Doc, folder string) (why string) {
	// a reading that fails takes nothing (the schema library on a form no
	// writer writes, which writtenForm refuses before it gets here)
	defer func() {
		if r := recover(); r != nil {
			why = fmt.Sprintf("the contract's schema can't be read of it: it failed (%v)", r)
		}
	}()
	if why := textRules(rec, ""); why != "" {
		return "the contract's validator refuses it: " + why
	}
	sch, err := sourceSchema()
	if err != nil {
		return "the contract's schema of a source record can't be read: " + err.Error()
	}
	if err := sch.Validate(schemaValue(rec)); err != nil {
		var v *jsonschema.ValidationError
		if errors.As(err, &v) {
			return "the contract's schema refuses it: " + schemaWhy(v)
		}
		return "the contract's schema refuses it: " + err.Error()
	}
	if id, _ := rec.Get("sourceId"); id != folder {
		return fmt.Sprintf("its sourceId %s doesn't name its folder %s", pyStr(id), folder)
	}
	return ""
}

// sourceRecordCheck is sourceRecordRefused, which tests that write a record by
// hand fill in first (TestMain).
var sourceRecordCheck = sourceRecordRefused

// sourceRecordTaken is the gate's reading of the bytes raw of a source
// record in the folder sources/<folder>/, and why it takes them for none -
// they nest past writtenDepth, LoadRecord can't read them, they are in no
// form its writers write (writtenForm), or the contract's validator takes
// them for no source record (sourceRecordRefused) -, "" when it takes them.
func sourceRecordTaken(raw []byte, folder string) (Doc, string) {
	if why := nestedPast(raw); why != "" {
		return nil, "it is in no form the platform's writers write: " + why
	}
	d, err := LoadRecord(raw)
	if err != nil {
		return nil, "it can't be read: " + err.Error()
	}
	if why := writtenForm(raw); why != "" {
		return d, "it is in no form the platform's writers write: " + why
	}
	return d, sourceRecordRefused(d, folder)
}

// schemaWhy is the first of the deepest causes v gives, where it lies.
func schemaWhy(v *jsonschema.ValidationError) string {
	for len(v.Causes) > 0 {
		v = v.Causes[0]
	}
	where := "/" + strings.Join(v.InstanceLocation, "/")
	return where + ": " + v.ErrorKind.LocalizedString(message.NewPrinter(language.English))
}

// schemaValue is the decoded record v as the schema is checked against it,
// with the validator's types: a number Python's json reads as no int (one
// written with a point or an exponent) or whose int lies outside 64 bits is
// no integer to the validator, which redefines integer so, while JSON Schema
// takes 7.0 for one - such a number is moved 1e-30 toward the inside of the
// bounds the schema states (up from 0, down from above it, up from below
// it), which no integer is: the schema bounds a number at 0 and 1 alone (a
// window's at, a weight, a confidence, a gop), so a number on a bound stays
// on its side of it, and no other bound or value lies within 1e-30.
func schemaValue(v any) any {
	switch x := v.(type) {
	case Doc:
		m := make(map[string]any, len(x))
		for _, f := range x {
			m[f.Key] = schemaValue(f.Value)
		}
		return m
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = schemaValue(e)
		}
		return out
	case json.Number:
		s := x.String()
		r, ok := new(big.Rat).SetString(s)
		if !ok || !r.IsInt() {
			return x
		}
		if !strings.ContainsAny(s, ".eE") && r.Num().IsInt64() {
			return x
		}
		if r.Sign() > 0 {
			return json.Number(r.Sub(r, schemaNudge).String())
		}
		return json.Number(r.Add(r, schemaNudge).String())
	}
	return v
}

// schemaNudge is 1e-30.
var schemaNudge = new(big.Rat).SetFrac(big.NewInt(1), new(big.Int).Exp(big.NewInt(10), big.NewInt(30), nil))

// schemaDateTime is the date-time format as no less strict than the
// validator's checker (RFC 3339 by rfc3339-validator): a time Go's own RFC
// 3339 reading takes, of a year from 1, its offset from UTC hours to 23 and
// minutes to 59 (Go's reading takes +24:00 and +23:60, the checker doesn't).
func schemaDateTime(v any) error {
	s, ok := v.(string)
	if !ok {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return err
	}
	if t.Year() < 1 {
		return errors.New("a year before 1")
	}
	if n := len(s); n > 6 && (s[n-6] == '+' || s[n-6] == '-') && (s[n-5:n-3] > "23" || s[n-2:] > "59") {
		return errors.New("an offset from UTC past 23:59")
	}
	return nil
}

// schemaPattern compiles a pattern of the schema to a matcher that matches no
// string Python's re.search doesn't, as the validator reads a pattern: Go's
// regular expressions, whose $ matches at the end alone (Python's before a
// final newline too); and the two patterns of the definitions that look
// ahead, which Go's don't, read as they say of a string of one line (one of
// more lines is none they take).
func schemaPattern(pattern string) (jsonschema.Regexp, error) {
	switch pattern {
	case `^(?!\.{1,2}$)[^/]+$`: // fileName
		return lineMatcher{pattern, func(s string) bool {
			return s != "" && s != "." && s != ".." && !strings.Contains(s, "/")
		}}, nil
	case `^(?!/)(?!.*(^|/)\.\.(/|$)).+$`: // relPath
		return lineMatcher{pattern, func(s string) bool {
			return s != "" && !strings.HasPrefix(s, "/") && !slices.Contains(strings.Split(s, "/"), "..")
		}}, nil
	}
	return regexp.Compile(pattern)
}

// lineMatcher is a pattern matched by its function, of a string of one line.
type lineMatcher struct {
	pattern string
	match   func(string) bool
}

func (m lineMatcher) MatchString(s string) bool { return !strings.Contains(s, "\n") && m.match(s) }
func (m lineMatcher) String() string            { return m.pattern }
