package scanner

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/zaentrum/katalog-manager/internal/processing"
)

// What a scan makes of a file, for whoever gives a title a file outside a
// scan (itemactions' replaceSource): whether the scan takes it for a title's
// file, and which subtitle files it pairs with a video.

// NoTitleFile says why a scan takes the file at path, an absolute path under
// its root, for no title's file; "" when it takes it for one. A scan passes
// over a hidden file (its name begins with a dot), a disc image and one that
// is no video by its extension, and takes a file for an extra by the
// convention as it reads with extras.scan on, which takes every file it takes
// with the setting off too (one the scanner always took for a trailer). A
// title whose file is such an extra loses it once a scan with the setting on
// meets it, as a trailer once scanned as a title of its own does (extras.go).
func NoTitleFile(path string) string {
	name := filepath.Base(path)
	switch {
	case strings.HasPrefix(name, "."):
		return "its name begins with a dot, and a scan passes over hidden files"
	case processing.IsDiscImage(name):
		return "it is a " + processing.DiscImageReason
	case !videoExts[strings.ToLower(filepath.Ext(name))]:
		return "it is no video file a scan takes (" + videoExtList() + ")"
	}
	if x, ok := newWalkState(true).extraFileOf(path, name); ok {
		return "it is an extra by the extras convention (" + x.kind + ")"
	}
	return ""
}

// SidecarOf reports whether the subtitle file at sub is one a scan pairs with
// the video file at video (scanSidecars): in the video's folder, a subtitle
// file named as the video, a language between maybe (Film.srt and
// Film.en.srt beside Film.mkv), letter case aside.
func SidecarOf(video, sub string) bool {
	video, sub = filepath.Clean(video), filepath.Clean(sub)
	if filepath.Dir(sub) != filepath.Dir(video) {
		return false
	}
	_, ok := sidecarName(stripExt(filepath.Base(video)), filepath.Base(sub))
	return ok
}

// videoExtList is the extensions of videoExts, sorted, for a refusal to name.
func videoExtList() string {
	out := make([]string, 0, len(videoExts))
	for e := range videoExts {
		out = append(out, e)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}
