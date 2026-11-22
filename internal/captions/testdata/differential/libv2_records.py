"""The v2 library records as their writers write them — one implementation, in plain Python 3.11
with nothing but the standard library, of what a source record says about an original, what a
version record says about the cut it is, what a package record says the package carries and
lost, what an extra record says about bonus material, and the checksums and the .complete marker
that close a version's chain.

Two writers use it, and they must agree byte for byte, because the deletion gate — the essence of
a version's sources minus the essence of its package — is computed from what they write:

  * the packager, which records an original the first time it packages it (sources/<sourceId>/),
    and every package it makes (versions/<versionId>/, extras/<extraId>/). It vendors this file
    byte-identically, and a hash test in each repository guards the copy;
  * library-v2-from-catalog.py, which records what a catalog already holds, and imports it.

Nothing here reads a database, the network or a clock: every moment is passed in, every id is
derived (did) or given, and the same inputs always make the same bytes. Nothing here writes a file
either — the builders return records and bytes, and the writer puts them where its layout says,
through a temporary file and a rename. What a builder could not know stays empty rather than
guessed, and what it normalises it says, in the notes it returns.

No record names a file as it arrived, and no file in the library is named so: a name, a folder or a
container title can tell where a file came from. An original a version keeps, a source describes or
an extra was packaged from is named by library_original_name(), original.<ext>; the copy of a
subtitle file that came with it by subtitle_copy_name(), subtitle-<n>.<lang>[.forced][.sdh].<ext>;
and the probe a source keeps is the tool's output with the two edits scrub_probe() makes. The name a
file arrived under is read for what it claims — its labels and its numbering — and the record keeps
those values, never the name.

The order a version's folder is written in, which only the writer can keep:

  version.json        version_record(), then json_bytes()
  checksums.sha256    checksums(): version.json and every package file
  package.json        package_record(), holding the checksums file's hash
  .complete           complete(): sha256:<hex> of package.json, written last

A package adopted from a store whose files nobody could read through in the time there was — a
migration of many terabytes — is recorded with its checksums pending: package_record(pending=True)
counts the files and their bytes and names no hash, no checksums.sha256 is written, and .complete
names that package.json. fill_checksums() closes the chain later: it hashes every file, and returns
the checksums file with the package.json and the .complete that name it, to be written in that order.

A subtitle attached to a version after its package completed (ATTACHED_SUBTITLE_RE) is written by
none of these writers and named by none of these records: it is no file of the package, so
chain_problems() and fill_checksums() leave it out of the chain, and a full check holds its bytes to
its name.

The original a version is established with is no link of that chain — its source record's fixity
covers it, and it is deleted later — and goes in under library_original_name() once the rest is
built: renamed last into the staged folder, which then goes into place whole. A version taken in
before anything packaged it is version.json and its original alone; its package is added later, in
the same order.

A source folder: source.json from source_record() (with the probe it returns, written beside it as
ffprobe.json), a copy of each subtitle file that came with the original under the name
sidecar_entry() gives it, then the checksums.sha256 that lists exactly those files. Nothing else that
sat beside the original is copied: NFO text routinely names where a file came from. An extra's
folder: extra.json from extra_record(), the package, then the chain as a version's, its checksums
over extra.json and every package file. chain_problems() checks a folder written that way.

Piped into a pod behind a tool that uses it (cat libv2_records.py tool.py | python3 - …), this
file runs as part of that tool, so it defines no main and runs nothing when it is read.
"""
import copy, datetime, hashlib, json, os, re, shutil, subprocess, threading, uuid

# The version of what this module writes. A change to the shape of a record it builds is a new
# value, and the copy a writer vendors names the one it was taken at. 2: no record names a file as
# it arrived, and a source copies only the subtitle files that came with its original. 3: a source
# covers the episodes its file holds as the database linked them, and its naming reads a file of
# several episodes as the catalog's scanner does, from the first to the last. 4: a package's checksums
# can be pending (package_listing, checksums and package_record with pending, fill_checksums), and
# chain_problems() reads such a chain. 5: package_files() and fill_checksums() read several files at a
# time (jobs). 6: a subtitle attached to a version after its package completed is no file of the
# package (attached_subtitle): chain_problems() and fill_checksums() leave it out of the chain, and a
# full check holds its bytes to its name. Every record 5 and 6 write is the one before. 7: a subtitle
# whose manifest entry says the stream it was made from is for the hard of hearing (hearingImpaired:
# its disposition said hearing_impaired or captions) is sdh by that disposition, as the stream is in
# its source's essence; a manifest that says nothing of it gives the record it gave. 8: a video
# rendition whose manifest entry says its init segment carries the original's HDR10 static metadata
# (hdr10Metadata: its mdcv and clli boxes) says so in its record, and the package's essence has the
# HDR10 metadata a source had when every rendition in PQ, the top one among them, says so, as when its
# top rendition is the original's own stream in PQ; an audio rendition whose entry says the stream it
# was made from is a commentary
# (commentary: its disposition said comment) or for the visually impaired (visualImpaired:
# visual_impaired or descriptions), and a subtitle whose entry says its stream is a commentary
# (commentary), are for that by their disposition, as the stream is in its source's essence; a
# manifest that says none of it gives the record it gave. 9: a title that says its track is no
# commentary — "No commentary", "without the director's commentary", "Kommentarlos", "ohne
# Kommentar", "sans commentaire" — makes it none (says_commentary): its words were read as one, in
# a source's streams and a package's renditions alike. A source recorded before keeps the essence it
# was written with. 10: a subtitle whose manifest entry names the styled original the package keeps
# beside it — the ASS/SSA script its text and styles came in (styled: {path, format}) — names it in its
# record too (styled_original), and the font files a manifest lists (fonts: [{path, format, names,
# sourceStreamIndex}]) are listed in the package's record (font_file); the package's essence has styled
# subtitles and fonts when it carries them, for every stream of its source that has them where the
# source's streams are known (package_essence). A manifest without them gives the record it gave. An
# attachment is a font by a MIME type that names one in any of the words a font's carries (font,
# opentype, truetype, sfnt), by a font's file name (.ttf .otf .ttc .woff .woff2), or, where neither says
# so and no picture's MIME type says otherwise, by its first bytes — a font's signature (attachment_role;
# attachment_heads reads them, and the stream then says it: signature) — in a source and as a package
# carries it alike, so no font goes uncounted with its original. 11: a video rendition whose manifest
# entry says its own stream carries closed captions (closedCaptions: CEA-608/708 in its SEI, as the
# packager read them from its frames) says so in its record, and the package's essence has closed
# captions when its top rendition says so (package_essence), where it had none whatever it carried; a
# rendition keeps the caption windows it was read in (captionWindows: {at, start, end, frames,
# captions}) and how far its times run ahead of its original's (timestampOffset), what its video
# shows, kept for a later day. A manifest that says nothing of them gives the record it gave. A
# source's picture whose caption windows were read (captions: caption_windows) keeps them
# (captionWindows), and its essence knows its captions where they tell them (closedCaptionsKnown); one
# they weren't read of says what the probe says, which can't tell none from not seen. A manifest's scan
# — what the transcoder made of the picture as far as fields go — is the package's (scan: done, found
# and how) and its essence's done of it (scan). The deletion gate does not use the package's captions:
# an original that has closed captions, or whose captions are not known, is kept (closedCaptions);
# and an original in fields, read widely (in_fields), is kept whatever was done to make its package
# progressive (interlaced): the gate does not read a scan's done. A stream that is closed captions of its
# own (eia_608, c608, c708) and a picture whose packets carry a caption signature (captionSignature) carry
# them, known; a window that counted interlaced frames (interlacedFrames) puts its picture in fields; a
# file off the closed lists of how captions and fields are carried (offList) knows neither, and is kept;
# and a deletion event's read (captionsRead) tells what its record did not (essence_as_read).
LIBV2_RECORDS = 11

NS = uuid.uuid5(uuid.NAMESPACE_URL, "https://zaentrum.github.io/schemas/library")
PACKAGE_DIRS = ("hls", "subs", "trickplay", "trailers")
# An extra's package is a version's without trailers: a trailer of its own is an extra.
EXTRA_DIRS = ("hls", "subs", "trickplay")
SUMS = "checksums.sha256"
OS_ARTEFACTS = re.compile(r"^(\.DS_Store|\._.*|Thumbs\.db|desktop\.ini|@eaDir|\.@__thumb|#recycle|\.AppleDouble)$")
UUID_RE = re.compile(r"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$")
TIMESTAMP_RE = re.compile(r"^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})$")
SEGMENT_KINDS = {"intro", "recap", "credits", "preview", "commercial", "other"}
# The command a source record's ffprobe.json is the output of, but for the edits scrub_probe() makes.
FFPROBE_ARGS = ("-v", "quiet", "-print_format", "json", "-show_format", "-show_streams", "-show_chapters")
# The names the library gives an original (library_original_name) and the copy of a subtitle file that
# came with it (subtitle_copy_name): nothing of the name either arrived under, and only [a-z0-9.-].
ORIGINAL_NAME_RE = re.compile(r"^original(-[0-9]+)?\.[a-z0-9]{1,8}$")
SUBTITLE_COPY_RE = re.compile(r"^subtitle-[0-9]+\.[a-z]{2,8}(\.forced)?(\.sdh)?\.[a-z0-9]{1,8}$")
# A subtitle attached to a title after its package completed — made from its audio or from another of
# its subtitles, or found elsewhere — lies in the subs/ of the version it was attached to, beside the
# package's own, named by its bytes: subs/attached-<sha256>.<language>[.forced][.sdh].vtt, <sha256> the
# hex sha256 of the file and <language> its BCP 47 tag in lower case. katalog-manager writes it, and a
# new version of the same file gets a copy. It is no file of the package: package.json and the
# checksums were written when the package completed, and nothing rewrites them for it, so its name is
# its check. Only a version folder holds one, directly in its subs/; the name anywhere else is any
# other file's.
ATTACHED_SUBTITLE_RE = re.compile(r"^subs/attached-([0-9a-f]{64})\.[a-z0-9]+(-[a-z0-9]+)*(\.forced)?(\.sdh)?\.vtt$")
# The values HLS gives VIDEO-RANGE, as a video rendition's videoRange keeps them.
VIDEO_RANGES = ("SDR", "PQ", "HLG")
# The format of a subtitle's styled original: an ASS or SSA script, as libass reads either (its
# [Script Info] says which), its text and styles as the stream or the file had them.
STYLED_FORMATS = ("ass",)
# What a package's font file is, by its bytes: TrueType, OpenType with CFF outlines, a collection of
# either, WOFF and WOFF2. A file of a font attachment whose bytes are none of them has no format.
FONT_FORMATS = ("ttf", "otf", "ttc", "woff", "woff2")
# A font's signature, its first four bytes, and the format it says (font_signature).
FONT_SIGNATURES = {b"\x00\x01\x00\x00": "ttf", b"true": "ttf", b"OTTO": "otf", b"ttcf": "ttc", b"wOFF": "woff",
                   b"wOF2": "woff2"}
# What says an attachment is a font (attachment_role): a MIME type in any of the words a font's carries —
# font/ttf, font/otf, font/collection, application/x-truetype-font, application/vnd.ms-opentype,
# application/font-sfnt, application/x-font-ttf — or a font's file name.
FONT_MIME_RE = re.compile(r"font|opentype|truetype|sfnt", re.I)
FONT_FILE_RE = re.compile(r"\.(ttf|otf|ttc|woff2?)$", re.I)
# How many of an attachment's first bytes attachment_heads reads, and how long it waits for them, in seconds:
# ffmpeg opens the file and reads its header for them, which a stalled storage can hold up for good.
HEAD_BYTES = 16
HEAD_TIMEOUT = 120


class AttachmentTimeout(Exception):
    """attachment_heads got no first bytes of an attachment within HEAD_TIMEOUT. What the attachment is
    can't be told then — a font by its bytes alone is none to a record without them — so a writer records
    no source of the file this time rather than one that may not count a font it holds."""


# Closed captions (EIA/CEA-608/708) are in a video stream itself — an A53 cc_data with each picture, in
# an H.264 or HEVC SEI or an MPEG-2 picture's user data — and nothing in a container says so: ffprobe
# gives a stream's closed_captions (7.1) only once it has decoded a frame of it, and (9.0) not at all.
# What tells is the decoder's own reading, the side data it exports with each frame that carries them:
# caption_windows decodes CAPTION_PACKETS packets of an original's picture in each window of the plan
# (library/v2/caption-windows.json, the shared gate block's CAPTION_PLAN), over the plan's span the gate knows
# without reading (plan_of: the probe's picture start_time + duration): the start, then 10, 30, 50, 70 and 90 %
# of it, each but the start at most CAPTION_TAIL_S short of its end, each from the keyframe the seek lands on
# and read within CAPTION_TIMEOUT seconds under the plan's bounds; a picture whose places lie closer than a
# window reaches (plan_short, by the gop of its first CAPTION_GOP_PACKETS packets) is read whole. An encoder
# may write caption data only where there is text, so the first frames of a programme whose captions start
# later carry none. A window tells only where it decoded at least CAPTION_MIN_FRAMES frames (half its
# packets, and 24 at least) and landed at its place by the plan's slack. A record keeps each window as
# caption_window makes it, {at, start, end, frames, captions}: the fraction it is at, the presentation seconds
# its decoded frames cover, how many were decoded — none: the window could not be read, its captions false
# says nothing — and whether one carried captions, and of the frames it decoded how many were interlaced
# (interlacedFrames) and how many repeat a field (repeatFrames), and keyframeBefore where its packets showed
# its first frame's keyframe the last before its place; a whole read keeps whole and its packets;
# and beside them the span the read measured (captionSpan: its start's first frame to the end of the last
# video packet) and its gop (captionGop). A package's renditions are read at the same times, for what their
# video shows. A window whose ffprobe exits with an error, or whose first frame is not at its place by the
# plan's slack (window_lands), failed: it landed elsewhere.
# How many packets picture_span reads from the last keyframe for the picture's end: past a group of
# pictures of any stream; a read that reaches this many did not land near the end.
SPAN_PACKETS = 2000
# How far after a window's first frame its keyframe may lie (an open group's leading pictures come before it),
# and how near its place the packets listed from it must reach (keyframe_before).
KEY_LEAD = 1.0
CAPTION_SIDE_DATA = "ATSC A53 Part 4 Closed Captions"
# What says a picture's packets carry closed captions (caption_signature): of an MPEG-2 picture, user
# data after its start code with A53 (GA94, type 3), DVD (CC 0x01 0xF8) or SCTE-20 (type 3, 0x01) captions,
# the three ffmpeg's MPEG-2 decoder exports; of any other, A53 in an SEI (ITU-T T.35: country 0xB5,
# provider 0x0031, GA94, type 3), as H.264 and HEVC carry it. caption_signature reads every packet of the
# picture SIGNATURE_CHUNK bytes at a time, and gives up on an ffmpeg that hands it nothing for
# SIGNATURE_STALL seconds.
T35_SIGNATURES = (b"\xb5\x00\x31GA94\x03",)
USER_DATA_SIGNATURES = (b"\x00\x00\x01\xb2GA94\x03", b"\x00\x00\x01\xb2CC\x01\xf8", b"\x00\x00\x01\xb2\x03\x01",
                        b"\x00\x00\x01\xb2\x03\x81")
SIGNATURE_CHUNK = 8 << 20
SIGNATURE_STALL = 120


class CaptionsTimeout(Exception):
    """caption_windows got no answer of a window of a picture's frames within CAPTION_TIMEOUT. Whether it carries
    closed captions can't be told then, so a writer records no source of the file this time rather than
    one that says it carries none."""


# What a package's video is as far as fields go, as the transcoder's contract says it (scan) and the
# manifest keeps it: what was done (done) — a film at its own frames (inverse-telecine), whole frames as
# found (progressive), video deinterlaced at the field rate (deinterlaced-field, one frame a field) or
# at the frame rate (deinterlaced-frame, every second moment dropped), HEVC field pictures kept as they
# are (fields-kept), film and video field-matched at the source's rate (field-matched), fields too few
# to make a source over left as they are (fields-left), nothing to do (none), or an examination that
# told nothing (unexamined) — and what the picture was found to be (found): the words the transcoder
# says them in. A word not known here is kept as it is. The deletion
# gate does not read done: no word of it lets an original in fields go. It reads found: any word but
# progressive, one not known here among them, says the original is in fields (in_fields).
SCAN_DONE = ("inverse-telecine", "progressive", "deinterlaced-field", "deinterlaced-frame", "fields-kept",
             "field-matched", "fields-left", "none", "unexamined")
SCAN_FOUND = ("fields", "film", "telecine", "mixed", "progressive", "unknown", "field-pictures", "unexamined")
# The words of a scan a record keeps beside done and found; the counts of the examination of a sample
# (idet: as the frames are, after fieldmatch, the 3:2 blocks, and its windows by what each saw); and what
# the read-through of the whole title found (verified), as the transcoder writes them. Each is kept
# where it is one, and left out where not.
SCAN_WORDS = ("judgedBy", "fieldOrder", "order", "orderBy", "frameRate", "filter")
SCAN_COUNTS = ("frames", "tff", "bff", "progressive", "undetermined", "repeatedTop", "repeatedBottom",
               "matchedCombed", "matchedDetermined", "matchedRun", "cadenceBlocks", "otherBlocks", "blindBlocks")
SCAN_WINDOWS = ("film", "video", "whole", "unsure", "detail")
VERIFIED_WORDS = ("why", "sampleDone", "sampleFound")
VERIFIED_COUNTS = ("frames", "chunksInFields", "otherOrder", "offCadence", "repeats", "matchedUnsure", "combedOut",
                   "combedOutRun", "droppedDistinct", "uncertainCycles", "seen", "blindRun", "leading", "packets")
VERIFIED_FLAGS = ("ok", "complete")


# ---------------------------------------------------------------- small helpers
def did(*parts):
    """A generated id: the same inputs always give the same id, so a re-run writes the same tree."""
    return str(uuid.uuid5(NS, ":".join(str(p) for p in parts)))


def sha_bytes(b):
    return "sha256:" + hashlib.sha256(b).hexdigest()


def json_bytes(doc):
    """A record as it is written: two-space indent, UTF-8, one trailing line break."""
    return (json.dumps(doc, indent=2, ensure_ascii=False) + "\n").encode("utf-8")


def sha_file(p):
    h = hashlib.sha256()
    with open(p, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return "sha256:" + h.hexdigest()


def qh1(p):
    """sha256(first 64 KiB || last 64 KiB || uint64be(size)): cheap proof a large file is the one
    recorded, two reads however big the file is."""
    size = os.path.getsize(p)
    h = hashlib.sha256()
    with open(p, "rb") as f:
        h.update(f.read(65536))
        if size > 65536:
            f.seek(max(size - 65536, 0))
            h.update(f.read(65536))
    h.update(size.to_bytes(8, "big"))
    return "sha256:" + h.hexdigest()


def ts(v):
    """RFC 3339 with an upper-case T and Z, or None."""
    if not v:
        return None
    s = str(v).strip().replace(" ", "T")
    if not re.search(r"(Z|[+-]\d\d:?\d\d)$", s):
        s += "Z"
    s = re.sub(r"([+-]\d\d)(\d\d)$", r"\1:\2", s)
    return s.replace("+00:00", "Z")


def is_moment(s):
    """Whether s is a timestamp as a record holds one: RFC 3339 with an upper-case T and Z, and a
    real moment."""
    if not s or not TIMESTAMP_RE.match(s):
        return False
    try:
        datetime.datetime.fromisoformat(s.replace("Z", "+00:00"))
    except ValueError:
        return False
    return True


def ts_of_mtime(p):
    t = datetime.datetime.fromtimestamp(os.path.getmtime(p), datetime.timezone.utc)
    return t.replace(microsecond=0).isoformat().replace("+00:00", "Z")


def num(v):
    try:
        return int(v)
    except (TypeError, ValueError):
        try:
            return int(float(v))
        except (TypeError, ValueError):
            return None


def text(v):
    """A one-line string, or None: a record's fields never carry a line break."""
    if v is None:
        return None
    s = str(v).replace("\r", " ").replace("\n", " ").strip()
    return s or None


def ratio(v):
    return v if v and v not in ("0:1", "N/A") else None


def listdir(d):
    return sorted(n for n in os.listdir(d) if not OS_ARTEFACTS.match(n))


def walk_files(root):
    """Every file under root, relative to it, in a stable order and without OS artefacts."""
    out = []
    for base, dirs, files in os.walk(root):
        dirs[:] = sorted(x for x in dirs if not OS_ARTEFACTS.match(x))
        out += [os.path.relpath(os.path.join(base, f), root) for f in sorted(files) if not OS_ARTEFACTS.match(f)]
    return sorted(out)


ISO639 = {
    "eng": "en", "ger": "de", "deu": "de", "fre": "fr", "fra": "fr", "ita": "it", "spa": "es", "jpn": "ja",
    "chi": "zh", "zho": "zh", "dut": "nl", "nld": "nl", "por": "pt", "rus": "ru", "pol": "pl", "cze": "cs",
    "ces": "cs", "ukr": "uk", "hin": "hi", "kor": "ko", "swe": "sv", "nor": "no", "nob": "nb", "dan": "da",
    "fin": "fi", "tur": "tr", "ara": "ar", "heb": "he", "hun": "hu", "gre": "el", "ell": "el", "rum": "ro",
    "ron": "ro", "tha": "th", "vie": "vi", "ind": "id", "may": "ms", "msa": "ms", "cat": "ca", "hrv": "hr",
    "slv": "sl", "slo": "sk", "slk": "sk", "srp": "sr", "bul": "bg", "est": "et", "lav": "lv", "lit": "lt",
    "ice": "is", "isl": "is", "fil": "fil", "tel": "te", "tam": "ta", "und": "und", "zxx": "zxx", "mul": "mul",
    "baq": "eu", "eus": "eu", "glg": "gl", "kan": "kn", "mal": "ml", "ben": "bn", "mar": "mr", "urd": "ur",
    "per": "fa", "fas": "fa", "wel": "cy", "cym": "cy", "arm": "hy", "hye": "hy", "geo": "ka", "kat": "ka",
    "mac": "mk", "mkd": "mk", "alb": "sq", "sqi": "sq", "tgl": "tl", "lao": "lo", "khm": "km", "bur": "my",
    "mya": "my", "sin": "si", "nep": "ne", "pan": "pa", "guj": "gu", "tib": "bo", "bod": "bo", "mon": "mn",
    "kaz": "kk", "uzb": "uz", "aze": "az", "bel": "be", "bos": "bs", "gle": "ga", "bre": "br", "ltz": "lb",
    "afr": "af", "swa": "sw", "zul": "zu", "xho": "xh", "amh": "am", "som": "so", "hau": "ha", "yor": "yo",
    "ibo": "ig", "epo": "eo", "lat": "la", "yid": "yi", "jav": "jv", "sun": "su", "mao": "mi", "mri": "mi",
    "grn": "gn", "que": "qu", "nno": "nn", "fao": "fo", "kal": "kl", "iku": "iu", "tat": "tt", "kur": "ku",
    "pus": "ps", "tuk": "tk", "kir": "ky", "tgk": "tg", "mlt": "mt", "roh": "rm", "sco": "sco", "haw": "haw",
}


def lang(raw):
    """(BCP 47 code, the code the file carried) — 'eng' becomes 'en' and keeps 'eng' beside it."""
    if not raw:
        return None, None
    r = str(raw).strip().lower()
    if re.fullmatch(r"[a-z]{2}(-[a-z0-9]{2,8})?", r):
        return r, None
    if r in ISO639:
        return ISO639[r], r
    if re.fullmatch(r"[a-z]{3}", r):
        return r, None
    return "und", r


def primary_language(raw):
    code = (lang(raw)[0] or "und").split("-")[0]
    return {"nb": "no", "nn": "no"}.get(code, code)


LANGUAGE_RE = re.compile(r"^([a-z]{2,3}(-[A-Za-z0-9]{2,8})*|und|zxx|mul)$")


def rendition_language(raw):
    """A package rendition keeps the code the media file carried ('eng'), not a normalised one: it
    describes the playlist as it was written. Only a code the format cannot hold is normalised."""
    r = str(raw or "").strip()
    return r if LANGUAGE_RE.match(r) else (lang(r)[0] or "und")


# ---------------------------------------------------------------- what a file name claims
QUALITY = re.compile(r"\b(Remux|Blu-?ray|DVD|WEB[A-Za-z-]*|HDTV|SDTV)[- ](\d{3,4}p)\b|\b(\d{3,4}p)\b", re.I)
EDITION_WORDS = [
    ("directors-cut", r"director'?s?[ ._-]?cut"), ("extended", r"\bextended\b"), ("unrated", r"\bunrated\b"),
    ("theatrical", r"\btheatrical\b"), ("special-edition", r"special[ ._-]?edition"),
    ("remastered", r"\bremaster(ed)?\b"), ("restored", r"\brestor(ed|ation)\b"), ("imax", r"\bimax\b"),
    ("final-cut", r"final[ ._-]?cut"),
]
# How a name numbers the episodes its file holds, read as the catalog's scanner reads a name, so that a
# source's naming says the range the catalog links the file's episodes by: S05E15, letter case aside, standing
# apart from the words around it — no letter, digit or underscore just before or after — and not followed by
# a digit; for a file of several, the episodes after it, each where the one before ends — S05E15E16,
# S05E15-E16 (or .E16, _E16, " E16"), S05E15-16, S05E15-E17, S05E15E16E17 — each after the one before, and
# at most MAX_COVERED episodes from the first to the last. A number after a dash that a p follows is none
# (S05E15-720p is the fifteenth episode in 720p), and neither is one a digit follows (S05E15-1080), one no
# higher than the one before, or one that would cover more than MAX_COVERED (S05E15-264); the token ends
# where it still stands apart (S05E15-E16x numbers the fifteenth alone). '1x05', which the scanner does not
# read, numbers one episode and never a range.
EPISODE_HEAD = re.compile(r"\bS(\d{1,2})E(\d{1,3})", re.I | re.A)
EPISODE_MORE = re.compile(r"(?:[ ._-]?E|-)(\d{1,3})", re.I | re.A)
EPISODE_NXN = re.compile(r"(?<![0-9a-z])(\d{1,2})x(\d{2,3})(?![0-9])", re.I)
# The most episodes one file covers, the first and the last among them.
MAX_COVERED = 10
DIGITS = "0123456789"


def medium_of(token):
    t = token.lower()
    return "web" if t.startswith("web") else "disc" if t in ("remux", "bluray", "blu-ray", "dvd") else \
        "broadcast" if t in ("hdtv", "sdtv") else "unknown"


def edition_word(t):
    if not t:
        return None
    for kind, pat in EDITION_WORDS:
        if re.search(pat, t, re.I):
            return kind
    return None


def labels(name, folder):
    hits = list(QUALITY.finditer(os.path.splitext(name)[0]))
    m = hits[-1] if hits else None
    fold_ed = None
    fm = re.search(r" - ([^()]+?) \(\d{4}\)$", folder or "")
    if fm and edition_word(fm.group(1)):
        fold_ed = fm.group(1).strip()
    return {
        "quality": m.group(0) if m else None,
        "medium": medium_of(m.group(1)) if m and m.group(1) else None,
        "resolution": ((m.group(2) or m.group(3)) if m else None),
        "edition": edition_word(name),
        "folderEdition": fold_ed,
    }


def episode_range(stem):
    """The first token of stem that numbers episodes as the catalog's scanner reads one (EPISODE_HEAD
    above): (where it starts, where it ends, its season, the first episode, the last) — the last the first
    for a file of one episode — or None."""
    word = lambda c: c in DIGITS or c == "_" or "a" <= c <= "z" or "A" <= c <= "Z"
    for m in EPISODE_HEAD.finditer(stem):
        end = m.end()
        if end < len(stem) and stem[end] in DIGITS:
            continue  # E1500: no episode of a season numbers so
        first = last = int(m.group(2))
        after, at = [], end
        while True:  # the episodes after the first, each where the one before ends
            x = EPISODE_MORE.match(stem, at)
            if not x:
                break
            n, nxt = int(x.group(1)), x.end()
            dashed = stem[at] == "-" and stem[at + 1] in DIGITS
            if nxt < len(stem) and (stem[nxt] in DIGITS or (dashed and stem[nxt] in "pP")):
                break  # -720p, -1080: no episode
            if n <= last or n - first + 1 > MAX_COVERED:
                break
            after.append((nxt, n))
            at, last = nxt, n
        for tok_end, tok_last in reversed([(end, first)] + after):  # the longest of it that stands apart
            if tok_end == len(stem) or not word(stem[tok_end]):
                return m.start(), tok_end, int(m.group(1)), first, tok_last
    return None


def naming(name):
    """How the file's own name numbered what it holds, read as the catalog's scanner reads it: a file of
    several episodes from its first to its last, episodeEnd the last. The name never says which ordering it
    used, so the scheme stays 'unknown' and only the numbers and the token itself are recorded."""
    stem = os.path.splitext(name)[0]
    tok, nxn = episode_range(stem), EPISODE_NXN.search(stem)
    if tok and not (nxn and nxn.start() < tok[0]):
        start, end, season, first, last = tok
        return {"scheme": "unknown", "seasonNumber": season, "episodeNumber": first,
                "episodeEnd": last if last != first else None, "raw": stem[start:end]}
    if nxn:
        return {"scheme": "unknown", "seasonNumber": int(nxn.group(1)), "episodeNumber": int(nxn.group(2)),
                "episodeEnd": None, "raw": nxn.group(0)}
    return None


# ---------------------------------------------------------------- the names the library gives
def _extension(name):
    """A file name's extension as a name in the library keeps it: lower-cased, when it is one to eight
    letters and digits, and bin when it is anything else or nothing."""
    ext = os.path.splitext(str(name or "").replace("\\", "/").rsplit("/", 1)[-1])[1][1:].lower()
    return ext if re.fullmatch(r"[a-z0-9]{1,8}", ext) else "bin"


def library_original_name(arrival_name: str, part: int | None = None) -> str:
    """The name the library gives an original: original.<ext>, or original-<part>.<ext> for one part
    of a version split into several, in part order from 1 — <ext> the extension of the name it arrived
    under, lower-cased when that is one to eight letters and digits, and bin when it is not. Nothing
    else of that name is kept. The same name is the original's in its version folder, its source
    record's file.name, the version's originalFiles and an extra's packagedFrom."""
    if part is not None and (not isinstance(part, int) or isinstance(part, bool) or part < 1):
        raise ValueError(f"a part of a version is numbered from 1, not {part!r}")
    return f"original.{_extension(arrival_name)}" if part is None else f"original-{part}.{_extension(arrival_name)}"


def subtitle_copy_name(n: int, language: str | None, forced: bool, sdh: bool, ext: str) -> str:
    """The name of the copy a source folder keeps of the nth subtitle file that came with its original
    (n from 1, in the order the catalog lists them): subtitle-<n>.<lang>[.forced][.sdh].<ext>, lang
    the BCP 47 primary language subtag of language, lower-cased ('ger' and 'de-CH' are de), or und when
    it names none, and ext the file's own extension, lower-cased, or bin when it is not one to eight
    letters and digits. It says only what the record says of the file, never the name it came with."""
    if not isinstance(n, int) or isinstance(n, bool) or n < 1:
        raise ValueError(f"the subtitle files of a source are numbered from 1, not {n!r}")
    primary = str(language or "").strip().replace("_", "-").split("-")[0]
    code = (lang(primary)[0] or "und").lower()
    if not re.fullmatch(r"[a-z]{2,8}", code):
        code = "und"
    return f"subtitle-{n}.{code}" + (".forced" if forced else "") + (".sdh" if sdh else "") + \
        f".{_extension('x.' + str(ext or '').lstrip('.'))}"


# ---------------------------------------------------------------- streams, as ffprobe reported them
TEXT_SUBS = {"subrip", "srt", "ass", "ssa", "webvtt", "mov_text", "text", "arib_caption"}
IMAGE_SUBS = {"hdmv_pgs_subtitle", "dvd_subtitle", "dvb_subtitle", "xsub"}
LOSSLESS_AUDIO = {"truehd", "mlp", "flac", "alac", "pcm_s16le", "pcm_s24le", "pcm_s32le", "pcm_bluray",
                  "pcm_dvd", "wavpack", "ape", "tta"}
CLAIM_CODECS = [("truehd", r"truehd|true-hd|true hd"), ("dts-hd ma", r"dts[- .]?hd[- .]?(ma|master)"),
                ("dts", r"\bdts\b"), ("e-ac-3", r"e-?ac-?3|dd\+|ddp|dolby digital plus"),
                ("ac-3", r"\bac-?3\b|dolby digital(?! plus)"), ("flac", r"\bflac\b"), ("opus", r"\bopus\b"),
                ("aac", r"\baac\b")]
VARIANT_WORDS = re.compile(r"\b(Latin[ -]?American|LatAm|European|Castilian|Canadian|Brazilian|Portugal|"
                           r"Simplified|Traditional|Cantonese|Mandarin|Flemish|Swiss|Austrian|Mexican|"
                           r"Hong Kong|Taiwanese|Belgian|Qu[eé]b[eé]cois)\b", re.I)
RE_ENCODE = re.compile(r"handbrake|x264|x265|lavf|lavc|ffmpeg|nvenc|vaapi|qsv|mencoder|staxrip|shutter", re.I)


def title_variant(t):
    m = VARIANT_WORDS.search(t or "")
    return m.group(1) if m else None


def claim(title):
    if not title:
        return None
    t = title.lower()
    codec = next((c for c, pat in CLAIM_CODECS if re.search(pat, t)), None)
    m = re.search(r"\b([1-9])[.,]([0-2])\b", t)
    channels = f"{m.group(1)}.{m.group(2)}" if m else None
    return None if not codec and not channels else {"codec": codec, "channels": channels}


def claim_contradicts(c, codec, channels):
    if not c:
        return False
    actual = (codec or "").lower()
    family = {"truehd": ("truehd", "mlp"), "dts-hd ma": ("dts",), "dts": ("dts",), "e-ac-3": ("eac3",),
              "ac-3": ("ac3",), "flac": ("flac",), "opus": ("opus",), "aac": ("aac",)}
    bad = bool(c["codec"] and not any(actual.startswith(x) for x in family.get(c["codec"], ())))
    if c["channels"]:
        a, b = c["channels"].split(".")
        bad = bad or int(a) + int(b) != channels
    return bad


def dispositions(s):
    """The record's dispositions of a probe's stream. attachedPic marks what the closed lists' rule calls a cover
    (attached_picture) and nothing else - a cover its filename and mimetype tags alone mark among them, and not a
    stream the attached_pic disposition marks that is no cover -, so that a record's picture, its first video stream
    not marked attachedPic, is its probe's film (film_of)."""
    d = s.get("disposition") if isinstance(s.get("disposition"), dict) else {}
    out = {"default": bool(d.get("default")), "forced": bool(d.get("forced")), "original": bool(d.get("original")),
           "dub": bool(d.get("dub")), "commentary": bool(d.get("comment")),
           "hearingImpaired": bool(d.get("hearing_impaired")), "visualImpaired": bool(d.get("visual_impaired"))}
    if s.get("codec_type") == "video" and attached_picture(s):
        out["attachedPic"] = True
    for raw, key in (("lyrics", "lyrics"), ("captions", "captions"), ("descriptions", "descriptions")):
        if d.get(raw):
            out[key] = True
    return out


# The words a title says a track is a commentary with, and those that take them back: one of them
# right after a negation, or past an article or a possessive alone ("No commentary", "without the
# director's commentary", "ohne Kommentar", "keine Kommentare", "sans commentaire", "sin comentarios",
# "senza commento", "zonder commentaar", "non-commentary"), or a word that says it itself
# ("Kommentarlos", "kommentarfrei", "commentary-free"). A negation of anything else leaves the
# commentary one: "No-spoiler commentary", "Sin City commentary", "Without Remorse - Commentary".
COMMENTARY_WORDS = r"commentary|kommentar|commentaire|comentario|commento|commentaar"
_ARTICLE_OR_POSSESSIVE = (r"(?:the|any|a|an|audio|director(?:'|’)?s|des|del|di|van|de|der|die|das|le|la|el)"
               r"[\s\-]+")
NO_COMMENTARY_RE = re.compile(
    r"\b(?:no|without|w/o|ohne|keine?[nrs]?|sans|sin|senza|zonder|geen)[\s\-:]+(?:" + _ARTICLE_OR_POSSESSIVE + r"){0,3}"
    r"(?:" + COMMENTARY_WORDS + r"|comments?)|\bnon[\s\-]?(?:" + COMMENTARY_WORDS + r")"
    r"|kommentar[\s\-]?(?:los|frei)|commentary[\s\-]?free", re.I)


def says_commentary(title):
    """Whether a title says its track is a commentary: one of COMMENTARY_WORDS that no negation
    takes back (NO_COMMENTARY_RE)."""
    return bool(re.search(COMMENTARY_WORDS, NO_COMMENTARY_RE.sub(" ", title or ""), re.I))


def subtitle_purpose(s):
    d = s.get("disposition") or {}
    t = ((s.get("tags") or {}).get("title") or "").lower()
    if d.get("forced"):
        return "forced", "disposition"
    if d.get("hearing_impaired") or d.get("captions"):
        return "sdh", "disposition"
    if d.get("comment"):
        return "commentary", "disposition"
    if d.get("lyrics"):
        return "lyrics", "disposition"
    if re.search(r"\bforced\b", t) and not re.search(r"\b(non|not|no|un)[- ]?forced\b", t):
        return "forced", "title"
    if re.search(r"\bsigns?\b.{0,12}\bsongs?\b|\bsigns only\b", t):
        return "signs-songs", "title"
    if re.search(r"\bsdh\b|\bcc\b|hearing|closed caption", t):
        return "sdh", "title"
    if says_commentary(t):
        return "commentary", "title"
    if re.search(r"\blyrics\b|karaoke", t):
        return "lyrics", "title"
    return "dialogue", "assumed"


def audio_purpose(s):
    d = s.get("disposition") or {}
    t = ((s.get("tags") or {}).get("title") or "").lower()
    if d.get("comment"):
        return "commentary", "disposition"
    if d.get("visual_impaired") or d.get("descriptions"):
        return "description", "disposition"
    if says_commentary(t):
        return "commentary", "title"
    if re.search(r"audio description|descriptive (video|audio)|audiodeskription|audiodescription", t):
        return "description", "title"
    return "main", "assumed"


def hdr_info(s):
    trc = s.get("color_transfer")
    side = s.get("side_data_list") or []
    dovi = next((x for x in side if "DOVI" in (x.get("side_data_type") or "")), None)
    mastering = next((x for x in side if "Mastering display" in (x.get("side_data_type") or "")), None)
    cll = next((x for x in side if "Content light" in (x.get("side_data_type") or "")), None)
    fmt = "dolby-vision" if dovi else "hdr10" if trc == "smpte2084" else "hlg" if trc == "arib-std-b67" else "sdr"
    out = {"format": fmt, "masteringDisplay": None, "contentLightLevel": None, "dolbyVision": None}
    if mastering:
        out["masteringDisplay"] = {k: v for k, v in mastering.items() if k != "side_data_type"}
    if cll:
        out["contentLightLevel"] = {k: v for k, v in cll.items() if k != "side_data_type"}
    if dovi:
        out["dolbyVision"] = {"profile": num(dovi.get("dv_profile")) or 0, "level": num(dovi.get("dv_level")) or 0,
                              "rpuPresent": bool(dovi.get("rpu_present_flag")),
                              "elPresent": bool(dovi.get("el_present_flag")),
                              "blPresent": bool(dovi.get("bl_present_flag")),
                              "blCompatibilityId": num(dovi.get("dv_bl_signal_compatibility_id")) or 0}
    return out


def stereo3d(s):
    tags = s.get("tags") or {}
    mode = (tags.get("stereo_mode") or tags.get("STEREO_MODE") or "").lower()
    for x in s.get("side_data_list") or []:
        if "Stereo 3D" in (x.get("side_data_type") or ""):
            mode = (x.get("type") or mode or "").lower()
    return None if not mode or mode in ("mono", "2d") else mode


def bit_depth(s):
    b = num(s.get("bits_per_raw_sample"))
    if b:
        return b
    pf = s.get("pix_fmt") or ""
    m = re.search(r"p(\d{2})(le|be)?$", pf)
    return int(m.group(1)) if m else (8 if pf else None)


def font_signature(head):
    """The font format a file's first bytes say it is (FONT_SIGNATURES), or None."""
    return FONT_SIGNATURES.get(bytes(head or b"")[:4])


def attachment_role(mime, filename, head=None):
    """What an attachment is: a font by a MIME type that names one or a font's file name (FONT_MIME_RE,
    FONT_FILE_RE); a cover by a picture's MIME type; else, where its first bytes were read (head), a font
    when they are a font's signature; else other. A source counts a font as a font, and a package carries
    every one it counts, so none leaves with its original unnoticed."""
    if (mime and FONT_MIME_RE.search(str(mime))) or (filename and FONT_FILE_RE.search(str(filename).strip())):
        return "font"
    if mime and str(mime).strip().lower().startswith("image/"):
        return "cover"
    return "font" if font_signature(head) else "other"


def attachment_heads(path, probe):
    """The first bytes (HEAD_BYTES) of each attachment of the file at path that its probe gives no role
    by itself — no font's MIME type or name, no picture's MIME type (attachment_role) —, by its stream
    index: what tells a font by its signature. ffmpeg hands each to a pipe, read no further than that and
    closed; an attachment ffmpeg can't read, or a file without ffmpeg, gives none. Nothing is written. An
    ffmpeg that gives no bytes within HEAD_TIMEOUT seconds is killed, and AttachmentTimeout raised: what
    the attachment is can't be told."""
    out = {}
    if not shutil.which("ffmpeg"):
        return out
    for s in (probe or {}).get("streams") or []:
        tags = s.get("tags") or {}
        index = s.get("index")
        if s.get("codec_type") != "attachment" or not isinstance(index, int) or s.get("extradata_size") == 0 \
                or attachment_role(tags.get("mimetype"), tags.get("filename")) != "other":
            continue
        expired = threading.Event()
        try:
            p = subprocess.Popen(["ffmpeg", "-nostdin", "-v", "quiet", "-y", f"-dump_attachment:{index}", "pipe:1",
                                  "-i", path, "-map", f"0:{index}", "-c", "copy", "-t", "0", "-f", "null", "-"],
                                 stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
            timer = threading.Timer(HEAD_TIMEOUT, lambda p=p: (expired.set(), p.kill()))
            timer.start()
            try:
                head = p.stdout.read(HEAD_BYTES)
            finally:
                timer.cancel()
                p.stdout.close()
                p.kill()
                p.wait(timeout=60)
        except (OSError, subprocess.SubprocessError):
            continue
        if expired.is_set() and len(head) < HEAD_BYTES:
            raise AttachmentTimeout(f"ffmpeg gave no first bytes of attachment {index} of {path} within "
                                    f"{HEAD_TIMEOUT} s")
        if head:
            out[index] = head
    return out


def picture_stream(probe):
    """The stream of a probe whose closed captions are read (caption_windows, caption_signature): its film, the
    one video stream that is no cover by the closed lists' rule (film_of, the plan's picture), wherever a cover
    lies; None where there is none, two streams could each be it, or it has no index."""
    film = film_of(probe)
    return film if film is not None and isinstance(film.get("index"), int) else None


def seconds(v):
    """A time ffprobe gives ('12.345000'), as seconds, or None."""
    try:
        return float(v)
    except (TypeError, ValueError):
        return None


def caption_window(at, frames, where, code=0, lands=None):
    """One window of a picture as a record keeps it, {at, start, end, frames, captions}, from the frames
    ffprobe decoded of it (-show_frames, best_effort_timestamp_time or pts_time, duration_time and the
    side data): at, the fraction of the picture's span it is at (0 to 1, CAPTION_WINDOWS); start and end, the
    presentation seconds its frames cover, the first one's time to the last one's end, rounded to the
    millisecond; frames, how many were decoded; captions, whether one exports A53 captions; and, where
    ffprobe gave each frame's interlaced_frame and repeat_pict, how many were interlaced
    (interlacedFrames) and how many repeat a field (repeatFrames). A window that decoded none could not
    be read: it covers its place, where (seconds), alone, and its captions false says nothing — no frames: it
    failed, never one without them, and a picture with such a window and none that shows captions does not
    know its captions (source_essence). So does one a frame of which has no time to place it by, one whose
    ffprobe exited with an error (code not 0), and,
    where lands is given, one whose first frame lands says is not at its place (window_lands): it landed
    elsewhere than it aimed."""
    spans = []
    for f in frames:
        t = seconds(f.get("best_effort_timestamp_time")) if f.get("best_effort_timestamp_time") not in (None, "N/A") \
            else seconds(f.get("pts_time"))
        if t is not None:
            spans.append((t, t + (seconds(f.get("duration_time")) or 0.0)))
    first = min((a for a, _ in spans), default=None)
    if not frames or not spans or len(spans) < len(frames) or code != 0 or lands is not None and not lands(first):
        point = round(float(where or 0.0), 3)
        return {"at": at, "start": point, "end": point, "frames": 0, "captions": False}
    out = {"at": at, "start": round(min(a for a, _ in spans), 3), "end": round(max(b for _, b in spans), 3),
           "frames": len(frames),
           "captions": any(x.get("side_data_type") == CAPTION_SIDE_DATA for f in frames for x in f.get("side_data_list") or [])}
    if all("interlaced_frame" in f for f in frames):
        out["interlacedFrames"] = sum(1 for f in frames if num(f.get("interlaced_frame")))
    if all("repeat_pict" in f for f in frames):
        out["repeatFrames"] = sum(1 for f in frames if (num(f.get("repeat_pict")) or 0) > 0)
    return out


def record_duration_ms(probe):
    """The container.durationMs the record of a probe keeps (probed), or None where its format gives no length."""
    duration = seconds(((probe or {}).get("format") or {}).get("duration"))
    return int(duration * 1000) if duration else None


def picture_span(path, probe, picture):
    """The presentation span of the stream picture (a probe's stream) of the file at path as the plan places
    its windows (plan_of, as the gate knows it without reading): (start, end) in seconds, the stream's start_time
    (the file's where it gives none) to that plus its duration - where it gives none, the record's
    container.durationMs (record_duration_ms), and the format's duration only where the record gives none.
    None without a start, a length or a frame rate."""
    plan = plan_of(probe, record_duration_ms(probe))
    return None if plan is None else (plan["start"], plan["end"])


def picture_end(path, picture, past):
    """The end of the last video packet of the stream picture (a probe's stream) of the file at path, read from
    the last keyframe a seek to past (seconds, beyond the end the plan gives) lands on, no frame decoded, in
    seconds; None where that read fails or does not reach the file's end. An ffprobe that answers nothing
    within CAPTION_TIMEOUT raises CaptionsTimeout."""
    try:
        p = bounded_probe(["-select_streams", str(picture["index"]), "-read_intervals", f"{past:.3f}%+#{SPAN_PACKETS}",
                           "-show_entries", "packet=pts_time,duration_time", "-of", "json"], path)
        packets = json.loads(p.stdout or b"{}").get("packets") or []
    except subprocess.TimeoutExpired:
        raise CaptionsTimeout(f"ffprobe gave nothing of the end of stream {picture['index']} of {path} within "
                              f"{CAPTION_TIMEOUT} s")
    except (OSError, subprocess.SubprocessError, ValueError, AttributeError):
        return None
    ends = [t + (seconds(x.get("duration_time")) or 0.0) for x in packets if isinstance(x, dict)
            and (t := seconds(x.get("pts_time"))) is not None]
    if p.returncode != 0 or not ends or len(packets) >= SPAN_PACKETS:
        return None
    return round(max(ends), 3)


def picture_keys(path, picture, step):
    """What the first CAPTION_GOP_PACKETS packets of the stream picture of the file at path, none decoded, listed in
    decode order with their times and flags, say of its keyframes: (gop, leading). gop, the longest run between two
    keyframes among them at the frame step step, in seconds: 0 where they list fewer than two keyframes, or the
    listing fails. leading, the stream's own leading pictures (the plan's wholeComplete): the packets before its
    first keyframe and the first gop's packets timed before that keyframe; None where no keyframe is listed, its
    time is not, or they are more than wholeLeadingMax."""
    try:
        p = bounded_probe(["-select_streams", str(picture["index"]), "-read_intervals", f"%+#{CAPTION_GOP_PACKETS}",
                           "-show_entries", "packet=pts_time,flags", "-of", "json"], path)
        packets = json.loads(p.stdout or b"{}").get("packets") or []
    except subprocess.TimeoutExpired:
        raise CaptionsTimeout(f"ffprobe gave nothing of the keyframes of stream {picture['index']} of {path} "
                              f"within {CAPTION_TIMEOUT} s")
    except (OSError, subprocess.SubprocessError, ValueError, AttributeError):
        return 0.0, None
    listed = [(seconds(x.get("pts_time")), str(x.get("flags") or "").startswith("K"))
              for x in packets[:CAPTION_GOP_PACKETS] if isinstance(x, dict)]
    keys = [i for i, (_, key) in enumerate(listed) if key]
    gop = round(max((b - a for a, b in zip(keys, keys[1:])), default=0) * step, 3)
    if not keys or listed[keys[0]][0] is None:
        return gop, None
    first, at = keys[0], listed[keys[0]][0]
    end = keys[1] if len(keys) > 1 else len(listed)
    leading = first + sum(1 for t, _ in listed[first + 1:end] if t is not None and t < at)
    return gop, leading if leading <= CAPTION_PLAN["wholeLeadingMax"] else None


def read_bounds():
    """Set the bounds of the plan (CAPTION_BOUNDS) on a read's ffprobe before it runs: its address space and its
    CPU time, where the platform takes them (its thread count is ffprobe's -threads). A bound that is hit ends the
    read with an error: the window failed, not known. Each bound is set on its own: a platform that does not
    take one (macOS refuses an address space) still gets the other."""
    try:
        import resource
    except ImportError:
        return
    for limit, value in ((resource.RLIMIT_AS, CAPTION_BOUNDS["addressSpaceMiB"] << 20),
                         (resource.RLIMIT_CPU, CAPTION_BOUNDS["cpuS"])):
        try:
            resource.setrlimit(limit, (value, value))
        except (ValueError, OSError):
            pass


def bounded_probe(args, path):
    """ffprobe of the file at path with args, under the plan's bounds and CAPTION_TIMEOUT (raising
    subprocess.TimeoutExpired)."""
    return subprocess.run(["ffprobe", "-v", "error", "-threads", str(CAPTION_BOUNDS["threads"]), *args, path],
                          stdin=subprocess.DEVNULL, capture_output=True, timeout=CAPTION_TIMEOUT,
                          preexec_fn=read_bounds if os.name == "posix" else None)


FRAME_ENTRIES = ("frame=best_effort_timestamp_time,pts_time,duration_time,interlaced_frame,repeat_pict:"
                 "frame_side_data=side_data_type")


def frames_of(p):
    """The frames an ffprobe run gave, [] of one that gave none or garbage."""
    try:
        return [f for f in json.loads(p.stdout or b"{}").get("frames") or [] if isinstance(f, dict)]
    except ValueError:
        return []


def keyframe_before(path, picture, first, place):
    """Whether the packets of the stream picture of the file at path from a window's first frame (first, seconds)
    to its place, none decoded, show that frame's keyframe as the last one before its place: a keyframe where
    its frames begin (up to KEY_LEAD seconds after its first frame, past an open group's leading pictures), none
    between it and the place, and the packets reaching the place within KEY_LEAD. False where the listing fails.
    An ffprobe that answers nothing within CAPTION_TIMEOUT raises CaptionsTimeout."""
    try:
        p = bounded_probe(["-select_streams", str(picture["index"]), "-read_intervals", f"{first:.3f}%{place:.3f}",
                           "-show_entries", "packet=pts_time,flags", "-of", "json"], path)
        packets = json.loads(p.stdout or b"{}").get("packets") or []
    except subprocess.TimeoutExpired:
        raise CaptionsTimeout(f"ffprobe gave nothing of the keyframes of stream {picture['index']} of {path} "
                              f"before {place:.3f} s within {CAPTION_TIMEOUT} s")
    except (OSError, subprocess.SubprocessError, ValueError, AttributeError):
        return False
    listed = [(t, str(x.get("flags") or "").startswith("K")) for x in packets if isinstance(x, dict)
              and (t := seconds(x.get("pts_time"))) is not None]
    keys = sorted(t for t, key in listed if key)
    own = next((t for t in keys if t >= first - 1e-3), None)  # none of an empty listing
    if p.returncode != 0 or own is None or own > first + KEY_LEAD or max(t for t, _ in listed) < place - KEY_LEAD:
        return False
    return not any(own + 1e-3 < t <= place + 1e-3 for t in keys)


def ffprobe_release():
    """The release of the ffprobe on PATH, as its -version line names it ("7.1.5"), or None where it gives none, asked
    under the plan's bounds as every read is. A read records it beside its windows (ffprobe)."""
    try:
        p = subprocess.run(["ffprobe", "-version"], stdin=subprocess.DEVNULL, capture_output=True, timeout=CAPTION_TIMEOUT,
                           preexec_fn=read_bounds if os.name == "posix" else None)
    except (OSError, subprocess.SubprocessError):
        return None
    m = re.match(rb"ffprobe version (\S+)", p.stdout or b"")
    return m.group(1).decode("utf-8", "replace") if p.returncode == 0 and m else None


def caption_windows(path, probe):
    """The caption read of the picture of the file at path, by its stream index: {index: {windows, span, gop,
    ffprobe}} - ffprobe the release of the ffprobe that read it (ffprobe_release), where it names one -
    for the stream its essence reads (picture_stream), read by the plan (CAPTION_PLAN) over the plan's span the
    gate knows without reading (plan_of: the probe's start_time, duration and frame rate; of a probe that states
    no rate there is no plan, and nothing is read): gop, the longest run
    between two keyframes of its first CAPTION_GOP_PACKETS packets, and its leading pictures (picture_keys);
    then, where two of the plan's places lie closer than a window reaches (plan_short), the whole picture, every
    packet counted and decoded, one window at 0 (whole, packets, leading); else one window (caption_window) at
    each place (plan_places), the
    CAPTION_PACKETS packets ffprobe decodes from the keyframe its seek to it lands on, at its place by the plan's
    slack (window_lands; where its first frame lies further before its place, up to 30 s, the packets listed
    from it showing no keyframe between - keyframe_before - say so: keyframeBefore). A window ffprobe fails on,
    exits with an error of, decodes no frame of or lands elsewhere for is kept as one that could not be read;
    one of fewer frames than CAPTION_MIN_FRAMES is kept as read, and tells nothing; a whole read that fails,
    or hits a bound, leaves the windows as they were read (of a short picture, a failed one at its start) -
    not known. span is what the read measured: the first frame of its start to the end of the picture's last
    video packet (picture_end), left out where either can't be read; a span that is not the plan's within the
    tolerance tells nothing (windows_tell_none). Every read runs under the plan's bounds (bounded_probe).
    Empty - nothing told - for a file without such a stream, without ffprobe, or of no plan. Nothing is
    written. An ffprobe that gives no answer within CAPTION_TIMEOUT seconds is killed, and CaptionsTimeout
    raised: what the picture carries can't be told."""
    v = picture_stream(probe)
    if v is None or not shutil.which("ffprobe"):
        return {}
    plan = plan_of(probe, record_duration_ms(probe))
    if plan is None:
        return {}
    tolerance = caption_tolerance(plan)
    gop, leading = picture_keys(path, v, plan["step"])
    end = picture_end(path, v, plan["end"] + 60)

    def window(at, where):
        try:
            p = bounded_probe(["-select_streams", str(v["index"]), "-read_intervals",
                               f"{where:.3f}%+#{CAPTION_PACKETS}", "-show_frames", "-show_entries", FRAME_ENTRIES,
                               "-of", "json"], path)
        except subprocess.TimeoutExpired:
            raise CaptionsTimeout(f"ffprobe gave nothing of the frames of stream {v['index']} of {path} from "
                                  f"{where:.3f} s within {CAPTION_TIMEOUT} s")
        except (OSError, subprocess.SubprocessError):
            return caption_window(at, [], where)
        w = caption_window(at, frames_of(p), where, p.returncode)
        if w["frames"] and not window_lands(w["start"], where, gop, plan):
            back = CAPTION_PLAN["placeSlack"]["backS"]
            if gop and where - back <= w["start"] <= where and keyframe_before(path, v, w["start"], where):
                w["keyframeBefore"] = True
            else:
                w = caption_window(at, [], where)
        return w

    def whole():
        try:
            counted = bounded_probe(["-select_streams", str(v["index"]), "-count_packets", "-show_entries",
                                     "stream=nb_read_packets", "-of", "json"], path)
            packets = int(((json.loads(counted.stdout or b"{}").get("streams") or [{}])[0]).get("nb_read_packets"))
            p = bounded_probe(["-select_streams", str(v["index"]), "-show_frames", "-show_entries", FRAME_ENTRIES,
                               "-of", "json"], path)
        except (subprocess.SubprocessError, OSError, ValueError, TypeError, IndexError, AttributeError):
            return None
        if counted.returncode != 0 or p.returncode != 0:
            return None
        w = caption_window(0.0, frames_of(p), plan["start"], p.returncode,
                           lambda first: abs(first - plan["start"]) <= tolerance)
        return [dict(w, whole=True, packets=packets, **({} if leading is None else {"leading": leading}))] \
            if w["frames"] else None

    if plan_short(plan, gop):
        out = whole() or [caption_window(0.0, [], plan["start"])]
    else:
        out = [window(at, where) for at, where in zip(CAPTION_WINDOWS, plan_places(plan))]
    read = {"windows": out, "gop": gop, **({"ffprobe": release} if (release := ffprobe_release()) else {})}
    if out[0]["frames"] and end is not None:
        read["span"] = {"start": out[0]["start"], "end": end}
    return {v["index"]: read}


def picture_windows(record):
    """The caption windows a source record's picture keeps (its first video stream that is no attached
    picture), or None where it keeps none."""
    picture = next((x for x in (record or {}).get("streams") or []
                    if x.get("type") == "video" and not (x.get("dispositions") or {}).get("attachedPic")), None)
    return caption_windows_of((picture or {}).get("captionWindows"))


def caption_signature(path, probe):
    """Whether the packets of the picture of the file at path (picture_stream) carry a closed-caption
    signature (USER_DATA_SIGNATURES of an MPEG-2 picture, T35_SIGNATURES of any other), every one of them
    read as ffmpeg copies it: {index: True} at the first
    that does, {index: False} where it read them all and none does: ffmpeg, stopping at the first error
    (-xerror), exited 0, said no error, and handed on some bytes. Empty — nothing told — for a file without
    such a stream, without ffmpeg, or that ffmpeg could not read to its end. Nothing is written.
    An ffmpeg that hands nothing on for SIGNATURE_STALL seconds is killed, and CaptionsTimeout raised.
    A guard beside the windows (caption_windows): captions outside them are found, and a picture without
    the signature is told nothing more by it."""
    v = picture_stream(probe)
    if v is None or not shutil.which("ffmpeg"):
        return {}
    try:
        p = subprocess.Popen(["ffmpeg", "-nostdin", "-v", "error", "-xerror", "-i", path, "-map", f"0:{v['index']}",
                              "-c", "copy", "-f", "data", "pipe:1"], stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                             stderr=subprocess.PIPE)
    except (OSError, subprocess.SubprocessError):
        return {}
    moved, stalled, said = threading.Event(), threading.Event(), []

    def errors():
        for line in p.stderr:
            if len(said) < 20:
                said.append(line)

    reader = threading.Thread(target=errors, daemon=True)
    reader.start()

    def watch():
        while p.poll() is None:
            if not moved.wait(SIGNATURE_STALL):
                stalled.set()
                p.kill()
                return
            moved.clear()

    threading.Thread(target=watch, daemon=True).start()
    signatures = USER_DATA_SIGNATURES if (v.get("codec_name") or "") in ("mpeg1video", "mpeg2video") else T35_SIGNATURES
    keep = max(len(x) for x in signatures) - 1
    found, tail, total = False, b"", 0
    try:
        while True:
            chunk = p.stdout.read(SIGNATURE_CHUNK)
            moved.set()
            if not chunk:
                break
            total += len(chunk)
            data = tail + chunk
            if any(x in data for x in signatures):
                found = True
                break
            tail = data[-keep:]
    finally:
        p.stdout.close()
        if found:
            p.kill()
        code = p.wait(timeout=60)
        moved.set()
        reader.join(timeout=10)
    if found:
        return {v["index"]: True}
    if stalled.is_set():
        raise CaptionsTimeout(f"ffmpeg handed nothing of stream {v['index']} of {path} on for {SIGNATURE_STALL} s")
    return {v["index"]: False} if code == 0 and total > 0 and not any(x.strip() for x in said) else {}


def norm_stream(s, head=None, captions=None, signature=None):
    """One stream of a probe as a record keeps it. head is an attachment's first bytes, where they were
    read (attachment_heads): what its role is told by when its MIME type and its name say nothing.
    captions are a video stream's caption read, where it was read (caption_windows: {windows, span, gop}, or
    its windows alone): the stream keeps its windows (captionWindows), the span it measured (captionSpan) and
    its gop (captionGop), and carries closed captions when a window shows them; without them only a
    probe that decoded a frame of it says it carries them, and one that says none may not have looked.
    signature is whether its packets carry a caption signature, where they were read for one
    (caption_signature): it keeps it (captionSignature), and carries closed captions where they do. A
    subtitle or data stream that is closed captions of its own (caption_stream) says so (closedCaptions)."""
    t = s.get("codec_type")
    tags = s.get("tags") or {}
    language, raw = lang(tags.get("language"))
    base = {"index": s["index"], "codec": s.get("codec_name"), "language": language, "languageRaw": raw,
            "title": text(tags.get("title")), "dispositions": dispositions(s)}
    if t == "video":
        read = captions if isinstance(captions, dict) else {"windows": captions}
        windows = caption_windows_of(read.get("windows"))
        base.update({
            "type": "video", "profile": s.get("profile"),
            "level": s.get("level") if isinstance(s.get("level"), (int, float)) and s.get("level") >= 0 else None,
            "bitDepth": bit_depth(s), "pixelFormat": s.get("pix_fmt"), "width": s.get("width"),
            "height": s.get("height"), "sampleAspectRatio": ratio(s.get("sample_aspect_ratio")),
            "displayAspectRatio": ratio(s.get("display_aspect_ratio")),
            "frameRate": s.get("avg_frame_rate") if s.get("avg_frame_rate") not in (None, "0/0") else s.get("r_frame_rate"),
            "fieldOrder": s.get("field_order"), "bitrate": num(s.get("bit_rate")),
            "colour": {"primaries": s.get("color_primaries"), "transfer": s.get("color_transfer"),
                       "matrix": s.get("color_space"), "range": s.get("color_range")},
            "hdr": hdr_info(s), "stereo3d": stereo3d(s),
            "closedCaptions": bool(s.get("closed_captions")) or windows_show(windows) or signature is True,
            "encoder": text(tags.get("ENCODER") or tags.get("encoder")),
        })
        if windows is not None:
            base["captionWindows"] = windows
            span = read.get("span") if isinstance(read.get("span"), dict) else {}
            if gate_number(span.get("start")) is not None and gate_number(span.get("end")) is not None:
                base["captionSpan"] = {"start": round(float(span["start"]), 3), "end": round(float(span["end"]), 3)}
            if gate_number(read.get("gop")) is not None and read["gop"] >= 0:
                base["captionGop"] = round(float(read["gop"]), 3)
            if isinstance(read.get("ffprobe"), str) and read["ffprobe"]:
                base["captionFfprobe"] = read["ffprobe"]
        if isinstance(signature, bool):
            base["captionSignature"] = signature
        return base
    if t == "audio":
        codec = (s.get("codec_name") or "").lower()
        profile = s.get("profile") or ""
        channels = num(s.get("channels")) or 1
        c = claim(tags.get("title"))
        if c is not None:
            c["contradictsActual"] = claim_contradicts(c, codec, channels)
        base.update({
            "type": "audio", "profile": profile or None, "channels": channels,
            "channelLayout": s.get("channel_layout"), "sampleRate": num(s.get("sample_rate")),
            "bitDepth": num(s.get("bits_per_raw_sample")) or None,
            "bitrate": num(s.get("bit_rate")) or num(tags.get("BPS")),
            "lossless": codec in LOSSLESS_AUDIO or (codec == "dts" and "MA" in profile),
            "objectAudio": "atmos" if "atmos" in profile.lower() else "dts-x" if "dts:x" in profile.lower() else "none",
            "titleClaim": c, "encoder": text(tags.get("ENCODER") or tags.get("encoder")),
        })
        base["purpose"], base["purposeFrom"] = audio_purpose(s)
        base["variant"] = title_variant(tags.get("title"))
        return base
    if t == "subtitle":
        codec = (s.get("codec_name") or "").lower()
        events = next((num(v) for k, v in tags.items() if k.upper().startswith("NUMBER_OF_FRAMES")), None)
        base.update({"type": "subtitle", "form": "image" if codec in IMAGE_SUBS else "text",
                     "styled": codec in ("ass", "ssa"), "variant": title_variant(tags.get("title")),
                     "events": events})
        base["purpose"], base["purposeFrom"] = subtitle_purpose(s)
        if caption_stream(s):
            base["closedCaptions"] = True
        return base
    if t == "attachment":
        fname, mime = tags.get("filename"), tags.get("mimetype")
        role = attachment_role(mime, fname, head)
        base.update({"type": "attachment", "filename": text(fname), "mimetype": text(mime), "role": role})
        if role == "font" and attachment_role(mime, fname) != "font":
            base["signature"] = font_signature(head)
        return base
    base.update({"type": "data"})
    if caption_stream(s):
        base["closedCaptions"] = True
    return base


def infer_forced_by_size(streams):
    """An untitled, unflagged subtitle with a small fraction of the events of the full track in its
    language translates a few lines, not the film: it is a forced track."""
    subs = [x for x in streams if x["type"] == "subtitle"]
    for x in subs:
        if x["purposeFrom"] != "assumed" or not x.get("events"):
            continue
        peers = [y["events"] for y in subs if y is not x and y.get("events")
                 and primary_language(y.get("language")) == primary_language(x.get("language"))]
        if peers and max(peers) >= 300 and x["events"] < 0.1 * max(peers):
            x["purpose"], x["purposeFrom"] = "forced", "content"


# ---------------------------------------------------------------- essence
def track_essence(audio, subs, closed_captions):
    def langs(items, purposes):
        return sorted({lang(x.get("language"))[0] for x in items if x.get("purpose") in purposes and x.get("language")})
    return {"commentarySubtitles": sum(1 for x in subs if x.get("purpose") == "commentary"),
            "audioDescriptionTracks": sum(1 for x in audio if x.get("purpose") == "description"),
            "sdhSubtitleLanguages": langs(subs, ("sdh",)),
            "forcedSubtitleLanguages": langs(subs, ("forced", "signs-songs")),
            "closedCaptions": bool(closed_captions)}


def source_essence(streams, chapters, off=None, plan=None):
    """What a file carries, reduced to what a package can fail to carry. Its closed captions are its
    first picture's, or a stream's that is closed captions of its own (closedCaptions of a subtitle or
    data stream), and known (closedCaptionsKnown) where such a stream or the picture's caption signature
    (captionSignature) says them, or where the picture's caption windows tell them — one shows them, or
    every one was read and none does, of a file on the closed lists of how captions are carried (off,
    its off_list, empty) and, of a picture whose packets carry A53 its decoder does not export
    (SCANNED_CODECS), whose packets were read whole and carry no caption signature: of a stream they
    weren't read of, of one a window of which could not be read (no frames, captions false) and none
    shows them, or of a file off the lists, closedCaptions false says nothing was seen, not that there is
    nothing, and the deletion gate keeps the original for it.
    Its picture is in fields (interlaced) where its field order marks it so (tt, bb, tb, bt) or a caption
    window of it counted interlaced frames (windows_count_fields). The picture's read tells none as
    windows_tell_none holds it over the plan's span of its probe (plan, plan_of): six at their places, on
    frames of their own, each of half its packets and 24 frames, or the whole picture (captions_known, the shared gate
    block's)."""
    v = [x for x in streams if x["type"] == "video" and not x["dispositions"].get("attachedPic")]
    a = [x for x in streams if x["type"] == "audio"]
    sub = [x for x in streams if x["type"] == "subtitle"]
    v0 = v[0] if v else {}
    hdr = v0.get("hdr") or {}
    own = any(x.get("closedCaptions") for x in streams if x["type"] in ("subtitle", "data"))
    return {
        "maxAudioChannels": max([x["channels"] for x in a], default=0),
        "surround": any(x["channels"] > 2 for x in a),
        "losslessAudio": any(x.get("lossless") for x in a),
        "objectAudio": any(x.get("objectAudio") in ("atmos", "dts-x") for x in a),
        "maxVideoHeight": v0.get("height"), "videoBitDepth": v0.get("bitDepth"),
        "hdr10Metadata": bool(hdr.get("masteringDisplay") or hdr.get("contentLightLevel")),
        "dolbyVision": hdr.get("format") == "dolby-vision", "stereo3d": bool(v0.get("stereo3d")),
        "interlaced": (v0.get("fieldOrder") or "progressive") not in ("progressive", "unknown", None)
        or "captionWindows" in v0 and windows_count_fields(v0.get("captionWindows")),
        "audioLanguages": sorted({x["language"] for x in a if x.get("language")}),
        "subtitleLanguages": sorted({x["language"] for x in sub if x.get("language")}),
        "subtitleTracks": len(sub), "imageSubtitles": any(x.get("form") == "image" for x in sub),
        "styledSubtitles": any(x.get("styled") for x in sub),
        "fonts": any(x["type"] == "attachment" and x.get("role") == "font" for x in streams),
        "chapters": bool(chapters),
        "commentaryTracks": sum(1 for x in a if x.get("purpose") == "commentary"),
        **track_essence(a, sub, v0.get("closedCaptions", False) or own),
        **({"closedCaptionsKnown": True} if captions_known(streams, off, plan) else {}),
    }


def counts_of(doc, keys):
    """The counts of doc (a dict) by keys, each a whole number from 0, and those alone."""
    return {k: doc[k] for k in keys if isinstance(doc.get(k), int) and not isinstance(doc.get(k), bool)
            and doc[k] >= 0}


def scan_block(scan):
    """A package's scan as its record keeps it, from the manifest's — the transcoder's contract's, as the
    packager copies it: what was done (done) and found (found), in the words it says them in, known here
    (SCAN_DONE, SCAN_FOUND) or not; the words of how (SCAN_WORDS: what judged it, the field order it was
    marked with and the one taken and what named it, the package's frame rate, the filters), each a text
    or null, and whether the examination saw that order (orderSeen, or null); the counts of the
    examination (idet: SCAN_COUNTS, and its windows, SCAN_WINDOWS), left out where it gives none; and
    what the read-through of the whole title found (verified: VERIFIED_FLAGS, VERIFIED_WORDS and
    VERIFIED_COUNTS),
    left out where none ran. None for a manifest without one: no object, or one that says neither what
    was done nor what was found. One that says what was done and not what was found was found unknown,
    and one that says what was found and not what was done did nothing it names (done null)."""
    if not isinstance(scan, dict) or not (text(scan.get("done")) or text(scan.get("found"))):
        return None
    out = {"found": text(scan.get("found")) or "unknown", "done": text(scan.get("done")),
           **{k: text(scan.get(k)) for k in SCAN_WORDS},
           "orderSeen": scan["orderSeen"] if isinstance(scan.get("orderSeen"), bool) else None}
    idet = scan.get("idet")
    if isinstance(idet, dict):
        counts = counts_of(idet, SCAN_COUNTS)
        windows = counts_of(idet.get("windows"), SCAN_WINDOWS) if isinstance(idet.get("windows"), dict) else {}
        if windows:
            counts["windows"] = windows
        if counts:
            out["idet"] = counts
    verified = scan.get("verified")
    if isinstance(verified, dict):
        out["verified"] = {**{k: verified[k] if isinstance(verified.get(k), bool) else None for k in VERIFIED_FLAGS},
                           **{k: text(verified.get(k)) for k in VERIFIED_WORDS},
                           **counts_of(verified, VERIFIED_COUNTS)}
    return out


def package_essence(man, source=None, streams=None):
    """What a package carries, in the terms of a source's essence. The 5.1 companions under
    renditions.audioSurround count for the channels and the languages it carries; a track is counted
    once, by its stereo rendition, for what a track is for. A top video rendition that is the
    original's own stream, copied (encoder 'copy'), in PQ, carries the HDR10 metadata in its bitstream
    when the original — source, its essence — had it, and so does a package every video rendition of
    which in PQ, the top one among them, says its init segment carries the original's (hdr10Metadata),
    encoded or not. One in PQ that says nothing of it — one whose init segment could not take it — and
    anything else re-encoded is taken to have lost it.

    A package has closed captions when its top video rendition says its own stream carries them
    (closedCaptions): CEA-608/708 captions in the stream's SEI, as the packager read them from the
    rendition's frames — a copy of the original's stream keeps the original's, an encode those
    its encoder wrote. The top rendition is the package's full picture, as a source's essence has the
    captions of its first video stream: what a lower rung lacks, the top one still carries. A top
    rendition that says nothing of them has none, and neither has the package.

    A package whose manifest says what its picture is as far as fields go (scan, scan_block) has what
    was done of it (scan: its done), and none says nothing of it.

    A package has styled subtitles when a subtitle keeps its styled original (styled), and fonts when
    it lists a font file (fonts). Where the source's streams are given — a source record's streams —
    it has them only when it carries them for every stream that has them: each styled subtitle stream
    by a subtitle made from it that keeps its original, each font attachment by a font file taken from
    it, both by sourceStreamIndex. A styled track kept as WebVTT alone, or a font left out, so leaves
    the package without them, and the deletion gate names them."""
    ren = man.get("renditions") or {}
    vids, auds = ren.get("video") or [], ren.get("audio") or []
    every = auds + (ren.get("audioSurround") or [])
    subs = man.get("subtitles") or []
    v0 = vids[0] if vids else {}
    codec = v0.get("codec") or ""
    copied_pq = v0.get("encoder") == "copy" and v0.get("videoRange") == "PQ"
    declared = v0.get("videoRange") == "PQ" and all(
        x.get("hdr10Metadata") is True for x in vids if x.get("videoRange") == "PQ")
    styled = {x.get("sourceStreamIndex") for x in subs if styled_original(x.get("styled"))}
    fonts = {x.get("sourceStreamIndex") for x in (font_file(f) for f in man.get("fonts") or []) if x}

    def carried(taken, has):
        # every stream of the source that has it is one the package took it from
        return bool(taken) and (streams is None or all(x.get("index") in taken for x in streams if has(x)))

    return {
        "maxAudioChannels": max([x.get("channels") or 0 for x in every], default=0),
        "surround": any((x.get("channels") or 0) > 2 for x in every),
        "losslessAudio": False, "objectAudio": False, "maxVideoHeight": v0.get("height"),
        "videoBitDepth": 10 if codec.startswith(("hev1.2", "hvc1.2")) else (8 if codec else None),
        "hdr10Metadata": bool((copied_pq or declared) and (source or {}).get("hdr10Metadata")),
        "dolbyVision": False,
        "stereo3d": False, "interlaced": False,
        "audioLanguages": sorted({lang(x.get("language"))[0] for x in every if x.get("language")}),
        "subtitleLanguages": sorted({lang(x.get("language"))[0] for x in subs if x.get("language")}),
        "subtitleTracks": len(subs),
        "imageSubtitles": any((x.get("format") or "") in ("pgs", "sup", "vobsub", "dvb") for x in subs),
        "styledSubtitles": carried(styled, lambda x: x.get("type") == "subtitle" and x.get("styled")),
        "fonts": carried(fonts, lambda x: x.get("type") == "attachment" and x.get("role") == "font"),
        "chapters": False,
        "commentaryTracks": sum(1 for x in auds if x.get("purpose") == "commentary"),
        **track_essence(auds, subs, v0.get("closedCaptions") is True),
        **({"scan": scan["done"]} if (scan := scan_block(man.get("scan"))) and scan["done"] else {}),
    }


CODEC_FAMILY = [("aac", r"^(mp4a\.40|aac)"), ("ac3", r"^(ac-3|ac3)"), ("eac3", r"^(ec-3|eac3)"),
                ("opus", r"^opus"), ("flac", r"^(fLaC|flac)"), ("alac", r"^alac"), ("truehd", r"^(truehd|mlp)"),
                ("dts", r"^dts"), ("hevc", r"^(hev1|hvc1|hevc)"), ("h264", r"^(avc1|avc3|h264)"),
                ("av1", r"^av0?1"), ("vp9", r"^vp0?9")]


def codec_family(codec):
    """'mp4a.40.2' and 'aac' are the same codec under two names; a package advertises the MP4 name
    and a probe reports the library name, so they are compared as families."""
    c = (codec or "").strip().lower()
    for family, pat in CODEC_FAMILY:
        if re.match(pat, c):
            return family
    return c


def losses_against(src, pkg_essence, streams, renditions, subs):
    """Every way the package is poorer than the original, in the package record's own terms. A source
    track is packaged once, as its stereo rendition, so only those count for a track that was dropped;
    its 5.1 companion counts for the codecs the package carries."""
    out = []
    a = [x for x in streams if x["type"] == "audio"]
    v = [x for x in streams if x["type"] == "video" and not x["dispositions"].get("attachedPic")]
    sub = [x for x in streams if x["type"] == "subtitle"]
    auds = renditions.get("audio") or []
    vids = renditions.get("video") or []
    if src.get("maxAudioChannels", 0) > pkg_essence.get("maxAudioChannels", 0):
        out.append({"kind": "audio-downmix",
                    "detail": f"{src['maxAudioChannels']}ch -> {pkg_essence.get('maxAudioChannels', 0)}ch",
                    "sourceStreamIndex": a[0]["index"] if a else None})
    codecs = sorted({codec_family(x.get("codec")) for x in a})
    packaged = sorted({codec_family(x.get("codec")) for x in auds + (renditions.get("audioSurround") or [])})
    if codecs and any(c not in codecs for c in packaged):
        out.append({"kind": "audio-codec", "detail": f"{'/'.join(codecs)} -> {'/'.join(packaged)}",
                    "sourceStreamIndex": a[0]["index"] if a else None})
    if len(a) > len(auds):
        out.append({"kind": "audio-dropped", "detail": f"{len(a) - len(auds)} of {len(a)} audio track(s) not packaged",
                    "sourceStreamIndex": None})
    if v and vids and (v[0].get("height") or 0) > (vids[0].get("height") or 0):
        out.append({"kind": "video-resolution", "detail": f"{v[0]['height']}p -> {vids[0]['height']}p",
                    "sourceStreamIndex": v[0]["index"]})
    if (src.get("videoBitDepth") or 0) > (pkg_essence.get("videoBitDepth") or 0):
        out.append({"kind": "video-bitdepth",
                    "detail": f"{src['videoBitDepth']}bit -> {pkg_essence.get('videoBitDepth')}bit",
                    "sourceStreamIndex": v[0]["index"] if v else None})
    if src.get("hdr10Metadata") and not pkg_essence.get("hdr10Metadata"):
        out.append({"kind": "dynamic-range", "detail": "HDR10 metadata not carried into the package",
                    "sourceStreamIndex": v[0]["index"] if v else None})
    if src.get("dolbyVision") and not pkg_essence.get("dolbyVision"):
        out.append({"kind": "dolby-vision", "detail": "Dolby Vision layer not carried into the package",
                    "sourceStreamIndex": v[0]["index"] if v else None})
    if src.get("stereo3d") and not pkg_essence.get("stereo3d"):
        out.append({"kind": "stereo3d", "detail": "3D presentation not carried into the package",
                    "sourceStreamIndex": v[0]["index"] if v else None})
    if len(sub) > len(subs):
        out.append({"kind": "subtitle-dropped",
                    "detail": f"{len(sub) - len(subs)} of {len(sub)} source subtitle track(s) not packaged",
                    "sourceStreamIndex": None})
    if src.get("styledSubtitles") and not pkg_essence.get("styledSubtitles"):
        out.append({"kind": "subtitle-styling", "detail": "ASS/SSA typesetting flattened", "sourceStreamIndex": None})
    if src.get("closedCaptions") and not pkg_essence.get("closedCaptions"):
        out.append({"kind": "closed-captions-dropped", "detail": "embedded CEA-608/708 captions not carried",
                    "sourceStreamIndex": v[0]["index"] if v else None})
    if src.get("fonts") and not pkg_essence.get("fonts"):
        out.append({"kind": "attachments-dropped", "detail": "attached fonts not carried", "sourceStreamIndex": None})
    return out


# ==== the deletion gate, shared ====
# From here to the end of this block the text is the same, byte for byte, in tools/libv2_records.py,
# tools/validate-library-v2.py and tools/library-v2-rebuild.py: every reader computes the gate with it, and
# test-library-v2-tools.py holds the three copies equal. It needs nothing but the modules json and re.
#
# A picture's closed captions are read in windows, by ONE plan for every reader: library/v2/caption-windows.json,
# whose bytes are CAPTION_PLAN_JSON here, held to the file by test-library-v2-tools.py and to its pinned sha256
# by every repository that vendors it. It is data the code checks itself against, not configuration: nothing
# reads it at run time from a path a setting could change. The plan's span is what the gate knows without
# reading (plan_of: the stored probe's picture start_time + duration); the start, then 10, 30, 50, 70 and 90 %
# of it, each but the start at most CAPTION_TAIL_S short of its end (plan_places), CAPTION_PACKETS packets
# each, at least CAPTION_MIN_FRAMES of them decoded (half, and 24 at least), each starting at its place by
# the plan's slack (window_lands: max(10 s, gop + 0.5 s), up to 30 s before it where no keyframe lies between,
# of a gop of 0 within the tolerance alone). A read whose span is not the plan's within the tolerance
# (caption_tolerance: a frame step and 0.5 s) tells nothing. Where two places lie closer than a window reaches (plan_short), the picture is read
# whole (a window that says so, whole, and counts its packets), and tells none only where every packet of it
# was decoded (windows_tell_none).
CAPTION_PLAN_JSON = r'''{
  "schema": "zaentrum.library.caption-windows/1",
  "note": "The plan every reader of an original's closed captions reads its windows by, and the data it checks itself against: not configuration. tools/libv2_records.py carries these bytes in its code and its tests hold them to this file; every other reader vendors this file, pinned by its sha256, and a copy that differs fails its build. No installation edits it, and nothing reads it from a path a setting could change.",
  "start": true,
  "at": [0.1, 0.3, 0.5, 0.7, 0.9],
  "minRate": 1,
  "endTolerance": {"frameSteps": 1, "s": 0.5, "words": "frameStep + 0.5: one frame step of the picture plus 0.5 s, the same on every side: both the start and the end of a read's measured span, and both the first frame and the end of a whole read, lie within it of the plan's span"},
  "tailS": 30,
  "packets": 48,
  "minFrames": 24,
  "packetsShare": 0.5,
  "gopPackets": 1200,
  "placeSlack": {"s": 10, "gopPlus": 0.5, "backS": 30, "words": "a window's first frame lies within max(10 s, gop + 0.5 s) of its place, before or after it; or up to backS before it where the packets from its seek show no keyframe between its first frame and its place (keyframeBefore); of a gop of 0 a window counts only at its own place, within endTolerance"},
  "shortReach": {"packets": 48, "gop": true, "s": 0.5, "words": "packets x frameStep + gop + 0.5"},
  "wholeLeadingMax": 16,
  "bounds": {"threads": 1, "addressSpaceMiB": 1280, "cpuS": 600, "timeoutS": 180},
  "words": {
    "picture": "the stored probe's film: its one video stream that is no cover by the closed lists' rule, a cover being all of these - marked as an attachment (the attached_pic disposition as the number 1 or true, or both a filename and a mimetype tag, never one alone), of a cover's codec (mjpeg, png, bmp; its mimetype, where it has one, image/*, and never in the codec's stead), of at most one frame by every count it is given (nb_frames, a tag that starts NUMBER_OF_FRAMES), each in digits alone - a count that is there but can't be read is no cover's - and of no length of its own (no tag that starts DURATION) -, wherever a cover lies; any other video stream may be the picture: where none is, or two could each be it, there is no plan",
    "end": "the stored probe's picture start_time + duration; the format's start_time where the picture gives none; where it gives no duration, the record's container.durationMs, and the format's duration only where the record gives none",
    "frameStep": "one over the stored probe's picture avg_frame_rate, or its r_frame_rate only where avg_frame_rate is exactly the text 0/0, each a ratio num/den of digits alone (no space, sign or point): what the gate can know without reading; an avg_frame_rate that is missing or no such ratio gives no plan, with no fallback to r_frame_rate, and so does a stated rate below minRate frames a second (0/1 among them); without a plan the read is not known; no reader measures a frame step of its own, which the gate could not check",
    "wholeComplete": "packets - leading <= frames <= packets: leading the stream's own leading pictures, the packets before its first keyframe in decode order and the first gop's packets timed before that keyframe, at most wholeLeadingMax; a decoder that outputs the leading pictures too is complete as well; a read that gives no leading is not complete; every picture it decoded has a time; and its packets at frameStep cover the plan's span within endTolerance (packets x frameStep >= end - start - endTolerance)"
  },
  "rule": "The picture is the stored probe's film by the closed lists' cover rule (picture); where two streams could each be it there is no plan. The plan's span is what the gate knows without reading: from the stored probe's picture start_time (the format's where the picture gives none) to that plus its duration (where the picture gives none, the record's container.durationMs, and the format's duration only where the record gives none); frameStep is one over the stored probe's picture avg_frame_rate, or its r_frame_rate only where avg_frame_rate is exactly 0/0 (frameStep); where avg_frame_rate is missing or no ratio, or the probe states a rate below minRate frames a second, there is no plan, and the read is not known. A number that is NaN or Infinity is no number stated. A read places six windows on it: the start, at the start itself, never capped by the tail; then each of at, at start + at x (end - start), at most tailS short of the end and never before the start - never half the span or any other share of it. A reader measures the end of the picture's last video packet, and the first frame of its start: a read whose measured span does not lie with both its start and its end within endTolerance of the plan's start and end fails, and tells nothing; it reports its span beside the file's length (duration), and a validator holds the span a read reports to its record's plan within endTolerance. gop is the longest run between two keyframes among the picture's first gopPackets packets, at frameStep, and 0 where they list fewer than two keyframes; a read reports it, and one whose gop is above gopPackets x frameStep is not known. Each window decodes packets packets from the keyframe its seek lands on, and counts only where it decoded at least packetsShare of them and at least minFrames frames, and its first frame lies at its place by placeSlack. The six tell none only where all six are such windows, one at each place, none shows captions, and their spans, in the order of their places, are disjoint and rise: a file whose gop is 0 never tells none of windows that overlap or don't start at their own places. Where two of the places lie closer than shortReach (packets x frameStep + gop + 0.5 s), as on a span too short for six disjoint windows at most tailS short of its end, the read is the whole picture, every packet decoded: one window at 0, whole: true, with its frames, its packets and its leading pictures (leading) counted. It stands for the six, and tells none only where it is complete by wholeComplete - it decoded every packet but the stream's own leading pictures, the packets before its first keyframe in decode order and the first gop's packets timed before that keyframe, at most wholeLeadingMax, or those too -, none shows captions, and its first frame and its end lie within endTolerance of the plan's start and end. A record tells the two apart by their shape: six windows, or one whole window alone; a picture whose places lie closer than shortReach tells none by a whole read alone, and a whole read of a picture long enough for six windows is accepted too: it is the stronger proof. Every read, a window's, the keyframe listings, the end's and the short picture's whole read alike, runs under the same bounds, on every side: threads decoding threads, addressSpaceMiB of address space, cpuS of CPU time and timeoutS seconds of wall time for each read; a bound that is hit means not known, never none. The read of every packet of an HEVC picture for a caption signature, which is deferred, is not bound by these: it gets a bound of its own once its form is fixed. Neither a whole read nor windows say none of a picture whose packets carry captions its decoder does not export (HEVC's suffix SEI) unless every packet of it was read for a caption signature and none carries one. Any other read is not known."
}
'''
CAPTION_PLAN = json.loads(CAPTION_PLAN_JSON)
CAPTION_PACKETS = CAPTION_PLAN["packets"]
CAPTION_WINDOWS = (0.0, *CAPTION_PLAN["at"]) if CAPTION_PLAN["start"] else tuple(CAPTION_PLAN["at"])
CAPTION_TAIL_S = float(CAPTION_PLAN["tailS"])
CAPTION_GOP_PACKETS = CAPTION_PLAN["gopPackets"]
CAPTION_MIN_FRAMES = max(CAPTION_PLAN["minFrames"], -(-CAPTION_PACKETS * CAPTION_PLAN["packetsShare"] // 1))
CAPTION_BOUNDS = CAPTION_PLAN["bounds"]
# The closed lists of how closed captions and fields are carried, kept the same as the analyzer's, by its
# names: the containers (CONTAINERS, ffprobe's format_name); the codecs of the film's picture (VIDEO_CODECS),
# of them those whose packets carry A53 the decoder does not export (SCANNED_CODECS: HEVC's suffix SEI); one
# film, beside attached pictures (an image the container marks as an attached file, of no length of its own:
# COVER_CODECS); streams of captions of their own (CAPTION_CODECS, CAPTION_TAGS); the subtitle streams
# (SUBTITLE_CODECS) and data streams (DATA_CODECS, DATA_TAGS) that carry none; audio and attachments of any
# kind; and no picture of an analog capture's height, whose blanking lines keep line 21 (VBI_HEIGHTS). A file
# with anything off them is one whose captions and fields can't be told (off_list, in the analyzer's offList
# names: container:flv, stream:data:bin_data, video-streams:2, codec:vc1, picture:vbi-lines), and the gate
# keeps it.
CONTAINERS = ("matroska,webm", "mov,mp4,m4a,3gp,3g2,mj2", "mpegts", "mpeg", "avi")
VIDEO_CODECS = ("h264", "hevc", "mpeg1video", "mpeg2video", "av1")
SCANNED_CODECS = ("hevc",)
COVER_CODECS = ("mjpeg", "png", "bmp")
CAPTION_CODECS = ("eia_608",)
CAPTION_TAGS = ("c608", "c708")
SUBTITLE_CODECS = ("subrip", "srt", "ass", "ssa", "webvtt", "mov_text", "text", "microdvd", "hdmv_pgs_subtitle",
                   "dvd_subtitle", "dvb_subtitle", "dvb_teletext")
DATA_CODECS = ("scte_35", "timed_id3", "epg")
DATA_TAGS = ("tmcd",)
# And an MP4's chapter track (chapter_track), by this rule, the same words in the analyzer, the record logic and
# the check made before an original is retired: a data stream is the file's chapter track, and on the list, only where all of these hold in
# the stored probe: the container is the MP4 family (mov,mp4,m4a,3gp,3g2,mj2); the stream is codec_type data,
# codec_name bin_data, codec_tag_string text; the file has chapters; the stream's sample count (nb_frames)
# equals the number of chapters; and it is the file's only such stream. A probe that lacks the tag or the
# count leaves it off the list. A package loses no chapters by it: they come from the catalog's marks, the
# build hands them to the packager, and the version keeps them.
CHAPTER_CONTAINER = "mov,mp4,m4a,3gp,3g2,mj2"
CHAPTER_CODEC, CHAPTER_TAG = "bin_data", "text"
VBI_HEIGHTS = (486, 512, 608)
# The one word, of a picture's field order (ffprobe's field_order) and of a scan's found, that says a picture
# is whole frames, as it is written. Every other — tt, bb, tb, bt, unknown, a word not known here, the same
# word written otherwise, and no field order at all, which is how ffprobe's JSON gives an unknown one — says
# it may be in fields.
WHOLE_FRAMES = "progressive"
# What an original carries that a package can fail to carry, in the terms an original-deleted event accepts.
# 'chapters' is not here (the version keeps the marks, not the file); 'closedCaptions' and 'interlaced' are
# the gate's own rules, which no event accepts (deletion_gate).
LOSS_FLAGS = ("surround", "losslessAudio", "objectAudio", "hdr10Metadata", "dolbyVision", "stereo3d",
              "imageSubtitles", "styledSubtitles", "fonts", "closedCaptions")
LOSS_COUNTS = ("maxAudioChannels", "maxVideoHeight", "videoBitDepth", "subtitleTracks",
               "commentaryTracks", "commentarySubtitles", "audioDescriptionTracks")
LOSS_LANGUAGES = ("audioLanguages", "subtitleLanguages", "sdhSubtitleLanguages", "forcedSubtitleLanguages")


def gate_number(v):
    """v where it is a finite number (no boolean, no text, no NaN or infinity, no whole number too large for a
    float), as a float, else None: not stated."""
    if not isinstance(v, (int, float)) or isinstance(v, bool):
        return None
    try:
        v = float(v)
    except OverflowError:
        return None
    return v if v == v and abs(v) != float("inf") else None


def gate_digits(v):
    """A count as a probe states it - a whole number from 0, or the same written in digits alone - as an int, else
    None: "1e3", " 900", "+900", "0x384", "1_0", 9.0 and ["900"] are no counts."""
    if isinstance(v, int) and not isinstance(v, bool):
        return v if v >= 0 else None
    try:
        return int(v) if isinstance(v, str) and re.fullmatch(r"[0-9]+", v) else None
    except ValueError:  # more digits than Python reads as a number
        return None


def gate_mark(v):
    """Whether a disposition marks a stream: the number 1 or true, nothing else ("1", "0" or 2 are no mark)."""
    return v is True or type(v) is int and v == 1


def gate_count(v, top=None):
    """v where it is a whole number from 0 (to top, where given), else None."""
    if not isinstance(v, int) or isinstance(v, bool) or v < 0 or (top is not None and v > top):
        return None
    return v


def window_of(w):
    """One entry of a picture's or a read's caption windows as every reader reads it: (window, sound). sound: it
    is a window — an object whose at is a number from 0 to 1, start and end numbers, end not before start,
    frames a whole number from 0, captions a boolean (null too, of one that decoded no frame; never true of
    one), and interlacedFrames and repeatFrames, where it gives them, whole numbers from 0 to frames; a read of
    the whole picture (whole true) at 0, its packets (packets) a whole number from frames, its leading pictures
    (leading), where it gives them, a whole number from 0 to wholeLeadingMax; keyframeBefore, where
    it gives it, a boolean (the packets from its seek show no keyframe between its first frame and its place).
    window: the window as
    a record keeps it; of an entry that is no window, a failed one (frames 0, captions false, at its fraction
    where it has one), never left out."""
    d = w if isinstance(w, dict) else {}
    at, start, end = gate_number(d.get("at")), gate_number(d.get("start")), gate_number(d.get("end"))
    frames, captions = gate_count(d.get("frames")), d.get("captions")
    counts = {k: d[k] for k in ("interlacedFrames", "repeatFrames") if k in d}
    whole = {k: d[k] for k in ("whole", "packets", "leading") if k in d}
    keyed = {"keyframeBefore": True} if d.get("keyframeBefore") is True else {}
    sound = (isinstance(w, dict) and at is not None and 0 <= at <= 1 and start is not None and end is not None
             and end >= start and frames is not None
             and (isinstance(captions, bool) and not (frames == 0 and captions) or captions is None and frames == 0)
             and all(gate_count(v, frames) is not None for v in counts.values())
             and ("keyframeBefore" not in d or isinstance(d["keyframeBefore"], bool))
             and (not whole or whole.get("whole") is True and at == 0 and gate_count(whole.get("packets")) is not None
                  and whole["packets"] >= frames
                  and ("leading" not in whole or gate_count(whole["leading"], CAPTION_PLAN["wholeLeadingMax"]) is not None)))
    if not sound:
        a = at if at is not None and 0 <= at <= 1 else 0.0
        p = start if start is not None else 0.0
        return {"at": round(a, 3), "start": round(p, 3), "end": round(p, 3), "frames": 0, "captions": False}, False
    return ({"at": round(at, 3), "start": round(start, 3), "end": round(end, 3), "frames": frames,
             "captions": captions is True, **counts, **whole, **keyed}, True)


def caption_windows_of(windows):
    """The caption windows a manifest, a reading or a record gives, as a record keeps them (window_of), in their
    order, every entry: one that is no window is a failed one. None for anything that is no list."""
    return [window_of(w)[0] for w in windows] if isinstance(windows, list) else None


def windows_show(windows):
    """Whether a window of windows (as given) is one that decoded frames and shows captions."""
    return isinstance(windows, list) and any(s and w["captions"] for w, s in map(window_of, windows))


def gate_seconds(v):
    """A time or a length as ffprobe gives one ('12.345000') or a record keeps one (a number), as seconds, or
    None."""
    if isinstance(v, bool):
        return None
    try:
        t = float(v)
    except (TypeError, ValueError, OverflowError):
        return None
    return t if t == t and abs(t) != float("inf") else None


def frame_step_of(stream):
    """One over the frame rate of a probe's picture stream (the plan's frameStep), in seconds: its avg_frame_rate,
    or its r_frame_rate only where avg_frame_rate is exactly "0/0", each a ratio 'num/den' of digits alone (no
    space, sign or point). None - no plan - where the rate it takes is no such ratio, or below minRate frames a
    second (one a second, 0/1 among them: a slower one would stretch the tolerance past any window). An
    avg_frame_rate that is missing or no ratio gives none, and so does a slow one: r_frame_rate stands in for
    "0/0" alone."""
    ratio = lambda v: re.fullmatch(r"([0-9]+)/([0-9]+)", v) if isinstance(v, str) else None
    avg = (stream or {}).get("avg_frame_rate")
    m = ratio((stream or {}).get("r_frame_rate")) if avg == "0/0" else ratio(avg)
    try:
        num, den = (int(m.group(1)), int(m.group(2))) if m else (0, 0)
        step = den / num if num > 0 and den > 0 else None
    except (ValueError, OverflowError):  # digits past what a number holds, or a step too large for a float
        return None
    return step if step is not None and step <= 1 / CAPTION_PLAN["minRate"] else None


def plan_of(probe, duration_ms=None):
    """The plan's span of the picture of a stored probe, as the gate knows it without reading: {start, end, step},
    start its picture's start_time (the format's where the picture gives none), end that plus its duration -
    where the picture gives none, the record's container.durationMs (duration_ms), and the format's duration
    only where the record gives none -, step its frame step (frame_step_of); its picture the probe's film
    (film_of). None where the probe is no probe, has no film or more than one, or gives no start, length or
    frame rate: where the probe states no rate there is no plan,
    and no reader measures one of its own, which the gate could not check."""
    if not isinstance(probe, dict) or not isinstance(probe.get("streams"), list):
        return None
    fmt = probe.get("format") if isinstance(probe.get("format"), dict) else {}
    picture = film_of(probe)
    if picture is None:
        return None
    start = gate_seconds(picture.get("start_time"))
    if start is None:
        start = gate_seconds(fmt.get("start_time"))
    length = gate_seconds(picture.get("duration"))
    if length is None or length <= 0:
        ms = gate_number(duration_ms)
        length = ms / 1000 if ms is not None and ms > 0 else gate_seconds(fmt.get("duration"))
    step = frame_step_of(picture)
    if start is None or length is None or length <= 0 or step is None:
        return None
    return {"start": start, "end": start + length, "step": step}


def film_of(probe):
    """The film of a probe: its one video stream that is no cover by the closed lists' rule (attached_picture:
    the attachment marks, an image, no length of its own), wherever a cover lies among its streams; None where
    there is none, or where two streams could each be it."""
    films = [x for x in (probe or {}).get("streams") or [] if isinstance(x, dict) and x.get("codec_type") == "video"
             and not attached_picture(x)]
    return films[0] if len(films) == 1 else None


def caption_tolerance(plan):
    """How far a read's span may lie from the plan's, at either end, and a window's first frame after its place:
    the plan's endTolerance, a frame step and 0.5 s."""
    t = CAPTION_PLAN["endTolerance"]
    return t["frameSteps"] * plan["step"] + t["s"]


def window_lands(start, place, gop, plan, keyed=False):
    """Whether a window whose first frame is at start lies at its place (seconds) by the plan's slack
    (placeSlack): within max(10 s, gop + 0.5 s) of it, before or after; or up to 30 s before it where the packets
    from its seek show no keyframe between its first frame and its place (keyed, its keyframeBefore). Of a gop
    of 0 - fewer than two keyframes listed - only at its own place, within the tolerance (caption_tolerance)."""
    if not gop:
        return abs(start - place) <= caption_tolerance(plan)
    slack = CAPTION_PLAN["placeSlack"]
    return abs(start - place) <= max(slack["s"], gop + slack["gopPlus"]) \
        or keyed is True and place - slack["backS"] <= start <= place


def plan_places(plan):
    """Where the plan's windows lie on the plan's span (plan_of), in seconds, one for each of CAPTION_WINDOWS:
    the start at the start itself, never capped by the tail; each other at start + at x (end - start), at most
    CAPTION_TAIL_S short of the end and never before the start."""
    start, end = plan["start"], plan["end"]
    return [start if at == 0 else max(start, min(start + at * (end - start), end - CAPTION_TAIL_S))
            for at in CAPTION_WINDOWS]


def plan_short(plan, gop):
    """Whether the plan's windows can't be read on frames of their own: two of its places (plan_places) lie
    closer than a window reaches, its packets at the frame step from a keyframe up to gop before its place, and
    0.5 s (the plan's shortReach). Such a picture is read whole."""
    r = CAPTION_PLAN["shortReach"]
    reach = r["packets"] * plan["step"] + (gop if r["gop"] else 0.0) + r["s"]
    places = sorted(plan_places(plan))
    return any(b - a < reach for a, b in zip(places, places[1:]))


def whole_complete(w):
    """Whether a read of the whole picture (a window as window_of keeps it) is complete by the plan (wholeComplete):
    it decoded at least every packet but the stream's own leading pictures (leading: the packets before its first
    keyframe in decode order and the first gop's packets timed before it, at most wholeLeadingMax) and no more than
    its packets - a decoder that outputs the leading pictures too is complete as well. One that gives no leading is
    not complete."""
    leading = gate_count(w.get("leading"), CAPTION_PLAN["wholeLeadingMax"])
    return leading is not None and w["frames"] > 0 and w["packets"] - leading <= w["frames"] <= w["packets"]


def caption_read_of(picture):
    """A record's picture's caption read, as windows_tell_none reads one: its windows (captionWindows), the span
    the read measured (captionSpan) and the gop it found (captionGop)."""
    picture = picture if isinstance(picture, dict) else {}
    return {"windows": picture.get("captionWindows"), "span": picture.get("captionSpan"), "gop": picture.get("captionGop")}


def windows_tell_none(read, plan):
    """Whether a read (a record's picture's, caption_read_of, or a deletion's captionsRead: {windows, span, gop})
    tells that its picture carries no closed captions, by the plan (CAPTION_PLAN) over the plan's span the gate
    knows (plan_of): every entry of its windows is a window and none shows captions; the span it measured is the
    plan's within the tolerance at either end (caption_tolerance); its gop a number from 0 (0 where it gives
    none) to the run CAPTION_GOP_PACKETS packets take at the frame step; and its windows are the plan's six - one at each of CAPTION_WINDOWS, each of at least
    CAPTION_MIN_FRAMES frames (half its CAPTION_PACKETS packets, and 24 at least), its first frame at its place
    (plan_places) by the plan's slack (window_lands), their spans, in the order of their places, disjoint
    and rising, of a picture whose places are not too close (plan_short) -, or the one read of the whole picture
    that is complete (whole_complete: every packet decoded but the stream's own leading pictures), of packets
    enough at the frame step to cover the plan's span within the tolerance, over that span within the tolerance.
    Anything else - no plan, a failed or malformed window, one too few (a read of five, without the start), one
    of fewer frames than that, elsewhere than its place, two on the same frames, six of a picture too
    short for them, a span that is not the plan's, a whole read that missed a packet or gives no leading - is not
    known: of a gop of 0 none that overlap or don't start at their places."""
    if plan is None or not isinstance(read, dict):
        return False
    windows = read.get("windows")
    if not isinstance(windows, list) or not windows:
        return False
    checked = [window_of(w) for w in windows]
    if not all(s for _, s in checked):
        return False
    ws = sorted((w for w, _ in checked), key=lambda w: w["at"])
    if any(w["captions"] for w in ws):
        return False
    tol = caption_tolerance(plan)
    span = read.get("span") if isinstance(read.get("span"), dict) else {}
    first, last = gate_number(span.get("start")), gate_number(span.get("end"))
    if first is None or last is None or abs(first - plan["start"]) > tol or abs(last - plan["end"]) > tol:
        return False
    gop = 0.0 if read.get("gop") is None else gate_number(read.get("gop"))
    if gop is None or gop < 0 or gop > CAPTION_GOP_PACKETS * plan["step"]:
        return False
    if any(w.get("whole") for w in ws):
        w = ws[0]
        return (len(ws) == 1 and whole_complete(w) and w["packets"] * plan["step"] >= plan["end"] - plan["start"] - tol
                and abs(w["start"] - plan["start"]) <= tol and abs(w["end"] - plan["end"]) <= tol)
    if plan_short(plan, gop) or len(ws) != len(CAPTION_WINDOWS):
        return False
    for w, at, place in zip(ws, CAPTION_WINDOWS, plan_places(plan)):
        if abs(w["at"] - at) > 1e-3 or w["frames"] < CAPTION_MIN_FRAMES \
                or not window_lands(w["start"], place, gop, plan, w.get("keyframeBefore")):
            return False
    return all(b["start"] >= a["end"] and b["start"] > a["start"] for a, b in zip(ws, ws[1:]))


def windows_count_fields(windows):
    """Whether windows (as given) put their picture in fields: a window counted interlaced frames, or says a
    count it can't be read for (no whole number from 0 to its frames, or one of an entry that is no window);
    one that is no list, of a read that has no windows, too."""
    if not isinstance(windows, list):
        return True
    for w in windows:
        if not isinstance(w, dict):
            continue
        frames = gate_count(w.get("frames"))
        for key in ("interlacedFrames", "repeatFrames"):
            if key in w and gate_count(w[key], frames) is None:
                return True
        if gate_count(w.get("interlacedFrames"), frames):
            return True
    return False


def picture_of(streams):
    """The picture of a record's streams: its first video stream that is no attached picture, or None."""
    return next((x for x in streams or [] if isinstance(x, dict) and x.get("type") == "video"
                 and not (x.get("dispositions") or {}).get("attachedPic")), None)


def captions_known(streams, off, plan):
    """Whether a source's essence knows its closed captions, of its record's streams, what of its file is off
    the closed lists (off) and the plan's span of its stored probe (plan_of): a stream that is closed captions
    of its own, or its picture's caption signature, says them; or its picture's caption windows show them; or
    its picture's read tells none (windows_tell_none, by the plan), of a file on the lists (off empty), and of a
    picture whose packets carry A53 its decoder does not export (SCANNED_CODECS) only where every packet of it
    was read and none carries a caption signature."""
    picture = picture_of(streams) or {}
    if any(x.get("closedCaptions") is True for x in streams or [] if isinstance(x, dict)
           and x.get("type") in ("subtitle", "data")) or picture.get("captionSignature") is True:
        return True
    if windows_show(picture.get("captionWindows")):
        return True
    return (windows_tell_none(caption_read_of(picture), plan) and not off
            and (picture.get("codec") not in SCANNED_CODECS or picture.get("captionSignature") is False))


def field_order_word(order):
    """A field order as in_fields reads it: as written, and unknown where none is given."""
    return order if isinstance(order, str) and order else "unknown"


def read_through(verified):
    """Whether a scan's read-through of the whole title (verified) bore out whole frames and read all of it: ok
    and complete true, and its counts adding up - it read every packet of the stream but its leading pictures,
    frames = packets - leading, no other slack. One without leading (a scan from before that rule), or without
    packets, is never complete."""
    if not isinstance(verified, dict) or verified.get("ok") is not True or verified.get("complete") is not True:
        return False
    frames, packets, leading = (gate_count(verified.get(k)) for k in ("frames", "packets", "leading"))
    return None not in (frames, packets, leading) and frames == packets - leading


def scan_holds(pkg):
    """Why the scan of the package record pkg keeps its title's originals in fields, or None where it says
    nothing (a package without a scan) or lets them go the ordinary way: one whose found is exactly progressive,
    whose read-through of the whole title is ok and complete (read_through), whose done is progressive or none,
    and whose own counts don't say otherwise: its read-through counted no chunk in fields (chunksInFields), and
    its examination (idet), where it gives one, no more top- and bottom-field pictures than whole frames. A record
    that is no object, a scan that is no object, any other found (unknown, blank, not text, a word not known here,
    progressive without its read-through, or with counts that say fields, or counts that can't be read) and any
    other done keep them."""
    if not isinstance(pkg, dict):
        return "a package record of the title can't be read"
    scan = pkg.get("scan")
    if scan is None:
        return None
    if not isinstance(scan, dict):
        return "the scan of a package of the title is no object"
    found, done, verified = scan.get("found"), scan.get("done"), scan.get("verified")
    if found != WHOLE_FRAMES:
        return f"the scan of a package of the title found {found!r}"
    if not read_through(verified):
        return "the scan of a package of the title found progressive without its whole title read through"
    if done not in ("progressive", "none"):
        return f"the scan of a package of the title says {done!r} was done"
    fields_seen = gate_count(verified.get("chunksInFields", 0))
    idet = scan.get("idet")
    if fields_seen is None or fields_seen > 0:
        return "the scan of a package of the title found progressive, but its read-through counted chunks in fields"
    if idet is not None:
        counts = {k: gate_count(idet.get(k, 0)) if isinstance(idet, dict) else None
                  for k in ("tff", "bff", "progressive")}
        if None in counts.values() or counts["tff"] + counts["bff"] > counts["progressive"]:
            return ("the scan of a package of the title found progressive, but its examination counted more fields "
                    "than frames")
    return None


def in_fields(sources, packages=(), probes=(), reads=()):
    """Why the originals of a version are in fields as the deletion gate reads it, widely, or None where nothing
    says so. sources are their source records, probes the probes kept of them (ffprobe.json), packages the
    package records of every version of the title, of other sources too, and reads the reads their deletion
    events carry (captionsRead). In fields: a record marked so (its essence's interlaced); a picture of a record,
    or a video stream of a probe that is no cover by the closed lists' rule (attached_picture), whose field
    order is not exactly progressive (tt, bb, tb, bt, unknown, a word written
    otherwise, none given, since ffprobe's JSON leaves an unknown one out); a caption window of a picture or of
    a read that counted interlaced frames, or a count that can't be read (windows_count_fields); a read that is
    no object or has no windows; or the scan of any package of the title that doesn't let it go (scan_holds).
    A package without a scan says nothing. No word of what a scan says was done lets an original go."""
    for s in sources:
        if not isinstance(s, dict):
            return "a source record of it can't be read"
        if (s.get("essence") or {}).get("interlaced") is True:
            return "its record is marked interlaced"
        for x in s.get("streams") or []:
            if isinstance(x, dict) and x.get("type") == "video" and not (x.get("dispositions") or {}).get("attachedPic"):
                if field_order_word(x.get("fieldOrder")) != WHOLE_FRAMES:
                    return f"its record's field order is {field_order_word(x.get('fieldOrder'))!r}"
                if "captionWindows" in x and windows_count_fields(x.get("captionWindows")):
                    return "a caption window of its picture counted interlaced frames, or a count it can't be read for"
    for p in probes:
        for x in (p.get("streams") if isinstance(p, dict) and isinstance(p.get("streams"), list) else []):
            if isinstance(x, dict) and x.get("codec_type") == "video" and not attached_picture(x):
                if field_order_word(x.get("field_order")) != WHOLE_FRAMES:
                    return f"its probe's field order is {field_order_word(x.get('field_order'))!r}"
    for r in reads:
        if not isinstance(r, dict) or windows_count_fields(r.get("windows")):
            return "the read its deletion carries counted interlaced frames, or can't be read"
    for pkg in packages:
        why = scan_holds(pkg)
        if why:
            return why
    return None


def caption_stream(s):
    """Whether the probe's stream s is closed captions of its own (CAPTION_CODECS, CAPTION_TAGS), whatever its
    kind."""
    return str(s.get("codec_name") or "").lower() in CAPTION_CODECS \
        or str(s.get("codec_tag_string") or "").lower() in CAPTION_TAGS


def attached_picture(st):
    """Whether a video stream of a probe is a cover, a still picture the container holds as an attached file, as the
    analyzer reads it - all of these: marked as one (the attached_pic disposition, the number 1 or true:
    gate_mark; or Matroska's filename and mimetype tags, both); an image of a cover's codec (COVER_CODECS: JPEG,
    PNG, BMP), its mimetype, where it has one, image/*, and without one the attached_pic disposition (an MP4
    cover); and of no length of its own: every count of its frames it is given (nb_frames, a tag that starts
    NUMBER_OF_FRAMES) a whole number in digits alone (gate_digits) and none above one - a count that is there but
    can't be read is no cover's -, and no tag that starts DURATION (the duration ffprobe gives a stream is the
    file's where the container gives none, so it is no mark). A stream of any other codec - a webp image among
    them - is a video stream, whatever its marks say. Tags are read without case; two keys that differ only by it
    and disagree are there, and make it no image, so no cover."""
    raw = st.get("tags") if isinstance(st.get("tags"), dict) else {}
    tags = {str(k).lower(): v for k, v in raw.items()}
    clash = any(tags[str(k).lower()] != v for k, v in raw.items())
    pic = gate_mark((st.get("disposition") if isinstance(st.get("disposition"), dict) else {}).get("attached_pic"))
    marked = pic or ("filename" in tags and "mimetype" in tags)
    mime = str(tags.get("mimetype") or "").strip().lower()
    image = not clash and st.get("codec_name") in COVER_CODECS \
        and (mime.startswith("image/") if "mimetype" in tags else pic)
    counts = ([st["nb_frames"]] if "nb_frames" in st else []) + [v for k, v in tags.items()
                                                               if k.startswith("number_of_frames")]
    one_frame = all(n is not None and n <= 1 for n in map(gate_digits, counts))
    length = any(k.startswith("duration") for k in tags)
    return marked and image and one_frame and not length


def chapter_stream(st):
    """codec_type data, codec_name bin_data, codec_tag_string text, each exactly so."""
    return isinstance(st, dict) and st.get("codec_type") == "data" and st.get("codec_name") == CHAPTER_CODEC \
        and st.get("codec_tag_string") == CHAPTER_TAG


def chapter_track(st, probe):
    """Whether a data stream of a probe is the file's chapter track, and on the list: only where all of these
    hold in the stored probe: the container is the MP4 family (mov,mp4,m4a,3gp,3g2,mj2); the stream is
    codec_type data, codec_name bin_data, codec_tag_string text; the file has chapters; the stream's sample
    count (nb_frames, in digits alone) equals the number of chapters; and it is the file's only such stream. A
    probe that lacks the tag or the count, or gives either otherwise (TEXT, " 3", "+3"), leaves it off the list."""
    fmt = probe.get("format") if isinstance(probe.get("format"), dict) else {}
    chapters = probe.get("chapters")
    marks = len(chapters) if isinstance(chapters, list) else 0
    counted = gate_digits(st.get("nb_frames")) == marks
    return fmt.get("format_name") == CHAPTER_CONTAINER and chapter_stream(st) and marks > 0 and counted \
        and sum(1 for x in probe.get("streams") or [] if chapter_stream(x)) == 1


def probe_shape(probe):
    """Why the probe kept of a source (ffprobe.json) can't be read as one, or None: it is no object, its format
    is no object, or its streams no list of objects, or none."""
    if not isinstance(probe, dict):
        return "its probe can't be read"
    if not isinstance(probe.get("format"), dict) or not isinstance(probe.get("streams"), list) \
            or not probe["streams"] or not all(isinstance(x, dict) for x in probe["streams"]):
        return "its probe is no probe: no format, or no list of streams"
    return None


def off_list(probe):
    """What of the file a probe describes is off the closed lists of how captions and fields are carried, in the
    analyzer's offList names and in the order the probe gives them: its container (container:<format_name>),
    each stream (stream:<codec_type>:<codec, else its tag, else none>), the count of its films
    (video-streams:<n>), each film's codec (codec:<codec_name>), its picture (picture:vbi-lines). Empty for a
    file on them."""
    fmt = probe.get("format") if isinstance((probe or {}).get("format"), dict) else {}
    off, films = [], []
    name = str(fmt.get("format_name") or "none")
    if name not in CONTAINERS:
        off.append(f"container:{name}")
    for st in (probe or {}).get("streams") or []:
        if not isinstance(st, dict):
            continue
        kind, codec = str(st.get("codec_type") or "none"), str(st.get("codec_name") or "none")
        tag = str(st.get("codec_tag_string") or "").lower()
        if codec == "unknown":
            codec = "none"
        what = codec if codec != "none" or not tag or tag.startswith("[") else tag
        if codec in CAPTION_CODECS or tag in CAPTION_TAGS:
            continue
        if kind == "video":
            if not attached_picture(st):
                films.append(st)
        elif kind in ("audio", "attachment") or (kind == "subtitle" and codec in SUBTITLE_CODECS) \
                or (kind == "data" and (codec in DATA_CODECS or tag in DATA_TAGS or chapter_track(st, probe))):
            continue
        else:
            off.append(f"stream:{kind}:{what}")
    if len(films) > 1:
        off.append(f"video-streams:{len(films)}")
    for st in films:
        codec = str(st.get("codec_name") or "none")
        if codec not in VIDEO_CODECS and f"codec:{codec}" not in off:
            off.append(f"codec:{codec}")
    if any(st.get("height") in VBI_HEIGHTS for st in films):
        off.append("picture:vbi-lines")
    return off


def record_off_list(record):
    """What of the file a source record describes is off the closed lists: its offList, where it keeps one (a
    list of words; one of another shape can't be read: off), else as its container and streams say it — a data
    stream's tag, which a record does not keep, is no tag, and a video stream an attached picture by its
    disposition alone —, of a record written before record logic 11."""
    if "offList" in (record or {}):
        off = record["offList"]
        return list(off) if isinstance(off, list) and all(isinstance(x, str) and x for x in off) \
            else ["offList:unreadable"]
    name = (((record or {}).get("container") or {}).get("format")) or "none"
    off = [] if name in CONTAINERS else [f"container:{name}"]
    films = []
    for x in (record or {}).get("streams") or []:
        if not isinstance(x, dict):
            off.append("stream:unreadable")
            continue
        kind, codec = x.get("type") or "none", x.get("codec") or "none"
        if codec == "unknown":
            codec = "none"
        if codec in CAPTION_CODECS or (kind in ("subtitle", "data") and x.get("closedCaptions")):
            continue
        if kind == "video":
            if not (x.get("dispositions") or {}).get("attachedPic"):
                films.append(x)
        elif kind in ("audio", "attachment") or (kind == "subtitle" and codec in SUBTITLE_CODECS) \
                or (kind == "data" and codec in DATA_CODECS):
            continue
        else:
            off.append(f"stream:{kind}:{codec}")
    if len(films) > 1:
        off.append(f"video-streams:{len(films)}")
    for x in films:
        if (x.get("codec") or "none") not in VIDEO_CODECS and f"codec:{x.get('codec') or 'none'}" not in off:
            off.append(f"codec:{x.get('codec') or 'none'}")
    if any(x.get("height") in VBI_HEIGHTS for x in films):
        off.append("picture:vbi-lines")
    return off


def gate_holds(sources, packages=(), probes=(), reads=()):
    """What the deletion gate keeps the originals of a version for that their essences may not say, read from
    their records, {key: why}. sources are their source records, one for each, or None where it can't be read;
    probes the probes kept of them, one for each, in their order, None where there is none or it can't be read
    (the plan's span of a record's picture is its probe's: plan_of); packages and
    reads as in_fields'. Both keys, closedCaptions and interlaced: no source; a record that can't be read, or
    whose streams are no list; a probe that can't be read as one (probe_shape); a probe that gives no one film
    (film_of), or whose film is not its record's picture; a record or a probe off the closed lists
    (record_off_list, off_list); a read that is no object or has no list of windows. interlaced: in
    fields (in_fields). closedCaptions: a stream of a record or a probe that is closed captions of its own
    (caption_stream), as of records written before they said so; a record's picture that says it carries them
    (a caption signature found, or windows that show them), whatever its essence says; an essence that says it
    knows its captions where its record's streams and its probe's plan don't bear that out (captions_known), as
    no record the
    record logic writes does; a picture - the record's, or its probe's film - whose packets carry A53 its decoder
    does not export (SCANNED_CODECS) and whose packets were not read whole with none carrying a caption
    signature (captionSignature not false)."""
    out = {}

    def both(why):
        out.setdefault("closedCaptions", why)
        out.setdefault("interlaced", why)

    if not sources:
        both("no source of it is known")
    for s in sources:
        if not isinstance(s, dict):
            both("a source record of it can't be read")
    good = [s for s in sources if isinstance(s, dict)]
    why = in_fields(good, packages, [p for p in probes if isinstance(p, dict)], reads)
    if why:
        out.setdefault("interlaced", why)
    probes = list(probes)
    probe_of = {id(s): probes[i] if i < len(probes) else None for i, s in enumerate(sources)}
    for s in good:
        streams = s.get("streams")
        if not isinstance(streams, list):
            both("its record's streams can't be read")
            streams = []
        if any(isinstance(x, dict) and x.get("type") in ("subtitle", "data")
               and (x.get("closedCaptions") or str(x.get("codec") or "").lower() in CAPTION_CODECS)
               for x in streams):
            out.setdefault("closedCaptions", "a stream of its record is closed captions of its own")
        picture = picture_of(streams) or {}
        if picture.get("codec") in SCANNED_CODECS and picture.get("captionSignature") is not False:
            out.setdefault("closedCaptions", f"its picture is {picture.get('codec')}, whose packets were not read "
                                             f"whole for a caption signature")
        if picture.get("captionSignature") is True or windows_show(picture.get("captionWindows")):
            out.setdefault("closedCaptions", "its record's picture carries closed captions")
        off = record_off_list(s)
        if off:
            both("off the closed lists: " + ", ".join(off))
        probe = probe_of.get(id(s))
        if probe_shape(probe) is None:
            film = film_of(probe)
            if film is None:
                both("its probe gives no one film: none, or two streams that could each be it")
            elif not picture or picture.get("index") != film.get("index"):
                both("its record's picture is not its probe's film")
            elif film.get("codec_name") in SCANNED_CODECS and picture.get("captionSignature") is not False:
                out.setdefault("closedCaptions", f"its probe's film is {film.get('codec_name')}, whose packets were "
                                                 f"not read whole for a caption signature")
        essence = s.get("essence") if isinstance(s.get("essence"), dict) else {}
        plan = plan_of(probe, (s.get("container") if isinstance(s.get("container"), dict) else {}).get("durationMs"))
        if essence.get("closedCaptionsKnown") is True and not captions_known(streams, off, plan):
            out.setdefault("closedCaptions", "its essence says it knows its closed captions, which its record's "
                                             "streams don't bear out")
    for p in probes:
        why = probe_shape(p)
        if why:
            both(why)
            continue
        if any(caption_stream(x) for x in p["streams"]):
            out.setdefault("closedCaptions", "a stream of its probe is closed captions of its own")
        off = off_list(p)
        if off:
            both("off the closed lists: " + ", ".join(off))
    for r in reads:
        if not isinstance(r, dict) or not isinstance(r.get("windows"), list):
            both("the read its deletion carries can't be read")
    return out


def essence_as_read(essence, reads, plan):
    """The essence of a source as the reads its deletion events carry (captionsRead: {windows, span, gop, at};
    one, or a list of them) tell it over the plan's span of its stored probe (plan_of), the worst deciding:
    where one of them shows captions, it carries them, known; where every one tells none (windows_tell_none, by
    the plan), it knows its captions, carried where its record says so; where one counted interlaced frames, or
    a count of it can't be read, its picture is in fields. Any other read tells nothing more: the essence as its
    record says it. Whether the file is on the closed lists is its record's and its probe's to say
    (gate_holds), not a read's."""
    reads = [reads] if isinstance(reads, dict) else list(reads or [])
    if not reads:
        return essence
    out = dict(essence)
    windows = [r.get("windows") if isinstance(r, dict) else None for r in reads]
    if any(windows_show(w) for w in windows):
        out.update(closedCaptions=True, closedCaptionsKnown=True)
    elif all(windows_tell_none(r, plan) for r in reads):
        out.update(closedCaptions=essence.get("closedCaptions") is not False, closedCaptionsKnown=True)
    if any(windows_count_fields(w) for w in windows):
        out["interlaced"] = True
    return out


def deletion_gate(sources, package, held):
    """What deleting a version's originals costs: the essence of its sources minus that of its package, sorted.
    Nothing records it, so every reader computes it the same way, and an original-deleted event accepts no more
    than it - never closedCaptions or interlaced, which keep the original (ADR-0015).

    sources are the essences of the version's sources (as their deletions' reads tell them: essence_as_read),
    package its package's, and held what gate_holds says of their records, each of its keys lost: required,
    as a reader that has only the essences reads neither the lists, the probes, the packages' scans nor the
    reads. An original whose closed captions are carried, or not known (closedCaptions not false, or
    closedCaptionsKnown not true), is kept - closedCaptions - whatever its package carries; an original in fields
    (interlaced not false) is kept - interlaced - whatever was done to make its package progressive. No source
    at all keeps both."""
    lost = set()
    if not sources:
        lost |= {"closedCaptions", "interlaced"}
    for key in LOSS_FLAGS:
        if any(s.get(key) for s in sources) and not package.get(key):
            lost.add(key)
    if any(s.get("closedCaptions") is not False or s.get("closedCaptionsKnown") is not True for s in sources):
        lost.add("closedCaptions")
    if any(s.get("interlaced") is not False for s in sources):
        lost.add("interlaced")
    for key in LOSS_COUNTS:
        had = [s[key] for s in sources if s.get(key) is not None]
        kept = package.get(key)
        if had and (kept is None or kept < max(had)):
            lost.add(key)
    for key in LOSS_LANGUAGES:
        kept = set(package.get(key) or [])
        lost |= {f"{key}:{x}" for s in sources for x in s.get(key) or [] if x not in kept}
    lost |= set(held)
    return sorted(lost)
# ==== the deletion gate, shared: end ====


# How long each read of a picture may take, in seconds: the plan's (CAPTION_BOUNDS, timeoutS).
CAPTION_TIMEOUT = CAPTION_BOUNDS["timeoutS"]


# ---------------------------------------------------------------- ffprobe
def have_ffprobe():
    return shutil.which("ffprobe") is not None


def ffprobe(path):
    """The verbatim probe of a file — ffprobe -show_format -show_streams -show_chapters, as JSON — or
    None. Reads headers only, so it is cheap even over NFS."""
    try:
        r = subprocess.run(["ffprobe", *FFPROBE_ARGS, path], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                           timeout=300)
        return json.loads(r.stdout) if r.returncode == 0 and r.stdout else None
    except (OSError, ValueError, subprocess.SubprocessError):
        return None


def ffprobe_version():
    try:
        r = subprocess.run(["ffprobe", "-version"], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=30)
        return text(r.stdout.decode("utf-8", "replace").splitlines()[0]) if r.returncode == 0 else None
    except (OSError, IndexError, subprocess.SubprocessError):
        return None


def scrub_probe(probe: dict, library_name: str) -> dict:
    """The probe as a source folder keeps it, ffprobe.json: a copy of the tool's output with two edits
    and nothing else changed — format.filename is library_name, the name the library gives the
    original, not the path the tool read, and every key of format.tags that is title, in any case, is
    gone. Both could tell where the file came from. The probe passed in is left as it was."""
    out = copy.deepcopy(probe)
    fmt = out.get("format") if isinstance(out, dict) else None
    if isinstance(fmt, dict):
        fmt["filename"] = library_name
        tags = fmt.get("tags")
        if isinstance(tags, dict):
            for key in [k for k in tags if str(k).lower() == "title"]:
                del tags[key]
    return out


def fidelity(streams, container):
    """Whether a file was untouched or already a re-encode, with what says so."""
    v = next((x for x in streams if x["type"] == "video" and not x["dispositions"].get("attachedPic")), None)
    evidence = []
    if v and v.get("encoder"):
        evidence.append({"signal": "encoder-tag", "value": v["encoder"], "weight": 0.9})
    for key in ("writingApp", "muxingApp"):
        if container.get(key) and RE_ENCODE.search(container[key]):
            evidence.append({"signal": "container-title", "value": container[key], "weight": 0.5,
                             "note": key})
    for a in [x for x in streams if x["type"] == "audio"]:
        if (a.get("titleClaim") or {}).get("contradictsActual"):
            evidence.append({"signal": "codec-vs-title", "value": a.get("title"), "weight": 0.8})
    fingerprint = None
    if v:
        hdr = (v.get("hdr") or {}).get("format") or "sdr"
        fingerprint = "/".join(str(x) for x in [v.get("codec") or "?", (v.get("profile") or "?").lower(),
                                                f"{v.get('bitDepth') or '?'}bit", hdr,
                                                f"{v.get('width') or '?'}x{v.get('height') or '?'}"])
    return {"class": "derivative" if evidence else "unknown", "fingerprint": fingerprint, "evidence": evidence}


def probed(probe, heads=None, captions=None, signature=None):
    """What a probe found in a file, in the terms a source record and an extra share: its container,
    every stream, whether it was already a re-encode, its essence, and what of it is off the closed lists
    of how captions and fields are carried (offList, which a source record keeps). The container's title tag is
    not among them, in any case: it routinely names where the file came from. heads are the first bytes
    of the attachments the probe gives no role by itself, by stream index (attachment_heads),
    captions a picture's caption read, by stream index (caption_windows), and signature whether its
    packets carry a caption signature, by stream index (caption_signature)."""
    fmt = probe.get("format") or {}
    tags = {k: text(v) for k, v in (fmt.get("tags") or {}).items() if text(v) and str(k).lower() != "title"}
    streams = [norm_stream(s, (heads or {}).get(s.get("index")), (captions or {}).get(s.get("index")),
                           (signature or {}).get(s.get("index"))) for s in probe.get("streams") or []]
    infer_forced_by_size(streams)
    container = {"format": fmt.get("format_name") or "", "durationMs": record_duration_ms(probe),
                 "bitrate": num(fmt.get("bit_rate")), "title": None,
                 "muxingApp": tags.get("muxing_application") or tags.get("encoder"),
                 "writingApp": tags.get("writing_application"),
                 "creationTime": tags.get("creation_time"), "tags": tags}
    off = off_list(probe)
    essence = source_essence(streams, probe.get("chapters") or [], off, plan_of(probe, container["durationMs"]))
    picture = picture_stream(probe)
    read = (captions or {}).get(picture["index"]) if picture is not None else None
    if picture is not None and picture["index"] in (captions or {}) \
            and windows_count_fields(read.get("windows") if isinstance(read, dict) else read):
        essence["interlaced"] = True
    return {"container": container, "streams": streams, "fidelity": fidelity(streams, container),
            "essence": essence, "offList": off}


def probe_chapters(probe):
    """The chapter marks a probe found in the file itself, on its timeline."""
    return [{"startMs": int(float(c.get("start_time", 0)) * 1000), "endMs": int(float(c.get("end_time", 0)) * 1000),
             "title": text((c.get("tags") or {}).get("title"))} for c in (probe or {}).get("chapters") or []]


# ---------------------------------------------------------------- the catalog's own words
def external_ids(entries, item_type):
    """The reference ids of an item from the catalog's [{source, externalId}], keyed as item.json and
    metadata.json key them. Returns (ids, notes): a source the format has no field for, and a TMDB id
    that is not a number, are dropped with a note, never renamed or guessed."""
    ids, notes = {}, []
    for e in entries or []:
        src, val = (e.get("source") or "").lower(), text(e.get("externalId"))
        if not val:
            continue
        if src in ("tmdb", "themoviedb"):
            ids["tmdbMovie" if item_type == "movie" else "tmdbTv"] = val
        elif src in ("tmdb-episode", "tmdbepisode"):
            if item_type == "episode":
                ids["tmdbEpisode"] = val
        elif src in ("tmdb-season", "tmdbseason"):
            if item_type != "movie":
                ids["tmdbSeason"] = val
        elif src == "imdb" and re.fullmatch(r"tt[0-9]+", val):
            ids["imdb"] = val
        elif src == "tvdb" and re.fullmatch(r"[0-9]+", val):
            ids["tvdb"] = val
        else:
            notes.append(f"external id source {src!r} has no v2 field, dropped")
    for key in ("tmdbMovie", "tmdbTv", "tmdbSeason", "tmdbEpisode"):
        if key in ids and not re.fullmatch(r"[0-9]+", ids[key]):
            notes.append(f"{key} {ids[key]!r} is not a number, dropped")
            del ids[key]
    return ids, notes


def chapter_marks(rows):
    """The catalog's chapter rows ({startMs, endMs, title, ordinal}) as marks on the version's
    timeline, in their order; a row without a start is no mark."""
    chapters = [c for c in (rows or []) if c.get("startMs") is not None]
    return [{"startMs": int(c["startMs"]), "endMs": int(c.get("endMs") or c["startMs"]), "title": text(c.get("title"))}
            for c in sorted(chapters, key=lambda c: c.get("ordinal") or 0)]


def segments(rows):
    """The catalog's detected ranges ({kind, startMs, endMs, source, confidence, label}) as a version
    keeps them: the detector that found each, and a kind the format does not know as 'other', labelled
    with the catalog's own kind."""
    out = []
    for s in rows or []:
        kind = (s.get("kind") or "other").lower()
        known = kind in SEGMENT_KINDS
        out.append({"kind": kind if known else "other", "startMs": int(s["startMs"]),
                    "endMs": int(s.get("endMs") or s["startMs"]), "detector": text(s.get("source")),
                    "confidence": s["confidence"] if isinstance(s.get("confidence"), (int, float)) else None,
                    "label": text(s.get("label")) if known else (text(kind) or text(s.get("label")))})
    return out


# ---------------------------------------------------------------- sources/<sourceId>/
def sidecar_language(raw):
    """A sidecar's language as the catalog's row has it, when the format can hold that code, and its
    BCP 47 code otherwise; None when the row names none."""
    r = str(raw or "").strip().lower()
    if not r:
        return None
    return r if LANGUAGE_RE.match(r) else lang(r)[0]


def sidecar_entry(source_id, n, path, arrival_name, language=None, forced=False):
    """One sidecars[] entry of a source record: the copy of the nth subtitle file that came with the
    original (n from 1, in the order the catalog lists them), kept as sources/<sourceId>/<the name
    subtitle_copy_name() gives it> — path is that copy, or the file it is taken from, the same bytes —
    with its size and hash, its language as the catalog's row has it, whether it is forced or for the
    hard of hearing, as the name it arrived under or the catalog says (".forced." / ".sdh.", ".cc."
    in the name), and so what it is for. That name is read for what it says, and kept nowhere: the
    entry's file is where the copy goes."""
    low = arrival_name.lower()
    forced = bool(forced) or ".forced." in low
    hearing = bool(re.search(r"\.(sdh|cc)\.", low))
    ext = os.path.splitext(arrival_name)[1]
    return {"file": f"sources/{source_id}/{subtitle_copy_name(n, language, forced, hearing, ext)}",
            "kind": "subtitle", "format": ext.lower().lstrip(".") or None, "language": sidecar_language(language),
            "forced": forced, "hearingImpaired": hearing, "purpose": "forced" if forced else "sdh" if hearing else "dialogue",
            "sizeBytes": os.path.getsize(path), "sha256": sha_file(path)}


def source_record(source_id, name, size_bytes, *, taken_at, taken_by, library_path, qh1=None, mtime=None,
                  probe=None, probe_version=None, sidecars=(), note=None, covers=(), heads=None,
                  captions=None, signature=None):
    """sources/<sourceId>/source.json for one original, and the bytes of the ffprobe.json written
    beside it (None without a probe). Returns (record, probe bytes).

    name is the name the original arrived under, and library_path where it sat, relative to the
    arrivals it came through: the record reads what they claim — its labels, its numbering — and
    keeps neither, nor where the file came from; file.name is the name the library gives it,
    library_original_name(name). probe is the tool's output (ffprobe()), whose facts — container,
    streams, fidelity, essence — the record carries, and which is kept beside it with the two edits
    scrub_probe() makes. Without one the record carries the file's size and fixity alone, and
    probe.note says why. Without qh1 — an original already gone, so nothing could fingerprint it —
    the record has no fixity, which only a note that says why may excuse.

    covers, for a file that holds several episodes, is the item ids of the episodes it holds in
    episode order, the holder first — the episode whose folder keeps this source — as the database
    linked them; empty, as it is by default, for a file of one episode. It is written as given: the
    record logic does not read it from the name, whose numbering is only what the release claimed.

    heads are the first bytes of the attachments the probe gives no role by itself, by stream index, as
    attachment_heads() reads them from the original: an attachment that is a font by its signature alone
    is one in the record, and its stream says so (signature).

    captions are the original's picture's caption read, by stream index, as caption_windows() reads it, by
    the plan: the stream keeps its windows (captionWindows), span (captionSpan) and gop (captionGop), and the
    essence knows its captions where they tell them (closedCaptionsKnown). Without them, as the probe alone
    says them — only one that decoded a frame says the stream carries them — the record can't tell none from
    not seen.
    signature is whether the picture's packets carry a caption signature, by stream index, as
    caption_signature() reads every one of them: where they do, the picture carries closed captions,
    known."""
    if qh1 is None and not text(note):
        raise ValueError("a source record without the original's fixity needs a note that says why it has none")
    covers = [str(c) for c in covers or ()]
    if covers and (len(set(covers)) != len(covers) or not all(UUID_RE.match(c) for c in covers)):
        raise ValueError(f"a file covers episodes by their item ids, each once: {covers!r}")
    folder = os.path.dirname(library_path.replace("\\", "/"))
    file = {"name": library_original_name(name), "kind": "stream-container", "sizeBytes": size_bytes}
    if mtime:
        file["mtime"] = mtime
    if qh1:
        file["fixity"] = {"qh1": qh1}
    rec = {"schema": "zaentrum.library.source/2", "sourceId": source_id, "takenAt": taken_at, "takenBy": taken_by,
           "file": file}
    named = naming(name)
    if named:
        rec["naming"] = named
    rec["labels"] = labels(name, folder)
    raw = None
    if probe:
        facts = probed(probe, heads, captions, signature)
        raw = json_bytes(scrub_probe(probe, file["name"]))
        rec.update(container=facts["container"], offList=facts["offList"], fidelity=facts["fidelity"],
                   streams=facts["streams"],
                   sidecars=list(sidecars), covers=covers, essence=facts["essence"],
                   probe={"tool": "ffprobe", "version": probe_version, "at": taken_at,
                          "file": f"sources/{source_id}/ffprobe.json", "sha256": sha_bytes(raw), "note": text(note)})
    else:
        rec.update(container={"format": "", "durationMs": None, "bitrate": None, "title": None, "muxingApp": None,
                              "writingApp": None, "creationTime": None, "tags": {}},
                   fidelity={"class": "unknown", "fingerprint": None, "evidence": []}, streams=[],
                   sidecars=list(sidecars), covers=covers, essence={},
                   probe={"tool": "ffprobe", "version": None, "at": None, "file": None, "sha256": None,
                          "note": text(note) or "ffprobe was not available where this record was written; the "
                                                "original's streams, fidelity and essence are unknown"})
    return rec, raw


# ---------------------------------------------------------------- versions/<versionId>/version.json
def version_record(version_id, source, *, created_at, created_by, chapters=(), chapters_from=None, segments=(),
                   original_files=()):
    """versions/<versionId>/version.json for a version made from source, a source record: its edition as
    the name the file arrived under and its folder claimed — the source's labels say what they did —
    its presentation and runtime as the probe found them, the marks it keeps, and the source it was
    made from. original_files names the originals the version keeps in its folder: the source's
    file.name when the version is established with its original, which goes in beside the package, or
    taken in before anything packaged it; none for a re-package, whose original stays in the version
    that holds it."""
    streams = source.get("streams") or []
    v0 = next((x for x in streams if x["type"] == "video" and not x["dispositions"].get("attachedPic")), None)
    claims = source.get("labels") or {}
    folder_edition = claims.get("folderEdition")
    # what the name the file arrived under claimed, read when its source was recorded; a source record
    # from before 2026-10-08 still names the file as it arrived, and that name claims the same
    named = edition_word(claims.get("edition")) or edition_word(source["file"]["name"])
    kind = named or edition_word(folder_edition)
    evidence = [{"signal": "filename", "value": named, "weight": 0.6}] if named else \
        [{"signal": "folder-name", "value": text(folder_edition), "weight": 0.6}] if kind else []
    edition = {"kind": kind or "unknown", "label": text(folder_edition), "decidedBy": "inferred",
               "decidedAt": created_at, "evidence": evidence}
    dynamic = (v0.get("hdr") or {}).get("format") if v0 else None
    three_d = (v0 or {}).get("stereo3d")
    presentation = {
        "colour": "unknown",
        "dynamicRange": dynamic if dynamic in ("sdr", "hdr10", "hdr10plus", "hlg", "dolby-vision") else "unknown",
        "stereo3d": "none" if v0 is not None and not three_d else
                    {"side_by_side": "side-by-side", "sbs": "side-by-side", "top_and_bottom": "top-and-bottom",
                     "tb": "top-and-bottom", "mvc": "mvc"}.get(three_d or "", "unknown"),
        "aspectRatio": (v0 or {}).get("displayAspectRatio"),
    }
    marks = list(chapters)
    return {"schema": "zaentrum.library.version/2", "versionId": version_id, "createdAt": created_at,
            "createdBy": created_by, "edition": edition, "presentation": presentation,
            "runtimeMs": (source.get("container") or {}).get("durationMs"), "chapters": marks,
            "chaptersFrom": chapters_from if marks else None, "segments": list(segments),
            "sourceIds": [source["sourceId"]], "originalFiles": list(original_files)}


# ---------------------------------------------------------------- package.json
def video_rendition(v):
    out = {"id": text(v.get("id")) or "v0", "dir": v.get("dir") or "hls/v0", "codec": v.get("codec") or "",
           "width": num(v.get("width")) or 0, "height": num(v.get("height")) or 0,
           "bitrateBps": num(v.get("bitrateBps")) or 0, "hdr": bool(v.get("hdr")),
           "frameRate": str(v.get("frameRate") or ""), "segments": num(v.get("segments")) or 0,
           "targetDuration": num(v.get("targetDuration")) or 0}
    if v.get("dynamicRange"):
        out["dynamicRange"] = v["dynamicRange"]
    if v.get("sourceStreamIndex") is not None:
        out["sourceStreamIndex"] = num(v["sourceStreamIndex"])
    if num(v.get("peakBitrateBps")) is not None and num(v["peakBitrateBps"]) >= 0:
        out["peakBitrateBps"] = num(v["peakBitrateBps"])
    if v.get("videoRange") in VIDEO_RANGES:
        out["videoRange"] = v["videoRange"]
    for key in ("label", "encoder"):
        if text(v.get(key)):
            out[key] = text(v[key])
    if v.get("hdr10Metadata") is True:
        out["hdr10Metadata"] = True
    if v.get("closedCaptions") is True:
        out["closedCaptions"] = True
    windows = caption_windows_of(v.get("captionWindows"))
    if windows is not None:
        out["captionWindows"] = windows
    offset = v.get("timestampOffset")
    if isinstance(offset, (int, float)) and not isinstance(offset, bool):
        out["timestampOffset"] = round(float(offset), 3)
    return out


def audio_rendition(a, i):
    title = text(a.get("title")) or ""
    purpose, purpose_from = audio_purpose({"disposition": {"comment": bool(a.get("commentary")),
                                                           "visual_impaired": bool(a.get("visualImpaired"))},
                                           "tags": {"title": title}})
    out = {"id": text(a.get("id")) or f"a{i}", "dir": a.get("dir") or f"hls/a{i}", "codec": a.get("codec") or "",
           "language": rendition_language(a.get("language")), "title": title, "default": bool(a.get("default")),
           "channels": num(a.get("channels")) or 0, "bitrateBps": num(a.get("bitrateBps")) or 0,
           "segments": num(a.get("segments")) or 0, "visible": bool(a.get("visible", True)),
           "purpose": purpose, "purposeFrom": purpose_from, "variant": title_variant(title)}
    for key in ("sourceStreamIndex", "sourceChannels"):
        if a.get(key) is not None:
            out[key] = num(a[key])
    if a.get("original") is not None:
        out["original"] = bool(a["original"])
    for key in ("group", "name"):
        if text(a.get(key)):
            out[key] = text(a[key])
    return out


def subtitle_rendition(s, i, from_sidecar=None):
    title = text(s.get("title")) or ""
    purpose, purpose_from = subtitle_purpose({"disposition": {"forced": bool(s.get("forced")),
                                                              "hearing_impaired": bool(s.get("hearingImpaired")),
                                                              "comment": bool(s.get("commentary"))},
                                              "tags": {"title": title}})
    out = {"id": text(s.get("id")) or f"sub{i}", "path": s.get("path") or f"subs/{i}.vtt",
           "language": rendition_language(s.get("language")), "title": title, "default": bool(s.get("default")),
           "forced": bool(s.get("forced")), "format": s.get("format") or "webvtt",
           "visible": bool(s.get("visible", True)), "purpose": purpose, "purposeFrom": purpose_from,
           "variant": title_variant(title)}
    if s.get("sourceStreamIndex") is not None:
        out["sourceStreamIndex"] = num(s["sourceStreamIndex"])
    if text(s.get("name")):
        out["name"] = text(s["name"])
    if text(s.get("hls")):
        out["hls"] = text(s["hls"])
    styled = styled_original(s.get("styled"))
    if styled:
        out["styled"] = styled
    if from_sidecar:
        out["fromSidecar"] = from_sidecar
    return out


def styled_original(v):
    """A subtitle's styled original as its record keeps it, {path, format}: the ASS/SSA script the
    package keeps beside the subtitle's own file (path), its text and styles as the stream or the file
    beside the original had them, in one of STYLED_FORMATS. None for an entry that names none, no path,
    or a format a record does not know."""
    if not isinstance(v, dict) or not text(v.get("path")) or v.get("format") not in STYLED_FORMATS:
        return None
    return {"path": text(v["path"]), "format": v["format"]}


def font_file(f):
    """A font file of the package as its record keeps it: its path; the format its bytes are, one of
    FONT_FORMATS, or None for bytes that are none of them; the names a script asks for it by — its
    family, full and PostScript names, as its name table gives them — each once, in their order; and the
    stream of the original it was taken from (sourceStreamIndex). None for an entry without a path."""
    if not isinstance(f, dict) or not text(f.get("path")):
        return None
    out = {"path": text(f["path"]), "format": f.get("format") if f.get("format") in FONT_FORMATS else None,
           "names": list(dict.fromkeys(n for n in (text(x) for x in f.get("names") or []) if n))}
    if f.get("sourceStreamIndex") is not None:
        out["sourceStreamIndex"] = num(f["sourceStreamIndex"])
    return out


def trickplay(tp):
    if not tp:
        return None
    return {"vttPath": tp.get("vttPath") or "trickplay/thumbnails.vtt",
            "spritePattern": tp.get("spritePattern") or "trickplay/sprite-%04d.jpg",
            "intervalSec": num(tp.get("intervalSec")) or 10, "thumbWidth": num(tp.get("thumbWidth")) or 0,
            "thumbHeight": num(tp.get("thumbHeight")) or 0, "gridCols": num(tp.get("gridCols")) or 1,
            "gridRows": num(tp.get("gridRows")) or 1}


def hls_block(h):
    """The package's HLS layout as the packager wrote it: the master playlist, the segment length,
    the audio groups and the subtitle group, or None for a package that does not say."""
    if not isinstance(h, dict) or not text(h.get("master")) or not num(h.get("segmentSeconds")) \
            or num(h["segmentSeconds"]) < 1:
        return None
    return {"master": text(h["master"]), "segmentSeconds": num(h["segmentSeconds"]),
            "audioGroups": [g for g in (text(x) for x in h.get("audioGroups") or []) if g],
            "subtitleGroup": text(h.get("subtitleGroup"))}


def peak_bandwidth(folder, master="hls/master.m3u8"):
    """The highest BANDWIDTH the package's master playlist advertises, or None without one."""
    p = os.path.join(folder, master)
    if os.path.isfile(p):
        with open(p, encoding="utf-8", errors="replace") as f:
            peaks = [int(m) for m in re.findall(r"BANDWIDTH=(\d+)", f.read())]
        if peaks:
            return max(peaks)
    return None


def package_files(folder, dirs=PACKAGE_DIRS, jobs=1, outside=None):
    """Every file of the package in folder, as [(path relative to folder, hex sha256, size)]: the
    rendition, subtitle, trickplay and trailer folders, never the records beside them. With jobs
    above one that many files are read at a time: a package is thousands of small files, and on a
    share the wait for each, not the bytes, is what reading it costs. outside, when given, tells by
    its path a file in those folders that is no file of the package — attached_subtitle does — which
    is neither listed nor read."""
    found = []
    for sub in dirs:
        base = os.path.join(folder, sub)
        if not os.path.isdir(base):
            continue
        for rel in walk_files(base):
            found.append((os.path.join(sub, rel).replace(os.sep, "/"), os.path.join(base, rel)))
    if outside is not None:
        found = [entry for entry in found if not outside(entry[0])]

    def one(entry):
        rel, p = entry
        return rel, sha_file(p).split(":", 1)[1], os.path.getsize(p)

    if jobs > 1 and len(found) > 1:
        from concurrent.futures import ThreadPoolExecutor
        with ThreadPoolExecutor(max_workers=jobs) as pool:
            return sorted(pool.map(one, found))
    return sorted(one(entry) for entry in found)


def package_listing(folder, dirs=PACKAGE_DIRS):
    """Every file of the package in folder as package_files() lists it, with nothing read:
    [(path relative to folder, None, size)] — what a record with its checksums pending counts."""
    out = []
    for sub in dirs:
        base = os.path.join(folder, sub)
        if not os.path.isdir(base):
            continue
        for rel in walk_files(base):
            out.append((os.path.join(sub, rel).replace(os.sep, "/"), None, os.path.getsize(os.path.join(base, rel))))
    return sorted(out)


def record_entry(name, data):
    """A record about to be written, as checksums() lists it: (name, hex sha256, size)."""
    return name, hashlib.sha256(data).hexdigest(), len(data)


def checksums(listed, pending=False):
    """checksums.sha256 over listed — [(relative path, hex sha256, size)] — and the object a package
    record keeps of it: '<hex>  <path>' lines, sorted by path. Returns (bytes, object). With pending,
    nothing was hashed: no file is to be written (None), and the object says so in place of the
    file's hash, with the count and the bytes of what the file will list."""
    listed = sorted(listed, key=lambda e: e[0])
    files, total = len(listed), sum(size for _, _, size in listed)
    if pending:
        return None, {"file": SUMS, "algorithm": "sha256", "pending": True, "files": files, "bytes": total}
    data = "".join(f"{digest}  {rel}\n" for rel, digest, _ in listed).encode("utf-8")
    return data, {"file": SUMS, "algorithm": "sha256", "sha256": sha_bytes(data), "files": files, "bytes": total}


def complete(package_bytes):
    """The .complete marker, written last: sha256:<hex> of package.json and a line break."""
    return (sha_bytes(package_bytes) + "\n").encode("utf-8")


def package_record(package_id, manifest, listed, *, source=None, role="canonical", created_at=None,
                   packaged_by=None, duration_ms=None, peak_bandwidth_bps=None, sidecars=None, pending=False):
    """package.json for the package a packager manifest describes — the manifest the packager builds,
    or one a legacy package folder holds — and listed, every file the checksums written below it list
    ([(path, hex sha256, size)], the record they cover among them, the package files under hls/ subs/
    trickplay/ trailers/ the rest). Returns (record, notes). With pending, listed is the files as
    package_listing() counts them, none hashed, and the record says its checksums are pending.

    source is the record the package was made from — a source record, or an extra record that was
    probed — whose streams and essence say what the package failed to carry; without them that is said
    to be unmeasured. role is derived while the folder keeps the original beside the package, and
    canonical when it keeps none. sidecars maps a subtitle rendition's id to the copy of the subtitle
    file it was made from, as its source's sidecars[] names it (sources/<sourceId>/subtitle-<n>…),
    relative to the item folder. A subtitle is for what its manifest entry says, read as a source's
    stream is (subtitle_purpose): its forced flag, whether the stream it was made from is for the hard
    of hearing (hearingImpaired, as that stream's disposition said) or a commentary (commentary), then
    its title — so a stream sdh or a commentary by its disposition alone is so in both essences; an
    audio rendition likewise (audio_purpose): a commentary (commentary), for the visually impaired
    (visualImpaired), then its title. A video rendition whose init segment carries the original's
    HDR10 static metadata (hdr10Metadata) says so, and the package's essence has it when the source's
    has and every rendition in PQ, the top one among them, says so (package_essence). A video rendition
    whose own stream carries closed captions (closedCaptions) says so, and the package's essence has
    them when its top rendition says so (package_essence). A subtitle that
    keeps its styled original names it (styled_original), and the font files the manifest lists are the
    record's fonts (font_file), listed only when there are any — each only when it is one of the files
    listed, the package's own, or it is left out with a note; the package's essence has styled
    subtitles and fonts when it carries them for every stream of the source that has them
    (package_essence). What the manifest says the picture is as far as fields go (scan) is the record's
    scan (scan_block), and the essence has what was done of it, left out where it says nothing the
    record can read. The playlist flags a reader could
    not follow are normalised, each with a note: a second default audio rendition is not one, a
    package with none has its first, a forced subtitle is never the default, and of the rest only
    the first default is one."""
    notes = []
    sidecars = sidecars or {}
    ren = manifest.get("renditions") or {}
    video = [video_rendition(v) for v in ren.get("video") or []]
    audio = [audio_rendition(a, i) for i, a in enumerate(ren.get("audio") or [])]
    surround = [audio_rendition(a, len(audio) + i) for i, a in enumerate(ren.get("audioSurround") or [])]
    subs = []
    for i, s in enumerate(manifest.get("subtitles") or []):
        sid = text(s.get("id")) or f"sub{i}"
        subs.append(subtitle_rendition(s, i, sidecars.get(sid)))
    # a styled original and a font file are files of the package, ones its checksums list: a path the
    # manifest names that is none of them is no file the package keeps, and no styling or font it carries
    own = {rel for rel, *_ in listed}
    for s in subs:
        if s.get("styled") and s["styled"]["path"] not in own:
            notes.append(f"subtitle {s['id']}'s styled original {s['styled']['path']} is no file of the package; "
                         f"left out")
            del s["styled"]
    fonts = []
    for x in (font_file(f) for f in manifest.get("fonts") or []):
        if x and x["path"] not in own:
            notes.append(f"font file {x['path']} is no file of the package; left out")
        elif x:
            fonts.append(x)
    defaults = [a for a in audio if a["default"]]
    for a in defaults[1:]:
        a["default"] = False
        notes.append(f"audio rendition {a['id']} was a second default in the package manifest; cleared")
    if audio and not defaults:
        audio[0]["default"] = True
        notes.append("no audio rendition was default in the package manifest; the first one is")
    for a in [a for a in surround if a["default"]][1:]:
        a["default"] = False
        notes.append(f"surround rendition {a['id']} was a second default in the package manifest; cleared")
    for s in subs:
        if s["default"] and (s["forced"] or s["purpose"] in ("forced", "signs-songs")):
            s["default"] = False
            notes.append(f"subtitle {s['id']} was a forced track flagged default; cleared")
    for s in [s for s in subs if s["default"] and not s["forced"]][1:]:
        s["default"] = False
        notes.append(f"subtitle {s['id']} was a second default; cleared")
    renditions = {"video": video, "audio": audio}
    if surround:
        renditions["audioSurround"] = surround
    streams = (source or {}).get("streams") or []
    scan = scan_block(manifest.get("scan"))
    pkg_essence = package_essence({"renditions": renditions, "subtitles": subs, "fonts": fonts, "scan": scan},
                                  (source or {}).get("essence"), streams)
    if streams:
        losses = losses_against(source.get("essence") or {}, pkg_essence, streams, renditions, subs)
    else:
        losses = [{"kind": "other", "detail": "the original was not probed, so what the package failed to "
                                              "carry was never measured", "sourceStreamIndex": None}]
    data, sums = checksums(listed, pending)
    doc = {"schema": "zaentrum.library.package/2", "packageId": package_id,
           "createdAt": ts(manifest.get("packagedAt")) or created_at,
           "packagedBy": text(manifest.get("packager")) or packaged_by or "unknown", "state": "complete",
           "role": role, "durationMs": num(manifest.get("durationMs")) or num(duration_ms) or 0,
           "renditions": renditions, "subtitles": subs}
    if fonts:
        doc["fonts"] = fonts
    doc["trickplay"] = trickplay(manifest.get("trickplay"))
    hls = hls_block(manifest.get("hls"))
    if hls:
        doc["hls"] = hls
    if scan:
        doc["scan"] = scan
    doc.update(trailers=[],
               sizeBytes=sum(size for rel, _, size in listed if rel.split("/", 1)[0] in PACKAGE_DIRS),
               peakBandwidthBps=peak_bandwidth_bps,
               fidelity={"lossless": not losses, "losses": losses, "droppedSourceStreams": []},
               essence=pkg_essence, checksums=sums)
    return doc, notes


# ---------------------------------------------------------------- extras/<extraId>/extra.json
def extra_record(extra_id, *, created_at, created_by, kind, title, localized_titles=None, language=None,
                 season_number=None, origin=None, probe=None, probe_version=None, probed_at=None,
                 original_files=(), originals=(), packaged_from=(), heads=None, captions=None, signature=None):
    """extras/<extraId>/extra.json: what the extra is, as the catalog took it in, and what a probe of
    its original found. An extra of the platform keeps no original — it is deleted once the package is
    recorded — so original_files and originals stay empty, and packaged_from describes the original the
    package was made from: [{name, sizeBytes, fixity: {qh1}}], each named here as the library names an
    original, library_original_name() of the name it arrived under — the nth of several original-<n> —
    whatever name it is given. Without a language, the language spoken in its first audio track is the
    extra's. heads, captions and signature, as a source record's (source_record), are its attachments'
    first bytes, its picture's caption windows and whether its picture's packets carry a caption
    signature."""
    doc = {"schema": "zaentrum.library.extra/2", "extraId": extra_id, "createdAt": created_at,
           "createdBy": created_by, "kind": kind, "title": title, "localizedTitles": dict(localized_titles or {}),
           "language": language, "runtimeMs": None}
    if season_number is not None:
        doc["seasonNumber"] = season_number
    doc.update(originalFiles=list(original_files), originals=list(originals))
    if packaged_from:
        parts = len(packaged_from) > 1
        doc["packagedFrom"] = [dict(o, name=library_original_name(o.get("name"), i + 1 if parts else None))
                               for i, o in enumerate(packaged_from)]
    if origin:
        doc["origin"] = origin
    if probe:
        facts = probed(probe, heads, captions, signature)
        spoken = [x["language"] for x in facts["streams"] if x["type"] == "audio" and x.get("language")]
        doc.update(language=language or (spoken[0] if spoken else None), runtimeMs=facts["container"]["durationMs"],
                   **{k: v for k, v in facts.items() if k != "offList"}, probe={"tool": "ffprobe", "version": probe_version, "at": probed_at or created_at,
                                   "note": None})
    return doc


# ---------------------------------------------------------------- reading a chain back
def read_sums(path):
    """A sha256sum file as ({path: hex digest}, [lines that are not '<64 hex>  <path>'])."""
    entries, bad = {}, []
    with open(path, encoding="utf-8", errors="replace") as f:
        for line in f:
            line = line.rstrip("\n")
            m = re.match(r"^([0-9a-f]{64})  (.+)$", line)
            if m:
                entries[m.group(2)] = m.group(1)
            elif line:
                bad.append(line)
    return entries, bad


def attached_subtitle(rel):
    """The hex sha256 an attached subtitle's name gives its bytes, when rel — a path in a version
    folder, relative to it — is the file of one (ATTACHED_SUBTITLE_RE); None for every other path."""
    m = ATTACHED_SUBTITLE_RE.match(rel.replace(os.sep, "/"))
    return m.group(1) if m else None


def chain_problems(folder, record="version.json", dirs=PACKAGE_DIRS, kept=(), full=False):
    """What is wrong with the chain of a version or an extra folder: .complete names package.json,
    package.json names checksums.sha256, which lists exactly the record, the originals kept beside it
    (kept) and every package file, of the count and total size package.json records, and the
    record's digest is the one listed. With full, every listed file is hashed too. An empty list is a
    chain that holds. A version's attached subtitle (attached_subtitle) is no file of its package:
    unless the checksums list it, the chain holds without it, and full holds its bytes to the hash its
    name gives them."""
    out = []
    version = record == "version.json"
    mark, pkg_path, sums_path = (os.path.join(folder, n) for n in (".complete", "package.json", SUMS))
    if not os.path.isfile(mark) or not os.path.isfile(pkg_path):
        return ["no package.json and .complete: the package never finished"]
    with open(mark, "rb") as f:
        head = f.read().decode("utf-8", "replace").strip()
    if head != sha_file(pkg_path):
        out.append(".complete does not name this package.json")
    try:
        with open(pkg_path, encoding="utf-8") as f:
            cs = (json.load(f) or {}).get("checksums") or {}
    except (OSError, ValueError) as e:
        return out + [f"package.json cannot be read: {e}"]
    if cs.get("pending") is True:
        # the chain is not closed yet: nothing was hashed, so there is no file to name, and what can
        # be held against the record is what it counted — the package's files, never an attached
        # subtitle, which nothing counted
        if os.path.isfile(sums_path):
            out.append(f"{SUMS} is here while package.json says its checksums are pending")
        found = [rel for rel, _, _ in _listing(folder, dirs)]
        attached = [rel for rel in found if version and attached_subtitle(rel)]
        present = [record] + [rel for rel in found if rel not in attached]
        if len(present) != cs.get("files"):
            out.append(f"the folder holds {len(present)} files, package.json counted {cs.get('files')}")
        total = sum(os.path.getsize(os.path.join(folder, rel)) for rel in present
                    if os.path.isfile(os.path.join(folder, rel)))
        if total != cs.get("bytes"):
            out.append(f"the folder's files total {total} bytes, package.json counted {cs.get('bytes')}")
        return out + (_attached_problems(folder, attached) if full else [])
    if not os.path.isfile(sums_path):
        return out + [f"{SUMS} is missing"]
    if sha_file(sums_path) != cs.get("sha256"):
        out.append(f"{SUMS} is not the one package.json names")
    listed, bad = read_sums(sums_path)
    out += [f"{SUMS} holds a line that is not one: {line[:80]!r}" for line in bad[:3]]
    found = {rel for rel, _, _ in _listing(folder, dirs)}
    attached = sorted(rel for rel in found if version and rel not in listed and attached_subtitle(rel))
    present = sorted({record} | {n for n in kept if os.path.isfile(os.path.join(folder, n))}
                     | (found - set(attached)))
    for rel in sorted(set(present) - set(listed)):
        out.append(f"{rel} is not listed in {SUMS}")
    for rel in sorted(set(listed) - set(present)):
        out.append(f"{SUMS} lists {rel}, which is not here")
    if len(listed) != cs.get("files"):
        out.append(f"{SUMS} lists {len(listed)} files, package.json says {cs.get('files')}")
    total = sum(os.path.getsize(os.path.join(folder, rel)) for rel in listed
                if os.path.isfile(os.path.join(folder, rel)))
    if total != cs.get("bytes"):
        out.append(f"the files {SUMS} lists total {total} bytes, package.json says {cs.get('bytes')}")
    rp = os.path.join(folder, record)
    if os.path.isfile(rp) and sha_file(rp).split(":", 1)[1] != listed.get(record):
        out.append(f"{record} does not match the digest {SUMS} lists for it")
    if full:
        for rel in sorted(set(listed) & set(present) - {record}):
            if sha_file(os.path.join(folder, rel)).split(":", 1)[1] != listed[rel]:
                out.append(f"{rel} does not match its checksum")
        out += _attached_problems(folder, attached)
    return out


def _attached_problems(folder, attached):
    """Each attached subtitle of attached, paths relative to folder, whose bytes are not the ones its
    name says."""
    out = []
    for rel in attached:
        digest = sha_file(os.path.join(folder, rel))
        if digest.split(":", 1)[1] != attached_subtitle(rel):
            out.append(f"{rel} is an attached subtitle whose bytes hash to {digest}, not to the one its name says")
    return out


def checksums_pending(folder):
    """True when the package.json in folder says its checksums are pending (package_record with
    pending): the chain .complete heads is to be closed by fill_checksums()."""
    try:
        with open(os.path.join(folder, "package.json"), encoding="utf-8") as f:
            return ((json.load(f) or {}).get("checksums") or {}).get("pending") is True
    except (OSError, ValueError):
        return False


def fill_checksums(folder, record="version.json", dirs=PACKAGE_DIRS, jobs=1):
    """Closes the chain of a version folder whose checksums are pending: every package file is
    hashed, with the record beside them, and what the folder is to hold is returned —
    (checksums.sha256 bytes, package.json bytes, .complete bytes) — to be written in that order, each
    in one step. The package record changes in its checksums alone. None when the folder's checksums
    are not pending, or when what it holds is no longer what the record counted: then nothing may be
    written, and chain_problems() says what is wrong. jobs is how many files are read at a time. An
    attached subtitle beside the package (attached_subtitle) is none of its files: it is neither read
    nor listed, and the chain closes as it would without it."""
    pkg_path = os.path.join(folder, "package.json")
    if not checksums_pending(folder) or chain_problems(folder, record, dirs):
        return None
    with open(pkg_path, encoding="utf-8") as f:
        package = json.load(f)
    with open(os.path.join(folder, record), "rb") as f:
        listed = package_files(folder, dirs, jobs, outside=attached_subtitle if record == "version.json" else None) \
            + [record_entry(record, f.read())]
    data, sums = checksums(listed)
    if (sums["files"], sums["bytes"]) != (package["checksums"].get("files"), package["checksums"].get("bytes")):
        return None  # a file changed between the count and the hash
    package["checksums"] = sums
    package_bytes = json_bytes(package)
    return data, package_bytes, complete(package_bytes)


def _listing(folder, dirs):
    """The package files under folder, by path, with nothing hashed."""
    out = []
    for sub in dirs:
        base = os.path.join(folder, sub)
        if os.path.isdir(base):
            out += [(os.path.join(sub, rel).replace(os.sep, "/"), None, None) for rel in walk_files(base)]
    return out
