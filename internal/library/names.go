package library

import (
	"regexp"
	"strconv"
	"strings"
)

// The names the library gives an original in its version folder
// (versions/<versionId>/), as the record logic names them (the schemas'
// libv2_records.library_original_name, which the packager and the
// migration's tool name it by): "original", "-<part>" for a version split in
// parts (counted from 1, in part order), and "." with the extension of the
// name it arrived with, lower-cased when that is 1 to 8 letters a-z and
// digits, else "bin". Nothing else of the name it arrived with is kept: no
// record, no folder and no file of the library says where a file came from.

// originalNameRE is a name the library gives an original.
var originalNameRE = regexp.MustCompile(`^original(-[1-9][0-9]*)?\.[a-z0-9]{1,8}$`)

// extRE is an extension the library keeps, lower-cased.
var extRE = regexp.MustCompile(`^[a-z0-9]{1,8}$`)

// OriginalName is the name an original that arrived as name (a file's name,
// or its path) gets in its version folder: original.<ext>, or
// original-<part>.<ext> for the part of a version split in parts (part 0
// for none).
func OriginalName(name string, part int) string {
	ext := strings.ToLower(strings.ReplaceAll(splitExt(name), "\u0130", "i\u0307"))
	if !extRE.MatchString(ext) {
		ext = "bin"
	}
	if part > 0 {
		return "original-" + strconv.Itoa(part) + "." + ext
	}
	return "original." + ext
}

// IsOriginalName reports whether name is one the library gives an original
// in its version folder.
func IsOriginalName(name string) bool { return originalNameRE.MatchString(name) }

// splitExt is the extension of name, as Python's os.path.splitext reads it (the
// record logic's): what follows the last dot of its last component, without
// the dot, unless only dots come before that one ("" for ".hidden" and
// "..x", and for a name with no dot).
func splitExt(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	dot := strings.LastIndex(name, ".")
	if dot < 0 || strings.TrimLeft(name[:dot], ".") == "" {
		return ""
	}
	return name[dot+1:]
}
