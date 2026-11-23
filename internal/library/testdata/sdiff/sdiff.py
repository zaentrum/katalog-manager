#!/usr/bin/env python3
"""The second differential: the gate's reading of a source record (library.sourceRecordTaken: its loader, the
forms its writers write, the contract's schema and the validator's own rules, the folder it names) against the
contract's validator, vendored and pinned in ../contract (tools/validate-library-v2.py at 71999959: its loader, its
schema with its own integer and its format checker, its rules of line breaks and of keys, the folder a record
names). The review's harness of its third look (sdiff.py, abase.py) is its start, as it was, but for the paths
and the summary it writes.

usage: sdiff.py gen <cases.jsonl> | sdiff.py cmp <cases.jsonl> <go.out> <summary.json> <tmp>
A case: {id, b64 (the bytes of source.json), folder}. The Go half (TestTheRecordCheckTakesNoSourceRecordTheValidatorRefuses)
says ACCEPT or REFUSE of each, and why.
"""
import base64, collections, copy, glob, importlib.util, json, os, sys, tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
CONTRACT = os.path.join(HERE, "..", "contract")
A = "a1a1a1a1-0000-4000-8000-0000000000a1"
B = "b2b2b2b2-0000-4000-8000-0000000000b2"
C = "c3c3c3c3-0000-4000-8000-0000000000c3"


# ---- writing a document with values no JSON library writes (as the first differential's generator does) ----
class Raw:
    def __init__(self, text):
        self.text = text


class Twice:
    def __init__(self, a, b):
        self.a, self.b = a, b


MISSING = object()


def dumps(o):
    if isinstance(o, Raw):
        return o.text
    if isinstance(o, dict):
        parts = []
        for k, v in o.items():
            if v is MISSING:
                continue
            if isinstance(v, Twice):
                parts += [json.dumps(k) + ": " + dumps(v.a), json.dumps(k) + ": " + dumps(v.b)]
            else:
                parts.append(json.dumps(k) + ": " + dumps(v))
        return "{" + ", ".join(parts) + "}"
    if isinstance(o, (list, tuple)):
        return "[" + ", ".join(dumps(x) for x in o) + "]"
    if isinstance(o, float):
        return repr(o)
    return json.dumps(o, ensure_ascii=False)


# ---- the plain record, and the validator's judgement of one (the oracle) ----
def essence():
    return {"maxAudioChannels": 2, "surround": False, "losslessAudio": False, "objectAudio": False,
            "maxVideoHeight": 1080, "videoBitDepth": 8, "hdr10Metadata": False, "dolbyVision": False,
            "stereo3d": False, "interlaced": False, "audioLanguages": [], "subtitleLanguages": [],
            "subtitleTracks": 0, "imageSubtitles": False, "styledSubtitles": False, "fonts": False,
            "chapters": False, "commentaryTracks": 0, "commentarySubtitles": 0, "audioDescriptionTracks": 0,
            "sdhSubtitleLanguages": [], "forcedSubtitleLanguages": [], "closedCaptions": False}


def vsource(ms=600000, sid=A):
    """The plain record of the first two looks (one progressive h264 picture, matroska, 600 s, its essence), with
    what the schema requires beside it."""
    return {"schema": "zaentrum.library.source/2", "sourceId": sid, "takenAt": "2026-09-18T09:05:00Z",
            "takenBy": "review",
            "file": {"name": "original.mkv", "kind": "stream-container", "sizeBytes": 56,
                     "mtime": "2026-09-18T09:00:00Z", "fixity": {"qh1": "sha256:" + "5" * 64}, "part": None},
            "container": {"format": "matroska,webm", "durationMs": ms, "bitrate": None, "title": None,
                          "muxingApp": None, "writingApp": None, "creationTime": None, "tags": {}},
            "fidelity": {"class": "original", "fingerprint": None, "evidence": []},
            "essence": essence(),
            "streams": [{"index": 0, "type": "video", "codec": "h264", "fieldOrder": "progressive", "height": 1080}],
            "probe": {"tool": "ffprobe", "version": None, "at": "2026-09-18T09:05:00Z",
                      "file": f"sources/{sid}/ffprobe.json", "sha256": "sha256:" + "3" * 64}}


_mod = _checker = _tmp = None
_cache = {}


def oracle(raw, folder=A, tmp=None):
    """Whether the contract's validator takes the bytes raw, as sources/<folder>/source.json, for a source record:
    Checker.document("source", ...) says it valid, and it names its folder (Checker.sources). (accepted, why).
    tmp is the folder its records are written in to be read."""
    global _mod, _checker, _tmp
    key = (raw, folder)
    if key in _cache:
        return _cache[key]
    if _mod is None:
        spec = importlib.util.spec_from_file_location("validator", os.path.join(CONTRACT, "tools", "validate-library-v2.py"))
        _mod = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(_mod)
        _checker = _mod.load_schemas(os.path.join(CONTRACT, "library", "v2"))
        _tmp = tempfile.mkdtemp(dir=tmp)
    d = os.path.join(_tmp, "sources", folder)
    os.makedirs(d, exist_ok=True)
    p = os.path.join(d, "source.json")
    with open(p, "wb") as f:
        f.write(raw)
    c = _mod.Checker(_checker, False)
    try:
        doc, valid = c.document("source", p)
        if doc is None or not valid:
            why = "; ".join(str(e)[len(p) + 2:][:160] if str(e).startswith(p) else str(e)[:200] for e in getattr(c, "errors", [])[:2]) or "not valid"
            out = (False, why)
        elif doc["sourceId"] != folder:
            out = (False, "names another folder")
        else:
            out = (True, "")
    except RecursionError:
        out = (False, "raises RecursionError")
    except Exception as e:  # the validator can't tell
        out = (False, f"raises {type(e).__name__}: {e}")
    _cache[key] = out
    return out


# ---- the inputs ----
CASES, SEEN = [], set()


def add(cid, raw, folder=A):
    if cid in SEEN:
        return
    SEEN.add(cid)
    if isinstance(raw, str):
        raw = raw.encode("utf-8", "surrogatepass")
    CASES.append({"id": cid, "b64": base64.b64encode(raw).decode(), "folder": folder})


def J(v):
    return json.dumps(v, ensure_ascii=False)


GENERIC = ['null', 'true', 'false', '0', '1', '-1', '7.0', '1e2', '0.0', '-0', '-0.0', '9223372036854775807',
           '9223372036854775808', '-9223372036854775808', '-9223372036854775809', '1e30', '1' + '0' * 30, '0.5', '""',
           '" "', '"x"', '"x\\n"', '"x\\r"', '"\\nx"', '"a\\nb"', '"a\\rb"', '" x "', '"x "', '[]', '{}', '[null]',
           '{"zz": 1}', '"\\ud800"', '"x\\u2028"', '"x\\u0085"', '"x\\u000b"', '"x\\u000c"', '"x\\u00a0"', '"x\\u0000"']
DIGITS = {"arabic": "٠١٢٣٤٥٦٧٨٩",
          "fullwidth": "０１２３４５６７８９"}


def typed(v):
    out = {}
    if isinstance(v, bool):
        out.update({"as-0": '0', "as-1": '1', "as-text": '"true"', "as-1.0": '1.0'})
    elif isinstance(v, int):
        out.update({"N.0": f"{v}.0", "Ne0": f"{v}e0", "NE0": f"{v}E0", "N.5": f"{v}.5", "as-text": J(str(v)),
                    "N.000": f"{v}.000", "Ne+0": f"{v}e+0", "N+2^63": str(v + 2 ** 63), "0N": f"0{v}",
                    "N0e-1": f"{v}0e-1", "-N": str(-v) if v else "-0"})
    elif isinstance(v, float):
        out.update({"as-int": str(int(v)), "as-text": J(str(v))})
    elif isinstance(v, str):
        out.update({"+nl": J(v + "\n"), "+cr": J(v + "\r"), "+crlf": J(v + "\r\n"), "nl+": J("\n" + v), "+sp": J(v + " "),
                    "sp+": J(" " + v), "upper": J(v.upper()), "+nbsp": J(v + " "), "cut": J(v[:-1]), "twice": J(v + v),
                    "inner-nl": J(v[:1] + "\n" + v[1:]), "inner-cr": J(v[:1] + "\r" + v[1:]), "+ls": J(v + " "),
                    "+nel": J(v + "\u0085"), "+vt": J(v + "\x0b"), "+ff": J(v + "\x0c"), "+tab": J(v + "\t"),
                    "+nlnl": J(v + "\n\n"), "+nul": J(v + "\x00")})
        for name, digits in DIGITS.items():
            for i, ch in enumerate(v):
                if ch.isdigit() and ch.isascii():
                    out[name + "-digit"] = J(v[:i] + digits[int(ch)] + v[i + 1:])
                    break
        for i, ch in enumerate(v):
            if ch.isascii() and ch.isalpha() and ch.islower():
                out["fullwidth-letter"] = J(v[:i] + chr(ord(ch) - ord("a") + 0xff41) + v[i + 1:])
                break
    elif v is None:
        pass
    elif isinstance(v, list):
        out.update({"doubled": J(v + v), "+null": J(v + [None]), "+x": J(v + ["x"]), "+1": J(v + [1])})
        if v and isinstance(v[0], str):
            out["+first-upper"] = J(v + [v[0].upper()])
    elif isinstance(v, dict):
        pass
    return out


def paths(o, at=()):
    yield at, o
    if isinstance(o, dict):
        for k, v in o.items():
            yield from paths(v, at + (k,))
    elif isinstance(o, list):
        for i, v in enumerate(o):
            yield from paths(v, at + (i,))


def with_at(doc, path, value):
    d = copy.deepcopy(doc)
    if not path:
        return value
    o = d
    for k in path[:-1]:
        o = o[k]
    if value is MISSING and isinstance(o, list):
        del o[path[-1]]
    else:
        o[path[-1]] = value
    return d


def pname(path):
    return ".".join(str(p) for p in path) or "$"


def gen_walk(tag, base, folder):
    for path, v in paths(base):
        if not path:
            continue
        for i, alt in enumerate(GENERIC):
            add(f"S/{tag}/{pname(path)}/generic-{i}:{alt[:24]}", dumps(with_at(base, path, Raw(alt))), folder)
        for name, alt in typed(v).items():
            add(f"S/{tag}/{pname(path)}/{name}", dumps(with_at(base, path, Raw(alt))), folder)
        add(f"S/{tag}/{pname(path)}/missing", dumps(with_at(base, path, MISSING)), folder)
        if isinstance(v, dict):
            for kname, key in {"zz": "zz", "sp-zz": " zz", "zz-sp": "zz ", "empty": "", "nl": "zz\n", "nbsp": " zz"}.items():
                d = copy.deepcopy(v)
                d[key] = 1
                add(f"S/{tag}/{pname(path)}/+key-{kname}", dumps(with_at(base, path, d)), folder)
                d[key] = "x"
                add(f"S/{tag}/{pname(path)}/+key-{kname}-text", dumps(with_at(base, path, d)), folder)
                d[key] = None
                add(f"S/{tag}/{pname(path)}/+key-{kname}-null", dumps(with_at(base, path, d)), folder)
            for k in list(v.keys()):
                d = copy.deepcopy(v)
                d[k] = Twice(v[k], v[k])
                add(f"S/{tag}/{pname(path)}/key-twice-{k}", dumps(with_at(base, path, d)), folder)
                d = collections.OrderedDict((kk.upper() if kk == k else kk, vv) for kk, vv in v.items())
                add(f"S/{tag}/{pname(path)}/key-upper-{k}", dumps(with_at(base, path, d)), folder)
                d = collections.OrderedDict((kk + " " if kk == k else kk, vv) for kk, vv in v.items())
                add(f"S/{tag}/{pname(path)}/key-sp-{k}", dumps(with_at(base, path, d)), folder)


TIMES = ["2026-09-18T09:05:00Z", "2026-09-18T09:05:00z", "2026-09-18t09:05:00Z", "2026-09-18 09:05:00Z",
         "2026-09-18T09:05:00", "2026-09-18T09:05:00+00:00", "2026-09-18T09:05:00-00:00", "2026-09-18T09:05:00+23:59",
         "2026-09-18T09:05:00+24:00", "2026-09-18T09:05:00-23:60", "2026-09-18T09:05:00+99:99", "2026-09-18T09:05:00+00:60",
         "2026-09-18T09:05:00.5Z", "2026-09-18T09:05:00.Z", "2026-09-18T09:05:00,5Z", "2026-09-18T9:05:00Z",
         "2026-09-18T09:5:00Z", "2026-09-18T09:05:0Z", "2026-9-18T09:05:00Z", "2026-09-1T09:05:00Z", "2026-09-18T24:00:00Z",
         "2026-09-18T23:59:60Z", "2026-09-18T23:60:00Z", "2026-06-30T23:59:60Z", "2026-02-29T00:00:00Z",
         "2024-02-29T00:00:00Z", "1900-02-29T00:00:00Z", "2000-02-29T00:00:00Z", "2026-02-30T09:05:00Z",
         "2026-04-31T09:05:00Z", "2026-13-01T09:05:00Z", "2026-00-10T09:05:00Z", "2026-01-00T09:05:00Z",
         "2026-01-32T09:05:00Z", "0000-01-01T00:00:00Z", "0000-12-31T23:59:59-23:59", "0001-01-01T00:00:00Z",
         "0001-01-01T00:00:00+23:59", "0001-01-01T00:00:00-23:59", "9999-12-31T23:59:59Z", "9999-12-31T23:59:59-23:59",
         "9999-12-31T23:59:59+23:59", "10000-01-01T00:00:00Z", "2026-09-18T09:05:00Z\n", "\n2026-09-18T09:05:00Z",
         " 2026-09-18T09:05:00Z", "2026-09-18T09:05:00Z ", "2026-09-18T09:05:00.123456789012Z",
         "2026-09-18T09:05:00." + "1234567890" * 4 + "Z", "２０２６-09-18T09:05:00Z",
         "2026-09-18T09:05:0٠Z", "2026-09-18T09:05:00+0100", "2026-09-18T09:05:00+01", "2026-09-18T09:05:00+1:00",
         "2026-09-18T09:05:00+01:00:00", "2026-09-18T09:05Z", "2026-09-18", "", "2026-09-18T09:05:00ZZ",
         "2026-09-18T09:05:00Z+01:00", "2026-09-18T09:05:00−01:00", "2026-09-18T09:05:00+23:5", "1970-01-01T00:00:00Z",
         "2026-12-31T23:59:59.999999999Z", "2026-09-18T09:05:00.0000000000Z", "2026-09-18T09:05:00+12:34",
         "2026-09-18T09:05:00.5+01:00", "2026-09-18T09:05:00.5,5Z", "2026-09-18T09:05:00 Z", "2026-09-18T09:05:00+ 1:00",
         "2026-09-18T09:05:00+-1:00", "2026-09-18T09:05:00+2a:00", "2026-09-18T09:05:00Z\r", "2026-09-18T09:05:00Z\r\n",
         "2026-09-18T09:05:00Z\n\n", "2026-09-18T09:05:00Z ", "-026-09-18T09:05:00Z", "+2026-09-18T09:05:00Z",
         "2026-09-18T-9:05:00Z", "2026-09-18T09:-5:00Z", "2026-09-18T09:05:00.5z", "2026-09-18T09:05:00+24:60",
         "2026-09-18T09:05:00+23:99", "2026-09-18T09:05:00-99:00", "2026-09-18T09:05:00+9a:00", "2026-09-18T09:05:00+2:000"]
UUIDS = [A, A.upper(), A.replace("-", ""), "{" + A + "}", A + "0", A[:-1], A + "\n", "\n" + A, A[:8] + "０" + A[9:],
         A.replace("a", "g", 1), "", " " + A, A + " ", A + "\r", "urn:uuid:" + A, A[:14] + "0" + A[15:]]
LANGS = ["en", "eng", "EN", "e", "engl", "en-US", "en-", "en-a", "en-abcdefgh", "en-abcdefghi", "und", "zxx", "mul", "en\n",
         "ｅｎ", "en-ＵＳ", "en ", " en", "", "en-US-x", "en_US", "en-١٢", "und\n", "en\r",
         "en-us\n", "en\n\n", "zxx-", "mul-12", "én", "en "]
NAMES = ["a", ".", "..", "...", "a/b", "", "a\n", "\n", "a\nb", "..\n", ".\n", "a\r", "a ", "\\", "a\x00", "é",
         "a" * 300, " ", ".. ", " ..", "..\r", "a\r\n", "\r", ".a", "a.", "a\\b", "/", "..\n\n", "a\u0085"]
RELS = ["sources/x/ffprobe.json", "/a", "..", "../a", "a/..", "a/../b", "a/..b", "a//b", "", "a\n", "a\nb", "..\n",
        "a/..\n", "\n", ".", "./a", "a/.", "a\\..\\b", "..a", "a/...", "a\r", "/", "a/", "..\r", "a/..\r", " ", " /a",
        "a /..", "a/ ..", "a/.. ", "a\r\n", "a/.. ", " ", "a\u0085", "//", "a/../", "..//", "a/\n..", "\r/.."]
SHAS = ["sha256:" + "0" * 64, "sha256:" + "A" * 64, "sha256:" + "0" * 63, "sha256:" + "0" * 65, "sha256:" + "0" * 64 + "\n",
        "SHA256:" + "0" * 64, "0" * 64, "sha256:" + "０" + "0" * 63, "sha256:" + "٠" + "0" * 63, "",
        " sha256:" + "0" * 64, "sha256:" + "0" * 64 + " ", "sha256:" + "0" * 64 + "\r", "sha256:" + "g" + "0" * 63]
TAGS = [{"title": "x"}, {"title": "x\n"}, {"title": "x\r\n"}, {"title": "x\r"}, {"title": "a\nb"}, {"name": "a\nb"},
        {"name": "ab"}, {"language": "a\rb"}, {"type": "x\n"}, {" title": "x"}, {"title ": "x"}, {"\ttitle": "x"},
        {"title\n": "x"}, {" title": "x"}, {"​title": "x"}, {"　title": "x"}, {"\u0085title": "x"},
        {"\x1ctitle": "x"}, {"title\x1f": "x"}, {"title ": "x"}, {"﻿title": "x"}, {"": "x"}, {"title": 1},
        {"title": None}, {"notes": "x\n"}, {"reason": "x\n"}, {"detail": "a\nb\n"}, {"path": "a\nb"}, {"ref": "a\rb"},
        {"sha256": "a\nb"}, {"kind": "a\nb"}, {"file": "a\nb"}, {"schema": "a\nb"}, {"sourceId": "a\nb"}, {"qh1": "a\rb"},
        {"overview": "x\r"}, {"note": "\n"}, {"x": "\n"}, {"x": "\r"}, {"x": " "}, {"x": ""}, {"x": " "},
        {"x": "y "}, {"x": "y\u0085"}, {"x": "y\x0b"}, {"x": "y\x0c"}, {"x": "y\x1c"}, {"x": "y\x1d"}, {"x": "y\x1e"},
        {"Title": "x", "title": "y"}, {"title": Twice("x", "x")}, {"t" * 300: "x"}, {"x": "y" * 70000},
        {"name": "a b"}, {"name": "a\x0bb"}, {"name": "a\x0cb"}, {"name": "a\x1cb"}, {"name": "a\u0085b"},
        {"NAME": "a\nb"}, {"Name": "a\nb"}, {"name ": "ab"}, {"na\nme": "ab"}]


def gen_targeted():
    base = vsource()

    def put(cid, path, value, folder=A, doc=None):
        add(cid, dumps(with_at(doc if doc is not None else base, path, value)), folder)
    for i, t in enumerate(TIMES):
        for where in (("takenAt",), ("file", "mtime"), ("probe", "at")):
            put(f"T/time/{pname(where)}/{i}:{t[:34]!r}", where, t)
    for i, u in enumerate(UUIDS):
        put(f"T/uuid/sourceId,folder-the-same/{i}:{u[:40]!r}", ("sourceId",), u, folder=u if u and "\n" not in u and "\r" not in u and "/" not in u else A)
        put(f"T/uuid/sourceId,folder-the-base/{i}:{u[:40]!r}", ("sourceId",), u)
        d = copy.deepcopy(base)
        d["covers"] = [u]
        add(f"T/uuid/covers/{i}:{u[:40]!r}", dumps(d))
    for i, l in enumerate(LANGS):
        put(f"T/language/stream/{i}:{l!r}", ("streams", 0, "language"), l)
        put(f"T/language/essence.audioLanguages/{i}:{l!r}", ("essence", "audioLanguages"), [l])
        put(f"T/language/essence.audioLanguages,twice/{i}:{l!r}", ("essence", "audioLanguages"), [l, l])
        d = copy.deepcopy(base)
        d["sidecars"] = [{"file": f"sources/{A}/s.srt", "kind": "subtitle", "language": l}]
        add(f"T/language/sidecar/{i}:{l!r}", dumps(d))
    put("T/language/essence.audioLanguages/en,EN", ("essence", "audioLanguages"), ["en", "EN"])
    put("T/language/essence.audioLanguages/en,de,en", ("essence", "audioLanguages"), ["en", "de", "en"])
    put("T/language/essence.audioLanguages/en,de", ("essence", "audioLanguages"), ["en", "de"])
    for i, n in enumerate(NAMES):
        put(f"T/fileName/{i}:{n[:20]!r}", ("file", "name"), n)
    for i, r in enumerate(RELS):
        put(f"T/relPath/probe.file/{i}:{r!r}", ("probe", "file"), r)
        d = copy.deepcopy(base)
        d["sidecars"] = [{"file": r, "kind": "other"}]
        add(f"T/relPath/sidecar/{i}:{r!r}", dumps(d))
    for i, s in enumerate(SHAS):
        put(f"T/sha256/probe/{i}:{s[:14]!r},{len(s)}", ("probe", "sha256"), s)
        put(f"T/sha256/fixity.qh1/{i}:{s[:14]!r},{len(s)}", ("file", "fixity", "qh1"), s)
    for i, t in enumerate(TAGS):
        name = dumps(t)[:60]
        put(f"T/tags/{i}:{name}", ("container", "tags"), t)
    # the facts beside the file's fixity: without it the probe says why
    for name, note in {"missing": MISSING, "null": None, "empty": "", "text": "no fixity", "space": " ", "5": 5,
                       "text-nl": "no fixity\n", "nl": "\n"}.items():
        d = copy.deepcopy(base)
        del d["file"]["fixity"]
        if note is not MISSING:
            d["probe"]["note"] = note
        add(f"T/fixity-none/note={name}", dumps(d))
        d2 = copy.deepcopy(base)
        if note is not MISSING:
            d2["probe"]["note"] = note
        add(f"T/fixity-there/note={name}", dumps(d2))
    # the record's own windows, span, gop and what is off the lists
    win = {"at": 0.0, "start": 0.0, "end": 1.6, "frames": 48, "captions": False, "interlacedFrames": 0, "repeatFrames": 0}
    for name, change in {"plain": {}, "at-1.0000001": {"at": 1.0000001}, "at--0.0": {"at": Raw("-0.0")}, "at-1": {"at": 1},
                         "at-1.0": {"at": 1.0}, "at-2": {"at": 2}, "at--1e-9": {"at": Raw("-1e-9")}, "frames-7.0": {"frames": Raw("7.0")},
                         "frames-48e0": {"frames": Raw("48e0")}, "frames--1": {"frames": -1}, "whole-false": {"whole": False},
                         "whole-true-no-packets": {"whole": True}, "whole-true,packets": {"whole": True, "packets": 48},
                         "whole-null": {"whole": None}, "whole-1": {"whole": 1}, "whole-1.0": {"whole": Raw("1.0")},
                         "packets-alone": {"packets": 48}, "leading-17": {"leading": 17}, "leading-16": {"leading": 16},
                         "leading-16.0": {"leading": Raw("16.0")}, "leading-1.6e1": {"leading": Raw("1.6e1")},
                         "captions-null": {"captions": None}, "captions-0": {"captions": 0}, "extra-key": {"zz": 1},
                         "keyframeBefore-1": {"keyframeBefore": 1}, "keyframeBefore-true": {"keyframeBefore": True},
                         "start-text": {"start": "0"}, "start--5": {"start": -5}, "no-captions": {"captions": MISSING},
                         "interlaced-48.0": {"interlacedFrames": Raw("48.0")}, "at-1e0": {"at": Raw("1e0")},
                         "at-10e-1": {"at": Raw("10e-1")}, "at-1.0000000000000000001": {"at": Raw("1.0000000000000000001")},
                         "at-0.99999999999999999999": {"at": Raw("0.99999999999999999999")},
                         "at-1.00000000000000000000000000000001": {"at": Raw("1.00000000000000000000000000000001")},
                         "leading-16.00000000000000000000000000000001": {"leading": Raw("16.00000000000000000000000000000001")},
                         "leading-17e0": {"leading": Raw("17e0")}, "leading-17.0": {"leading": Raw("17.0")},
                         "at-1e400": {"at": Raw("1e400")}, "frames-2^63": {"frames": 2 ** 63}}.items():
        w = dict(win)
        w.update(change)
        d = copy.deepcopy(base)
        d["streams"][0]["captionWindows"] = [w]
        add(f"T/captionWindow/{name}", dumps(d))
    for name, v in {"0": 0, "0.0": 0.0, "-0.0": Raw("-0.0"), "-1": -1, "-1e-30": Raw("-1e-30"), "text": "2", "null": None,
                    "2": 2, "2.5": 2.5, "-0": Raw("-0"), "1e400": Raw("1e400"), "-1e-400": Raw("-1e-400")}.items():
        put(f"T/captionGop/{name}", ("streams", 0, "captionGop"), v)
    for name, v in {"ok": {"start": 0, "end": 600}, "no-end": {"start": 0}, "extra": {"start": 0, "end": 600, "zz": 1},
                    "text": {"start": "0", "end": 600}, "null": None, "list": []}.items():
        put(f"T/captionSpan/{name}", ("streams", 0, "captionSpan"), v)
    for name, v in {"[]": [], "['']": [""], "[1]": [1], "text": "x", "[' ']": [" "], "['a\\n']": ["a\n"], "null": None,
                    "['x','x']": ["x", "x"], "[null]": [None], "['codec:vc1']": ["codec:vc1"]}.items():
        put(f"T/offList/{name}", ("offList",), v)
    # constants and choices, written apart
    for path, forms in {("schema",): ["zaentrum.library.source/2", "zaentrum.library.source/2\n", " zaentrum.library.source/2",
                                      "zaentrum.library.source/1", "ZAENTRUM.LIBRARY.SOURCE/2", "zaentrum.library.source/2.0"],
                        ("probe", "tool"): ["ffprobe", "ffprobe\n", "FFprobe", "ffprobe "],
                        ("file", "kind"): ["stream-container", "stream-container\n", "Stream-Container", "disc-image"],
                        ("fidelity", "class"): ["original", "original\n", "Original", "unknown"],
                        ("essence", "closedCaptionsKnown"): [True, 1, Raw("1.0"), "true", False, None],
                        ("streams", 0, "type"): ["video", "video\n", "Video", "audio", "data", "attachment", "subtitle", "x"],
                        ("streams", 0, "index"): [0, Raw("0.0"), Raw("-0"), Raw("0e0"), -1, "0", None, 2 ** 63, 2 ** 63 - 1, Raw("1E0")],
                        ("container", "durationMs"): [600000, Raw("600000.0"), Raw("6e5"), Raw("6E5"), "600000", -1, 0, None,
                                                      2 ** 63, 2 ** 63 - 1, -2 ** 63, -2 ** 63 - 1, Raw("600000.5")],
                        ("file", "sizeBytes"): [56, Raw("56.0"), Raw("5.6e1"), -1, Raw("-0"), Raw("-0.0"), 2 ** 63 - 1, 2 ** 63,
                                                Raw("0.0"), Raw("1e-400"), Raw("-1e-400"), Raw("0e0"), Raw("0E400"),
                                                Raw("0.00000000000000000000000000000000000001"), Raw("9223372036854775807.0"),
                                                Raw("9223372036854775807e0"), Raw("-0e0"), Raw("-1e-30"), Raw("-0.0000000000000000000000000000001")],
                        ("streams", 0, "level"): [4.1, 4, Raw("4.0"), "4.1", None, Raw("1e400"), 2 ** 70],
                        ("streams", 0, "height"): [1080, Raw("1080.0"), None, "1080", 486, Raw("486.0"), -1]}.items():
        for i, f in enumerate(forms):
            put(f"T/const/{pname(path)}/{i}:{dumps(f)[:28]}", path, f)
    # an audio stream's channels (from 1), a subtitle's events (from 0), a part's index (from 1)
    for name, v in {"1": 1, "0": 0, "1.0": Raw("1.0"), "0.9999999999999999999999999999999": Raw("0.9999999999999999999999999999999"),
                    "1e0": Raw("1e0"), "-1": -1, "missing": MISSING, "null": None, "2": 2}.items():
        d = copy.deepcopy(base)
        st = {"index": 1, "type": "audio", "codec": "aac", "channels": v}
        if v is MISSING:
            del st["channels"]
        d["streams"].append(st)
        add(f"T/audio.channels/{name}", dumps(d))
        d = copy.deepcopy(base)
        d["file"]["part"] = {"index": v, "of": 2} if v is not MISSING else {"of": 2}
        add(f"T/file.part.index/{name}", dumps(d))
    for name, st in {"data-plain": {"index": 1, "type": "data"}, "data-codec": {"index": 1, "type": "data", "codec": "bin_data"},
                     "data-cc-true": {"index": 1, "type": "data", "closedCaptions": True},
                     "data-cc-false": {"index": 1, "type": "data", "closedCaptions": False},
                     "data-cc-1": {"index": 1, "type": "data", "closedCaptions": 1},
                     "sub-cc-false": {"index": 1, "type": "subtitle", "closedCaptions": False},
                     "sub-events--1": {"index": 1, "type": "subtitle", "events": -1},
                     "sub-events-0.0": {"index": 1, "type": "subtitle", "events": Raw("0.0")},
                     "video-channels": {"index": 1, "type": "video", "channels": 2},
                     "video-plain": {"index": 1, "type": "video"}, "no-index": {"type": "video"}, "no-type": {"index": 1},
                     "type-null": {"index": 1, "type": None}, "attachment": {"index": 1, "type": "attachment", "filename": "c.jpg"},
                     "attachment-role-x": {"index": 1, "type": "attachment", "role": "x"},
                     "cover-marked": {"index": 1, "type": "video", "codec": "mjpeg", "dispositions": {"attachedPic": True}},
                     "cover-marked-1": {"index": 1, "type": "video", "codec": "mjpeg", "dispositions": {"attachedPic": 1}},
                     "dispositions-text": {"index": 1, "type": "video", "dispositions": "x"},
                     "dispositions-extra": {"index": 1, "type": "video", "dispositions": {"zz": True}},
                     "same-index": {"index": 0, "type": "audio", "channels": 2},
                     "text-string": "x", "null": None, "list": []}.items():
        d = copy.deepcopy(base)
        d["streams"].append(st)
        add(f"T/stream/{name}", dumps(d))


def gen_bytes():
    text = dumps(vsource())
    raw = text.encode()
    inside = lambda b: raw.replace(b'"review"', b'"r' + b + b'view"')
    for name, b in {"latin1-e9": b"\xe9", "overlong-c0af": b"\xc0\xaf", "surrogate-eda080": b"\xed\xa0\x80", "ff": b"\xff",
                    "cut-e282": b"\xe2\x82", "fe": b"\xfe", "f8-5byte": b"\xf8\x88\x80\x80\x80", "f4-9080-past-10ffff": b"\xf4\x90\x80\x80",
                    "c1-bf-overlong": b"\xc1\xbf", "e0-80-80-overlong": b"\xe0\x80\x80", "80-lone": b"\x80",
                    "valid-e9": "é".encode(), "valid-astral": "\U0001f600".encode(), "raw-tab": b"\t", "raw-nl": b"\n",
                    "raw-nul": b"\x00", "raw-del": b"\x7f", "raw-1f": b"\x1f", "esc-nul": b"\\u0000", "esc-d800": b"\\ud800",
                    "esc-dc00-d800": b"\\udc00\\ud800", "esc-pair": b"\\ud83d\\ude00", "esc-x": b"\\x41", "esc-slash": b"\\/",
                    "esc-U": b"\\U00000041", "esc-u-short": b"\\u41", "esc-upper-hex": b"\\u00E9", "esc-0": b"\\0", "esc-a": b"\\a",
                    "esc-v": b"\\v", "esc-quote": b"\\'"}.items():
        add(f"B/in-string/{name}", inside(b))
    keyed = lambda b: raw.replace(b'"takenBy"', b'"taken' + b + b'By"')
    for name, b in {"latin1-e9": b"\xe9", "raw-nul": b"\x00", "esc-nul": b"\\u0000", "ff": b"\xff"}.items():
        add(f"B/in-key/{name}", keyed(b))
    for name, b in {"bom": b"\xef\xbb\xbf" + raw, "bom-twice": b"\xef\xbb\xbf\xef\xbb\xbf" + raw, "utf16le": text.encode("utf-16-le"),
                    "utf16-bom": text.encode("utf-16"), "utf32": text.encode("utf-32"), "latin1": text.replace("review", "réview").encode("latin-1"),
                    "trailing-nul": raw + b"\x00", "trailing-nl": raw + b"\n", "trailing-crlf": raw + b"\r\n", "trailing-ff": raw + b"\x0c",
                    "trailing-vt": raw + b"\x0b", "trailing-nbsp": raw + " ".encode(), "trailing-bom": raw + b"\xef\xbb\xbf",
                    "leading-nl": b"\n" + raw, "leading-ff": b"\x0c" + raw, "leading-nbsp": " ".encode() + raw,
                    "leading-ls": " ".encode() + raw, "trailing-ls": raw + " ".encode(), "two": raw + b" " + raw,
                    "empty": b"", "space": b" ", "null": b"null", "list": b"[" + raw + b"]", "comment": b"// x\n" + raw,
                    "trailing-comma": raw[:-1] + b",}", "single-quotes": raw.replace(b'"review"', b"'review'"),
                    "key-twice-empty": raw[:1] + b'"": 1, "": 2, ' + raw[1:],
                    "key-twice-empty-in-tags": raw.replace(b'"tags": {}', b'"tags": {"": "x", "": "x"}'),
                    "key-empty-in-tags": raw.replace(b'"tags": {}', b'"tags": {"": "x"}'),
                    "key-twice-takenBy": raw.replace(b'"takenBy": "review"', b'"takenBy": "review", "takenBy": "review"'),
                    "key-twice-by-escape": raw.replace(b'"takenBy": "review"', b'"takenBy": "review", "taken\\u0042y": "review"'),
                    "key-escaped-alone": raw.replace(b'"takenBy"', b'"taken\\u0042y"'),
                    "key-twice-in-tags-by-escape": raw.replace(b'"tags": {}', b'"tags": {"a": "x", "\\u0061": "x"}'),
                    "NaN": raw.replace(b'"sizeBytes": 56', b'"sizeBytes": NaN'), "Infinity": raw.replace(b'"sizeBytes": 56', b'"sizeBytes": Infinity'),
                    "1e999": raw.replace(b'"sizeBytes": 56', b'"sizeBytes": 1e999'), "4301-digits": raw.replace(b'"sizeBytes": 56', b'"sizeBytes": ' + b"1" * 4301),
                    "4300-digits": raw.replace(b'"sizeBytes": 56', b'"sizeBytes": ' + b"1" * 4300),
                    "bitrate-4300-digits": raw.replace(b'"bitrate": null', b'"bitrate": ' + b"1" * 4300),
                    "level-1e308": raw.replace(b'"height": 1080', b'"height": 1080, "level": 1e308'),
                    "level-1.8e308": raw.replace(b'"height": 1080', b'"height": 1080, "level": 1.8e308'),
                    "level-4301-digit-float": raw.replace(b'"height": 1080', b'"height": 1080, "level": ' + b"1" * 4301 + b".5"),
                    "level-5000-digit-fraction": raw.replace(b'"height": 1080', b'"height": 1080, "level": 4.' + b"1" * 5000),
                    "hex": raw.replace(b'"sizeBytes": 56', b'"sizeBytes": 0x38'), "plus": raw.replace(b'"sizeBytes": 56', b'"sizeBytes": +56'),
                    "dot5": raw.replace(b'"sizeBytes": 56', b'"sizeBytes": .5'), "5dot": raw.replace(b'"sizeBytes": 56', b'"sizeBytes": 56.'),
                    "True": raw.replace(b'"interlaced": false', b'"interlaced": False'), "nul-in-middle": raw[:40] + b"\x00" + raw[40:],
                    "depth-600": raw[:1] + b'"takenBy": ' + b"[" * 600 + b"]" * 600 + b", " + raw[1:].replace(b'"takenBy": "review", ', b""),
                    }.items():
        add(f"B/file/{name}", b)


def generate(path):
    gen_walk("base", vsource(), A)
    for p in sorted(glob.glob(os.path.join(CONTRACT, "library", "v2", "examples", "series", "*", "*", "episodes", "*", "sources", "*", "source.json"))):
        doc = json.load(open(p), object_pairs_hook=collections.OrderedDict)
        gen_walk("example-" + doc["sourceId"][:8], doc, doc["sourceId"])
    gen_targeted()
    gen_bytes()
    with open(path, "w", encoding="utf-8") as f:
        for c in CASES:
            assert "\t" not in c["id"] and "\n" not in c["id"], c["id"]
            f.write(json.dumps(c) + "\n")
    print(len(CASES), "cases", dict(collections.Counter(c["id"].split("/")[0] + "/" + c["id"].split("/")[1] for c in CASES)))


def compare(cases_path, out_path, summary_path, tmp):
    go = {}
    for line in open(out_path, encoding="utf-8"):
        cid, verdict, why = (line.rstrip("\n").split("\t") + ["", ""])[:3]
        go[cid] = (verdict, why)
    agree, must, lesser = collections.Counter(), [], []
    n = 0
    for line in open(cases_path, encoding="utf-8"):
        c = json.loads(line)
        n += 1
        raw = base64.b64decode(c["b64"])
        ok, why = oracle(raw, c["folder"], tmp)
        verdict, gwhy = go[c["id"]]
        if verdict == "ACCEPT" and not ok:
            must.append((c["id"], why[:200]))
        elif verdict != "ACCEPT" and ok:
            lesser.append((c["id"], verdict, gwhy[:200]))
        else:
            agree[f"{verdict}/{'takes' if ok else 'refuses'}"] += 1
    print(f"{n} records; agree {dict(agree)}")
    print(f"the gate's check takes it and the validator refuses it: {len(must)}")
    print(f"the gate's check refuses it and the validator takes it: {len(lesser)}")
    for row in must[:12]:
        print("MUST", row)
    # the kinds the gate's check holds alone: by what it says, its numbers and texts aside
    import re
    kinds, examples = collections.Counter(), collections.defaultdict(list)
    for cid, _, w in lesser:
        k = re.sub(r'"[^"]*"|[0-9]+', "#", w.split(": ", 1)[-1])[:90]
        kinds[k] += 1
        if len(examples[k]) < 3:
            examples[k].append(cid)
    summary = {"cases": n, "agree": dict(agree), "must": must, "lesser": len(lesser),
               "lesser_by_kind": dict(kinds.most_common()), "lesser_examples": dict(examples),
               "lesser_by_family": dict(collections.Counter("/".join(r[0].split("/")[:2]) for r in lesser))}
    with open(summary_path, "w", encoding="utf-8") as f:
        json.dump(summary, f, indent=1)


if __name__ == "__main__":
    if sys.argv[1] == "gen":
        generate(sys.argv[2])
    else:
        compare(sys.argv[2], sys.argv[3], sys.argv[4], sys.argv[5])
