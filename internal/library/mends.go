package library

import (
	"fmt"
	"sort"
	"strings"
)

// MendKinds are the kinds of mend package.schema.json lists (mend.kind): a
// closed list, as a new kind is a change of the format.
var MendKinds = map[string]bool{"avsync-editlist": true}

// mend is one entry of a package's mends: a change made to one file of the
// package after it completed, which only boxes of a rendition's init
// segment take part in. A mend writes the file's line in checksums.sha256
// again; the entries are its history, the first one's before the hash the
// file had as the package was written.
type mend struct {
	kind, file, before, after string
}

// mendsOf is the entries of package.json's mends by the file each changed,
// in the order they were made, or why they are no mends.
func mendsOf(pkg Doc) (map[string][]mend, error) {
	v, ok := pkg.Get("mends")
	if !ok {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok || len(list) == 0 {
		return nil, fmt.Errorf("its mends is no list of entries")
	}
	out := map[string][]mend{}
	for i, x := range list {
		e, ok := x.(Doc)
		if !ok {
			return nil, fmt.Errorf("its mends[%d] is no entry", i)
		}
		m := mend{kind: str(e, "kind"), file: str(e, "file"), before: str(e, "sha256Before"), after: str(e, "sha256After")}
		if !MendKinds[m.kind] {
			return nil, fmt.Errorf("its mends[%d] is of the kind %q, which the format does not list", i, m.kind)
		}
		if m.file == "" || !strings.HasPrefix(m.before, "sha256:") || !strings.HasPrefix(m.after, "sha256:") {
			return nil, fmt.Errorf("its mends[%d] names no file, or not the hashes before and after", i)
		}
		out[m.file] = append(out[m.file], m)
	}
	return out, nil
}

// mendsProblem is what is wrong with the entries of a package's mends
// against its checksums (listed: path → hex digest), "" when nothing is: a
// mend writes the mended file's line in checksums.sha256 again, so each
// file's entries run with no gap — each made to the bytes the one before left
// — and the last one ends at the digest the checksums list now. (That the
// file matches its line is the check every listed file gets.)
func mendsProblem(mended map[string][]mend, listed map[string]string) string {
	files := make([]string, 0, len(mended))
	for f := range mended {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, f := range files {
		entries := mended[f]
		digest, ok := listed[f]
		if !ok {
			return fmt.Sprintf("its mends name %s, which checksums.sha256 does not list", f)
		}
		for i := 1; i < len(entries); i++ {
			if entries[i].before != entries[i-1].after {
				return fmt.Sprintf("its mends of %s have a gap: one was made to %s, and the one before left %s",
					f, entries[i].before, entries[i-1].after)
			}
		}
		if last := entries[len(entries)-1].after; last != "sha256:"+digest {
			return fmt.Sprintf("the last of its mends of %s left %s, and checksums.sha256 lists sha256:%s: "+
				"the checksums were not written again after it", f, last, digest)
		}
	}
	return ""
}
