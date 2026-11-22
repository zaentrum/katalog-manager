#!/usr/bin/env python3
"""Validate a zaentrum library v2 tree against the JSON Schemas and the rules that span files.

Usage:
  validate-library-v2.py [--check-media | --check-checksums] [--schemas URL-or-dir] [--shard AA ...]
                         ROOT [ROOT ...]

ROOT is a folder holding movies/, series/ and people/. In v2 the database is the working copy and
this tree is the record that can rebuild it: every file is either written once (item, source,
version, package, event, extra) or replaced whole by the one service that owns it (metadata,
person). Nothing here is merged, so the rules below are about records agreeing with each other and
with the bytes beside them, never about a document being up to date.

  layout       only movies/, series/ and people/ at the root, beside entries whose name begins with a
               dot — .work/, where the platform keeps what is not the record — which are not looked
               at, and, as a note until the migration's cleanup moves them aside, the folders of the
               store before the library (media/, packages/, extras/, incoming/); shard folders of two
               characters; itemId
               and personId equal the folder name and its shard; an item folder holds only item.json,
               checksums.sha256, metadata.json, metadata/, sources/, versions/, events/, extras/ (a
               series: episodes/ instead of sources/ and versions/; an episode: no extras/) — so no
               media can sit in the item folder; every source and every event is a folder of its own
  covered      every write-once folder proves its records: an item folder's checksums.sha256 lists
               exactly item.json, a source folder's lists exactly source.json, its probe and its
               sidecars, an event folder's exactly event.json, and each digest matches the file; a
               version is one chain — .complete holds sha256:<hex> of package.json, package.json the
               hash of checksums.sha256, and checksums.sha256 lists version.json beside the package
               files and never .complete, package.json or itself. Projections are not covered: they
               are replaced whole, and an image's name is its own hash
  people       a person folder holds person.json and the images it lists, at most one portrait of them
               primary; a death is not before the birth; a credit whose person has no folder is a
               note, not an error, because the person may not have been projected yet
  versions     every version is a folder under versions/ whose name is its versionId; its sourceIds
               name source records that exist and its originalFiles are those sources' file names,
               each with its fixity while it is kept (a source without one says why in probe.note);
               package.json exists exactly when .complete does, and a version taken in before anything
               packaged it holds its originals alone, and the subtitles attached to it since in its
               subs/ — no package, and nothing unfinished; a package is canonical exactly when
               the version keeps no original; lossless means no losses; chapters are ordered and
               kept when the original carried them; every track — a 5.1 companion's too — says what
               it is for and the playlist flags do not contradict it; a rendition is in an audio group
               the HLS layout names; a video rendition whose init segment carries HDR10 static
               metadata says it is in PQ; a subtitle made from a sidecar names one a source of the version
               keeps, and an extra's never does; a source a version is made of whose original no version
               folder lists (one a version-removed event removed counts while its folder is there) and
               no original-deleted event names is gone outside the record: a note
  names        no record names a file as it arrived, and no file in the record is named so: a source's
               file.name, a version's and an extra's originalFiles and what an extra was packaged from
               are original.<ext>, or original-<n>.<ext> for a part, a source's sidecars are copies of
               subtitle files named subtitle-<n>.<lang>[.forced][.sdh].<ext>, without originalName, a
               source says nothing of where its original came from (origin), no record keeps a
               container title tag, and a probe names its file as its record does and keeps no title
               tag either — library-v2-neutral-names.py gives a tree written before 2026-10-08 the names
  metadata     same item and type as item.json, and reference ids of its type; every image exists
               with the recorded hash, size, content type and dimensions, is named by its own content
               hash, is listed once as each kind it is — bytes of several kinds share one file — and
               nothing unlisted sits in metadata/ (the same for a person's images beside person.json);
               at most one image of a kind is primary, in a series one per kind and season;
               library.primaryVersionId and library.versionLabels name versions that are really there
  series       an episode names its enclosing series, agrees with it on the reference id, numbers
               itself consistently, does not collide with another episode, and sits in a season the
               series' metadata lists; its own numbering does not contradict the numbers item.json
               was created with, and no two episodes claim one place in one ordering
  covering     one file, several episodes: a source's covers lists the episodes its file holds, the
               holder first — the episode whose folder keeps the source — and the others after it in
               episode order, and a movie's file covers none; an episode whose projection names a
               holder (library.coveredBy) is a later episode of the holder's season, keeps no source
               or version of its own, is listed by a source of the holder, plays and labels only
               versions of the holder (primaryVersionId, versionLabels) — the one it plays made from
               a source that covers it — and the holder's numbering ends (episodeEnd) at the place of
               the last episode it covers, a covered episode's own at none. A covers entry no
               projection agrees with — an episode unlinked since, or never recorded — an episodeEnd
               no covered episode explains, and a holder whose file has no source yet are notes
  events       the folder name is the event's own moment, eventId and kind; every source, version, package
               and extra it names exists, or is a version a version-removed event of the same item
               removed — or that version's package, by the packageId an event of the item pairs with
               it — because the events that named a version are its history and stay valid once it is
               gone (a version-removed or extra-removed event's own subject may be gone, and a folder
               it names is ignored altogether, an extra's with a note for the sweep; an episode has no
               extra to remove); a deletion names one of its
               version's own sources when it names one — and always when the version never kept the
               original in its folder, so it was deleted outside the record — and accepts no more than
               the deletion gate — the essence of the version's sources minus that of its package,
               computed here because no record holds it, with the same code every reader computes it
               with (the shared gate block) — and never closedCaptions or interlaced; one dated
               2026-11-16 or later deletes no original the gate keeps (closed captions carried or not
               known, in fields, off the closed lists, a record or probe that can't be read), and one
               before is a note of what was lost; the read a deletion carries (captionsRead) tells
               none — by the plan, the six windows at their places, on frames of their own, each of
               half its packets and 24 frames, or the whole picture, over its record's span —, reports a
               span that is its record's within a frame step and 0.5 s, counted no interlaced frame and
               is dated no later than the deletion - every such event, whatever became of its version,
               its source's every read in the item counted; a
               package-superseded event names a successor that exists in another version folder
  captions     a source's essence knows its closed captions exactly when the shared rule says so
               (captions_known), carries them when a stream, the signature or a window says so, and is
               in fields where a window counted interlaced frames; a caption window counts no more
               frames than it decoded; a source's kept probe is a probe that gives no key twice, its
               offList what the closed lists say of it, its picture the probe's film (film_of), of its
               codec and its field order; no record gives NaN or Infinity
  JSON         no record gives a key twice
  extras       bonus material sits in extras/<extraId>/ inside its movie or series, never an
               episode; each folder is named by its extraId and holds extra.json, the originals it
               names and/or a package (hls/ subs/ trickplay/); originals describes exactly the files
               originalFiles names, and packagedFrom the ones a package that keeps none was made from; its checksums.sha256 lists extra.json and every file beside it,
               the originals included, and never itself, package.json or .complete; a packaged extra
               closes the chain a version does and carries no trailers, one that keeps only its
               original is finished by its checksums file, and one that never finished is a note;
               only a series' extra names a season, and only one the series' metadata lists;
               library.extras names extras that are there
  checksums    the checksums file matches its recorded hash and file count; a package adopted with
               its checksums pending has no checksums file yet and is a note, counted (pending) —
               and a problem with --no-pending, once a migration says every one is filled
  --check-media  no file under an item folder is a hard link shared with another path — but, with
               --shared-originals, a version's original, which a migration that keeps the old
               library beside this one leaves under its old path too (a note, counted); every
               original a version names is there with its size and qh1, unless an original-deleted
               event covers it — in which case it must NOT be there; every rendition folder, 5.1
               companion, subtitle, subtitle HLS rendition, master playlist and trickplay sheet a
               package names exists and the cues cover the duration;
               checksums.sha256 lists exactly version.json and the package's files, with their total
               size (pending, the folder holds the files and the bytes the record counted) — a
               subtitle attached to the version, after its package completed or while it was taken in
               before anything packaged it, named by its bytes as
               subs/attached-<sha256>.<lang>[.forced][.sdh].vtt, is no file of a package, and is
               counted (attached) — and an extra's lists files of that total; an extra's every
               original has the size and qh1 extra.json records
  --check-checksums  also hash every package file and every file an extra's checksums list, an
               extra's originals against the sha256 extra.json records, and each attached subtitle
               against the hash its name gives it (implies --check-media)

The schemas are loaded from the library/v2 folder next to this tool by default; pass
--schemas https://zaentrum.github.io/schemas/library/v2 to use the published copies.
Needs: pip install "jsonschema[format-nongpl]>=4.23" referencing
"""
import argparse, datetime, hashlib, json, os, re, sys, urllib.request
from jsonschema import Draft202012Validator, FormatChecker, validators
from jsonschema.exceptions import best_match
from referencing import Registry, Resource

HERE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "library", "v2")
NAMES = ["defs", "item", "metadata", "person", "source", "version", "package", "event", "extra"]
DOCUMENTS = ["item", "metadata", "person", "source", "version", "package", "event", "extra"]
BASE = "https://zaentrum.github.io/schemas/library/v2/"

CATEGORIES = (("movies", "movie"), ("series", "series"))
ROOT_ENTRIES = {"movies", "series", "people"}
# The folders of the store the platform kept before the library, at the same root: the migration
# empties them of everything the catalog knows, and its cleanup moves what is left to .work/legacy/.
OLD_STORE = ("media", "packages", "extras", "incoming")
SUMS = "checksums.sha256"
# a version's original, relative to its item folder: versions/<versionId>/original[-n].<ext>
SHARED_ORIGINAL = re.compile(r"^versions/[^/]+/original(-\d+)?\.[a-z0-9]+$")
ITEM_ENTRIES = {"item.json", SUMS, "metadata.json", "metadata", "sources", "versions", "events", "extras"}
SERIES_ENTRIES = {"item.json", SUMS, "metadata.json", "metadata", "episodes", "events", "extras"}
VERSION_ENTRIES = {"version.json", "package.json", SUMS, ".complete", "hls", "subs", "trickplay", "trailers"}
PACKAGE_DIRS = ("hls", "subs", "trickplay", "trailers")
# An extra's package is the folders of a version's, without trailers: a trailer of its own is an extra.
EXTRA_DIRS = ("hls", "subs", "trickplay")
EXTRA_ENTRIES = {"extra.json", "package.json", SUMS, ".complete", *EXTRA_DIRS}
# Each link of a version's chain holds the hash of the next one down, so none of them can be listed by
# the checksums file it sits above.
CHAIN = (".complete", "package.json", SUMS)
EVENT_KINDS = ("original-deleted", "version-removed", "package-superseded", "source-removed", "extra-removed", "note")
EVENT_FOLDER = re.compile(r"^(\d{8}T\d{6}Z)-([0-9a-f]{8})-(" + "|".join(EVENT_KINDS) + r")$")
OLD_EVENT_FILE = re.compile(r"^\d{8}T\d{6}Z(?:-[0-9a-f]{8})?-(" + "|".join(EVENT_KINDS) + r")\.json$")
SUM_LINE = re.compile(r"^([0-9a-f]{64})  (.+)$")
COMPLETE = re.compile(r"^sha256:[0-9a-f]{64}$")
UPGRADE = "library-v2-upgrade.py upgrades a tree from before 2026-10-02 (b) in place"
UUID = re.compile(r"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$")
VTT_CUE = re.compile(r"^(\d+):(\d\d):(\d\d)\.(\d{3}) --> (\d+):(\d\d):(\d\d)\.(\d{3})")
OS_ARTEFACTS = re.compile(r"^(\.DS_Store|\._.*|Thumbs\.db|desktop\.ini|@eaDir|\.@__thumb|#recycle|\.AppleDouble)$")
EXT_TYPE = {"jpg": "image/jpeg", "png": "image/png", "webp": "image/webp"}
IMAGE_NAME = re.compile(r"^([0-9a-f]{64})\.(jpg|png|webp)$")
SWEEP = "library-v2-sweep.py collects it once it is older than the grace period"
# The names the library gives an original — in a version's or an extra's folder, in its source's record,
# among what an extra was packaged from — and the copy a source keeps of a subtitle file that came with it:
# nothing of the name either arrived under, and only [a-z0-9.-]. libv2_records.py makes them.
ORIGINAL_NAME = re.compile(r"^original(-[0-9]+)?\.[a-z0-9]{1,8}$")
SUBTITLE_COPY = re.compile(r"^subtitle-[0-9]+\.[a-z]{2,8}(\.forced)?(\.sdh)?\.[a-z0-9]{1,8}$")
# A subtitle attached to a version after its package completed — or while it is taken in, before anything
# packaged it — relative to the version folder: in its subs/, named by the hex sha256 of its bytes, its
# BCP 47 language in lower case and its flags. It is no file of a package, and no checksums list it: its
# name is its check. libv2_records.py
# (ATTACHED_SUBTITLE_RE) and katalog-manager, which writes it, read the name the same way.
ATTACHED_SUBTITLE = re.compile(r"^subs/attached-([0-9a-f]{64})\.[a-z0-9]+(-[a-z0-9]+)*(\.forced)?(\.sdh)?\.vtt$")
NEUTRAL ="library-v2-neutral-names.py gives a tree written before 2026-10-08 the names the library gives its files"
ID_KEYS = {"schema", "itemId", "seriesId", "sourceId", "versionId", "packageId", "eventId", "personId", "file",
           "path", "dir", "vttPath", "manifestPath", "originalFile", "name", "language", "type", "kind",
           "tmdbMovie", "tmdbTv", "tmdbSeason", "tmdbEpisode", "tmdbCollection", "imdb", "tvdb", "tmdbPerson",
           "sha256", "qh1", "ref"}
FREE_TEXT = {"overview", "tagline", "notes", "note", "detail", "reason"}
INT64 = (-(1 << 63), (1 << 63) - 1)


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


# What no original-deleted event accepts: an original that has closed captions, or whose captions are not
# known, and one in fields, are kept (ADR-0015).
NEVER_ACCEPTED = ("closedCaptions", "interlaced")
# What says an attachment is a font, as libv2_records.py reads it (attachment_role): a MIME type in any of the
# words a font's carries, or a font's file name; else, read from its first bytes, its signature.
FONT_MIME = re.compile(r"font|opentype|truetype|sfnt", re.I)
FONT_FILE = re.compile(r"\.(ttf|otf|ttc|woff2?)$", re.I)


def windows_of(stream):
    """The caption windows a picture or a rendition keeps, or None, the schema having held each to its
    shape."""
    w = (stream or {}).get("captionWindows")
    return w if isinstance(w, list) else None


# From this moment on an original-deleted event that deletes an original the gate keeps is an error, before
# it a note of what was lost (ADR-0015).
RULE_FROM = datetime.datetime(2026, 11, 16, tzinfo=datetime.timezone.utc)


def moment(timestamp):
    """A timestamp as an aware datetime, or the earliest one of a text that is none."""
    try:
        t = datetime.datetime.fromisoformat(str(timestamp).replace("Z", "+00:00"))
    except ValueError:
        return datetime.datetime.min.replace(tzinfo=datetime.timezone.utc)
    return t if t.tzinfo else t.replace(tzinfo=datetime.timezone.utc)


def kept_why(key, essences):
    """Why the gate keeps an original for key where its essence says so (what gate_holds read says none)."""
    if key == "closedCaptions":
        return "its closed captions are carried" if any(e.get("closedCaptions") is True for e in essences) \
            else "its closed captions are not known"
    return "it is marked as fields"


def no_constant(name):
    """json's parse_constant: NaN, Infinity and -Infinity are no JSON numbers a record holds."""
    raise ValueError(f"{name} is no number a record holds")


def no_overflow(text):
    """json's parse_float: a number written past what a float holds (1e999) is Infinity, no number a record holds."""
    if abs(float(text)) == float("inf"):
        no_constant(text)
    return float(text)


def never_held(e, src):
    """Whether an original-deleted event e records an original the library never held - history, not a deletion:
    it names its source, whose record src keeps no fixity, and it accepted nothing and carries no read. A source
    without fixity whose original a version lists is refused (Checker.originals_kept), so no original the library
    held is one of these."""
    return bool(e.get("sourceId")) and isinstance(src, dict) and isinstance(src.get("file"), dict) \
        and "fixity" not in src["file"] and not e.get("accepted") and "captionsRead" not in e


def load_strict(p):
    """The JSON document at p, refusing a key given twice in an object, and NaN or Infinity (ValueError)."""
    twice = []

    def pairs(items):
        keys = [k for k, _ in items]
        twice.extend(k for k in dict.fromkeys(keys) if keys.count(k) > 1)
        return dict(items)

    with open(p, encoding="utf-8") as f:
        doc = json.load(f, object_pairs_hook=pairs, parse_constant=no_constant, parse_float=no_overflow)
    if twice:
        raise ValueError(f"gives the key {', '.join(dict.fromkeys(twice))} twice: which one is meant can't be told")
    return doc


def readable(p):
    """The JSON object at p, or None where there is none or it can't be read - a key given twice, NaN or Infinity
    among them (load_strict; the checks of the record there say why)."""
    try:
        doc = load_strict(p)
    except (OSError, ValueError):
        return None
    return doc if isinstance(doc, dict) else None


def plan_for(d, src):
    """The plan's span of a source record's picture (plan_of), of the probe kept beside it in the item folder d
    and its container's length; None where either can't be read."""
    if not d or not isinstance(src, dict) or not isinstance(src.get("sourceId"), str) \
            or not UUID.match(src["sourceId"]):
        return None
    probe = readable(os.path.join(d, "sources", src["sourceId"], "ffprobe.json"))
    return plan_of(probe, (src.get("container") if isinstance(src.get("container"), dict) else {}).get("durationMs"))


def reads_by_source(events):
    """The reads (captionsRead) the original-deleted events of an item carry, by the source each names, every one
    of them, whichever version's event it is."""
    out = {}
    for e in events:
        if e["kind"] == "original-deleted" and e.get("sourceId") and "captionsRead" in e:
            out.setdefault(e["sourceId"], []).append(e["captionsRead"])
    return out


def title_holds(d, records, reads=()):
    """gate_holds() of the source records records of a version of the item folder d (None of one that can't be
    read): the probe kept of each (sources/<sourceId>/ffprobe.json), every one passed, None where there is none
    or it can't be read; the package record of every version of the title, finished or not, None of one there
    that can't be read; and every read its deletion events carry."""
    probes = [readable(os.path.join(d, "sources", r["sourceId"], "ffprobe.json"))
              if isinstance(r, dict) and isinstance(r.get("sourceId"), str) and UUID.match(r["sourceId"]) else None
              for r in records]
    base = os.path.join(d, "versions")
    names = listdir(base) if os.path.isdir(base) else []
    packages = [readable(os.path.join(base, n, "package.json")) for n in names
                if UUID.match(n) and os.path.isfile(os.path.join(base, n, "package.json"))]
    return gate_holds(records, packages, probes, reads)


def listdir(d):
    """Directory entries without the files operating systems and NAS software drop into shared folders."""
    return sorted(n for n in os.listdir(d) if not OS_ARTEFACTS.match(n))


def attached_only(vp):
    """Whether the subs/ of the version folder vp holds subtitles attached to the version and nothing
    else, each a file directly in it named as one is (ATTACHED_SUBTITLE): a version taken in before
    anything packaged it keeps the ones attached to it since there, which are no package."""
    subs = os.path.join(vp, "subs")
    names = listdir(subs) if os.path.isdir(subs) and not os.path.islink(subs) else []
    return bool(names) and all(os.path.isfile(os.path.join(subs, n)) and not os.path.islink(os.path.join(subs, n))
                               and ATTACHED_SUBTITLE.match(f"subs/{n}") for n in names)


def sha_file(p):
    h = hashlib.sha256()
    with open(p, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return "sha256:" + h.hexdigest()


def qh1(p):
    """sha256(first 64 KiB || last 64 KiB || uint64be(size)): cheap proof a large file is the one recorded."""
    size = os.path.getsize(p)
    h = hashlib.sha256()
    with open(p, "rb") as f:
        h.update(f.read(65536))
        if size > 65536:
            f.seek(max(size - 65536, 0))
            h.update(f.read(65536))
    h.update(size.to_bytes(8, "big"))
    return "sha256:" + h.hexdigest()


def stamp_of(timestamp):
    """The <YYYYMMDDTHHMMSSZ> an event file name carries, from the event's own 'at'."""
    t = datetime.datetime.fromisoformat(timestamp.replace("Z", "+00:00"))
    return t.astimezone(datetime.timezone.utc).strftime("%Y%m%dT%H%M%SZ")


def sniff_image(p):
    """(content type, width, height) from the file header; None where unknown."""
    with open(p, "rb") as f:
        b = f.read(1 << 24)
    if b[:8] == b"\x89PNG\r\n\x1a\n" and len(b) >= 24:
        return "image/png", int.from_bytes(b[16:20], "big"), int.from_bytes(b[20:24], "big")
    if b[:4] == b"RIFF" and b[8:12] == b"WEBP":
        chunk = b[12:16]
        if chunk == b"VP8X" and len(b) >= 30:
            return "image/webp", 1 + int.from_bytes(b[24:27], "little"), 1 + int.from_bytes(b[27:30], "little")
        if chunk == b"VP8 " and len(b) >= 30:
            return "image/webp", int.from_bytes(b[26:28], "little") & 0x3FFF, int.from_bytes(b[28:30], "little") & 0x3FFF
        if chunk == b"VP8L" and len(b) >= 25:
            v = int.from_bytes(b[21:25], "little")
            return "image/webp", (v & 0x3FFF) + 1, ((v >> 14) & 0x3FFF) + 1
        return "image/webp", None, None
    if b[:2] == b"\xff\xd8":
        i = 2
        while i + 9 < len(b):
            if b[i] != 0xFF:
                i += 1
                continue
            m = b[i + 1]
            if m in (0xC0, 0xC1, 0xC2, 0xC3, 0xC5, 0xC6, 0xC7, 0xC9, 0xCA, 0xCB, 0xCD, 0xCE, 0xCF):
                return "image/jpeg", int.from_bytes(b[i + 7:i + 9], "big"), int.from_bytes(b[i + 5:i + 7], "big")
            if m in (0xD8, 0x01) or 0xD0 <= m <= 0xD7:
                i += 2
                continue
            i += 2 + int.from_bytes(b[i + 2:i + 4], "big")
        return "image/jpeg", None, None
    return None, None, None


def read_checksums(path):
    entries = {}
    for line in open(path, encoding="utf-8"):
        line = line.rstrip("\n")
        if not line:
            continue
        digest, _, rel = line.partition("  ")
        entries[rel] = digest
    return entries


def read_sums(path):
    """A sha256sum file as ({name: hex digest}, [lines that are not '<64 hex>  <name>'])."""
    entries, bad = {}, []
    with open(path, encoding="utf-8", errors="replace") as f:
        for line in f:
            line = line.rstrip("\n")
            m = SUM_LINE.match(line)
            if m:
                entries[m.group(2)] = m.group(1)
            elif line:
                bad.append(line)
    return entries, bad


def package_files(vp, folders=PACKAGE_DIRS):
    """Every file of the package in a version folder, relative to it: the rendition, subtitle,
    trickplay and trailer folders. The records, the chain above them and the original beside them are
    not part of the package."""
    out = []
    for d in folders:
        for root, dirs, files in os.walk(os.path.join(vp, d)):
            dirs[:] = sorted(x for x in dirs if not OS_ARTEFACTS.match(x))
            out += [os.path.relpath(os.path.join(root, f), vp) for f in sorted(files) if not OS_ARTEFACTS.match(f)]
    return sorted(out)


def extra_files(xp, originals):
    """Every file an extra's checksums must list, relative to its folder: extra.json, the originals
    that are there and every file of its package. Only the chain above them is left out."""
    named = [n for n in ["extra.json", *originals] if os.path.isfile(os.path.join(xp, n))]
    return sorted(named + package_files(xp, EXTRA_DIRS))


def walk_keys(o):
    if isinstance(o, dict):
        for k, v in o.items():
            yield k
            yield from walk_keys(v)
    elif isinstance(o, list):
        for v in o:
            yield from walk_keys(v)


def walk_strings(o, key=None):
    if isinstance(o, dict):
        for k, v in o.items():
            yield from walk_strings(v, k)
    elif isinstance(o, list):
        for v in o:
            yield from walk_strings(v, key)
    elif isinstance(o, str):
        yield key, o


# ---------------------------------------------------------------- schemas
def load_schemas(where):
    fc = FormatChecker()
    if "date-time" not in fc.checkers:
        raise SystemExit('date-time values cannot be checked in this environment: pip install "jsonschema[format-nongpl]>=4.23"')
    docs = {}
    for n in NAMES:
        if where.startswith("http"):
            with urllib.request.urlopen(f"{where.rstrip('/')}/{n}.schema.json", timeout=30) as r:
                docs[n] = json.load(r)
        else:
            docs[n] = json.load(open(os.path.join(where, f"{n}.schema.json")))
    registry = Registry().with_resources((BASE + f"{n}.schema.json", Resource.from_contents(d)) for n, d in docs.items())
    # JSON Schema counts 7.0 as an integer; the readers of the playback fields do not.
    types = Draft202012Validator.TYPE_CHECKER.redefine(
        "integer", lambda checker, x: isinstance(x, int) and not isinstance(x, bool) and INT64[0] <= x <= INT64[1])
    Strict = validators.extend(Draft202012Validator, type_checker=types)
    for n, d in docs.items():
        Draft202012Validator.check_schema(d)
        if d["$id"] != BASE + f"{n}.schema.json":
            raise SystemExit(f"{n}.schema.json: $id {d['$id']} is not {BASE}{n}.schema.json")
    return {n: Strict(docs[n], registry=registry, format_checker=fc) for n in DOCUMENTS}


def describe(e):
    """A short, specific reason: the deepest relevant error, never a dump of the whole document."""
    if e.context:
        ctx = list(e.context)
        if e.instance is not None:
            # an object that fails a "value or null" choice: the null branch's complaint is noise
            ctx = [c for c in ctx if not (c.validator == "type" and c.validator_value == "null")] or ctx
        deepest = max(len(c.absolute_path) for c in ctx)
        return describe(best_match([c for c in ctx if len(c.absolute_path) == deepest]))
    where = e.json_path
    if e.validator == "not" and isinstance(e.instance, dict):
        forbidden = [r for sub in (e.validator_value.get("anyOf") or [e.validator_value]) for r in (sub.get("required") or [])]
        present = [f for f in forbidden if f in e.instance]
        return f"{where}: must not have {', '.join(present) or 'this combination'} here"
    msg = e.message
    r = repr(e.instance)
    if len(r) > 80 and r in msg:
        msg = msg.replace(r, "<value>")
    return f"{where}: {msg[:240]}"


# ---------------------------------------------------------------- checker
class Checker:
    def __init__(self, validators, check_media, check_checksums=False, shared_originals=False, no_pending=False):
        self.v = validators
        self.check_media = check_media or check_checksums
        self.check_checksums = check_checksums
        # a migration that keeps the old library beside this one: a version's original is the old
        # path's file too, a hard link; and, once it says its checksums are filled, none is pending
        self.shared_originals = shared_originals
        self.no_pending = no_pending
        self.pending, self.shared = 0, 0
        self.errors = []
        self.notes = []
        self.root_dir = None
        self.only_shards = None  # --shard: the shards to look at; None, every one
        self.counts = {"movie": 0, "series": 0, "episode": 0, "people": 0, "versions": 0, "packages": 0,
                       "sources": 0, "images": 0, "events": 0, "extras": 0, "attached": 0}

    def err(self, where, msg):
        self.errors.append(f"{where}: {msg}")

    def note(self, where, msg):
        """Something worth knowing that is not wrong: the tree is still a valid record."""
        self.notes.append(f"{where}: {msg}")

    def load(self, p):
        twice = []

        def pairs(items):
            keys = [k for k, _ in items]
            twice.extend(sorted({k for k in keys if keys.count(k) > 1}))
            return dict(items)

        try:
            with open(p) as f:
                doc = json.load(f, object_pairs_hook=pairs, parse_constant=no_constant, parse_float=no_overflow)
        except FileNotFoundError:
            self.err(p, "missing")
            return None
        except ValueError as e:
            self.err(p, f"not JSON: {e}")
            return None
        if twice:
            self.err(p, f"gives the key {', '.join(dict.fromkeys(twice))} twice: which one is meant can't be told")
        return doc

    def document(self, kind, p, required=True):
        """Load one record and check it against its schema. Returns (document, whether it is valid);
        a document that fails its schema is still returned, so the checks that only need a name or an
        id can still run and report what is really wrong."""
        if not required and not os.path.isfile(p):
            return None, True
        doc = self.load(p)
        if doc is None:
            return None, False
        if not isinstance(doc, dict):
            self.err(p, "not a JSON object")
            return None, False
        return doc, self.schema_ok(kind, doc, p)

    def covered(self, folder, names, what):
        """folder/checksums.sha256 lists exactly names — no more, no fewer — and each digest is that of
        the file now there: the proof a write-once file is still the bytes that were written with it."""
        sums = os.path.join(folder, SUMS)
        if not os.path.isfile(sums):
            self.err(sums, f"missing: {what} is covered by a checksums file written with it "
                           f"({UPGRADE})")
            return
        entries, bad = read_sums(sums)
        for line in bad[:3]:
            self.err(sums, f"not a sha256sum line: {line[:80]!r}")
        for name in sorted(set(names) - set(entries)):
            self.err(sums, f"does not list {name}")
        for name in sorted(set(entries) - set(names)):
            self.err(sums, f"lists {name}, which is not {what}")
        for name in sorted(set(names) & set(entries)):
            f = os.path.join(folder, name)
            if os.path.isfile(f) and sha_file(f).split(":", 1)[1] != entries[name]:
                self.err(f, f"does not match the checksum {SUMS} recorded for it: changed after it was written")

    def schema_ok(self, kind, doc, where):
        errs = list(self.v[kind].iter_errors(doc))
        for e in sorted(errs, key=lambda e: list(map(str, e.absolute_path))):
            self.err(where, describe(e))
        for k, s in walk_strings(doc):
            if (k in ID_KEYS and ("\n" in s or "\r" in s)) or (k not in FREE_TEXT and s != s.rstrip("\r\n")):
                self.err(where, f"{k} contains a line break: {s!r}")
                errs.append(k)
        for k in walk_keys(doc):
            if k != k.strip():
                self.err(where, f"key {k!r} has surrounding whitespace")
                errs.append(k)
        return not errs

    # ------------------------------------------------------------ one item folder
    def item(self, d, expect_type, series=None):
        try:
            self._item(d, expect_type, series)
        except Exception as e:  # a malformed record must not stop the run
            self.err(d, f"cannot check: {type(e).__name__}: {e}")

    def _item(self, d, expect_type, series):
        ip = os.path.join(d, "item.json")
        item, valid = self.document("item", ip)
        if item is None:
            return
        folder = os.path.basename(d.rstrip("/"))
        if item.get("itemId") != folder:
            self.err(ip, f"itemId {item.get('itemId')} does not match folder {folder}")
        if item.get("type") != expect_type:
            self.err(ip, f"type {item.get('type')} where {expect_type} was expected")
        self.counts[expect_type] += 1
        self.covered(d, {"item.json"}, "item.json")

        allowed = SERIES_ENTRIES if expect_type == "series" else ITEM_ENTRIES
        for name in listdir(d):
            if name == "extras" and expect_type == "episode":
                self.err(os.path.join(d, name), "an episode has no extras: bonus material belongs to its series, "
                                                "in the series' own extras/ folder, where it may name its season")
            elif name not in allowed:
                self.err(os.path.join(d, name), f"unexpected entry in a {expect_type} folder"
                                                f"{'; media belongs in versions/<versionId>/' if expect_type != 'series' else ''}")
        events = self.events(d)
        removed = {e["versionId"] for e in events if e["kind"] == "version-removed" and e.get("versionId")}
        retired = {e["extraId"] for e in events if e["kind"] == "extra-removed" and e.get("extraId")}
        sources, versions, packages = {}, {}, {}
        if expect_type != "series":
            sources = self.sources(d)
            versions, packages = self.versions(d, sources, events, removed)
            self.originals_kept(d, sources, versions, events, removed)
            self.held_deletions(d, sources, events, versions)
        extras =self.extras(d, retired) if expect_type != "episode" else {}
        meta = self.metadata(d, item, versions, removed, extras, retired)
        if not valid:
            return  # the rules that span files assume the documented shape
        self.extra_seasons(d, expect_type, extras, meta)
        if expect_type == "episode":
            for e in events:
                if e["kind"] == "extra-removed":
                    self.err(e["where"], "an episode has no extras, so none can be removed from it")

        # the ids it was created with, and the ones the database holds now
        for where, ids in ((ip, item.get("externalIds") or {}),
                           (os.path.join(d, "metadata.json"), (meta or {}).get("externalIds") or {})):
            if expect_type == "movie" and set(ids) & {"tmdbTv", "tmdbSeason", "tmdbEpisode"}:
                self.err(where, "a movie carries series or episode reference ids")
            if expect_type == "series" and set(ids) & {"tmdbMovie", "tmdbSeason", "tmdbEpisode", "tmdbCollection"}:
                self.err(where, "a series carries movie, season or episode reference ids")
            if expect_type == "episode" and set(ids) & {"tmdbMovie", "tmdbCollection"}:
                self.err(where, "an episode carries movie reference ids")
        if expect_type == "episode":
            self.episode(ip, item, meta, series)
            if series is not None:
                # what the rules of a file that covers several episodes read of each episode, once they
                # have all been read: they span episode folders
                series["members"][item["itemId"]] = {
                    "dir": d, "item": item, "meta": meta, "sources": sources, "versions": versions,
                    "removed": removed,
                    "dropped": {e["sourceId"] for e in events if e["kind"] == "source-removed" and e.get("sourceId")}}
        elif expect_type == "movie":
            for sid, src in sorted(sources.items()):
                if src.get("covers"):
                    self.err(os.path.join(d, "sources", sid, "source.json"),
                             "covers names episodes, and a movie's file holds none: only an episode's file covers episodes")

        self.event_subjects(events, sources, versions, packages, removed, extras, retired)
        if expect_type == "series":
            self.series(d, item, meta)
        if self.check_media:
            self.hard_links(d)

    # ------------------------------------------------------------ metadata.json + metadata/
    def metadata(self, d, item, versions=(), removed=(), extras=(), retired=()):
        where = os.path.join(d, "metadata.json")
        meta, valid = self.document("metadata", where)
        if meta is None:
            return None
        if meta.get("itemId") != item.get("itemId"):
            self.err(where, "itemId differs from item.json")
        if meta.get("type") != item.get("type"):
            self.err(where, "type differs from item.json")
        if not valid:
            return None
        md = os.path.join(d, "metadata")
        seasons = {s["number"] for s in (meta.get("series") or {}).get("seasons") or []}
        numbers = [s["number"] for s in (meta.get("series") or {}).get("seasons") or []]
        if len(numbers) != len(set(numbers)):
            self.err(where, "series.seasons lists a season number twice")
        listed = self.images(md, meta["images"], "metadata.json")
        self.primaries(where, meta["images"], series=item.get("type") == "series")
        for img in meta["images"]:
            f = os.path.join(md, img["file"])
            if item.get("type") != "series" and img.get("season") is not None:
                self.err(f, "season-specific image on a non-series item")
            if img.get("season") is not None and img["season"] not in seasons:
                self.err(f, f"names season {img['season']}, which metadata.json does not describe")
        if os.path.isdir(md):
            for name in listdir(md):
                if name not in listed:
                    self.unlisted(os.path.join(md, name), "metadata.json")
        self.projected_decisions(where, meta, versions, removed, extras, retired)
        self.credits(where, meta)
        return meta

    def images(self, folder, entries, owner):
        """The images a projection lists, each against the file it names in folder: there, named by
        the hash of its own content, of the recorded size, type and dimensions. An entry is an image
        as one kind, so bytes that are several kinds — a backdrop that is the poster — are an entry
        of each, sharing the one file; a kind lists a file once (in a series once for the series
        itself and once for each season). Returns the names listed."""
        listed, kinds, digests = set(), set(), {}
        for img in entries:
            name = img["file"]
            f = os.path.join(folder, name)
            as_kind = (name, img["kind"], img.get("season"))
            if as_kind in kinds:
                self.err(f, f"listed twice in {owner} as a {img['kind']}"
                            + (f" of season {img['season']}" if img.get("season") is not None else ""))
            kinds.add(as_kind)
            listed.add(name)
            if not os.path.isfile(f):
                self.err(f, f"listed in {owner} but does not exist")
                continue
            self.counts["images"] += 1
            if name not in digests:  # a file several kinds share is hashed once
                digests[name] = sha_file(f)
            digest = digests[name]
            if digest != img["sha256"]:
                self.err(f, f"sha256 does not match {owner}")
            if name.rsplit(".", 1)[0] != digest.split(":", 1)[1]:
                self.err(f, "file name is not the hash of its own content")
            if os.path.getsize(f) != img["sizeBytes"]:
                self.err(f, f"size {os.path.getsize(f)} != recorded {img['sizeBytes']}")
            ctype, w, h = sniff_image(f)
            if ctype != img["contentType"]:
                self.err(f, f"content is {ctype or 'not a known image'} but recorded as {img['contentType']}")
            if EXT_TYPE.get(name.rsplit(".", 1)[-1]) != img["contentType"]:
                self.err(f, f"extension does not match {img['contentType']}")
            for label, actual, recorded in (("width", w, img.get("width")), ("height", h, img.get("height"))):
                if recorded is not None and actual is None:
                    self.err(f, f"{label} is recorded as {recorded} but cannot be read from the file")
                elif recorded is not None and actual != recorded:
                    self.err(f, f"{label} is {actual} but recorded as {recorded}")
        return listed

    def primaries(self, where, entries, series=False):
        """At most one image of a kind is primary in a projection: the one a reader shows. In a series'
        metadata.json that is one per kind and season, since a season's poster stands for the season,
        not the series. A schema cannot count, so the rule is here."""
        marked = {}
        for img in entries:
            if img.get("primary") is True:
                marked.setdefault((img["kind"], img.get("season")), []).append(img["file"])
        for (kind, season), files in sorted(marked.items(), key=lambda x: (x[0][0], x[0][1] is not None, x[0][1] or 0)):
            if len(files) > 1:
                scope = "" if not series else " for the series itself" if season is None else f" for season {season}"
                self.err(where, f"{len(files)} {kind} images{scope} are primary ({', '.join(f[:12] + '…' for f in files)}): "
                                f"at most one image of a kind may be"
                                + (", for the series itself and for each season" if series else ""))

    def unlisted(self, f, owner):
        """A file beside a projection that the projection does not list. An image named by the hash of
        its own bytes is one an earlier projection listed and a later one dropped: garbage the sweep
        collects, not a broken record, so it is a note. Anything else is an error."""
        m = IMAGE_NAME.match(os.path.basename(f))
        if m and os.path.isfile(f) and sha_file(f).split(":", 1)[1] == m.group(1):
            self.note(f, f"an image {owner} no longer lists; {SWEEP}")
        elif m and os.path.isfile(f):
            self.err(f, f"not listed in {owner}, and not named by the hash of its own content")
        else:
            self.err(f, f"not listed in {owner}" + ("" if owner == "metadata.json" else
                                                     f": not person.json and not an image {owner} lists"))

    def credits(self, where, meta):
        """A credit keys a person the database holds. Without a record in people/ the tree cannot
        restore that person's texts and images, which is worth saying — but the person may simply
        not have been projected yet, so it is a note."""
        for c in meta.get("credits") or []:
            pid = c["personId"]
            if not os.path.isfile(os.path.join(self.root_dir or "", "people", pid[:2], pid, "person.json")):
                self.note(where, f"credit {c['name']!r} ({c['role']}) names person {pid}, who has no "
                                 f"people/{pid[:2]}/{pid}/person.json yet")

    def projected_decisions(self, where, meta, versions, removed, extras=(), retired=()):
        """metadata.library holds what the database decided about this item's storage, so every
        version and every extra it names has to be one that is really there. The versions a covered
        episode names are its holder's, in another episode's folder, and covering() checks them once
        every episode of the series has been read."""
        lib = meta.get("library") or {}
        primary = lib.get("primaryVersionId")
        if primary and primary not in versions and not lib.get("coveredBy"):
            self.err(where, f"library.primaryVersionId {primary} names "
                            + ("a version that was removed" if primary in removed else "no version folder"))
        for vid in sorted(lib.get("versionLabels") or {}) if not lib.get("coveredBy") else []:
            if vid not in versions:
                self.err(where, f"library.versionLabels names {vid}, which is "
                                + ("a removed version" if vid in removed else "no version folder"))
        for xid in sorted(lib.get("extras") or {}):
            if xid not in extras:
                self.err(where, f"library.extras names {xid}, which is "
                                + ("a removed extra" if xid in retired else "no extras/<extraId>/ folder of this item"))

    # ------------------------------------------------------------ events/
    def events(self, d):
        """events/<YYYYMMDDTHHMMSSZ>-<eventId8>-<kind>/, each holding event.json and the checksums
        written with it."""
        base = os.path.join(d, "events")
        out = []
        if not os.path.isdir(base):
            return out
        for name in listdir(base):
            p = os.path.join(base, name)
            m = EVENT_FOLDER.match(name)
            if not m or not os.path.isdir(p):
                self.err(p, f"an event record in the layout before 2026-10-02 (b): each event is a folder "
                            f"events/<YYYYMMDDTHHMMSSZ>-<eventId8>-<kind>/ holding event.json ({UPGRADE})"
                         if OLD_EVENT_FILE.match(name) and os.path.isfile(p) else
                         "not an events/<YYYYMMDDTHHMMSSZ>-<8 hex of the eventId>-<kind>/ folder")
                continue
            ep = os.path.join(p, "event.json")
            for entry in listdir(p):
                if entry not in ("event.json", SUMS):
                    self.err(os.path.join(p, entry), "not event.json or the checksums written with it")
            ev, valid = self.document("event", ep)
            if ev is None or not valid:
                continue
            self.counts["events"] += 1
            self.covered(p, {"event.json"}, "event.json")
            if m.group(3) != ev["kind"]:
                self.err(ep, f"folder name says {m.group(3)} but the record's kind is {ev['kind']}")
            if m.group(1) != stamp_of(ev["at"]):
                self.err(ep, f"folder name says {m.group(1)} but the record happened at {stamp_of(ev['at'])}")
            if not ev["eventId"].startswith(m.group(2)):
                self.err(ep, f"folder name says {m.group(2)}, which does not start the eventId {ev['eventId']}")
            out.append({"where": ep, **ev})
        return out

    # ------------------------------------------------------------ sources/
    def sources(self, d):
        """sources/<sourceId>/, each holding source.json, the probe and sidecars it names, and the
        checksums written with all of them."""
        base = os.path.join(d, "sources")
        records = {}
        if not os.path.isdir(base):
            return records
        for name in listdir(base):
            p = os.path.join(base, name)
            if not os.path.isdir(p):
                self.err(p, f"a source record in the layout before 2026-10-02 (b): each source is a folder "
                            f"sources/<sourceId>/ holding source.json ({UPGRADE})"
                         if name.endswith(".json") and UUID.match(name[:-5]) else "not a sources/<sourceId>/ folder")
                continue
            if not UUID.match(name):
                self.err(p, "a source folder is named by its sourceId")
                continue
            sp = os.path.join(p, "source.json")
            if not os.path.isfile(sp):
                self.err(p, "a source folder without its source.json record")
                continue
            src, valid = self.document("source", sp)
            if src is None or not valid:
                continue
            self.counts["sources"] += 1
            if src["sourceId"] != name:
                self.err(sp, f"sourceId {src['sourceId']} does not match the folder name")
                continue
            records[name] = src
            self.source_files(d, sp, src)
            self.neutral_source(d, sp, src)
            self.attachments(sp, src)
            self.captions(sp, src, d)
            named = {"source.json"} | {os.path.basename(x["file"]) for x in src.get("sidecars") or []} | \
                ({os.path.basename(src["probe"]["file"])} if src["probe"].get("file") else set())
            for entry in listdir(p):
                if entry not in named and entry != SUMS:
                    self.err(os.path.join(p, entry), "not the record, the probe, a sidecar or the checksums of this source")
            self.covered(p, named, "source.json, its probe or one of its sidecars")
        return records

    def attachments(self, where, src):
        """An attachment is a font by its MIME type or its name, or by the signature its first bytes are,
        which its stream then says (signature) — and only a font says one. A record written before
        record logic 10 that calls a font by its MIME type or name something else is a note."""
        for x in src.get("streams") or []:
            if x.get("type") != "attachment":
                continue
            claims = bool(FONT_MIME.search(x.get("mimetype") or "") or FONT_FILE.search(x.get("filename") or ""))
            if x.get("signature") and x.get("role") != "font":
                self.err(where, f"attachment stream {x['index']} says a font's signature, {x['signature']}, "
                                f"but its role is {x.get('role')}")
            elif x.get("role") == "font" and not claims and not x.get("signature"):
                self.err(where, f"attachment stream {x['index']} is a font by neither its MIME type nor its name, "
                                f"and says no signature its bytes are")
            elif claims and x.get("role") != "font":
                # a record written before record logic 10 read fewer MIME types and names as a font's, and
                # keeps what it said: its source counts no font there, so the deletion gate holds none
                self.note(where, f"attachment stream {x['index']} is a font by its MIME type or its name, but its "
                                 f"record calls it {x.get('role')}: the deletion gate counts no font there")

    def windows(self, where, what, windows):
        """A window that decoded no frame could not be read, and shows no captions (false, or a reader's
        null); one that decoded frames says whether they show them; each fraction once, none ending before
        it starts, and none counting more interlaced or repeating frames than it decoded."""
        for w in windows or []:
            if w["frames"] == 0 and w["captions"]:
                self.err(where, f"{what}'s caption window at {w['at']} decoded no frame and says it shows "
                                f"captions: a window that could not be read shows none")
            elif w["frames"] > 0 and w["captions"] is None:
                self.err(where, f"{what}'s caption window at {w['at']} decoded {w['frames']} frames and says "
                                f"nothing of captions: null is a failed window's, of no frame")
            if w["end"] < w["start"]:
                self.err(where, f"{what}'s caption window at {w['at']} ends before it starts")
            for k in ("interlacedFrames", "repeatFrames"):
                if w.get(k, 0) > w["frames"]:
                    self.err(where, f"{what}'s caption window at {w['at']} counts {w[k]} {k} of the {w['frames']} "
                                    f"frames it decoded")
        ats = [w["at"] for w in windows or []]
        if len(ats) != len(set(ats)):
            self.err(where, f"{what} keeps a caption window at one fraction twice")
        wholes = [w for w in windows or [] if w.get("whole")]
        if wholes and (len(windows) != 1 or wholes[0]["at"] != 0):
            self.err(where, f"{what} keeps a read of the whole picture beside other windows, or not at 0: a read is "
                            f"the plan's six windows, or the whole picture alone")
        for w in wholes:
            if w.get("packets") is None or w["packets"] < w["frames"]:
                self.err(where, f"{what}'s read of the whole picture counts {w.get('packets')} packets of the "
                                f"{w['frames']} frames it decoded")

    def captions(self, where, src, d=None):
        """A source's essence knows its closed captions exactly when the shared rule says so (captions_known): a
        stream that is closed captions of its own, its picture's caption signature, a window that shows them, or
        a read that tells none (windows_tell_none, by the plan over the span its kept probe gives: plan_for) of a
        file on the closed lists and, of a picture in one of SCANNED_CODECS, whose packets carry no caption
        signature. It carries them when such a stream, the
        signature or a window says so, or the probe did; its picture is in fields where a window counted
        interlaced frames. Its kept probe is a probe, its offList what the closed lists say of it (off_list),
        and its picture's field order the one the probe gives."""
        streams = src.get("streams") or []
        picture = picture_of(streams) or {}
        essence = src.get("essence") or {}
        windows = windows_of(picture)
        self.windows(where, "the picture", windows)
        own = any(x.get("closedCaptions") for x in streams if x.get("type") in ("subtitle", "data"))
        signed = picture.get("captionSignature") is True
        shows = windows_show(windows)
        if bool(essence.get("closedCaptionsKnown")) != captions_known(streams, record_off_list(src), plan_for(d, src)):
            self.err(where, "the essence knows the closed captions exactly when a stream that is closed captions "
                            "of its own, the picture's caption signature or its caption read tell them: a window "
                            "shows them, or the read tells none by the plan - the six windows at their places, on "
                            "frames of their own, each of half its packets and 24 frames, or the whole picture, over the span its "
                            "probe gives -, of a file on the closed lists, and of HEVC whose packets carry no caption "
                            "signature")
        if (shows or signed) and not picture.get("closedCaptions"):
            self.err(where, "a caption window or the caption signature of the picture shows closed captions its "
                            "stream does not say")
        if (own or picture.get("closedCaptions")) and essence and not essence.get("closedCaptions"):
            self.err(where, "a stream carries closed captions the essence does not say")
        pf = (src.get("probe") or {}).get("file")
        if d and isinstance(pf, str) and os.path.isfile(os.path.join(d, pf)):
            probe = readable(os.path.join(d, pf))
            why = probe_shape(probe)
            if why:
                self.err(where, f"{why} ({pf}): the deletion gate keeps its original, and the record says what it "
                                f"can't hold it to")
            elif "offList" in src:  # a record of record logic 11, which writes all of these from its probe
                if (src.get("container") or {}).get("format") != probe["format"].get("format_name"):
                    self.err(where, f"the container's format is {(src.get('container') or {}).get('format')!r}, where its "
                                    f"probe gives {probe['format'].get('format_name')!r}")
                if isinstance(src.get("offList"), list) and src["offList"] != off_list(probe):
                    self.err(where, f"offList says {src['offList']}, where the closed lists say {off_list(probe)} "
                                    f"of its probe")
                film = film_of(probe)
                if film is not None and (not picture or picture.get("index") != film.get("index")):
                    self.err(where, f"the record's picture is stream {picture.get('index') if picture else None}, where "
                                    f"its probe's film (by the closed lists' cover rule) is stream {film.get('index')}")
                elif film is not None and picture.get("codec") != film.get("codec_name"):
                    self.err(where, f"the picture's codec is {picture.get('codec')!r}, where its probe's film is "
                                    f"{film.get('codec_name')!r}")
                elif film is not None and picture.get("fieldOrder") != film.get("field_order"):
                    self.err(where, f"the picture's field order is {picture.get('fieldOrder')!r}, where its probe "
                                    f"gives {film.get('field_order')!r}")
        if windows is not None and windows_count_fields(windows) and essence and essence.get("interlaced") is not True:
            self.err(where, "a caption window of the picture counted interlaced frames, and the essence does not "
                            "say the picture is in fields (interlaced)")

    def deletion_read(self, where, read, at=None, plan=None):
        """The read an original-deleted event carries (captionsRead): its windows hold as a picture's, it shows
        no captions, it counted no interlaced frame, it was made no later than the deletion at (ADR-0015), and the
        span it reports is its record's - the plan's span of the record's kept probe (plan) - within the plan's
        tolerance at either end. One that does not tell the original's captions by the plan (windows_tell_none:
        the six windows, or the whole picture read; a read of five windows without the start, made before the
        plan, among them) is a note: it tells nothing, and the gate holds the deletion to what the records tell
        (Checker.version)."""
        windows = read.get("windows") or []
        self.windows(where, "the read", windows)
        span = read.get("span") if isinstance(read.get("span"), dict) else {}
        first, last = gate_number(span.get("start")), gate_number(span.get("end"))
        if first is not None and last is not None and plan is not None:
            tol = caption_tolerance(plan)
            if abs(first - plan["start"]) > tol or abs(last - plan["end"]) > tol:
                self.err(where, f"the read it carries reports the span {span['start']}-{span['end']} s, where its "
                                f"record's probe gives {plan['start']:.3f}-{plan['end']:.3f} s: not within one frame "
                                f"step and 0.5 s ({tol:.3f} s)")
        if not windows:
            self.err(where, "the read it carries has no window: a read that tells nothing allows no deletion")
        elif windows_show(windows):
            self.err(where, "the read it carries shows closed captions: an original that has them is not retired "
                            "(ADR-0015)")
        elif not windows_tell_none(read, plan):
            self.note(where, "the read it carries does not tell the original's captions by the plan (the six windows, "
                             "or the whole picture read, over its record's span): a window failed, lies off its place, "
                             "shares its frames with another or decoded fewer than half its packets, the start is missing, "
                             "its span is not its record's, or its record's probe gives no span; it tells nothing")
        if windows_count_fields(windows):
            self.err(where, "the read it carries counted interlaced frames: an original in fields is not retired "
                            "(ADR-0015)")
        if at and read.get("at") and moment(read["at"]) > moment(at):
            self.err(where, f"the read it carries is dated {read['at']}, after the deletion it allowed ({at})")

    def source_files(self, d, where, src):
        pf, psha = src["probe"].get("file"), src["probe"].get("sha256")
        if pf:
            if pf != f"sources/{src['sourceId']}/ffprobe.json":
                self.err(where, f"probe file {pf} is not sources/{src['sourceId']}/ffprobe.json")
            f = os.path.join(d, pf)
            if not os.path.isfile(f):
                self.err(f, "probe file missing")
            elif sha_file(f) != psha:
                self.err(f, "probe sha256 does not match the source record")
        elif psha:
            self.err(where, "records a probe sha256 but no probe file")
        for sc in src.get("sidecars") or []:
            if not sc["file"].startswith(f"sources/{src['sourceId']}/"):
                self.err(where, f"sidecar {sc['file']} is not in sources/{src['sourceId']}/")
            f = os.path.join(d, sc["file"])
            if not os.path.isfile(f):
                self.err(f, "sidecar missing")
                continue
            if "sizeBytes" in sc and os.path.getsize(f) != sc["sizeBytes"]:
                self.err(f, "sidecar size does not match the source record")
            if "sha256" in sc and sha_file(f) != sc["sha256"]:
                self.err(f, "sidecar sha256 does not match the source record")

    def neutral_source(self, d, where, src):
        """A source record names no file as it arrived and says nothing of where it came from: its file is
        named as the library names an original, it has no origin, it keeps no container title tag, its
        sidecars are copies of subtitle files named as the library names them, without the names they
        came with, and its probe names the file as the record does and keeps no title tag either."""
        name = src["file"]["name"]
        if not ORIGINAL_NAME.match(name):
            self.err(where, f"file.name {name!r} is a name the original arrived under, not the one the library gives "
                            f"it (original.<ext>, original-<n>.<ext>): no record names a file as it arrived ({NEUTRAL})")
        if "origin" in src:
            self.err(where, f"origin names where the original came from, which no record says ({NEUTRAL})")
        tags = src["container"].get("tags") or {}
        if src["container"].get("title") is not None or any(str(k).lower() == "title" for k in tags):
            self.err(where, f"keeps the container's title tag, which routinely names where a file came from ({NEUTRAL})")
        for sc in src.get("sidecars") or []:
            copy = os.path.basename(sc["file"])
            if sc["kind"] != "subtitle" or not SUBTITLE_COPY.match(copy):
                self.err(where, f"sidecar {sc['file']} is not the copy of a subtitle file under the name the library "
                                f"gives it (subtitle-<n>.<lang>[.forced][.sdh].<ext>), and nothing else that came with "
                                f"the original is kept ({NEUTRAL})")
            if "originalName" in sc:
                self.err(where, f"sidecar {sc['file']} names the file it was copied from (originalName) ({NEUTRAL})")
        pf = src["probe"].get("file")
        f = os.path.join(d, pf) if pf else None
        if not f or not os.path.isfile(f):
            return  # source_files says so
        try:
            fmt = (load_strict(f) or {}).get("format")
        except (OSError, ValueError, AttributeError) as e:
            self.err(f, f"the probe cannot be read: {e}")
            return
        if not isinstance(fmt, dict):
            return
        if "filename" in fmt and fmt["filename"] != name:
            self.err(f, f"format.filename {str(fmt['filename'])[:80]!r} is not the name the library gives the original, "
                        f"{name}: a probe names the file as its record does ({NEUTRAL})")
        if any(str(k).lower() == "title" for k in fmt.get("tags") or {}):
            self.err(f, f"keeps the container's title tag (format.tags.title), which a probe the library keeps leaves "
                        f"out ({NEUTRAL})")

    # ------------------------------------------------------------ versions/
    def held_deletions(self, d, sources, events, versions):
        """Every original-deleted event is held to the gate of the source it names, whatever became of its version:
        one whose version this item no longer counts - a version-removed event removed it, or its folder is gone -
        gets the date rule (RULE_FROM) and its read the checks (deletion_read) one of a version that is there gets
        (Checker.version), its source's essence as every read of it in the item tells it. Its package gone, what it
        accepted beyond the gate's own keys can't be held to it. From RULE_FROM on an event names the source whose
        original it deletes, one event a source, so that no later removal changes what it is held to; an older one
        that names none, its version gone, can no longer be held to its gate: a note."""
        reads = reads_by_source(events)
        for e in events:
            if e["kind"] != "original-deleted":
                continue
            sid = e.get("sourceId")
            if not sid and moment(e["at"]) >= RULE_FROM:
                self.err(e["where"], "names no source: from 2026-11-16 on an original-deleted event names the source "
                                     "whose original it deletes (sourceId), one event a source")
            if e.get("versionId") in versions:
                continue
            if not sid:
                self.note(e["where"], f"names no source, and its version {e.get('versionId')} is gone: it can no "
                                      f"longer be held to its gate")
            never = sorted(set(e.get("accepted") or []) & set(NEVER_ACCEPTED))
            if never:
                self.err(e["where"], f"accepted names {', '.join(never)}, which no deletion accepts: an original that "
                                     f"has closed captions or whose captions are not known, and one in fields, are "
                                     f"kept (ADR-0015)")
            if e.get("captionsRead"):
                self.deletion_read(e["where"], e["captionsRead"], e.get("at"), plan_for(d, sources.get(sid)) if sid else None)
            if not sid:
                continue
            src = sources.get(sid)
            of = reads.get(sid, [])
            essences = [essence_as_read(src["essence"], of, plan_for(d, src))] \
                if isinstance(src, dict) and isinstance(src.get("essence"), dict) else []
            held = title_holds(d, [src], of)
            if not essences:
                held.setdefault("closedCaptions", "the source it deleted is not known, or its record can't be read")
                held.setdefault("interlaced", "the source it deleted is not known, or its record can't be read")
            keeps = sorted(set(deletion_gate(essences, {}, held)) & set(NEVER_ACCEPTED))
            if keeps:
                self.gate_rule(e, keeps, held, essences, src)

    def gate_rule(self, e, keeps, held, essences, src=None):
        """The date rule of an original-deleted event e whose gate keeps its original for keeps (ADR-0015): from
        RULE_FROM on an error, before it a note of what was lost; at any date a note where the event records an
        original the library never held (never_held, its source's record src)."""
        why = "; ".join(f"{k}: {held.get(k) or kept_why(k, essences)}" for k in keeps)
        if never_held(e, src):
            self.note(e["where"], f"records an original the library never held - its source record keeps no fixity, "
                                  f"and it accepted nothing and carries no read -: history, not a deletion ({why})")
        elif moment(e["at"]) >= RULE_FROM:
            self.err(e["where"], f"deletes an original the gate keeps ({why}): from 2026-11-16 on, no original "
                                 f"that has closed captions, whose captions are not known, or that is in "
                                 f"fields is retired (ADR-0015)")
        else:
            self.note(e["where"], f"deleted, before 2026-11-16, an original the gate keeps today ({why}): what "
                                  f"it carried of them is lost with it")

    def originals_kept(self, d, sources, versions, events, removed):
        """Every source a version of the item is made of has its original listed by a version folder - one a
        version-removed event removed too, while its folder is there - or an original-deleted event that
        names it says it was deleted. An original gone without one - its folder removed by hand, or a package
        a tree of v1 kept alone - is gone outside the record: a note, as the rebuild counts it deleted."""
        folders = dict(versions)
        for vid in removed:
            try:
                with open(os.path.join(d, "versions", vid, "version.json"), encoding="utf-8") as f:
                    folders[vid] = json.load(f)
            except (OSError, ValueError):
                continue
        name_of = lambda s: (sources.get(s) or {}).get("file", {}).get("name")  # noqa: E731
        listed = {s for v in folders.values() if isinstance(v, dict) for s in v.get("sourceIds") or []
                  if s in sources and name_of(s) in (v.get("originalFiles") or [])}
        # an original a version lists, kept or deleted since, is one the library held: its record carries its
        # fixity, so a record without one is an original the library never held (never_held)
        for s in sorted(listed):
            if isinstance(sources[s].get("file"), dict) and "fixity" not in sources[s]["file"]:
                self.err(os.path.join(d, "sources", s, "source.json"),
                         f"keeps no fixity, though a version lists its original ({name_of(s)}): the record of an "
                         f"original the library holds or held carries the file's fixity")
        deleted = {e["sourceId"] for e in events if e["kind"] == "original-deleted" and e.get("sourceId")}
        for vid, v in sorted(versions.items()):
            for s in v["sourceIds"]:
                if s in sources and s not in listed and s not in deleted:
                    self.note(os.path.join(d, "versions", vid, "version.json"),
                              f"is made of source {s}, whose original no version folder lists and no "
                              f"original-deleted event says was deleted: it is gone outside the record, and the "
                              f"rebuild counts it as deleted, its loss as the gate says")

    def versions(self, d, sources, events, removed):
        base = os.path.join(d, "versions")
        versions, packages = {}, {}
        if os.path.isdir(base):
            for name in listdir(base):
                p = os.path.join(base, name)
                if not os.path.isdir(p):
                    self.err(p, "every version is a folder under versions/")
                    continue
                if not UUID.match(name):
                    self.err(p, "a version folder is named by its versionId")
                    continue
                if name in removed:
                    continue  # a version-removed event says to ignore this folder, so nothing in it counts
                got = self.version(d, p, name, sources, events)
                if got:
                    versions[name] = got[0]
                    if got[1]:
                        packages[got[1]["packageId"]] = got[1]
        return versions, packages

    def version(self, d, vp, vid, sources, events):
        where = os.path.join(vp, "version.json")
        v, valid = self.document("version", where)
        if v is None or not valid:
            return None
        self.counts["versions"] += 1
        if v["versionId"] != vid:
            self.err(where, f"versionId {v['versionId']} does not match folder {vid}")
        for name in listdir(vp):
            if name not in VERSION_ENTRIES and name not in v["originalFiles"]:
                self.err(os.path.join(vp, name), "not a record, the package or an original this version names")
        for sid in v["sourceIds"]:
            if sid not in sources:
                self.err(where, f"names source {sid}, which has no record under sources/")
        named = {sources[s]["file"]["name"] for s in v["sourceIds"] if s in sources}
        for name in v["originalFiles"]:
            if named and name not in named:
                self.err(where, f"originalFiles names {name}, which is not the file name of any source this version names")
            if not ORIGINAL_NAME.match(name):
                self.err(where, f"originalFiles names {name!r}, which is not a name the library gives an original "
                                f"(original.<ext>, original-<n>.<ext>) ({NEUTRAL})")
        for kind in ("chapters", "segments"):
            marks = v.get(kind) or []
            if any(m["endMs"] < m["startMs"] for m in marks):
                self.err(where, f"a {kind[:-1]} ends before it starts")
        chapters = v.get("chapters") or []
        if any(chapters[i]["startMs"] > chapters[i + 1]["startMs"] for i in range(len(chapters) - 1)):
            self.err(where, "chapters are not in timeline order")
        if bool(chapters) != (v.get("chaptersFrom") is not None):
            self.err(where, "chaptersFrom must be set exactly when there are chapters")
        if any((sources[s]["essence"].get("chapters") for s in v["sourceIds"] if s in sources)) and not chapters:
            self.err(where, "the original carries chapter marks but the version keeps none")

        marker = os.path.isfile(os.path.join(vp, ".complete"))
        pkg, pkg_valid = self.document("package", os.path.join(vp, "package.json"), required=False)
        pkg = pkg if pkg_valid else None
        has_package = os.path.isfile(os.path.join(vp, "package.json"))
        if has_package != marker:
            self.err(vp, "package.json exists exactly when .complete does, and here only "
                         + ("package.json" if has_package else ".complete") + " is present")
        elif not marker and any(os.path.exists(os.path.join(vp, n)) for n in PACKAGE_DIRS + (SUMS,)
                                if not (n == "subs" and attached_only(vp))):
            # a version taken in keeps the subtitles attached to it since in its subs/, which is no package
            self.note(vp, f"a package that never finished: no .complete beside it; {SWEEP}")
        if pkg is not None:
            self.counts["packages"] += 1
            made_from = [sources[s] for s in v["sourceIds"] if s in sources]
            self.package(vp, v, pkg, streams=made_from[0].get("streams") or None if len(made_from) == 1 else None)
            self.from_sidecars(vp, v, pkg, sources)
            if marker:
                self.chain(vp)
        deletions = [x for x in events if x["kind"] == "original-deleted" and x.get("versionId") == vid]
        # every read of a source in the item, whichever version's event carries it; one of an event that names no
        # source is its version's one source's
        reads_of = reads_by_source(events)
        own = [e["captionsRead"] for e in deletions if "captionsRead" in e and not e.get("sourceId")]

        def reads_for(sid):
            return reads_of.get(sid, []) + (own if len(v["sourceIds"]) == 1 else [])

        def gate_of(sids):
            """The gate of the sources sids of this version, as their records, probes, the title's packages and
            every read their deletions carry tell it, and what holds it beyond their essences."""
            recs = [sources.get(s) for s in sids]
            essences = [essence_as_read(r["essence"], reads_for(s), plan_for(d, r)) for s, r in zip(sids, recs)
                        if isinstance(r, dict) and isinstance(r.get("essence"), dict)]
            held = title_holds(d, recs, [x for s in sids for x in reads_for(s)])
            if len(essences) < len(sids):
                held.setdefault("closedCaptions", "a source record of it can't be read")
                held.setdefault("interlaced", "a source record of it can't be read")
            return set(deletion_gate(essences, (pkg or {}).get("essence") or {}, held)), held, essences

        gate = gate_of(v["sourceIds"])[0]
        gone, whole = set(), False
        for e in deletions:
            if not v["originalFiles"] and not e.get("sourceId"):
                self.err(e["where"], f"version {vid} keeps no original in its folder, so an original-deleted event for "
                                     f"it records one deleted outside the record, and names its source (sourceId)")
            never = sorted(set(e.get("accepted") or []) & set(NEVER_ACCEPTED))
            if never:
                self.err(e["where"], f"accepted names {', '.join(never)}, which no deletion accepts: an original that "
                                     f"has closed captions or whose captions are not known, and one in fields, are "
                                     f"kept (ADR-0015)")
            if e.get("captionsRead"):
                # one that names no source is held to no record's span: none it could be held to once its version
                # is gone
                self.deletion_read(e["where"], e["captionsRead"], e.get("at"),
                                   plan_for(d, sources.get(e["sourceId"])) if e.get("sourceId") else None)
            egate, eheld, eess = gate_of([e["sourceId"]] if e.get("sourceId") else v["sourceIds"])
            keeps = sorted(egate & set(NEVER_ACCEPTED))
            if keeps:
                self.gate_rule(e, keeps, eheld, eess, sources.get(e.get("sourceId")))
            over = sorted(set(e.get("accepted") or []) - gate - set(NEVER_ACCEPTED))
            if over:
                self.err(e["where"], f"accepted names {', '.join(over)}, which the records do not say the "
                                     f"package failed to carry; a person may accept less than the gate, never more")
            if e.get("sourceId"):
                if e["sourceId"] not in v["sourceIds"]:
                    self.err(e["where"], f"names source {e['sourceId']}, which version {vid} was not made from")
                gone.add(e["sourceId"])
            else:
                whole = True
        if self.check_media:
            self.media(vp, v, pkg, sources, gone, whole)
        return v, pkg

    def package(self, vp, v, pkg, what="version", streams=None):
        """A package and the record it was made for, v: a version's or an extra's. streams are the streams
        of the one record it was made from, as that record keeps them — None where that can't be told."""
        where = os.path.join(vp, "package.json")
        if pkg["fidelity"]["lossless"] != (not pkg["fidelity"]["losses"]):
            self.err(where, "fidelity.lossless must be true exactly when losses is empty")
        if (pkg["role"] == "canonical") != (not v["originalFiles"]):
            self.err(where, f"role must be 'canonical' exactly when the {what} keeps no original "
                            f"({what}.json originalFiles is empty)"
                            + ("; a deletion afterwards is an event, not a rewrite" if what == "version" else ""))
        audio = pkg["renditions"]["audio"]
        surround = pkg["renditions"].get("audioSurround") or []
        video = pkg["renditions"]["video"]
        subs = pkg.get("subtitles") or []
        for kind, items in (("video", video), ("audio", audio + surround), ("subtitle", subs)):
            ids = [x["id"] for x in items]
            if len(ids) != len(set(ids)):
                self.err(where, f"{kind} rendition ids are not unique")
        for x in video:
            if x.get("hdr10Metadata") and x.get("videoRange") != "PQ":
                self.err(where, f"video rendition {x['id']} says its init segment carries HDR10 static metadata, "
                                f"which only a rendition in PQ carries")
        if audio and sum(1 for a in audio if a.get("default")) != 1:
            self.err(where, f"exactly one audio rendition must be default, found {sum(1 for a in audio if a.get('default'))}")
        if sum(1 for a in surround if a.get("default")) > 1:
            self.err(where, "more than one 5.1 companion is default")
        if sum(1 for s in subs if s.get("default") and not s.get("forced")) > 1:
            self.err(where, "more than one non-forced subtitle is default")
        for kind, x in [("audio", a) for a in audio + surround] + [("subtitle", s) for s in subs]:
            if x.get("purposeFrom") == "assumed" and x.get("purpose") not in ("dialogue", "main"):
                self.err(where, f"{kind} {x['id']} purpose {x.get('purpose')} cannot be assumed; only dialogue or main can")
            if (x.get("purposeFrom") is None) != (x.get("purpose") in (None, "unknown")):
                self.err(where, f"{kind} {x['id']} needs purposeFrom exactly when its purpose is known")
        for x in subs:
            if x.get("forced") and x.get("purpose") not in (None, "forced", "signs-songs", "unknown"):
                self.err(where, f"subtitle {x['id']} is flagged forced but its purpose is {x['purpose']}")
            if x.get("default") and (x.get("forced") or x.get("purpose") in ("forced", "signs-songs")):
                self.err(where, f"subtitle {x['id']} is a forced track flagged default: a reader would open with it instead of no subtitles")
        # a styled original and a font file are files of the package: in its subs/, each one its checksums list
        cs = pkg["checksums"]
        sums = os.path.join(vp, cs["file"])
        own = set(read_checksums(sums)) if not cs.get("pending") and os.path.isfile(sums) else None
        for named, path in [(f"the styled original of subtitle {x['id']}", x["styled"]["path"]) for x in subs
                            if x.get("styled")] + [("a font file", f["path"]) for f in pkg.get("fonts") or []]:
            if not path.startswith("subs/") or path in {x["path"] for x in subs}:
                self.err(where, f"{named} is {path}: a package keeps it in its subs/, a file of its own")
            elif own is not None and path not in own:
                self.err(where, f"{named} is {path}, which is no file of the package: {cs['file']} lists none of "
                                f"that name")
        # the essence has them when the package carries them for every stream of its source that has them,
        # by the stream each was made from (libv2_records.package_essence); of a source whose streams can't
        # be told, when it carries any
        essence = pkg.get("essence") or {}
        styled = {x.get("sourceStreamIndex") for x in subs if x.get("styled")}
        fonts = {f.get("sourceStreamIndex") for f in pkg.get("fonts") or []}
        for flag, taken, has, said in (
                ("styledSubtitles", styled, lambda x: x.get("type") == "subtitle" and x.get("styled"),
                 "styled subtitle stream(s) {} of the source keep no styled original in the package"),
                ("fonts", fonts, lambda x: x.get("type") == "attachment" and x.get("role") == "font",
                 "font attachment(s) {} of the source are no font file of the package")):
            if not essence.get(flag):
                continue
            left = [x.get("index") for x in streams or [] if has(x) and x.get("index") not in taken]
            if not taken:
                self.err(where, "the essence has styled subtitles, but no subtitle keeps its styled original"
                         if flag == "styledSubtitles" else "the essence has fonts, but the package lists no font file")
            elif left:
                self.err(where, f"the essence has {'styled subtitles' if flag == 'styledSubtitles' else 'fonts'}, "
                                f"but the " + said.format(", ".join(str(i) for i in left)))
        # what the picture is as far as fields go: the essence has what the record's scan says was done
        if essence.get("scan") != (pkg.get("scan") if isinstance(pkg.get("scan"), dict) else {}).get("done"):
            self.err(where, f"the essence's scan is {essence.get('scan')}, but the package's scan says "
                            f"{(pkg.get('scan') or {}).get('done')} was done")
        if essence.get("closedCaptionsKnown"):
            self.err(where, "a package's essence never knows closed captions it did not read from its own frames: "
                            "closedCaptionsKnown is a source's")
        if bool(essence.get("closedCaptions")) != bool(video and video[0].get("closedCaptions")):
            self.err(where, "the essence has closed captions exactly when the top video rendition's stream carries them")
        for x in video:
            windows = windows_of(x)
            self.windows(where, f"video rendition {x['id']}", windows)
            if windows is not None and bool(x.get("closedCaptions")) != any(w["captions"] for w in windows):
                self.err(where, f"video rendition {x['id']} carries closed captions exactly when a caption window of "
                                f"it shows them")
        hls = pkg.get("hls")
        if hls:
            for a in audio + surround:
                if a.get("group") and a["group"] not in hls["audioGroups"]:
                    self.err(where, f"audio rendition {a['id']} is in group {a['group']}, which hls.audioGroups does not name")
            if hls.get("subtitleGroup") and not any(x.get("hls") for x in subs):
                self.err(where, f"hls.subtitleGroup names {hls['subtitleGroup']}, but no subtitle has an HLS rendition")
        if what == "extra":
            for x in subs:
                if x.get("fromSidecar"):
                    self.err(where, f"subtitle {x['id']} is made from {x['fromSidecar']}, but an extra has no source "
                                    f"record, so none of its subtitles is made from a source's sidecar")
        cs = pkg["checksums"]
        f = os.path.join(vp, cs["file"])
        if cs.get("pending"):
            # adopted before anything hashed its files: the chain closes when they are (fill-checksums)
            self.pending += 1
            if what != "version":
                self.err(where, f"an {what}'s checksums are never pending: only a version adopted by a migration")
            elif os.path.isfile(f):
                self.err(where, f"checksums file {cs['file']} is here while the record says its checksums are pending")
            elif self.no_pending:
                self.err(where, "checksums are pending: no file of this package was ever hashed "
                                "(library-v2-fill-checksums.py closes the chain)")
        elif not os.path.isfile(f):
            self.err(where, f"checksums file {cs['file']} missing")
        elif sha_file(f) != cs["sha256"]:
            self.err(where, f"checksums file {cs['file']} does not match its recorded sha256")
        elif len(read_checksums(f)) != cs["files"]:
            self.err(where, f"checksums file lists {len(read_checksums(f))} files, the record says {cs['files']}")

    def from_sidecars(self, vp, v, pkg, sources):
        """A subtitle made from a sidecar names the copy of it a source of this version keeps: one its
        source.json lists among its sidecars, in the source folder. The sidecar is the record of what
        the subtitle was made from, kept after the original and the file beside it are gone."""
        where = os.path.join(vp, "package.json")
        for x in pkg.get("subtitles") or []:
            f = x.get("fromSidecar")
            if not f:
                continue
            sid = f.split("/")[1] if f.startswith("sources/") and f.count("/") == 2 else None
            if sid not in v["sourceIds"]:
                self.err(where, f"subtitle {x['id']} is made from {f}, which is no sidecar of a source this version "
                                f"was made from (sources/<sourceId>/<name>)")
            elif sid in sources and f not in {sc["file"] for sc in sources[sid].get("sidecars") or []}:
                self.err(where, f"subtitle {x['id']} is made from {f}, which source {sid} does not list among its sidecars")

    def head(self, vp, what="version"):
        """.complete, the head of the chain: sha256:<hex> of the package.json beside it, and nothing else."""
        mark = os.path.join(vp, ".complete")
        with open(mark, "rb") as f:
            body = f.read().decode("utf-8", "replace").strip()
        if not COMPLETE.match(body):
            self.err(mark, f"holds {body[:40]!r}, not sha256:<hex> of package.json, the head of the chain "
                           f"that covers this {what}" + (f" ({UPGRADE})" if what == "version" else ""))
        elif body != sha_file(os.path.join(vp, "package.json")):
            self.err(mark, "does not name this package.json: package.json changed after the package completed")

    def chain_links(self, sums):
        """The sha256sum file sums as {name: digest}, with what is wrong with it said: a line that is
        not one, and a link of the chain listed below itself."""
        listed, bad = read_sums(sums)
        for line in bad[:3]:
            self.err(sums, f"not a sha256sum line: {line[:80]!r}")
        why = {".complete": "which is written after it and holds the hash of package.json",
               "package.json": "which holds the hash of this file", SUMS: "itself"}
        for name in CHAIN:
            if name in listed:
                self.err(sums, f"lists {name}, {why[name]}: a link of the chain cannot be listed below itself")
        return listed

    def chain(self, vp):
        """.complete -> package.json -> checksums.sha256 -> version.json and every package file. The
        records are checked always; the package files, which can be many and large, with
        --check-media and --check-checksums."""
        self.head(vp)
        sums = os.path.join(vp, SUMS)
        if not os.path.isfile(sums):
            return  # package() says so
        listed = self.chain_links(sums)
        vj = os.path.join(vp, "version.json")
        if "version.json" not in listed:
            self.err(sums, f"does not list version.json, the record its package was made for ({UPGRADE})")
        elif sha_file(vj).split(":", 1)[1] != listed["version.json"]:
            self.err(vj, f"does not match the checksum {SUMS} recorded for it: changed after the package completed")

    # ------------------------------------------------------------ the bytes
    def media(self, vp, v, pkg, sources, gone, whole):
        """gone: sources an original-deleted event named one by one; whole: an event that named none,
        so every original of the version is gone."""
        for name in v["originalFiles"]:
            f = os.path.join(vp, name)
            sid = next((s for s in v["sourceIds"] if s in sources and sources[s]["file"]["name"] == name), None)
            src = sources.get(sid)
            if whole or sid in gone:
                if os.path.isfile(f):
                    self.err(f, "an original-deleted event names this original, but it is still here")
            elif not os.path.isfile(f):
                self.err(f, "original file missing, and no original-deleted event says it was removed")
            elif src and os.path.getsize(f) != src["file"]["sizeBytes"]:
                self.err(f, f"original is {os.path.getsize(f)} bytes, its source record says {src['file']['sizeBytes']}")
            elif src and (src["file"].get("fixity") or {}).get("qh1") and qh1(f) != src["file"]["fixity"]["qh1"]:
                self.err(f, "original does not match its qh1 fixity")
        if pkg is None:
            if not any(os.path.isfile(os.path.join(vp, n)) for n in ("package.json", ".complete")):
                # taken in before anything packaged it: the subtitles attached to it since, in its subs/,
                # are counted and held to their names as beside a package
                self.attached_subtitles(vp, self.beside(vp, package_files(vp, ("subs",)), {})[1])
            return
        where = os.path.join(vp, "package.json")
        self.playable(vp, pkg)
        cs = pkg["checksums"]
        if cs.get("pending"):
            # nothing to hash against yet: the folder holds the files and the bytes the record counted
            files, attached = self.beside(vp, package_files(vp), {})
            on_disk = files + (["version.json"] if os.path.isfile(os.path.join(vp, "version.json")) else [])
            total = sum(os.path.getsize(os.path.join(vp, rel)) for rel in on_disk)
            if len(on_disk) != cs["files"]:
                self.err(where, f"the package holds {len(on_disk)} files, the record counted {cs['files']}")
            elif total != cs["bytes"]:
                self.err(where, f"the package's files total {total} bytes, the record counted {cs['bytes']}")
            self.attached_subtitles(vp, attached)
        elif os.path.isfile(os.path.join(vp, cs["file"])):
            listed = read_checksums(os.path.join(vp, cs["file"]))
            files, attached = self.beside(vp, package_files(vp), listed)
            self.attached_subtitles(vp, attached)
            on_disk = files + (["version.json"] if os.path.isfile(os.path.join(vp, "version.json")) else [])
            for rel in sorted(set(on_disk) - set(listed)):
                self.err(where, f"package file {rel} is not in {cs['file']}")
            for rel in sorted(set(listed) - set(on_disk) - set(CHAIN)):
                self.err(where, f"{cs['file']} lists {rel}, which is not a file of this package")
            present = [rel for rel in listed if rel in set(on_disk)]
            total = sum(os.path.getsize(os.path.join(vp, rel)) for rel in present)
            if len(present) == len(listed) and total != cs["bytes"]:
                self.err(where, f"the files {cs['file']} lists total {total} bytes, the record says {cs['bytes']}")
            if self.check_checksums:
                for rel in present:
                    if sha_file(os.path.join(vp, rel)).split(":", 1)[1] != listed[rel]:
                        self.err(where, f"{rel} does not match its checksum")

    def beside(self, vp, files, listed):
        """The files of the package in vp, files without the subtitles attached to the version — after its
        package completed, or before, while it was taken in — and those apart: each in subs/, named as one
        is, and not listed (listed: the checksums, {} while they are pending or there are none) — one the
        checksums list is a file of the chain."""
        attached = [rel for rel in files if rel not in listed and ATTACHED_SUBTITLE.match(rel.replace(os.sep, "/"))]
        return [rel for rel in files if rel not in attached], attached

    def attached_subtitles(self, vp, attached):
        """The subtitles attached to the version in vp: no file of its package, counted, and with
        --check-checksums each one's bytes held to the hash its name gives them — a problem of its own,
        since no checksum covers them."""
        self.counts["attached"] += len(attached)
        if not self.check_checksums:
            return
        for rel in attached:
            digest = sha_file(os.path.join(vp, rel))
            if digest.split(":", 1)[1] != ATTACHED_SUBTITLE.match(rel.replace(os.sep, "/")).group(1):
                self.err(os.path.join(vp, rel), f"an attached subtitle whose bytes hash to {digest}, not to the one its "
                                                f"name says: changed since it was attached, or named for other bytes")

    def playable(self, vp, pkg):
        """Every rendition folder — the 5.1 companions' too — subtitle, subtitle HLS rendition and styled
        original, font file, master playlist and trickplay sheet the package in vp names is there, and
        the trickplay cues cover its duration."""
        where = os.path.join(vp, "package.json")
        ren = pkg["renditions"]
        if not ren["video"]:
            self.err(where, "a package needs at least one video rendition")
        for r in ren["video"] + ren["audio"] + (ren.get("audioSurround") or []):
            if not os.path.isdir(os.path.join(vp, r["dir"])):
                self.err(where, f"rendition dir {r['dir']} missing")
        for sub in pkg.get("subtitles") or []:
            if not os.path.isfile(os.path.join(vp, sub["path"])):
                self.err(where, f"subtitle {sub['path']} missing")
            if sub.get("hls") and not os.path.isdir(os.path.join(vp, sub["hls"])):
                self.err(where, f"the HLS rendition {sub['hls']} of subtitle {sub['id']} is missing")
            if sub.get("styled") and not os.path.isfile(os.path.join(vp, sub["styled"]["path"])):
                self.err(where, f"the styled original {sub['styled']['path']} of subtitle {sub['id']} is missing")
        for font in pkg.get("fonts") or []:
            if not os.path.isfile(os.path.join(vp, font["path"])):
                self.err(where, f"font {font['path']} missing")
        if pkg.get("hls") and not os.path.isfile(os.path.join(vp, pkg["hls"]["master"])):
            self.err(where, f"master playlist {pkg['hls']['master']} missing")
        tp = pkg.get("trickplay")
        if tp:
            vtt = os.path.join(vp, tp["vttPath"])
            if not os.path.isfile(vtt):
                self.err(where, f"trickplay {tp['vttPath']} missing")
            else:
                self.trickplay(where, tp, vtt, pkg["durationMs"])

    def trickplay(self, where, tp, vtt, duration_ms):
        """Every sprite sheet the VTT names exists, and the cues cover the whole duration."""
        base = os.path.dirname(vtt)
        cues, sheets = 0, set()
        for line in open(vtt, encoding="utf-8", errors="replace"):
            line = line.strip()
            if VTT_CUE.match(line):
                cues += 1
            elif line and not line.startswith("WEBVTT") and "#xywh=" in line:
                sheets.add(line.split("#", 1)[0])
        for sheet in sorted(sheets):
            if not os.path.isfile(os.path.join(base, sheet)):
                self.err(where, f"trickplay sprite sheet {sheet} named by the VTT is missing")
        expected = duration_ms // (tp["intervalSec"] * 1000)
        if cues < expected:
            self.err(where, f"trickplay covers {cues} of {expected} thumbnails")

    def hard_links(self, d):
        """A library is portable only if its files are its own: a hard link ties a file to another path."""
        linked = []
        for root, dirs, files in os.walk(d):
            dirs[:] = sorted(x for x in dirs if not OS_ARTEFACTS.match(x) and not (root == d and x == "episodes"))
            for f in sorted(files):
                p = os.path.join(root, f)
                if not OS_ARTEFACTS.match(f) and not os.path.islink(p) and os.stat(p).st_nlink > 1:
                    rel = os.path.relpath(p, d)
                    if self.shared_originals and SHARED_ORIGINAL.match(rel.replace(os.sep, "/")):
                        self.shared += 1  # the old library's file too, until that library is gone
                        continue
                    linked.append(rel)
        if linked:
            self.err(d, f"{len(linked)} file(s) are hard links shared with another path, e.g. {linked[0]}; "
                        f"the library must hold independent files")

    # ------------------------------------------------------------ extras/
    def extras(self, d, retired=()):
        """extras/<extraId>/, each a write-once folder: extra.json, the originals it keeps and/or a
        package, and the checksums written with them. Returns {extraId: the record, or None when it
        does not hold to its schema} for every extra folder that holds an extra.json. The folder of an
        extra an extra-removed event retired is ignored, whatever it holds."""
        base = os.path.join(d, "extras")
        found = {}
        if not os.path.isdir(base):
            return found
        for name in listdir(base):
            p = os.path.join(base, name)
            if name in retired:
                self.note(p, f"the folder of an extra an extra-removed event removed: it is ignored, and {SWEEP}")
                continue
            if not os.path.isdir(p):
                self.err(p, "every extra is a folder under extras/: extras/<extraId>/ holding extra.json")
                continue
            if not UUID.match(name):
                self.err(p, "an extra folder is named by its extraId")
                continue
            xp = os.path.join(p, "extra.json")
            if not os.path.isfile(xp):
                self.err(p, "an extra folder without its extra.json record")
                continue
            x, valid = self.document("extra", xp)
            found[name] = x if valid else None
            if x is None or not valid:
                continue
            self.counts["extras"] += 1
            if x["extraId"] != name:
                self.err(xp, f"extraId {x['extraId']} does not match folder {name}")
            self.extra(p, x)
        return found

    def extra(self, xp, x):
        """What one extra folder holds, whether it finished, and the checksums that cover it. A
        packaged extra is finished by its .complete, one that keeps only its original by its checksums
        file, written last; anything else never finished, and is a note, as a package that never
        finished is."""
        for name in listdir(xp):
            if name not in EXTRA_ENTRIES and name not in x["originalFiles"]:
                self.err(os.path.join(xp, name), "not a record, the package or an original this extra names")
        if [o["name"] for o in x["originals"]] != x["originalFiles"]:
            self.err(os.path.join(xp, "extra.json"), "originals describes other files than originalFiles names: "
                                                     "each original has one entry, in the same order")
        for what, names in (("originalFiles", x["originalFiles"]), ("packagedFrom", [o["name"] for o in x.get("packagedFrom") or []])):
            for name in names:
                if not ORIGINAL_NAME.match(name):
                    self.err(os.path.join(xp, "extra.json"), f"{what} names {name!r}, which is not a name the library "
                                                             f"gives an original (original.<ext>, original-<n>.<ext>) "
                                                             f"({NEUTRAL})")
        tags = (x.get("container") or {}).get("tags") or {}
        if (x.get("container") or {}).get("title") is not None or any(str(k).lower() == "title" for k in tags):
            self.err(os.path.join(xp, "extra.json"), f"keeps the container's title tag, which routinely names where a "
                                                     f"file came from ({NEUTRAL})")
        marker = os.path.isfile(os.path.join(xp, ".complete"))
        has_package = os.path.isfile(os.path.join(xp, "package.json"))
        if x.get("packagedFrom") and x["originalFiles"]:
            self.err(os.path.join(xp, "extra.json"), "packagedFrom describes the originals of an extra that keeps none, "
                                                     "and this one keeps its originals: originals describes them")
        elif x.get("packagedFrom") and not marker and not any(os.path.exists(os.path.join(xp, n)) for n in EXTRA_DIRS):
            self.err(os.path.join(xp, "extra.json"), "packagedFrom says what the package was made from, but this extra "
                                                     "has no package")
        if has_package != marker:
            self.err(xp, "package.json exists exactly when .complete does, and here only "
                         + ("package.json" if has_package else ".complete") + " is present")
            return
        pkg = None
        if not marker:
            if any(os.path.exists(os.path.join(xp, n)) for n in EXTRA_DIRS):
                self.note(xp, f"an extra whose package never finished: no .complete beside it; {SWEEP}")
                return
            if not os.path.isfile(os.path.join(xp, SUMS)):
                self.note(xp, "an extra that never finished: no checksums.sha256 beside it, so neither the "
                              "database nor a rebuild uses it until the writer that took it in finishes it")
                return
            if not x["originalFiles"]:
                self.err(os.path.join(xp, "extra.json"), "an extra that holds neither an original nor a package")
        else:
            pkg, ok = self.document("package", os.path.join(xp, "package.json"))
            pkg = pkg if ok else None
            if pkg is not None:
                self.package(xp, x, pkg, what="extra", streams=x.get("streams") or None)
                if pkg.get("trailers"):
                    self.err(os.path.join(xp, "package.json"), "an extra's package lists no trailers: a trailer "
                                                               "that is a file of its own is an extra of kind trailer")
            self.head(xp, "extra")
        self.extra_covered(xp, x)
        if self.check_media:
            self.extra_originals(xp, x)
            if pkg is not None:
                self.playable(xp, pkg)
                self.extra_total(xp, pkg)

    def extra_covered(self, xp, x):
        """checksums.sha256 lists extra.json and every file beside it — the originals and every file of
        the package — and nothing else: never itself, package.json or .complete, the links above it.
        Listing is a walk of the folder; of the digests, extra.json's is checked always and the rest,
        which can be large, with --check-checksums."""
        sums = os.path.join(xp, SUMS)
        if not os.path.isfile(sums):
            return  # package() says so; an extra without a package and without checksums never finished
        listed = self.chain_links(sums)
        for name in x["originalFiles"]:
            if not os.path.isfile(os.path.join(xp, name)):
                self.err(os.path.join(xp, name), "an original extra.json names is not here: an extra keeps its "
                                                 "originals for as long as it exists")
        present = extra_files(xp, x["originalFiles"])
        for rel in sorted(set(present) - set(listed)):
            self.err(sums, f"does not list {rel}: an extra's checksums cover extra.json and every file beside it")
        for rel in sorted(set(listed) - set(present) - set(CHAIN) - set(x["originalFiles"])):
            self.err(sums, f"lists {rel}, which is not a file of this extra")
        xj = os.path.join(xp, "extra.json")
        if "extra.json" in listed and sha_file(xj).split(":", 1)[1] != listed["extra.json"]:
            self.err(xj, f"does not match the checksum {SUMS} recorded for it: changed after it was written")
        if self.check_checksums:
            for rel in sorted(set(present) & set(listed) - {"extra.json"}):
                if sha_file(os.path.join(xp, rel)).split(":", 1)[1] != listed[rel]:
                    self.err(os.path.join(xp, rel), "does not match its checksum")

    def extra_originals(self, xp, x):
        """Each original against the fixity extra.json records: its size and qh1, and with
        --check-checksums its sha256 when recorded — the checks a version's original gets from its
        source record."""
        for o in x["originals"]:
            f = os.path.join(xp, o["name"])
            if not os.path.isfile(f):
                continue  # extra_covered says so
            if os.path.getsize(f) != o["sizeBytes"]:
                self.err(f, f"original is {os.path.getsize(f)} bytes, extra.json says {o['sizeBytes']}")
            elif qh1(f) != o["fixity"]["qh1"]:
                self.err(f, "original does not match the qh1 fixity extra.json records")
            elif self.check_checksums and o["fixity"].get("sha256") and sha_file(f) != o["fixity"]["sha256"]:
                self.err(f, "original does not match the sha256 extra.json records")

    def extra_total(self, xp, pkg):
        """The files an extra's checksums list add up to the size its package record says."""
        cs = pkg["checksums"]
        sums = os.path.join(xp, cs["file"])
        if not os.path.isfile(sums):
            return
        listed = read_checksums(sums)
        present = [rel for rel in listed if os.path.isfile(os.path.join(xp, rel))]
        total = sum(os.path.getsize(os.path.join(xp, rel)) for rel in present)
        if len(present) == len(listed) and total != cs["bytes"]:
            self.err(os.path.join(xp, "package.json"),
                     f"the files {cs['file']} lists total {total} bytes, the record says {cs['bytes']}")

    def extra_seasons(self, d, expect_type, extras, meta):
        """A season is something only a series has, and an extra can only belong to one its projection
        knows."""
        seasons = {s["number"] for s in ((meta or {}).get("series") or {}).get("seasons") or []}
        for xid, x in sorted(extras.items()):
            if not x or "seasonNumber" not in x:
                continue
            where = os.path.join(d, "extras", xid, "extra.json")
            if expect_type != "series":
                self.err(where, f"names season {x['seasonNumber']}, but only a series' extra belongs to a season")
            elif meta is not None and x["seasonNumber"] not in seasons:
                self.err(where, f"season {x['seasonNumber']}, which the series' metadata.json does not list")

    # ------------------------------------------------------------ events against the records
    def event_subjects(self, events, sources, versions, packages, removed, extras=(), retired=()):
        # A version a version-removed event of this item removed is gone from the item, not from its
        # history: the events that named it — the deletion of its original, the supersession of its
        # package — stay valid once its folder is ignored or deleted. That folder is not read, so the
        # events say which package was its: the packageId an event of this item names beside it, as
        # its subject or as a successor. A removal among another item's events removes nothing here.
        pairs = [(e.get("versionId"), e.get("packageId")) for e in events] + \
                [((e.get("supersededBy") or {}).get("versionId"), (e.get("supersededBy") or {}).get("packageId"))
                 for e in events]
        was = {pid for vid, pid in pairs if vid in removed and pid}
        known_versions, known_packages = set(versions) | set(removed), set(packages) | was
        version_what = "version folder under versions/, nor one a version-removed event of this item removed"
        package_what = "package of any version, nor the package of one a version-removed event of this item removed"
        for e in events:
            subjects = [("sourceId", e.get("sourceId"), sources, "source record under sources/"),
                        ("packageId", e.get("packageId"), known_packages, package_what)]
            if e["kind"] != "version-removed":
                # the one event whose subject is allowed to be gone: that is what it records
                subjects.append(("versionId", e.get("versionId"), known_versions, version_what))
            if e["kind"] != "extra-removed":
                # and its counterpart for an extra
                subjects.append(("extraId", e.get("extraId"), extras, "extra folder under extras/"))
            by = e.get("supersededBy") or {}
            subjects += [("supersededBy.versionId", by.get("versionId"), known_versions, version_what),
                         ("supersededBy.packageId", by.get("packageId"), known_packages, package_what)]
            for field, value, known, what in subjects:
                if value and value not in known:
                    self.err(e["where"], f"{field} {value} names no {what}"
                                         + (", and a removed extra is not one" if value in retired else ""))
            if by.get("versionId") and by["versionId"] == e.get("versionId"):
                self.err(e["where"], "supersededBy names the version it supersedes; a re-package is a new folder")

    # ------------------------------------------------------------ series and episodes
    def series(self, d, item, meta):
        ed = os.path.join(d, "episodes")
        folders = set()
        if os.path.isdir(ed):
            for name in listdir(ed):
                if os.path.isdir(os.path.join(ed, name)):
                    folders.add(name)
                else:
                    self.err(os.path.join(ed, name), "unexpected file among the episode folders")
        context = {"itemId": item["itemId"], "tmdbTv": (item.get("externalIds") or {}).get("tmdbTv"),
                   "seasons": {s["number"] for s in ((meta or {}).get("series") or {}).get("seasons") or []},
                   "listed": {}, "places": {}, "where": os.path.join(d, "metadata.json"),
                   "folders": folders, "members": {}}
        for name in sorted(folders):
            self.item(os.path.join(ed, name), "episode", series=context)
        try:
            self.covering(context)
        except Exception as e:  # a malformed record must not stop the run
            self.err(ed, f"cannot check the episodes a file covers: {type(e).__name__}: {e}")

    def episode(self, where, item, meta, series):
        if series is None:
            self.err(where, "episode outside a series folder")
            return
        if item["seriesId"] != series["itemId"]:
            self.err(where, f"seriesId {item['seriesId']} is not the enclosing series {series['itemId']}")
        tv = (item.get("externalIds") or {}).get("tmdbTv")
        if tv and series["tmdbTv"] and tv != series["tmdbTv"]:
            self.err(where, f"externalIds.tmdbTv {tv} differs from the series' {series['tmdbTv']}")
        season, number = item["seasonNumber"], item["episodeNumber"]
        code = item.get("episodeCode")
        if code and (int(code[1:code.index("E")]), int(code[code.index("E") + 1:])) != (season, number):
            self.err(where, f"episodeCode {code} differs from seasonNumber {season} and episodeNumber {number}")
        if series["seasons"] and season not in series["seasons"]:
            self.err(where, f"season {season}, which the series' metadata.json does not list")
        other = series["listed"].get((season, number))
        if other:
            self.err(where, f"S{season:02d}E{number:02d} is already the numbering of episode {other}")
        series["listed"][(season, number)] = item["itemId"]
        self.numbering(os.path.join(os.path.dirname(where), "metadata.json"), item, meta, series)

    def numbering(self, where, item, meta, series):
        """The episode's place in each ordering. Nothing on the series repeats it, so the only things
        to check are that it does not contradict the numbers item.json was created with, and that no
        two episodes of the series claim one place in one ordering."""
        mine = ((meta or {}).get("library") or {}).get("numbering") or {}
        aired = mine.get("aired")
        if aired and (aired.get("season"), aired["episode"]) != (item["seasonNumber"], item["episodeNumber"]):
            self.err(where, f"library.numbering.aired is S{aired.get('season')}E{aired['episode']}, but item.json was "
                            f"created with S{item['seasonNumber']}E{item['episodeNumber']}")
        for name, place in sorted(mine.items()):
            at = (name, place.get("season"), place["episode"])
            other = series["places"].get(at)
            if other:
                self.err(where, f"library.numbering.{name} puts this episode where {other} already is")
            series["places"][at] = item["itemId"]

    def covering(self, series):
        """One file, several episodes. The file is its holder's — the first episode it covers, whose
        folder keeps its source and versions — and its source's covers lists the episodes it holds,
        the holder first; every other one keeps its own item and projection and nothing else, and its
        projection names the holder (library.coveredBy) and plays and labels the holder's versions. The
        rules span the episode folders of a series, so they run once all of them have been read.

        covers is written once and coveredBy projected, so the two can part: the database may no longer
        count an episode as covered, or a projection may not say so yet. Where a record and a projection
        merely part like that it is a note; where a projection contradicts the records, an error."""
        members, folders = series["members"], series["folders"]
        number = lambda m: (m["item"]["seasonNumber"], m["item"]["episodeNumber"])
        code = lambda m: f"S{m['item']['seasonNumber']:02d}E{m['item']['episodeNumber']:02d}"
        lib = lambda m: (m["meta"] or {}).get("library") or {}
        meta_of = lambda m: os.path.join(m["dir"], "metadata.json")
        covered_by = {iid: lib(m)["coveredBy"] for iid, m in sorted(members.items()) if lib(m).get("coveredBy")}

        # the source records: what a file of several episodes holds, its holder first, in episode order
        for iid, m in sorted(members.items()):
            for sid, src in sorted(m["sources"].items()):
                covers = src.get("covers") or []
                if not covers:
                    continue
                where = os.path.join(m["dir"], "sources", sid, "source.json")
                if covers[0] != iid:
                    self.err(where, f"covers begins with {covers[0]}: a file's holder — the episode whose folder keeps "
                                    f"its source, the first episode it covers — is listed first")
                twice = sorted({c for c in covers if covers.count(c) > 1})
                if twice:
                    self.err(where, f"covers lists {', '.join(twice)} more than once")
                places = [(number(members[c]), c) for c in dict.fromkeys(covers) if c in members]
                if any(a[0] >= b[0] for a, b in zip(places, places[1:])):
                    self.err(where, "covers lists the episodes the file holds out of episode order: "
                                    + ", ".join(f"{c} ({code(members[c])})" for _, c in places))
                for c in covers[1:] if sid not in m["dropped"] else []:
                    if c == iid:
                        continue
                    if c not in folders:
                        self.note(where, f"covers {c}, which has no episode folder in this series: an episode the "
                                         f"database removed since, or one it never recorded")
                    elif c in members and covered_by.get(c) != iid:
                        self.note(where, f"covers {c} ({code(members[c])}), whose projection does not name this "
                                         f"episode as the holder of its file (library.coveredBy): unlinked since the "
                                         f"source was recorded, or not linked yet")

        # the covered episodes: nothing of their own but their item and projection, and their holder's versions
        held = {}
        for iid, holder in sorted(covered_by.items()):
            m, where = members[iid], meta_of(members[iid])
            for sub in ("sources", "versions"):
                p = os.path.join(m["dir"], sub)
                if os.path.isdir(p) and listdir(p):
                    self.err(p, f"a covered episode keeps no {sub[:-1]} of its own: the file that covers it is its "
                                f"holder's, {holder}, whose folder keeps its source and versions")
            if holder == iid:
                self.err(where, "library.coveredBy names this episode itself: a covered episode names the holder "
                                "whose file covers it, another episode of its series")
                continue
            if holder not in members:
                if holder not in folders:  # one that is there but cannot be read says so itself
                    self.err(where, f"library.coveredBy names {holder}, which is no episode of this series")
                continue
            h = members[holder]
            if covered_by.get(holder):
                self.err(where, f"library.coveredBy names {holder}, which is covered by {covered_by[holder]} itself: a "
                                f"file has one holder, the first episode it covers, and every other episode it "
                                f"covers names that one")
                continue
            if number(m)[0] != number(h)[0] or number(m) <= number(h):
                self.err(where, f"this episode is {code(m)} and its holder {holder} {code(h)}: a file's holder is the "
                                f"first episode it covers, and the others follow it in its season")
            else:
                held.setdefault(holder, []).append(iid)
            counted = {sid: s for sid, s in h["sources"].items() if sid not in h["dropped"]}
            holding = {sid for sid, s in counted.items() if iid in (s.get("covers") or [])[1:]}
            if counted and not holding:
                self.err(where, f"library.coveredBy names {holder}, but no source of {holder} covers this episode: "
                                f"the source of a file that holds several episodes lists every one of them "
                                f"(source.json covers)")
            elif not counted:
                self.note(where, f"covered by {holder}, whose file has no source recorded yet")
            primary = lib(m).get("primaryVersionId")
            if primary and primary in h["removed"]:
                self.err(where, f"library.primaryVersionId {primary} names a version of its holder {holder} that was "
                                f"removed")
            elif primary and primary not in h["versions"]:
                self.err(where, f"library.primaryVersionId {primary} names no version folder of its holder {holder}: a "
                                f"covered episode plays a version of the episode whose file covers it")
            elif primary and not set(h["versions"][primary]["sourceIds"]) & holding:
                self.err(where, f"library.primaryVersionId {primary} names a version of {holder} made from no source "
                                f"that covers this episode")
            for vid in sorted(lib(m).get("versionLabels") or {}):
                if vid not in h["versions"]:
                    self.err(where, f"library.versionLabels names {vid}, which is "
                                    + (f"a removed version of its holder {holder}" if vid in h["removed"]
                                       else f"no version folder of its holder {holder}"))
            for name, place in sorted((lib(m).get("numbering") or {}).items()):
                if place.get("episodeEnd") is not None:
                    self.err(where, f"library.numbering.{name}.episodeEnd is {place['episodeEnd']}, and this episode is "
                                    f"covered by {holder}: only the holder, whose file it is, numbers the last episode "
                                    f"the file covers")

        # the holders: where the last episode a file covers sits, in each ordering the holder has a place in
        for iid, m in sorted(members.items()):
            if iid in covered_by:
                continue
            last = members[max(held[iid], key=lambda c: number(members[c]))] if held.get(iid) else None
            for name, place in sorted((lib(m).get("numbering") or {}).items()):
                end = place.get("episodeEnd")
                if last is None:
                    if end is not None:
                        self.note(meta_of(m), f"library.numbering.{name}.episodeEnd is {end}, and no episode names this "
                                              f"one as the holder of its file (library.coveredBy)")
                    continue
                theirs = ((lib(last).get("numbering") or {}).get(name) or {}).get("episode") if name != "aired" \
                    else last["item"]["episodeNumber"]
                if theirs is not None and end != theirs:
                    self.err(meta_of(m), f"library.numbering.{name}.episodeEnd is {'null' if end is None else end}, but "
                                         f"the last episode its file covers, {last['item']['itemId']} ({code(last)}), is "
                                         f"episode {theirs} there: the holder's place ends where that one sits")

    # ------------------------------------------------------------ people/
    def person(self, d):
        try:
            self._person(d)
        except Exception as e:  # a malformed record must not stop the run
            self.err(d, f"cannot check: {type(e).__name__}: {e}")

    def _person(self, d):
        where = os.path.join(d, "person.json")
        doc, valid = self.document("person", where)
        if doc is None:
            return
        self.counts["people"] += 1
        folder = os.path.basename(d.rstrip("/"))
        if doc.get("personId") != folder:
            self.err(where, f"personId {doc.get('personId')} does not match folder {folder}")
        if not valid:
            return
        listed = self.images(d, doc["images"], "person.json")
        self.primaries(where, doc["images"])
        for name in listdir(d):
            if name != "person.json" and name not in listed:
                self.unlisted(os.path.join(d, name), "person.json")
        born, died = doc.get("birthDate"), doc.get("deathDate")
        if born and died and died[:min(len(born), len(died))] < born[:min(len(born), len(died))]:
            self.err(where, f"deathDate {died} is before birthDate {born}")
        if self.check_media:
            self.hard_links(d)

    def shards(self, base, what):
        """The <aa>/<id>/ folders of one category, with the rules every category shares; with --shard,
        those of the shards named alone."""
        for shard in listdir(base):
            if self.only_shards is not None and shard not in self.only_shards:
                continue
            sd = os.path.join(base, shard)
            if not os.path.isdir(sd):
                self.err(sd, "unexpected file among the shard folders")
                continue
            if not re.fullmatch(r"[0-9a-f]{2}", shard):
                self.err(sd, f"shard folders are the first two characters of {what} id")
            for name in listdir(sd):
                p = os.path.join(sd, name)
                if name[:2] != shard:
                    self.err(p, f"{what.split()[-1]} folder is not in shard {name[:2]}")
                if not os.path.isdir(p):
                    self.err(p, "unexpected file in a shard folder")
                    continue
                yield p

    # ------------------------------------------------------------ roots
    def root(self, r):
        self.root_dir = r
        found = 0
        # the root's own entries are checked once: without --shard, or with the shard 00
        for name in (listdir(r) if self.only_shards is None or "00" in self.only_shards else []):
            if name.startswith("."):
                continue  # .work/ and every other dot-entry: what is beside the record is not the record
            if name in OLD_STORE and os.path.isdir(os.path.join(r, name)):
                self.note(os.path.join(r, name), "a folder of the store before the library: the migration's cleanup "
                                                 "moves what is left in it to .work/legacy/")
                continue
            if name == "_swept":
                self.err(os.path.join(r, name), "the quarantine of a library-v2-sweep.py --apply from before "
                                                "2026-10-06 that did not finish; the next --apply finishes it")
            elif name not in ROOT_ENTRIES:
                self.err(os.path.join(r, name), "a library root holds only movies/, series/ and people/, and "
                                                "entries whose name begins with a dot, such as .work/")
        for category, kind in CATEGORIES:
            base = os.path.join(r, category)
            if not os.path.isdir(base):
                continue
            for p in self.shards(base, "an item"):
                self.item(p, kind)
                found += 1
        if os.path.isdir(os.path.join(r, "people")):
            for p in self.shards(os.path.join(r, "people"), "a person"):
                self.person(p)
        if not found and self.only_shards is None:
            self.err(r, "no items found under movies/ or series/")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("roots", nargs="+")
    ap.add_argument("--schemas", default=HERE)
    ap.add_argument("--check-media", action="store_true")
    ap.add_argument("--check-checksums", action="store_true",
                    help="also hash every package file against its checksums file (implies --check-media)")
    ap.add_argument("--shared-originals", action="store_true",
                    help="a version's original may be a hard link shared with another path: a migration keeps "
                         "the old library beside this one, and the original is that library's file too")
    ap.add_argument("--no-pending", action="store_true",
                    help="a package whose checksums are pending is a problem, not a note: every one is filled")
    ap.add_argument("--shard", action="append", metavar="AA",
                    help="only the items and people of this shard (the first two characters of an id; may be "
                         "given more than once); the root's own entries are checked with the shard 00. A large "
                         "library is validated a few shards at a time, side by side")
    args = ap.parse_args()
    for s in args.shard or []:
        if not re.fullmatch(r"[0-9a-f]{2}", s):
            ap.error(f"--shard {s!r}: a shard is two hex characters, e.g. 0f")
    c = Checker(load_schemas(args.schemas), args.check_media, args.check_checksums,
                shared_originals=args.shared_originals, no_pending=args.no_pending)
    c.only_shards = set(args.shard) if args.shard else None
    for r in args.roots:
        c.root(r)
    print("checked:", c.counts)
    if c.pending:
        c.notes.insert(0, f"{c.pending} package(s) with their checksums pending: adopted before their files were "
                          f"hashed; library-v2-fill-checksums.py closes their chains")
    if c.shared:
        c.notes.insert(0, f"{c.shared} original(s) shared with another path (--shared-originals): the old library's "
                          f"files too, until that library is gone")
    for n in c.notes[:50]:
        print("  note " + n)
    if len(c.notes) > 50:
        print(f"  ... and {len(c.notes) - 50} more notes")
    if c.errors:
        for e in c.errors[:200]:
            print("  " + e)
        if len(c.errors) > 200:
            print(f"  ... and {len(c.errors) - 200} more")
        print(f"FAILED: {len(c.errors)} problem(s)")
        sys.exit(1)
    print("OK")


if __name__ == "__main__":
    main()
