package compliance

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/zaentrum/katalog-manager/internal/graph"
)

// The orders the report's rows are sorted in (ComplianceSort): by the size
// of their package (the default, then their original's), of their original
// (then their package's), by what processing them again saves (their
// package's size less their estimate), by their video's bit rate, and by
// their title (an episode by its series', then by its season and its
// number).
const (
	SortPackageSize  = "PACKAGE_SIZE"
	SortOriginalSize = "ORIGINAL_SIZE"
	SortSaving       = "SAVING"
	SortVideoBitrate = "VIDEO_BITRATE"
	SortTitle        = "TITLE"
)

// Sorts are the orders, as the report names them.
var Sorts = []string{SortPackageSize, SortOriginalSize, SortSaving, SortVideoBitrate, SortTitle}

// sortKey is a key an order sorts by: its SQL over the report's rows (f),
// and its type, which its value in a cursor is read as.
type sortKey struct{ expr, typ string }

// sortKeys are the keys of each order, in turn; a row's id ends every one.
var sortKeys = map[string][]sortKey{
	SortPackageSize:  {{"f.packagebytes", "bigint"}, {"f.originalbytes", "bigint"}},
	SortOriginalSize: {{"f.originalbytes", "bigint"}, {"f.packagebytes", "bigint"}},
	SortSaving:       {{"f.packagebytes - f.estimatedbytes", "bigint"}, {"f.packagebytes", "bigint"}},
	SortVideoBitrate: {{"f.videobitrate", "float8"}, {"f.packagebytes", "bigint"}},
	SortTitle: {{"lower(COALESCE(f.series_title, f.title))", "text"}, {"f.seasonnumber", "integer"},
		{"f.episodenumber", "integer"}, {"lower(f.title)", "text"}},
}

// zeros are the values a null key is read as, beside its flag.
var zeros = map[string]string{"bigint": "0", "float8": "0", "integer": "0", "text": "''"}

// sorting is an order of the report's rows as SQL: each key a flag that
// sorts a null one last, whichever the direction, and its value, in the
// order's direction, then the row's id, ascending.
type sorting struct {
	order graph.ComplianceOrder
	keys  []sortKey
	// cols are the order's columns, every key's flag and value, then the id;
	// orderBy orders by them.
	cols    []string
	orderBy string
}

// sortingOf is the sorting of o; an order of no sort Sorts names is an error.
func sortingOf(o graph.ComplianceOrder) (sorting, error) {
	if o.Sort == "" {
		o.Sort = SortPackageSize
	}
	keys, ok := sortKeys[o.Sort]
	if !ok {
		return sorting{}, fmt.Errorf("unknown sort %q: one of %s", o.Sort, strings.Join(Sorts, ", "))
	}
	dir, flag := "ASC", "IS NULL"
	if o.Descending {
		// a null key's flag is false, which a descending order puts last
		dir, flag = "DESC", "IS NOT NULL"
	}
	s := sorting{order: o, keys: keys}
	for _, k := range keys {
		s.cols = append(s.cols, "(("+k.expr+") "+flag+")", "COALESCE("+k.expr+", "+zeros[k.typ]+")")
	}
	parts := make([]string, len(s.cols))
	for i, c := range s.cols {
		parts[i] = c + " " + dir
	}
	s.cols = append(s.cols, "f.item_id")
	s.orderBy = strings.Join(append(parts, "f.item_id ASC"), ", ")
	return s, nil
}

// after is the SQL condition that a row comes after the cursor's, its
// values the arguments from $first on, as their types read them: its keys
// past the cursor's in the order's direction, or the same keys and its id
// past the cursor's.
func (s sorting) after(first int) string {
	params := make([]string, 0, len(s.cols))
	n := first
	for _, k := range s.keys {
		params = append(params, "$"+strconv.Itoa(n)+"::boolean", "$"+strconv.Itoa(n+1)+"::"+k.typ)
		n += 2
	}
	op := ">"
	if s.order.Descending {
		op = "<"
	}
	keys := "(" + strings.Join(s.cols[:len(s.cols)-1], ", ") + ")"
	cur := "(" + strings.Join(params, ", ") + ")"
	return "(" + keys + " " + op + " " + cur + " OR (" + keys + " = " + cur + " AND f.item_id > $" + strconv.Itoa(n) + "::text))"
}

// orderCursor is where a page of the report ended, in its order: the order,
// and the last row's columns of it (each key's flag and value, then its
// id).
type orderCursor struct {
	V    int    `json:"v"`
	Sort string `json:"s"`
	Desc bool   `json:"d"`
	Cols []any  `json:"k"`
}

// columns are the destinations a row's order columns are scanned into.
func (s sorting) columns() []any {
	out := make([]any, 0, len(s.cols))
	for _, k := range s.keys {
		var flag bool
		out = append(out, &flag)
		switch k.typ {
		case "text":
			out = append(out, new(string))
		case "float8":
			out = append(out, new(float64))
		default:
			out = append(out, new(int64))
		}
	}
	return append(out, new(string))
}

// values are the values scanned into columns.
func (s sorting) values(cols []any) []any {
	out := make([]any, len(cols))
	for i, c := range cols {
		switch v := c.(type) {
		case *bool:
			out[i] = *v
		case *string:
			out[i] = *v
		case *float64:
			out[i] = *v
		case *int64:
			out[i] = *v
		}
	}
	return out
}

// cursorOf is the cursor after a row whose order columns are cols.
func (s sorting) cursorOf(cols []any) string {
	b, _ := json.Marshal(orderCursor{V: 2, Sort: s.order.Sort, Desc: s.order.Descending, Cols: cols})
	return base64.RawURLEncoding.EncodeToString(b)
}

// parse reads a cursor of this order into its arguments, nil for none: a
// cursor that is none of the report's, or one of another order, is an
// error.
func (s sorting) parse(after string) ([]any, error) {
	if after == "" {
		return nil, nil
	}
	bad := fmt.Errorf("after is %q, which is no cursor of the report: pass a page's nextCursor", after)
	b, err := base64.RawURLEncoding.DecodeString(after)
	if err != nil {
		return nil, bad
	}
	var c orderCursor
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if dec.Decode(&c) != nil || c.V != 2 {
		return nil, bad
	}
	if c.Sort != s.order.Sort || c.Desc != s.order.Descending {
		return nil, fmt.Errorf("after is a cursor of another order (%s, descending %v): pass a nextCursor of this one's",
			c.Sort, c.Desc)
	}
	if len(c.Cols) != 2*len(s.keys)+1 {
		return nil, bad
	}
	args := make([]any, 0, len(c.Cols))
	for i, k := range s.keys {
		flag, ok := c.Cols[2*i].(bool)
		if !ok {
			return nil, bad
		}
		v, ok := valueOf(c.Cols[2*i+1], k.typ)
		if !ok {
			return nil, bad
		}
		args = append(args, flag, v)
	}
	id, ok := c.Cols[len(c.Cols)-1].(string)
	if !ok || id == "" {
		return nil, bad
	}
	return append(args, id), nil
}

// valueOf is a key's value in a cursor as its type reads it.
func valueOf(v any, typ string) (any, bool) {
	switch typ {
	case "text":
		s, ok := v.(string)
		return s, ok
	case "bigint", "integer":
		n, ok := v.(json.Number)
		if !ok {
			return nil, false
		}
		i, err := n.Int64()
		return i, err == nil
	case "float8":
		n, ok := v.(json.Number)
		if !ok {
			return nil, false
		}
		f, err := n.Float64()
		return f, err == nil
	}
	return nil, false
}
