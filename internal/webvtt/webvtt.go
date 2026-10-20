// Package webvtt makes the WebVTT file of a subtitle attached to a title from
// the text a caller sends: WebVTT as it is, or SubRip converted. Either way
// the file is UTF-8 with LF line ends, starts with the WEBVTT header and
// holds at least one cue whose times hold.
package webvtt

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The formats a subtitle is sent in.
const (
	FormatWebVTT = "webvtt"
	FormatSRT    = "srt"
)

// Invalid says why a text is no subtitle the catalog takes.
type Invalid struct{ Reason string }

func (e *Invalid) Error() string { return e.Reason }

func invalid(format string, args ...any) error { return &Invalid{Reason: fmt.Sprintf(format, args...)} }

// FormatOf is the format name means: webvtt for "webvtt" and "vtt", srt for
// "srt" and "subrip", "" for an empty name (the text says), false for any
// other.
func FormatOf(name string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "":
		return "", true
	case "webvtt", "vtt":
		return FormatWebVTT, true
	case "srt", "subrip":
		return FormatSRT, true
	}
	return "", false
}

var (
	// a cue's timing line: start --> end, each [hh:]mm:ss.mmm (SubRip's
	// comma taken for the dot), and WebVTT's cue settings after it
	timingRE = regexp.MustCompile(`^\s*((?:\d+:)?\d{1,2}:\d{2}[.,]\d{1,3})\s*-->\s*((?:\d+:)?\d{1,2}:\d{2}[.,]\d{1,3})(.*)$`)
	// what SubRip files carry of other formats' styling: {\an8}, {\i1}
	braceTagRE = regexp.MustCompile(`\{\\[^}]*\}`)
	fontTagRE  = regexp.MustCompile(`(?i)</?font[^>]*>`)
)

// Normalize is the WebVTT file of content, sent as format ("" lets the text
// say: WebVTT when it starts with the header, SubRip otherwise), and how many
// cues it holds. A byte order mark goes, CR LF and CR become LF. SubRip is
// converted: each block's number goes, its times are WebVTT's, the styling
// of other formats it carries ({\an8}, <font>) goes, and "-->" in a line of
// text becomes "->". Refused (Invalid): text that is not UTF-8 — or that
// holds U+FFFD, the character a reader puts for bytes it could not read, as
// JSON carries text that was not UTF-8 —, WebVTT without its header, and a
// text with no cue, or a cue that ends before it starts.
func Normalize(content []byte, format string) ([]byte, int, error) {
	content = bytes.TrimPrefix(content, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(content) || bytes.ContainsRune(content, utf8.RuneError) {
		return nil, 0, invalid("the subtitle is not UTF-8 text, or holds characters that could not be read (U+FFFD): convert it to UTF-8 first")
	}
	text := strings.ReplaceAll(strings.ReplaceAll(string(content), "\r\n", "\n"), "\r", "\n")
	if format == "" {
		format = FormatSRT
		if isWebVTT(text) {
			format = FormatWebVTT
		}
	}
	switch format {
	case FormatWebVTT:
		return fromWebVTT(text)
	case FormatSRT:
		return fromSRT(text)
	}
	return nil, 0, invalid("no subtitle format %q: webvtt or srt", format)
}

// isWebVTT reports whether text starts with the WebVTT header: WEBVTT, then
// the end of its line, a space or a tab.
func isWebVTT(text string) bool {
	if !strings.HasPrefix(text, "WEBVTT") {
		return false
	}
	rest := text[len("WEBVTT"):]
	return rest == "" || rest[0] == '\n' || rest[0] == ' ' || rest[0] == '\t'
}

// fromWebVTT checks a WebVTT text and answers it with one LF at its end: its
// header and blocks as they are, but each cue's timing line written as
// WebVTT spells times (hh:mm:ss.mmm), its cue settings kept, since a player
// drops a cue whose times are written otherwise.
func fromWebVTT(text string) ([]byte, int, error) {
	if !isWebVTT(text) {
		return nil, 0, invalid("the subtitle is not WebVTT: its first line is not the WEBVTT header")
	}
	cues := 0
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i, line := range lines {
		if !strings.Contains(line, "-->") {
			continue
		}
		m := timingRE.FindStringSubmatch(line)
		if m == nil {
			return nil, 0, invalid("line %d is no cue timing a player reads: %q", i+1, cut(line))
		}
		start, end, err := span(m[1], m[2])
		if err != nil {
			return nil, 0, invalid("line %d: %v", i+1, err)
		}
		if end < start {
			return nil, 0, invalid("line %d: the cue ends before it starts", i+1)
		}
		settings := strings.TrimSpace(m[3])
		lines[i] = stamp(start) + " --> " + stamp(end)
		if settings != "" {
			lines[i] += " " + settings
		}
		cues++
	}
	if cues == 0 {
		return nil, 0, invalid("the subtitle holds no cue")
	}
	return []byte(strings.Join(lines, "\n") + "\n"), cues, nil
}

// fromSRT converts a SubRip text to WebVTT, block by block: a block is its
// number (optional), its timing line and its lines of text up to a blank
// line. Lines before a block's timing that are not its number, and a block
// without text, are left out.
func fromSRT(text string) ([]byte, int, error) {
	var b strings.Builder
	b.WriteString("WEBVTT\n")
	cues := 0
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		m := timingRE.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		start, end, err := span(m[1], m[2])
		if err != nil {
			return nil, 0, invalid("line %d: %v", i+1, err)
		}
		if end < start {
			return nil, 0, invalid("line %d: the cue ends before it starts", i+1)
		}
		var cue []string
		for i+1 < len(lines) && strings.TrimSpace(lines[i+1]) != "" {
			i++
			if timingRE.MatchString(lines[i]) { // a block without its blank line
				i--
				break
			}
			line := strings.TrimRight(fontTagRE.ReplaceAllString(braceTagRE.ReplaceAllString(lines[i], ""), ""), " \t")
			if strings.TrimSpace(line) == "" {
				continue
			}
			cue = append(cue, strings.ReplaceAll(line, "-->", "->"))
		}
		// the next block's number is no line of this one
		if n := len(cue); n > 0 && i+1 < len(lines) && isNumber(cue[n-1]) && timingRE.MatchString(lines[i+1]) {
			cue = cue[:n-1]
		}
		if len(cue) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n%s --> %s\n%s\n", stamp(start), stamp(end), strings.Join(cue, "\n"))
		cues++
	}
	if cues == 0 {
		return nil, 0, invalid("the subtitle holds no cue: neither WebVTT nor SubRip")
	}
	return []byte(b.String()), cues, nil
}

func isNumber(s string) bool {
	_, err := strconv.Atoi(strings.TrimSpace(s))
	return err == nil
}

// span reads a cue's start and end in milliseconds.
func span(from, to string) (int64, int64, error) {
	start, err := millis(from)
	if err != nil {
		return 0, 0, err
	}
	end, err := millis(to)
	return start, end, err
}

// millis reads a time [hh:]mm:ss(.|,)fff in milliseconds; a fraction of fewer
// than three digits counts as tenths or hundredths.
func millis(s string) (int64, error) {
	s = strings.ReplaceAll(s, ",", ".")
	dot := strings.LastIndex(s, ".")
	if dot < 0 {
		return 0, errors.New("a time without its fraction: " + s)
	}
	frac := s[dot+1:]
	for len(frac) < 3 {
		frac += "0"
	}
	ms, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, errors.New("no time: " + s)
	}
	fields := strings.Split(s[:dot], ":")
	var secs int64
	for i, f := range fields {
		n, err := strconv.ParseInt(f, 10, 64)
		if err != nil || (i > 0 && n > 59) {
			return 0, errors.New("no time: " + s)
		}
		secs = secs*60 + n
	}
	return secs*1000 + ms, nil
}

// stamp writes milliseconds as WebVTT's hh:mm:ss.mmm.
func stamp(ms int64) string {
	h, ms := ms/3600000, ms%3600000
	m, ms := ms/60000, ms%60000
	s, ms := ms/1000, ms%1000
	return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, ms)
}

// cut is a line as an error quotes it: at most 60 characters.
func cut(line string) string {
	if r := []rune(line); len(r) > 60 {
		return string(r[:59]) + "…"
	}
	return line
}
