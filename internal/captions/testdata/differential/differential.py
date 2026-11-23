#!/usr/bin/env python3
"""The differential of katalog-manager's deletion gate against the record logic (the schemas'
tools/libv2_records.py, vendored beside this file and pinned by its sha256): generates the cases, and compares
what the Go test (TestTheGateHoldsWhatTheRecordLogicHolds) says its gate and its port of the record logic's gate
(library.RecordLogicHolds) do with each against what the record logic says. Taken from the review's harness of
katalog-manager#12; nothing reads a path outside this folder.

usage: differential.py gen <cases.jsonl> | differential.py cmp <cases.jsonl> <go.out> <summary.json>
"""
import sys, os, json, math, copy, collections

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import libv2_records as L  # noqa: E402

NEVER = {"closedCaptions", "interlaced"}


# ---- the record logic's loader (validate-library-v2.py load_strict/readable), of a text ----
def _no_constant(name):
    raise ValueError(name)


def _no_overflow(text):
    if abs(float(text)) == float("inf"):
        raise ValueError(text)
    return float(text)


def load_strict(text):
    twice = []

    def pairs(items):
        keys = [k for k, _ in items]
        twice.extend(k for k in dict.fromkeys(keys) if keys.count(k) > 1)
        return dict(items)

    doc = json.loads(text, object_pairs_hook=pairs, parse_constant=_no_constant, parse_float=_no_overflow)
    if twice:
        raise ValueError("twice")
    return doc


def readable(text):
    if text is None:
        return None
    try:
        doc = load_strict(text)
    except (ValueError, RecursionError):
        return None
    return doc if isinstance(doc, dict) else None


def truth(case):
    """HOLD/GO of the record logic, the keys it holds, and why."""
    src, probe = readable(case["source"]), readable(case["probe"])
    pkgs = []
    if case["package"] is not None:
        pkgs.append(readable(case["package"]))
    for o in case.get("others") or []:
        if o.get("package") is not None:
            pkgs.append(readable(o["package"]))
    reads = []
    if case.get("read") is not None:
        try:
            orig = load_strict(case["read"])
            r = {"windows": orig.get("captionWindows")} if isinstance(orig, dict) else None
            if r is not None:
                for k in ("span", "gop"):
                    if k in orig:
                        r[k] = orig[k]
            reads = [r]
        except (ValueError, RecursionError):
            reads = [None]
    try:
        plan, essences = None, []
        if isinstance(src, dict):
            c = src.get("container") if isinstance(src.get("container"), dict) else {}
            plan = L.plan_of(probe, c.get("durationMs"))
            if isinstance(src.get("essence"), dict):
                essences = [L.essence_as_read(src["essence"], reads, plan)]
        held = L.gate_holds([src], pkgs, [probe], reads)
        if len(essences) < 1:
            held.setdefault("closedCaptions", "a source record of it can't be read")
            held.setdefault("interlaced", "a source record of it can't be read")
        pkg = pkgs[0] if case["package"] is not None else None
        gate = set(L.deletion_gate(essences, (pkg or {}).get("essence") or {}, held))
        never = sorted(gate & NEVER)
        why = "; ".join(f"{k}: {held.get(k, 'its essence')}" for k in never)
        return ("HOLD" if never else "GO"), never, why
    except Exception as e:  # the record logic can't tell: it holds
        return "HOLD", ["raises"], f"raises {type(e).__name__}: {e}"


# ---- writing a document with values no JSON library writes ----
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


def setp(doc, path, value):
    d = doc
    for k in path[:-1]:
        d = d[k]
    d[path[-1]] = value
    return doc


def getp(doc, path):
    d = doc
    for k in path:
        d = d[k]
    return d


# ---- the plain case: a complete progressive H.264 title the Go gate lets go ----
def essence():
    return {"maxAudioChannels": 2, "surround": False, "losslessAudio": False, "objectAudio": False,
            "maxVideoHeight": 1080, "videoBitDepth": 8, "hdr10Metadata": False, "dolbyVision": False,
            "stereo3d": False, "interlaced": False, "audioLanguages": [], "subtitleLanguages": [],
            "subtitleTracks": 0, "imageSubtitles": False, "styledSubtitles": False, "fonts": False,
            "chapters": False, "commentaryTracks": 0, "commentarySubtitles": 0, "audioDescriptionTracks": 0,
            "sdhSubtitleLanguages": [], "forcedSubtitleLanguages": [], "closedCaptions": False}


# the source the gate is asked of, and another one, by the ids the schema takes (lower-case UUIDs); the test writes
# a source's records into the folder its id names
SOURCE = "a0a0a0a0-0000-4000-8000-0000000000a0"
OTHER = "b0b0b0b0-0000-4000-8000-0000000000b0"


def source(ms=600000):
    """A source record the contract's schema takes (source.schema.json), of what the gate reads and what the record
    logic writes beside it."""
    return {"schema": "zaentrum.library.source/2", "sourceId": SOURCE, "takenAt": "2026-10-01T10:00:00Z",
            "takenBy": "analyzer test",
            "file": {"name": "original.mkv", "kind": "stream-container", "sizeBytes": 6, "mtime": "2026-10-01T09:00:00Z",
                     "fixity": {"qh1": "sha256:" + "0" * 64}, "part": None},
            "container": {"format": "matroska,webm", "durationMs": ms}, "essence": essence(),
            "streams": [{"index": 0, "type": "video", "codec": "h264", "fieldOrder": "progressive", "height": 1080}],
            "fidelity": {"class": "original", "fingerprint": None, "evidence": []},
            "probe": {"tool": "ffprobe", "version": None, "at": "2026-10-01T10:00:00Z",
                      "file": f"sources/{SOURCE}/ffprobe.json", "sha256": None, "note": None}}


def probe(seconds="600.000000", rate="24/1", start="0.000000"):
    return {"format": {"format_name": "matroska,webm", "start_time": start, "duration": seconds},
            "streams": [{"index": 0, "codec_type": "video", "codec_name": "h264", "field_order": "progressive",
                         "avg_frame_rate": rate, "r_frame_rate": rate, "start_time": start, "height": 1080}]}


def scan():
    return {"found": "progressive", "done": "progressive",
            "verified": {"ok": True, "complete": True, "frames": 14400, "packets": 14400, "leading": 0}}


def package():
    return {"schema": "zaentrum.library.package/2", "packageId": "p", "essence": essence(), "subtitles": [],
            "renditions": {"video": [{"id": "v0", "encoder": "libx264", "codec": "avc1.640028", "height": 1080,
                                      "videoRange": "SDR"}]}, "scan": scan()}


def version(ids=(SOURCE,), vid="v-a"):
    return {"schema": "zaentrum.library.version/2", "versionId": vid, "sourceIds": list(ids)}


def window(at, start, frames=48, length=1.88, **more):
    w = {"at": at, "start": start, "end": start + length, "frames": frames, "captions": False,
         "interlacedFrames": 0, "repeatFrames": 0}
    w.update(more)
    return w


def read_of(plan, gop=2.0, whole=None):
    """A read by the plan: six windows at their places, or the whole picture where its places lie too close."""
    if plan is None:
        plan = {"start": 0.0, "end": 600.0, "step": 1 / 24}
    start, end, step = plan["start"], plan["end"], plan["step"]
    r = {"span": {"start": start, "end": end}, "duration": end - start, "sizeBytes": 6, "ffprobe": "7.1.5"}
    if gop is not MISSING:
        r["gop"] = gop
    g = gop if isinstance(gop, (int, float)) and not isinstance(gop, bool) else 0.0
    if whole is None:
        whole = L.plan_short(plan, g)
    if whole:
        packets = math.ceil((end - start) / step)
        r["captionWindows"] = [{"at": 0, "start": start, "end": end, "frames": packets, "captions": False,
                                "interlacedFrames": 0, "repeatFrames": 0, "whole": True, "packets": packets,
                                "leading": 0}]
    else:
        r["captionWindows"] = [window(at, place, length=min(1.88, 40 * step))
                               for at, place in zip(L.CAPTION_WINDOWS, L.plan_places(plan))]
    return r


CASES = []
SEEN = set()


def add(cid, src=None, prb=None, pkg=None, ver=None, others=None, read="plan", gop=2.0):
    """A case: the documents (a dict, Raw text, or MISSING: no file); read "plan": the read the record logic's plan
    of them asks for; None: no read."""
    if cid in SEEN:
        return
    SEEN.add(cid)
    src = source() if src is None else src
    prb = probe() if prb is None else prb
    pkg = package() if pkg is None else pkg
    ver = version() if ver is None else ver
    text = lambda d: None if d is MISSING else (d.text if isinstance(d, Raw) else dumps(d))
    case = {"id": cid, "source": text(src), "probe": text(prb), "package": text(pkg), "version": text(ver),
            "others": [{"id": o["id"], "package": text(o.get("package", MISSING)),
                        "version": text(o.get("version", MISSING))} for o in (others or [])]}
    if isinstance(read, str) and read == "plan":
        s, p = readable(case["source"]), readable(case["probe"])
        plan = None
        try:
            if isinstance(s, dict):
                c = s.get("container") if isinstance(s.get("container"), dict) else {}
                plan = L.plan_of(p, c.get("durationMs"))
        except Exception:
            plan = None
        read = read_of(plan, gop)
    case["read"] = None if read is None else (read.text if isinstance(read, Raw) else dumps(read))
    CASES.append(case)


def nxt(x):
    return math.nextafter(x, math.inf)


def prv(x):
    return math.nextafter(x, -math.inf)


BIG400, DIG4301 = Raw("1" + "0" * 400), Raw("1" + "0" * 4300)


def generic(orig, other):
    return {"missing": MISSING, "null": None, "NaN": Raw("NaN"), "Infinity": Raw("Infinity"),
            "-Infinity": Raw("-Infinity"), "text": "12", "empty": "", "1e999": Raw("1e999"),
            "-1e999": Raw("-1e999"), "10^400": BIG400, "4301digits": DIG4301, "true": True, "false": False,
            "list": [], "object": {}, "twice-same": Twice(orig, orig), "twice-diff": Twice(orig, other),
            "twice-diff2": Twice(other, orig)}


# ---- P: what the plan reads of the probe and the record ----
def gen_plan():
    times = {"ffprobe": "0.000000", "0": 0, "0.0": 0.0, "12.5": 12.5, "t12.5": "12.5", "spaces": " 12.5 ",
             "plus": "+12.5", "exp": "1e1", "hex": "0x10", "underscore": "1_0.5", "fullwidth": "１２",
             "nan": "nan", "inf": "inf", "-inf": "-inf", "tInfinity": "Infinity", ".5": ".5", "5.": "5.",
             "t1e999": "1e999", "fs": "\u001c5", "nbsp": "5 ", "nl": "5\n", "-0.0": "-0.0", "-5": "-5",
             "tiny": Raw("1e-320"), "t1e-999": "1e-999", "comma": "12,5", "t600": "600.000000", "t10": "10.000000",
             "600": 600, "tnull": "null", "tTrue": "True", "bigtext": "1" + "0" * 400, "t4301": "1" + "0" * 4300}
    targets = [
        ("pic.start_time", "probe", ("streams", 0, "start_time"), {}),
        ("pic.start_time,no-format", "probe", ("streams", 0, "start_time"), {("format", "start_time"): MISSING}),
        ("format.start_time,no-pic", "probe", ("format", "start_time"), {("streams", 0, "start_time"): MISSING}),
        ("pic.duration", "probe", ("streams", 0, "duration"), {}),
        ("pic.duration,no-ms", "probe", ("streams", 0, "duration"), {"src": {("container", "durationMs"): MISSING}}),
        ("format.duration,no-ms", "probe", ("format", "duration"), {"src": {("container", "durationMs"): MISSING}}),
        ("format.duration,ms", "probe", ("format", "duration"), {}),
    ]
    for name, where, path, more in targets:
        forms = dict(times)
        forms.update(generic("0.000000" if "start" in name else "600.000000", "5.000000"))
        for fname, form in forms.items():
            p, s = probe(), source()
            for k, v in more.items():
                if k == "src":
                    for kk, vv in v.items():
                        setp(s, kk, vv)
                else:
                    setp(p, k, v)
            setp(p, path, form)
            add(f"P/{name}/{fname}", src=s, prb=p)
    ms = {"600000": 600000, "600000.0": 600000.0, "t": "600000", "0": 0, "-1": -1, "0.5": 0.5, "1": 1,
          "40000": 40000, "7200000": 7200000}
    ms.update(generic(600000, 5000))
    for fmt_duration in ("600.000000", MISSING, "10.000000"):
        for fname, form in ms.items():
            p, s = probe(), source()
            setp(p, ("format", "duration"), fmt_duration)
            setp(s, ("container", "durationMs"), form)
            add(f"P/durationMs,format.duration={'none' if fmt_duration is MISSING else fmt_duration}/{fname}", src=s, prb=p)
    for cont in (MISSING, None, [], "x", {}):
        s = source()
        s["container"] = cont
        add(f"P/container/{'missing' if cont is MISSING else json.dumps(cont)}", src=s)
    rates = {"24/1": "24/1", "24000/1001": "24000/1001", "0/0": "0/0", "24": "24", "24/0": "24/0", "0/1": "0/1",
             "1/1": "1/1", "1/2": "1/2", "2/1": "2/1", "just-below-1": "999999/1000000",
             "just-above-1": "1000001/1000000", "+24/1": "+24/1", " 24/1": " 24/1", "24/1 ": "24/1 ",
             "24.0/1": "24.0/1", "24/1.0": "24/1.0", "fullwidth": "２４/１", "n24": 24, "24/ 1": "24/ 1",
             "4301/1": "9" * 4301 + "/1", "1/4301": "1/" + "9" * 4301, "4300/4300": "9" * 4300 + "/" + "9" * 4300,
             "huge/1": "9" * 400 + "/1", "1/huge": "1/" + "9" * 400, "30000/1001": "30000/1001", "1/1000": "1/1000",
             "00024/00001": "00024/00001", "24/1+nl": "24/1\n", "arabic": "٢٤/١"}
    rates.update(generic("24/1", "25/1"))
    for fname, form in rates.items():
        p = probe()
        setp(p, ("streams", 0, "avg_frame_rate"), form)
        add(f"P/avg_frame_rate/{fname}", prb=p)
        p = probe()
        setp(p, ("streams", 0, "avg_frame_rate"), form)
        setp(p, ("streams", 0, "r_frame_rate"), "25/1")
        add(f"P/avg_frame_rate,r=25/{fname}", prb=p)
        p = probe()
        setp(p, ("streams", 0, "avg_frame_rate"), "0/0")
        setp(p, ("streams", 0, "r_frame_rate"), form)
        add(f"P/r_frame_rate,avg=0/0/{fname}", prb=p)
    for name, form in {"missing": MISSING, "null": None, "list": [], "text": "x"}.items():
        p = probe()
        p["format"] = form
        add(f"P/format/{name}", prb=p)
        p = probe()
        p["streams"] = form
        add(f"P/streams/{name}", prb=p)
    add("P/probe/none", prb=MISSING)
    add("P/probe/list", prb=Raw("[]"))


# ---- R: the read's thresholds ----
def setups():
    out = {}
    out["600s@24"] = (source(), probe())
    out["7200s@23.976"] = (source(7200000), probe("7200.000000", "24000/1001"))
    out["3600s@1"] = (source(3600000), probe("3600.000000", "1/1"))
    out["600s@24,start12.5"] = (source(), probe("600.000000", "24/1", "12.500000"))
    out["40s@24"] = (source(40000), probe("40.000000"))
    # long enough to be read in windows, short enough that the tail places the last (at 90 s, not 108 s)
    out["120s@24"] = (source(120000), probe("120.000000"))
    out["20000s@24"] = (source(20000000), probe("20000.000000"))
    out["600s@25"] = (source(), probe("600.000000", "25/1"))
    # the review's widened pictures: other rates (where a step is no binary fraction), starts that are none either
    # (where the compiler may fuse start + at x length into one multiply-add, which rounds once), a day, and a
    # picture whose average rate is not its real one, or none
    S = lambda ms, secs, rate, start="0.000000": (source(ms), probe(secs, rate, start))
    out.update({
        "600s@29.97": S(600000, "600.000000", "30000/1001"),
        "5400s@59.94": S(5400000, "5400.000000", "60000/1001"),
        "600s@50,start0.08": S(600000, "600.000000", "50/1", "0.080000"),
        "600s@30,start1.4": S(600000, "600.000000", "30/1", "1.400000"),
        "600s@120": S(600000, "600.000000", "120/1"),
        "600s@12": S(600000, "600.000000", "12/1"),
        "900s@15": S(900000, "900.000000", "15/1"),
        "3600s@2": S(3600000, "3600.000000", "2/1"),
        "3600s@1.5": S(3600000, "3600.000000", "3/2"),
        "1800s@1.001": S(1800000, "1800.000000", "1001/1000"),
        "601.337s@23.976": S(601337, "601.337000", "24000/1001"),
        "45s@59.94": S(45000, "45.000000", "60000/1001"),
        "86400s@25": S(86400000, "86400.000000", "25/1"),
        "200s@29.97": S(200000, "200.000000", "30000/1001"),
    })
    s, p = S(600000, "600.000000", "24000/1001")
    p["streams"][0]["r_frame_rate"] = "30000/1001"
    out["600s@avg23.976,r29.97"] = (s, p)
    s, p = S(600000, "600.000000", "0/0")
    p["streams"][0]["r_frame_rate"] = "30000/1001"
    out["600s@avg0:0,r29.97"] = (s, p)
    return out


# the widened pictures (setups)
WIDE = {"600s@29.97", "5400s@59.94", "600s@50,start0.08", "600s@30,start1.4", "600s@120", "600s@12", "900s@15", "3600s@2",
        "3600s@1.5", "1800s@1.001", "601.337s@23.976", "45s@59.94", "86400s@25", "200s@29.97", "600s@avg23.976,r29.97",
        "600s@avg0:0,r29.97"}


def plan_for(s, p):
    return L.plan_of(readable(dumps(p)), s["container"]["durationMs"])


def edges(x):
    return [x, nxt(x), prv(x), x + 4e-4, x - 4e-4, x + 6e-4, x - 6e-4, x + 1e-3, x - 1e-3]


def wide_input(sname, cid):
    """Whether the read cid of the picture sname is one the generator makes: of the widened pictures (WIDE) where a
    window lies and the fraction it says, not what a window says of itself or how many there are (those don't
    depend on the rate), and keyframeBefore given true or not at all."""
    if sname not in WIDE:
        return True
    family = cid.split("/")[0].split(".")[0]
    return family not in ("window", "windows", "at-form", "counts") and \
        (family != "keyframeBefore" or cid.endswith(("/kb=true", "/kb=missing")))


def gen_read():
    for sname, (s, p) in setups().items():
        plan = plan_for(s, p)
        tol = L.caption_tolerance(plan)
        ceiling = L.CAPTION_GOP_PACKETS * plan["step"]
        A = lambda cid, read: wide_input(sname, cid) and add(f"R/{sname}/{cid}", src=copy.deepcopy(s), prb=copy.deepcopy(p),
                                                             read=read)
        A("base", read_of(plan))
        A("base,gop-none", read_of(plan, MISSING))
        A("whole", read_of(plan, 2.0, whole=True))
        # the span it measured
        for end in ("start", "end"):
            for sign in (1, -1):
                for d in edges(tol):
                    for whole in (False, True):
                        r = read_of(plan, 2.0, whole=whole or None)
                        r["span"][end] = plan[end] + sign * d
                        A(f"span.{end}{'+' if sign > 0 else '-'}{d!r}{',whole' if whole else ''}", r)
        for name, form in {"missing": MISSING, "null": None, "list": [], "start-null": {"start": None, "end": plan["end"]},
                           "start-text": {"start": "0", "end": plan["end"]}, "end-missing": {"start": plan["start"]},
                           "reversed": {"start": plan["end"], "end": plan["start"]},
                           "NaN": {"start": Raw("NaN"), "end": plan["end"]},
                           "1e999": {"start": plan["start"], "end": Raw("1e999")},
                           "bool": {"start": False, "end": plan["end"]},
                           "twice": {"start": Twice(plan["start"], plan["start"]), "end": plan["end"]}}.items():
            r = read_of(plan)
            r["span"] = form
            A(f"span/{name}", r)
        # its gop
        gops = {"missing": MISSING, "null": None, "0": 0, "0.0": 0.0, "-0.0": -0.0, "1e-9": 1e-9, "2": 2.0, "9.5": 9.5,
                "9.5+": nxt(9.5), "ceiling": ceiling, "ceiling+ulp": nxt(ceiling), "ceiling-ulp": prv(ceiling),
                "ceiling+0.1": ceiling + 0.1, "50.1": 50.1, "50": 50.0, "50+ulp": nxt(50.0), "-1e-9": -1e-9,
                "text": "2.0", "true": True, "false": False, "NaN": Raw("NaN"), "1e999": Raw("1e999"),
                "10^400": BIG400, "list": [], "twice": Twice(2.0, 2.0), "int": 2, "1200": 1200, "1200.5": 1200.5}
        for name, g in gops.items():
            for whole in (False, True):
                r = read_of(plan, 2.0, whole=whole or None)
                if g is MISSING:
                    r.pop("gop", None)
                else:
                    r["gop"] = g
                A(f"gop/{name}{',whole' if whole else ''}", r)
        if L.plan_short(plan, 2.0):
            continue
        places = L.plan_places(plan)
        # where a window lies
        for gop in (MISSING, 0.0, 2.0, 12.0):
            g = 0.0 if gop is MISSING else gop
            if L.plan_short(plan, g):  # of this gop, a picture read whole (the 120 s one at 12 s)
                continue
            slack = tol if not g else max(10.0, g + 0.5)
            for i in range(6):
                for sign in (1, -1):
                    for d in edges(slack):
                        r = read_of(plan, gop)
                        w = r["captionWindows"][i]
                        w["start"] = places[i] + sign * d
                        w["end"] = w["start"] + 1.5
                        A(f"place/gop={'none' if gop is MISSING else gop}/w{i}{'+' if sign > 0 else '-'}{d!r}", r)
                for d in [-30.0, prv(-30.0), nxt(-30.0), -30.0004, -29.9996, -30.0006, 0.0, 4e-4, -4e-4,
                          slack + 4e-4, -(slack + 1), -15.0, 1e-9, slack + 1, 15.0]:
                    for kb in (True, False, None, "true", 1, MISSING):
                        r = read_of(plan, gop)
                        w = r["captionWindows"][i]
                        w["start"] = places[i] + d
                        w["end"] = w["start"] + 1.5
                        if kb is not MISSING:
                            w["keyframeBefore"] = kb
                        A(f"keyframeBefore/gop={'none' if gop is MISSING else gop}/w{i}{d!r}/kb={'missing' if kb is MISSING else json.dumps(kb)}", r)
        # the fraction it says
        for i in range(6):
            for sign in (1, -1):
                for d in [0.0, 4e-4, 4.9e-4, 5e-4, 5.1e-4, 6e-4, 9e-4, 9.9e-4, 1e-3, nxt(1e-3), prv(1e-3), 1.1e-3, 1.5e-3]:
                    at = L.CAPTION_WINDOWS[i] + sign * d
                    r = read_of(plan)
                    r["captionWindows"][i]["at"] = at
                    A(f"at/w{i}/{at!r}", r)
                    # and its first frame as far from the plan's place as the slack lets it
                    for off in (10.0, 10.0 + 4e-4, 10.3, -10.3, 7.0):
                        r = read_of(plan)
                        w = r["captionWindows"][i]
                        w["at"], w["start"] = at, places[i] + off
                        w["end"] = w["start"] + 1.5
                        A(f"at+place/w{i}/{at!r}/{off!r}", r)
        for name, form in {"text": "0.1", "null": None, "missing": MISSING, "true": True, "NaN": Raw("NaN"),
                           "-0.0": -0.0, "1e999": Raw("1e999"), "10^400": BIG400, "1": 1, "1.0000001": 1.0000001,
                           "-1e-9": -1e-9}.items():
            for i in (0, 1):
                r = read_of(plan)
                r["captionWindows"][i]["at"] = form
                A(f"at-form/w{i}/{name}", r)
        # what a window says of itself
        wforms = {
            "frames": {"23": 23, "24": 24, "0": 0, "47": 47, "49": 49, "text": "48", "48.0": 48.0, "null": None,
                       "missing": MISSING, "true": True, "-1": -1, "1e2": Raw("1e2"), "10^400": BIG400,
                       "twice": Twice(48, 48), "twice-diff": Twice(48, 23)},
            "captions": {"false": False, "true": True, "null": None, "missing": MISSING, "0": 0, "text": "false",
                         "twice": Twice(False, False), "twice-diff": Twice(False, True), "twice-diff2": Twice(True, False)},
            "interlacedFrames": {"0": 0, "1": 1, "48": 48, "49": 49, "null": None, "missing": MISSING, "text": "0",
                                 "0.0": 0.0, "true": True, "false": False, "-1": -1, "10^400": BIG400,
                                 "twice": Twice(0, 0), "twice-diff": Twice(0, 1)},
            "repeatFrames": {"0": 0, "1": 1, "48": 48, "49": 49, "null": None, "missing": MISSING, "text": "0",
                             "0.0": 0.0, "true": True, "false": False, "-1": -1},
            "start": {"null": None, "missing": MISSING, "text": "0", "true": True, "NaN": Raw("NaN"), "1e999": Raw("1e999"),
                      "10^400": BIG400, "-1": -1.0},
            "end": {"null": None, "missing": MISSING, "text": "0", "before-start": -5.0, "NaN": Raw("NaN"),
                    "10^400": BIG400, "equal-start": "=start"},
            "packets": {"48": 48, "null": None, "0": 0, "text": "48", "47": 47},
            "leading": {"0": 0, "null": None, "17": 17, "text": "0"},
            "whole": {"false": False, "null": None, "true": True, "text": "true", "1": 1, "0": 0},
            "keyframeBefore": {"false": False, "null": None, "true": True, "text": "true", "1": 1, "0": 0},
            "unreadable": {"x": "x", "empty": "", "null": None},
            "countsUnreadable": {"true": True, "false": False},
            "extra": {"x": 1},
        }
        for key, forms in wforms.items():
            for name, form in forms.items():
                for i in (0, 2, 5):
                    r = read_of(plan)
                    w = r["captionWindows"][i]
                    if form == "=start":
                        form = w["start"]
                    w[key] = form
                    A(f"window.{key}/w{i}/{name}", r)
                r = read_of(plan)
                for w in r["captionWindows"]:
                    w[key] = w["start"] if form == "=start" else form
                A(f"window.{key}/all/{name}", r)
        # both counts missing (an older read), each window
        r = read_of(plan)
        for w in r["captionWindows"]:
            del w["interlacedFrames"], w["repeatFrames"]
        A("counts/none-given", r)
        # how many windows, and of what shape
        r = read_of(plan)
        r["captionWindows"] = r["captionWindows"][1:]
        A("windows/five-without-start", r)
        r = read_of(plan)
        r["captionWindows"] = r["captionWindows"][:5]
        A("windows/five-without-90", r)
        r = read_of(plan)
        r["captionWindows"].append(window(0.2, places[1] + 30))
        A("windows/seven", r)
        r = read_of(plan)
        r["captionWindows"].append(dict(r["captionWindows"][2]))
        A("windows/seven-one-twice", r)
        r = read_of(plan)
        r["captionWindows"][3] = dict(r["captionWindows"][2])
        A("windows/six-one-twice", r)
        for name, form in {"null": None, "empty": [], "text": "x", "object": {}, "missing": MISSING}.items():
            r = read_of(plan)
            r["captionWindows"] = form
            A(f"windows/{name}", r)
        for name, form in {"null": None, "text": "x", "list": [], "number": 5, "empty-object": {}}.items():
            r = read_of(plan)
            r["captionWindows"][2] = form
            A(f"windows/entry-{name}", r)
            r = read_of(plan)
            r["captionWindows"].append(form)
            A(f"windows/extra-entry-{name}", r)
        r = read_of(plan)
        r["captionWindows"].reverse()
        A("windows/reversed-order", r)
        # a whole read beside windows, and a whole read's own numbers
        r = read_of(plan)
        r["captionWindows"].append(read_of(plan, 2.0, whole=True)["captionWindows"][0])
        A("windows/six-and-whole", r)
    # windows on one another's frames, of a picture whose gop lets them lie far from their places
    s, p = setups()["3600s@1"]
    plan = plan_for(s, p)
    places = L.plan_places(plan)
    for gop in (150.0, 350.0):
        for d in [0.0, prv(0.0), -4e-4, 4e-4, -6e-4, 1e-3, -1e-3, -1.0]:
            r = read_of(plan, gop, whole=False)
            w1, w2 = r["captionWindows"][1], r["captionWindows"][2]
            w1["end"] = w1["start"] + 40
            w2["start"] = w1["end"] + d
            w2["end"] = w2["start"] + 40
            add(f"R/3600s@1/disjoint/gop={gop}/next-start=end{d!r}", src=copy.deepcopy(s), prb=copy.deepcopy(p), read=r)
        for a, e, b in [(360.0001, 360.0002, 360.0004), (360.0, 360.0, 360.0), (360.0, 360.0, 360.0004),
                        (360.0004, 360.0004, 360.0001), (360.0, 360.0004, 360.0004), (360.0, 360.0006, 360.0006),
                        (360.0, 400.0, 400.0), (360.0, 400.0004, 400.0001), (360.0, 400.0005, 400.0005),
                        (360.0006, 360.0006, 360.0014)]:
            r = read_of(plan, gop, whole=False)
            w1, w2 = r["captionWindows"][1], r["captionWindows"][2]
            w1["start"], w1["end"], w2["start"] = a, e, b
            w2["end"] = max(w2["end"], b)
            add(f"R/3600s@1/order/gop={gop}/{a!r},{e!r},{b!r}", src=copy.deepcopy(s), prb=copy.deepcopy(p), read=r)


def gen_whole():
    for sname in ("40s@24", "600s@24", "3600s@1", "600s@24,start12.5", "45s@59.94", "600s@29.97", "600s@50,start0.08",
                  "3600s@2"):
        s, p = setups()[sname]
        plan = plan_for(s, p)
        tol = L.caption_tolerance(plan)
        need = (plan["end"] - plan["start"] - tol) / plan["step"]
        A = lambda cid, read: add(f"W/{sname}/{cid}", src=copy.deepcopy(s), prb=copy.deepcopy(p), read=read)

        def W(**kw):
            r = read_of(plan, 2.0, whole=True)
            w = r["captionWindows"][0]
            for k, v in kw.items():
                if v is MISSING:
                    w.pop(k, None)
                else:
                    w[k] = v
            return r, w
        for packets in sorted({math.floor(need) - 1, math.floor(need), math.ceil(need), math.ceil(need) + 1}):
            for lead in (0, 1, 16):
                for frames in sorted({packets - lead - 1, packets - lead, packets, packets + 1}):
                    r, w = W(packets=packets, frames=frames, leading=lead)
                    A(f"packets={packets},frames={frames},leading={lead}", r)
        for name, form in {"missing": MISSING, "null": None, "0": 0, "16": 16, "17": 17, "text": "0", "1.0": 1.0,
                           "true": True, "-1": -1, "10^400": BIG400, "twice": Twice(0, 0), "twice-diff": Twice(0, 16)}.items():
            r, w = W(leading=form)
            A(f"leading/{name}", r)
        for name, form in {"missing": MISSING, "null": None, "text": "14400", "0": 0, "1.0": 14400.0, "true": True,
                           "10^400": BIG400}.items():
            r, w = W(packets=form)
            A(f"packets/{name}", r)
        for at in (0, 0.0, -0.0, 1e-9, 4e-4, 5e-4, 9.9e-4, 1e-3, 0.1):
            r, w = W(at=at)
            A(f"at/{at!r}", r)
        for end in ("start", "end"):
            for sign in (1, -1):
                for d in edges(tol):
                    r, w = W()
                    w[end] = plan[end] + sign * d
                    A(f"{end}{'+' if sign > 0 else '-'}{d!r}", r)
        for name, form in {"true": True, "false": False, "null": None, "text": "true", "1": 1, "missing": MISSING}.items():
            r, w = W(whole=form)
            A(f"whole/{name}", r)
            r, w = W(keyframeBefore=form)
            A(f"keyframeBefore/{name}", r)
        for key in ("interlacedFrames", "repeatFrames"):
            for name, form in {"1": 1, "null": None, "missing": MISSING}.items():
                r, w = W(**{key: form})
                A(f"{key}/{name}", r)
        r, w = W(captions=True)
        A("captions/true", r)
        r, w = W(frames=0, captions=None)
        A("frames/0", r)
        r, w = W()
        r["captionWindows"].append(dict(w))
        A("twice", r)
        r, w = W()
        r["captionWindows"].append(window(0.5, plan["start"] + 0.5 * (plan["end"] - plan["start"])))
        A("and-a-window", r)


# ---- C: the cover rule ----
def gen_cover():
    tagsets = {
        "none": MISSING, "empty": {}, "both": {"filename": "cover.jpg", "mimetype": "image/jpeg"},
        "filename-only": {"filename": "cover.jpg"}, "mimetype-only": {"mimetype": "image/jpeg"},
        "UPPER-KEYS": {"FILENAME": "cover.jpg", "MIMETYPE": "image/jpeg"},
        "UPPER-MIME": {"filename": "cover.jpg", "mimetype": "IMAGE/JPEG"},
        "spaced-mime": {"filename": "cover.jpg", "mimetype": " image/jpeg "},
        "video-mime": {"filename": "cover.jpg", "mimetype": "video/mp4"},
        "empty-mime": {"filename": "cover.jpg", "mimetype": ""}, "null-mime": {"filename": "cover.jpg", "mimetype": None},
        "number-mime": {"filename": "cover.jpg", "mimetype": 5}, "list-mime": {"filename": "cover.jpg", "mimetype": ["image/png"]},
        "false-mime": {"filename": "cover.jpg", "mimetype": False},
        "empty-filename": {"filename": "", "mimetype": "image/png"}, "null-filename": {"filename": None, "mimetype": "image/png"},
        "clash-filename": {"filename": "a.jpg", "Filename": "b.jpg", "mimetype": "image/jpeg"},
        "twice-filename-same": {"filename": "a.jpg", "Filename": "a.jpg", "mimetype": "image/jpeg"},
        "clash-mime": {"filename": "a.jpg", "mimetype": "image/jpeg", "Mimetype": "image/png"},
        "clash-mime-rev": {"filename": "a.jpg", "Mimetype": "video/mp4", "mimetype": "image/jpeg"},
        "DURATION": {"filename": "a.jpg", "mimetype": "image/jpeg", "DURATION": "00:00:00.040000000"},
        "duration-eng": {"filename": "a.jpg", "mimetype": "image/jpeg", "duration-eng": "x"},
        "Durations": {"filename": "a.jpg", "mimetype": "image/jpeg", "Durations": ""},
        "NOF-1": {"filename": "a.jpg", "mimetype": "image/jpeg", "NUMBER_OF_FRAMES": "1"},
        "NOF-2": {"filename": "a.jpg", "mimetype": "image/jpeg", "NUMBER_OF_FRAMES": "2"},
        "NOF-eng-x": {"filename": "a.jpg", "mimetype": "image/jpeg", "NUMBER_OF_FRAMES-eng": "x"},
        "nof-number": {"filename": "a.jpg", "mimetype": "image/jpeg", "number_of_frames": 1},
        "nof-1.0": {"filename": "a.jpg", "mimetype": "image/jpeg", "number_of_frames": 1.0},
        "nof-null": {"filename": "a.jpg", "mimetype": "image/jpeg", "number_of_frames": None},
        "nof-true": {"filename": "a.jpg", "mimetype": "image/jpeg", "number_of_frames": True},
        "nof-10^400": {"filename": "a.jpg", "mimetype": "image/jpeg", "number_of_frames": BIG400},
        "dotted-I": {"filename": "a.jpg", "mimetype": "İMAGE/jpeg"}, "dotted-I-key": {"filename": "a.jpg", "MİMETYPE": "image/jpeg"},
        "kelvin-key": {"filename": "a.jpg", "mimetype": "image/jpeg", "K": 1, "K": 2},
        "eq-1-1.0": {"filename": "a.jpg", "mimetype": "image/jpeg", "a": 1, "A": 1.0},
        "eq-1-true": {"filename": "a.jpg", "mimetype": "image/jpeg", "a": 1, "A": True},
        "eq-0-false": {"filename": "a.jpg", "mimetype": "image/jpeg", "a": 0, "A": False},
        "eq-lists": {"filename": "a.jpg", "mimetype": "image/jpeg", "a": [1], "A": [1.0]},
        "eq-objects": {"filename": "a.jpg", "mimetype": "image/jpeg", "a": {"x": 1}, "A": {"x": 1}},
        "ne-text-case": {"filename": "a.jpg", "mimetype": "image/jpeg", "a": "x", "A": "X"},
        "ne-1-text": {"filename": "a.jpg", "mimetype": "image/jpeg", "a": 1, "A": "1"},
        "ne-null-0": {"filename": "a.jpg", "mimetype": "image/jpeg", "a": None, "A": 0},
        "eq-big": {"filename": "a.jpg", "mimetype": "image/jpeg", "a": BIG400, "A": BIG400},
        "eq-big-float": {"filename": "a.jpg", "mimetype": "image/jpeg", "a": Raw("1e22"), "A": Raw("10000000000000000000000")},
        "ne-big-float": {"filename": "a.jpg", "mimetype": "image/jpeg", "a": Raw("1e23"), "A": Raw("100000000000000000000000")},
        "eq-float-int53": {"filename": "a.jpg", "mimetype": "image/jpeg", "a": Raw("9007199254740993"), "A": Raw("9007199254740992.0")},
        "eq-neg-zero": {"filename": "a.jpg", "mimetype": "image/jpeg", "a": Raw("-0.0"), "A": 0},
        "text": "x", "list": [], "null": None,
    }
    pics = {"missing": MISSING, "1": 1, "true": True, "t1": "1", "2": 2, "1.0": 1.0, "0": 0, "false": False,
            "null": None, "1e0": Raw("1e0"), "10^400": BIG400}
    codecs = {"mjpeg": "mjpeg", "png": "png", "bmp": "bmp", "webp": "webp", "MJPEG": "MJPEG", "missing": MISSING,
              "null": None, "list": ["mjpeg"], "h264": "h264"}
    nbs = {"missing": MISSING, "t1": "1", "t0": "0", "t2": "2", "1": 1, "0": 0, "2": 2, "1.0": 1.0, "t1.0": "1.0",
           " 1": " 1", "+1": "+1", "empty": "", "null": None, "fullwidth": "１", "true": True, "01": "01",
           "zeros1": "0" * 30 + "1", "4301": "0" * 4300 + "1", "t-0": "-0", "-0": Raw("-0"), "arabic": "١", "10^400": BIG400,
           "list": ["1"], "false": False}

    def case(cid, pic, codec, tags, nb, first=False, rec="film-only", disp_key="disposition"):
        p, s = probe(), source()
        cover = {"index": 1, "codec_type": "video", "codec_name": codec, "nb_frames": nb, "tags": tags}
        if pic is not MISSING or disp_key != "disposition":
            cover[disp_key] = {"attached_pic": pic} if pic is not MISSING else {}
        rcover = {"index": 1, "type": "video", "codec": codec if isinstance(codec, str) else "mjpeg"}
        if rec == "marked":
            rcover["dispositions"] = {"attachedPic": True}
        if first:
            p["streams"][0]["index"], cover["index"] = 1, 0
            p["streams"].insert(0, cover)
            s["streams"][0]["index"], rcover["index"] = 1, 0
            if rec != "film-only":
                s["streams"].insert(0, rcover)
        else:
            p["streams"].append(cover)
            if rec != "film-only":
                s["streams"].append(rcover)
        add(cid, src=s, prb=p)
    for rec in ("film-only", "marked", "unmarked"):
        for first in (False, True):
            suffix = f"{rec}{',cover-first' if first else ''}"
            for tname, tags in tagsets.items():
                for pname, cname, nname in (("1", "mjpeg", "missing"), ("missing", "mjpeg", "missing"), ("1", "png", "t1"),
                                            ("missing", "png", "t1")):
                    case(f"C/tags={tname}/pic={pname},codec={cname},nb={nname}/{suffix}", pics[pname], codecs[cname], tags,
                         nbs[nname], first, rec)
            for pname, pic in pics.items():
                for cname, codec in codecs.items():
                    for tname in ("none", "both", "filename-only"):
                        case(f"C/pic={pname}/codec={cname},tags={tname}/{suffix}", pic, codec, tagsets[tname], MISSING, first, rec)
            for nname, nb in nbs.items():
                for pname, tname in (("1", "none"), ("missing", "both"), ("1", "NOF-1")):
                    case(f"C/nb_frames={nname}/pic={pname},tags={tname}/{suffix}", pics[pname], "mjpeg", tagsets[tname], nb, first, rec)
    # the disposition itself of another shape
    for name, disp in {"text": "x", "list": [], "null": None, "attached_pic-twice": {"attached_pic": Twice(1, 1)}}.items():
        p = probe()
        p["streams"].append({"index": 1, "codec_type": "video", "codec_name": "mjpeg", "disposition": disp})
        add(f"C/disposition/{name}", prb=p)
    # the film itself marked a cover: no film at all
    for name, st in {"film-marked-h264": {"disposition": {"attached_pic": 1}},
                     "film-is-mjpeg-marked": {"codec_name": "mjpeg", "disposition": {"attached_pic": 1}},
                     "film-is-mjpeg-unmarked": {"codec_name": "mjpeg"},
                     "film-is-mjpeg-marked-2frames": {"codec_name": "mjpeg", "disposition": {"attached_pic": 1}, "nb_frames": "14400"}}.items():
        for recmark in (False, True):
            p, s = probe(), source()
            p["streams"][0].update(st)
            if "codec_name" in st:
                s["streams"][0]["codec"] = st["codec_name"]
            if recmark:
                s["streams"][0]["dispositions"] = {"attachedPic": True}
            add(f"C/{name}/record-marked={recmark}", src=s, prb=p)


# ---- L: the closed lists, as written ----
def gen_lists():
    names = {"matroska,webm": "matroska,webm", "Mixed": "Matroska,WebM", "lead-space": " matroska,webm",
             "trail-space": "matroska,webm ", "flv": "flv", "mp4": "mov,mp4,m4a,3gp,3g2,mj2", "avi": "avi",
             "mpegts": "mpegts", "mpeg": "mpeg", "MPEGTS": "MPEGTS", "missing": MISSING, "null": None, "empty": "",
             "number": 5, "list": ["matroska,webm"], "webm-alone": "webm", "twice": Twice("matroska,webm", "matroska,webm"),
             "twice-diff": Twice("matroska,webm", "flv"), "false": False, "0": 0}
    offs = {"no-key": MISSING, "[]": []}
    for oname, off in offs.items():
        for name, form in names.items():
            p, s = probe(), source()
            setp(p, ("format", "format_name"), form)
            if off is not MISSING:
                s["offList"] = off
            add(f"L/probe.format_name/{name}/offList={oname}", src=s, prb=p)
            p, s = probe(), source()
            setp(s, ("container", "format"), form)
            if off is not MISSING:
                s["offList"] = off
            add(f"L/record.container.format/{name}/offList={oname}", src=s, prb=p)
    for name, form in {"[codec:vc1]": ["codec:vc1"], "text": "x", "[1]": [1], "['']": [""], "null": None, "object": {},
                       "[container:flv]": ["container:flv"], "twice": Twice([], []), "twice-diff": Twice([], ["codec:vc1"]),
                       "[[]]": [[]], "false": False, "0": 0, "[null]": [None], "[' ']": [" "]}.items():
        s = source()
        s["offList"] = form
        add(f"L/record.offList/{name}", src=s)
    codecs = {"h264": "h264", "H264": "H264", " h264": " h264", "hevc": "hevc", "mpeg2video": "mpeg2video",
              "mpeg1video": "mpeg1video", "av1": "av1", "vc1": "vc1", "unknown": "unknown", "missing": MISSING,
              "null": None, "empty": "", "number": 5, "mpeg4": "mpeg4", "false": False, "list": ["h264"]}
    for oname, off in offs.items():
        for name, form in codecs.items():
            p, s = probe(), source()
            setp(p, ("streams", 0, "codec_name"), form)
            if off is not MISSING:
                s["offList"] = off
            add(f"L/probe.film.codec_name/{name}/offList={oname}", src=s, prb=p)
            p, s = probe(), source()
            setp(s, ("streams", 0, "codec"), form)
            if off is not MISSING:
                s["offList"] = off
            add(f"L/record.film.codec/{name}/offList={oname}", src=s, prb=p)
            p, s = probe(), source()
            setp(p, ("streams", 0, "codec_name"), form)
            setp(s, ("streams", 0, "codec"), form)
            if off is not MISSING:
                s["offList"] = off
            add(f"L/both.film.codec/{name}/offList={oname}", src=s, prb=p)
    for name, form in {"486": 486, "486.0": 486.0, "t486": "486", "512": 512, "608": 608, "1080": 1080,
                       "missing": MISSING, "null": None, "487": 487, "true": True, "t486 ": "486 "}.items():
        for where in ("probe", "record", "both"):
            for oname, off in offs.items():
                p, s = probe(), source()
                if where in ("probe", "both"):
                    setp(p, ("streams", 0, "height"), form)
                if where in ("record", "both"):
                    setp(s, ("streams", 0, "height"), form)
                if off is not MISSING:
                    s["offList"] = off
                add(f"L/height/{where}/{name}/offList={oname}", src=s, prb=p)
    # another stream beside the film: (probe's stream, record's stream)
    extra = {
        "audio-aac": ({"codec_type": "audio", "codec_name": "aac"}, {"type": "audio", "codec": "aac", "language": "und"}),
        "Audio": ({"codec_type": "Audio", "codec_name": "aac"}, {"type": "Audio", "codec": "aac"}),
        "subrip": ({"codec_type": "subtitle", "codec_name": "subrip"}, {"type": "subtitle", "codec": "subrip"}),
        "SubRip": ({"codec_type": "subtitle", "codec_name": "SubRip"}, {"type": "subtitle", "codec": "SubRip"}),
        "pgs": ({"codec_type": "subtitle", "codec_name": "hdmv_pgs_subtitle"}, {"type": "subtitle", "codec": "hdmv_pgs_subtitle"}),
        "sub-unknown": ({"codec_type": "subtitle", "codec_name": "xsub"}, {"type": "subtitle", "codec": "xsub"}),
        "sub-nocodec": ({"codec_type": "subtitle"}, {"type": "subtitle"}),
        "data-tmcd": ({"codec_type": "data", "codec_tag_string": "tmcd"}, {"type": "data"}),
        "data-TMCD": ({"codec_type": "data", "codec_tag_string": "TMCD"}, {"type": "data"}),
        "data-scte": ({"codec_type": "data", "codec_name": "scte_35"}, {"type": "data", "codec": "scte_35"}),
        "data-bin": ({"codec_type": "data", "codec_name": "bin_data", "codec_tag_string": "text", "nb_frames": "2"},
                     {"type": "data", "codec": "bin_data"}),
        "data-none": ({"codec_type": "data"}, {"type": "data"}),
        "attachment": ({"codec_type": "attachment", "codec_name": "ttf"}, {"type": "attachment", "codec": "ttf"}),
        "eia_608": ({"codec_type": "subtitle", "codec_name": "eia_608"}, {"type": "subtitle", "codec": "eia_608", "closedCaptions": True}),
        "EIA_608": ({"codec_type": "subtitle", "codec_name": "EIA_608"}, {"type": "subtitle", "codec": "EIA_608"}),
        "c608": ({"codec_type": "data", "codec_tag_string": "c608"}, {"type": "data", "closedCaptions": True}),
        "C708": ({"codec_type": "data", "codec_tag_string": "C708"}, {"type": "data"}),
        "second-film": ({"codec_type": "video", "codec_name": "h264", "field_order": "progressive", "avg_frame_rate": "24/1"},
                        {"type": "video", "codec": "h264", "fieldOrder": "progressive"}),
        "no-kind": ({"codec_name": "aac"}, {"codec": "aac"}),
        "null-kind": ({"codec_type": None, "codec_name": "aac"}, {"type": None, "codec": "aac"}),
        "no-object": ("x", "x"), "null-stream": (None, None),
        "record-cc-on-subrip": ({"codec_type": "subtitle", "codec_name": "subrip"}, {"type": "subtitle", "codec": "subrip", "closedCaptions": True}),
        "record-cc-text-on-subrip": ({"codec_type": "subtitle", "codec_name": "subrip"}, {"type": "subtitle", "codec": "subrip", "closedCaptions": "yes"}),
        "record-cc-on-audio": ({"codec_type": "audio", "codec_name": "aac"}, {"type": "audio", "codec": "aac", "closedCaptions": True, "language": "und"}),
    }
    for ename, (ps, rs) in extra.items():
        for which in ("both", "probe-only", "record-only"):
            for oname, off in {"no-key": MISSING, "[]": [], "as-probe": "probe"}.items():
                p, s = probe(), source()
                if which != "record-only":
                    p["streams"].append(dict(ps, index=1) if isinstance(ps, dict) else ps)
                if which != "probe-only":
                    s["streams"].append(dict(rs, index=1) if isinstance(rs, dict) else rs)
                if ename == "data-bin":
                    p["format"]["format_name"] = "mov,mp4,m4a,3gp,3g2,mj2"
                    s["container"]["format"] = "mov,mp4,m4a,3gp,3g2,mj2"
                    p["chapters"] = [{}, {}]
                if off == "probe":
                    try:
                        s["offList"] = L.off_list(readable(dumps(p)))
                    except Exception:
                        continue
                elif off is not MISSING:
                    s["offList"] = off
                add(f"L/stream/{ename}/{which}/offList={oname}", src=s, prb=p)


# ---- E: what the record says ----
def gen_record():
    def E(cid, f, **kw):
        s, p = source(), probe()
        f(s, p)
        add(f"E/{cid}", src=s, prb=p, **kw)
    for name, form in {"false": False, "true": True, "missing": MISSING, "null": None, "0": 0, "text": "false", "1": 1,
                       "list": [], "twice": Twice(False, False), "twice-diff": Twice(False, True)}.items():
        E(f"essence.interlaced/{name}", lambda s, p: setp(s, ("essence", "interlaced"), form))
        E(f"essence.closedCaptions/{name}", lambda s, p: setp(s, ("essence", "closedCaptions"), form))
        E(f"essence.closedCaptionsKnown/{name}", lambda s, p: setp(s, ("essence", "closedCaptionsKnown"), form))
    for name, form in {"missing": MISSING, "null": None, "list": [], "text": "x", "empty": {}}.items():
        E(f"essence/{name}", lambda s, p: s.__setitem__("essence", form))
    for name, form in {"missing": MISSING, "null": None, "empty": [], "text": "x", "[text]": ["x"], "[null]": [None],
                       "object": {}}.items():
        E(f"streams/{name}", lambda s, p: s.__setitem__("streams", form))
    add("E/source/none", src=MISSING)
    add("E/source/list", src=Raw("[]"))
    add("E/source/null", src=Raw("null"))
    orders = {"progressive": "progressive", "Progressive": "Progressive", "lead": " progressive", "trail": "progressive ",
              "UPPER": "PROGRESSIVE", "tt": "tt", "unknown": "unknown", "missing": MISSING, "null": None, "empty": "",
              "number": 5, "true": True, "list": ["progressive"]}
    for name, form in orders.items():
        E(f"record.fieldOrder/{name}", lambda s, p: setp(s, ("streams", 0, "fieldOrder"), form))
        E(f"probe.field_order/{name}", lambda s, p: setp(p, ("streams", 0, "field_order"), form))
        E(f"both.fieldOrder/{name}", lambda s, p: (setp(s, ("streams", 0, "fieldOrder"), form),
                                                   setp(p, ("streams", 0, "field_order"), form)))
    for codec in ("h264", "hevc"):
        for name, form in {"missing": MISSING, "false": False, "true": True, "null": None, "text-false": "false", "0": 0,
                           "1": 1, "text-true": "true"}.items():
            def f(s, p):
                setp(s, ("streams", 0, "codec"), codec)
                setp(p, ("streams", 0, "codec_name"), codec)
                setp(s, ("streams", 0, "captionSignature"), form)
            E(f"captionSignature/{codec}/{name}", f)
        E(f"codec/{codec}-by-probe-alone", lambda s, p: setp(p, ("streams", 0, "codec_name"), codec))
        E(f"codec/{codec}-by-record-alone", lambda s, p: setp(s, ("streams", 0, "codec"), codec))
    for name, form in {"missing": MISSING, "empty": {}, "false": {"attachedPic": False}, "true": {"attachedPic": True},
                       "text": {"attachedPic": "yes"}, "1": {"attachedPic": 1}, "0": {"attachedPic": 0},
                       "null": {"attachedPic": None}, "list": {"attachedPic": [0]}, "not-object": "x",
                       "captions": {"captions": True}, "empty-text": {"attachedPic": ""}, "empty-list": {"attachedPic": []},
                       "0.0": {"attachedPic": 0.0}, "text-false": {"attachedPic": "false"}}.items():
        E(f"record.film.dispositions/{name}", lambda s, p: setp(s, ("streams", 0, "dispositions"), form))
    for name, form in {"0": 0, "0.0": 0.0, "t0": "0", "1": 1, "missing": MISSING, "null": None, "false": False,
                       "-0": Raw("-0"), "10^400": BIG400}.items():
        E(f"record.film.index/{name}", lambda s, p: setp(s, ("streams", 0, "index"), form))
        E(f"probe.film.index/{name}", lambda s, p: setp(p, ("streams", 0, "index"), form))
        E(f"both.film.index/{name}", lambda s, p: (setp(s, ("streams", 0, "index"), form), setp(p, ("streams", 0, "index"), form)))
    for name, form in {"missing": MISSING, "0": 0, "1": 1, "t1": "1", "true": True, "2": 2, "t0": "0"}.items():
        E(f"probe.film.closed_captions/{name}", lambda s, p: setp(p, ("streams", 0, "closed_captions"), form))
    E("probe.film.disposition.captions", lambda s, p: setp(p, ("streams", 0, "disposition"), {"captions": 1}))
    # the record's own read of its picture
    plan = plan_for(source(), probe())
    six = read_of(plan)["captionWindows"]
    shows = copy.deepcopy(six)
    shows[3]["captions"] = True
    fields = copy.deepcopy(six)
    fields[2]["interlacedFrames"] = 5
    badcount = copy.deepcopy(six)
    badcount[2]["interlacedFrames"] = "5"
    forms = {"missing": MISSING, "empty": [], "six": six, "shows-captions": shows, "counted-fields": fields,
             "count-unreadable": badcount, "text": "x", "null": None, "[text]": ["x"], "object": {}}
    for name, form in forms.items():
        for known in (MISSING, True, False):
            for with_read in (True, False):
                def f(s, p):
                    setp(s, ("streams", 0, "captionWindows"), form)
                    if isinstance(form, list) and form:
                        s["streams"][0]["captionSpan"] = {"start": 0.0, "end": 600.0}
                        s["streams"][0]["captionGop"] = 2.0
                    setp(s, ("essence", "closedCaptionsKnown"), known)
                E(f"record.captionWindows/{name}/known={'missing' if known is MISSING else known}/read={with_read}", f,
                  read="plan" if with_read else None)
    # the record's own read tells none, its essence may know it: a read of the original that shows captions, or one
    # that can't be read, holds
    showing = read_of(plan)
    showing["captionWindows"][3]["captions"] = True
    windows_text = read_of(plan)
    windows_text["captionWindows"] = "x"
    twice = read_of(plan)
    twice["gop"] = Twice(2.0, 2.0)
    for rname, read in {"shows-captions": showing, "windows-text": windows_text, "key-twice": twice,
                        "no-object": Raw("[]")}.items():
        for known in (MISSING, True, False):
            def f(s, p):
                setp(s, ("streams", 0, "captionWindows"), copy.deepcopy(six))
                s["streams"][0]["captionSpan"] = {"start": 0.0, "end": 600.0}
                s["streams"][0]["captionGop"] = 2.0
                setp(s, ("essence", "closedCaptionsKnown"), known)
            E(f"record.captionWindows/six/known={'missing' if known is MISSING else known}/read={rname}", f,
              read=copy.deepcopy(read))
    add("E/no-read", read=None)


# ---- S: scans ----
def gen_scan():
    def S(cid, f, **kw):
        pkg = package()
        f(pkg)
        add(f"S/{cid}", pkg=pkg, **kw)
    for name, form in {"progressive": "progressive", "Progressive": "Progressive", "fields": "fields", "unknown": "unknown",
                       "empty": "", "null": None, "missing": MISSING, "number": 5, "list": ["progressive"],
                       "trail": "progressive ", "true": True}.items():
        S(f"found/{name}", lambda p: setp(p, ("scan", "found"), form))
    for name, form in {"progressive": "progressive", "none": "none", "deinterlaced-frame": "deinterlaced-frame",
                       "fields-kept": "fields-kept", "null": None, "missing": MISSING, "None": "None", "number": 5,
                       "inverse-telecine": "inverse-telecine", "NONE": "NONE", "list": ["none"]}.items():
        S(f"done/{name}", lambda p: setp(p, ("scan", "done"), form))
    ok = {"ok": True, "complete": True}
    verified = {"missing": MISSING, "null": None, "empty": {}, "ok-complete-only": ok, "text": "x", "list": [],
                "ok-text": dict(ok, ok="true", frames=10, packets=10, leading=0),
                "ok-1": dict(ok, ok=1, frames=10, packets=10, leading=0),
                "complete-false": dict(ok, complete=False, frames=10, packets=10, leading=0),
                "complete-missing": {"ok": True, "frames": 10, "packets": 10, "leading": 0},
                "frames=packets-leading": dict(ok, frames=14399, packets=14400, leading=1),
                "frames=packets,leading=1": dict(ok, frames=14400, packets=14400, leading=1),
                "frames<packets-leading": dict(ok, frames=14398, packets=14400, leading=1),
                "frames>packets": dict(ok, frames=14401, packets=14400, leading=0),
                "no-leading": dict(ok, frames=14400, packets=14400),
                "no-packets": dict(ok, frames=14400, leading=0),
                "no-frames": dict(ok, packets=14400, leading=0),
                "frames-text": dict(ok, frames="14400", packets=14400, leading=0),
                "frames-float": dict(ok, frames=14400.0, packets=14400, leading=0),
                "leading-null": dict(ok, frames=14400, packets=14400, leading=None),
                "leading-17": dict(ok, frames=14383, packets=14400, leading=17),
                "leading-true": dict(ok, frames=14399, packets=14400, leading=True),
                "all-zero": dict(ok, frames=0, packets=0, leading=0),
                "negative": dict(ok, frames=-1, packets=0, leading=1),
                "big": dict(ok, frames=BIG400, packets=BIG400, leading=0)}
    for name, form in verified.items():
        S(f"verified/{name}", lambda p: setp(p, ("scan", "verified"), form))
    for name, form in {"missing": MISSING, "0": 0, "1": 1, "null": None, "text": "0", "0.0": 0.0, "true": True,
                       "false": False, "10^400": BIG400, "-1": -1, "list": [], "twice": Twice(0, 0), "twice-diff": Twice(0, 1)}.items():
        S(f"chunksInFields/{name}", lambda p: setp(p, ("scan", "verified", "chunksInFields"), form))
    idets = {"missing": MISSING, "null": None, "empty": {}, "all-progressive": {"tff": 0, "bff": 0, "progressive": 100},
             "more-fields": {"tff": 60, "bff": 50, "progressive": 100}, "equal": {"tff": 50, "bff": 50, "progressive": 100},
             "tff-null": {"tff": None, "progressive": 100}, "tff-text": {"tff": "1", "progressive": 100},
             "tff-float": {"tff": 1.0, "progressive": 100}, "progressive-true": {"progressive": True},
             "tff-true": {"tff": True, "progressive": 100}, "text": "x", "list": [], "tff-only": {"tff": 1},
             "big-equal": {"tff": BIG400, "progressive": BIG400}, "big-more": {"tff": BIG400, "bff": 1, "progressive": BIG400},
             "negative": {"tff": -1, "progressive": 100}, "false": False, "0": 0, "empty-text": ""}
    for name, form in idets.items():
        S(f"idet/{name}", lambda p: setp(p, ("scan", "idet"), form))
    for name, form in {"missing": MISSING, "null": None, "text": "x", "list": [], "empty": {}, "false": False, "0": 0}.items():
        S(f"scan/{name}", lambda p: p.__setitem__("scan", form))
    # a scan under the essence alone, and one that holds there beside a good one
    def under(p):
        p["essence"]["scan"] = {"found": "fields"}
    S("scan/under-essence-found-fields", under)
    add("S/package/list", pkg=Raw("[]"))
    add("S/package/null", pkg=Raw("null"))
    add("S/package/none", pkg=MISSING)
    pk = package()
    pk["scan"] = Twice(scan(), scan())
    add("S/package/scan-twice", pkg=pk)
    pk = package()
    pk["essence"] = "x"
    add("S/package/essence-text", pkg=pk)
    # the packages of the title's other versions
    older = {
        "good": scan(), "no-scan": MISSING, "scan-null": None, "found-fields": {"found": "fields", "done": "deinterlaced-frame"},
        "found-progressive-unverified": {"found": "progressive", "done": "none"},
        "found-progressive-no-done": {"found": "progressive", "verified": scan()["verified"]},
        "found-progressive-did-something": dict(scan(), done="deinterlaced-frame"),
        "verified-no-leading": {"found": "progressive", "done": "none", "verified": {"ok": True, "complete": True, "frames": 10, "packets": 10}},
        "verified-counts-off": {"found": "progressive", "done": "none", "verified": {"ok": True, "complete": True, "frames": 9, "packets": 10, "leading": 0}},
        "chunks-in-fields": dict(scan(), verified=dict(scan()["verified"], chunksInFields=3)),
        "idet-more-fields": dict(scan(), idet={"tff": 60, "bff": 50, "progressive": 100}),
        "scan-text": "x", "scan-list": [], "scan-empty": {}, "found-unknown": {"found": "unknown"}, "found-null": {"found": None},
        "found-missing-done-none": {"done": "none"}, "found-number": {"found": 5},
    }
    vers = {"same-source": version((SOURCE,), "v-old"), "other-source": version((OTHER,), "v-old"),
            "both-sources": version((SOURCE, OTHER), "v-old"), "no-version.json": MISSING,
            "sourceIds-null": dict(version((), "v-old"), sourceIds=None), "sourceIds-text": dict(version((), "v-old"), sourceIds=SOURCE),
            "version.json-list": Raw("[]")}
    for sname, sc in older.items():
        for vname, ver in vers.items():
            pk = package()
            if sc is MISSING:
                del pk["scan"]
            else:
                pk["scan"] = sc
            add(f"S/older/{sname}/{vname}", others=[{"id": "v-old", "package": pk, "version": ver}])
    for vname, ver in vers.items():
        add(f"S/older/package-list/{vname}", others=[{"id": "v-old", "package": Raw("[]"), "version": ver}])
        add(f"S/older/package-not-json/{vname}", others=[{"id": "v-old", "package": Raw("{ not json"), "version": ver}])
        pk = package()
        pk["scan"] = Twice(scan(), scan())
        add(f"S/older/package-key-twice/{vname}", others=[{"id": "v-old", "package": pk, "version": ver}])
        add(f"S/older/no-package/{vname}", others=[{"id": "v-old", "version": ver}])


# ---- J: what a loader refuses ----
def gen_loader():
    for which in ("source", "probe", "package", "version"):
        base = {"source": source, "probe": probe, "package": package, "version": version}[which]
        forms = {"x-NaN": Raw("NaN"), "x-Infinity": Raw("Infinity"), "x-1e999": Raw("1e999"), "x-10^400": BIG400,
                 "x-4300digits": Raw("9" * 4300), "x-4301digits": DIG4301, "x-twice": Twice(1, 1), "x-twice-diff": Twice(1, 2),
                 "x-nested-twice": {"y": Twice(1, 1)}, "x-1e400-text": "1e400", "x-float-long": Raw("0." + "1" * 5000),
                 "x-neg-4301": Raw("-" + "1" + "0" * 4300), "x-1e-999": Raw("1e-999"), "x-list-twice": [{"y": Twice(1, 2)}],
                 "x-5000digits.0": Raw("1" + "0" * 5000 + ".0"), "x-1E999": Raw("1E999"), "x-10e998": Raw("10e998"),
                 "x-1.7976931348623157e308": Raw("1.7976931348623157e308"), "x-1.7976931348623159e308": Raw("1.7976931348623159e308"),
                 "x-1e309": Raw("1e309"), "x-4300digits.5": Raw("9" * 4300 + ".5")}
        for name, form in forms.items():
            d = base()
            d["x"] = form
            add(f"J/{which}/{name}", **{{"source": "src", "probe": "prb", "package": "pkg", "version": "ver"}[which]: d})
        # the empty key given twice: at the top, and where any key may stand (a container's or a format's tags)
        d = base()
        d[""] = Twice(1, 2)
        add(f"J/{which}/empty-key-twice", **{{"source": "src", "probe": "prb", "package": "pkg", "version": "ver"}[which]: d})
        d = base()
        d[""] = Twice(1, 1)
        add(f"J/{which}/empty-key-twice-alike", **{{"source": "src", "probe": "prb", "package": "pkg", "version": "ver"}[which]: d})
        if which in ("source", "probe"):
            d = base()
            d["container" if which == "source" else "format"]["tags"] = {"": Twice("x", "y")}
            add(f"J/{which}/tags-empty-key-twice", **{{"source": "src", "probe": "prb"}[which]: d})
            d = base()
            d["container" if which == "source" else "format"]["tags"] = {"": "x"}
            add(f"J/{which}/tags-empty-key", **{{"source": "src", "probe": "prb"}[which]: d})
        text = dumps(base())
        for name, raw in {"bom": "﻿" + text, "trailing": text + " x", "trailing-object": text + " {}", "empty": "",
                          "spaces": "  ", "list": "[" + text + "]", "comment": "// x\n" + text, "single-quotes": text.replace('"', "'"),
                          "trailing-comma": text[:-1] + ",}", "nul-after": text + "\x00", "newline": text + "\n"}.items():
            add(f"J/{which}/text-{name}", **{{"source": "src", "probe": "prb", "package": "pkg", "version": "ver"}[which]: Raw(raw)})


    # a read's report that gives the empty key twice, at the top or in its span
    plan = {"start": 0.0, "end": 600.0, "step": 1 / 24}
    for name, f in {"empty-key-twice": lambda r: r.__setitem__("", Twice(1, 2)),
                    "empty-key-twice-alike": lambda r: r.__setitem__("", Twice(1, 1)),
                    "span-empty-key-twice": lambda r: r["span"].__setitem__("", Twice(1, 2)),
                    "empty-key": lambda r: r.__setitem__("", 1)}.items():
        r = read_of(plan)
        f(r)
        add(f"J/read/{name}", read=r)


# ---- X: the gate's own reading against the record's key and mark, each way (the review's second batch) ----
def cover_probe(marked=1, codec="mjpeg", tags=True, index=1):
    st = {"index": index, "codec_type": "video", "codec_name": codec}
    if tags:
        st["tags"] = {"filename": "cover.jpg", "mimetype": "image/jpeg"}
    if marked is not MISSING:
        st["disposition"] = {"attached_pic": marked}
    return st


def gen_extra():
    # ---- b3: what the record marks, and what the probe's rule says ----
    forms = {"true": True, "1": 1, "t-true": "true", "t-false": "false", "0": 0, "false": False, "null": None,
             "list": [], "empty-text": "", "object": {}, "missing": MISSING, "1.0": 1.0, "2": 2}
    # the probe's rule: a cover; the record's mark of it in every form
    for name, v in forms.items():
        p, s = probe(), source()
        p["streams"].append(cover_probe())
        rc = {"index": 1, "type": "video", "codec": "mjpeg"}
        if v is not MISSING:
            rc["dispositions"] = {"attachedPic": v}
        s["streams"].append(rc)
        add(f"X/b3/probe-cover/record-attachedPic={name}", src=s, prb=p)
    # the probe's rule: a film (one film, nothing else); the record marks its only video stream a cover, every form
    for name, v in forms.items():
        s = source()
        if v is not MISSING:
            s["streams"][0]["dispositions"] = {"attachedPic": v}
        add(f"X/b3/probe-film/record-film-attachedPic={name}", src=s)
    # swapped: the probe's film is stream 0 and its cover stream 1; the record marks stream 0 and leaves stream 1
    p, s = probe(), source()
    p["streams"].append(cover_probe())
    s["streams"][0]["dispositions"] = {"attachedPic": True}
    s["streams"].append({"index": 1, "type": "video", "codec": "mjpeg"})
    add("X/b3/swapped/record-marks-the-film", src=s, prb=p)
    p, s = probe(), source()
    p["streams"].append(cover_probe())
    s["streams"][0]["dispositions"] = {"attachedPic": True}
    s["streams"].append({"index": 1, "type": "video", "codec": "h264", "fieldOrder": "progressive", "height": 1080})
    add("X/b3/swapped/record-marks-the-film,its-other-an-h264", src=s, prb=p)
    # the record's cover at another index than the probe's
    for ri in (2, 0, "1", 1.0, None, MISSING):
        p, s = probe(), source()
        p["streams"].append(cover_probe())
        rc = {"index": ri, "type": "video", "codec": "mjpeg", "dispositions": {"attachedPic": True}}
        if ri is MISSING:
            del rc["index"]
        s["streams"].append(rc)
        add(f"X/b3/record-cover-index={ri if ri is not MISSING else 'missing'}", src=s, prb=p)
    for ri in (2, 0, "1", 1.0, None, MISSING):
        p, s = probe(), source()
        p["streams"].append(cover_probe())
        rc = {"index": ri, "type": "video", "codec": "mjpeg"}
        if ri is MISSING:
            del rc["index"]
        s["streams"].append(rc)
        add(f"X/b3/record-unmarked-cover-index={ri if ri is not MISSING else 'missing'}", src=s, prb=p)
    # the record marks a cover the probe does not list at all; and the probe lists a cover the record lists as
    # another kind
    s = source()
    s["streams"].append({"index": 1, "type": "video", "codec": "mjpeg", "dispositions": {"attachedPic": True}})
    add("X/b3/record-cover-the-probe-lists-none", src=s)
    s = source()
    s["streams"].append({"index": 1, "type": "video", "codec": "mjpeg"})
    add("X/b3/record-unmarked-video-the-probe-lists-none", src=s)
    for kind in ("attachment", "audio", "data", "subtitle"):
        p, s = probe(), source()
        p["streams"].append(cover_probe())
        s["streams"].append({"index": 1, "type": kind, "codec": "mjpeg"})
        add(f"X/b3/probe-cover/record-lists-it-as-{kind}", src=s, prb=p)
    # the probe's second stream a film by the rule (h264, no mark, no tags); the record marks it a cover: every form
    for name, v in forms.items():
        p, s = probe(), source()
        p["streams"].append({"index": 1, "codec_type": "video", "codec_name": "h264", "field_order": "progressive",
                             "avg_frame_rate": "24/1", "r_frame_rate": "24/1", "start_time": "0.000000", "height": 1080})
        rc = {"index": 1, "type": "video", "codec": "h264"}
        if v is not MISSING:
            rc["dispositions"] = {"attachedPic": v}
        s["streams"].append(rc)
        add(f"X/b3/probe-second-film/record-attachedPic={name}", src=s, prb=p)
    # the probe's cover first (index 0), the film second; the record lists the film alone, as index 0 / as index 1
    for ri in (0, 1):
        p, s = probe(), source()
        p["streams"][0]["index"] = 1
        p["streams"].insert(0, cover_probe(index=0))
        s["streams"][0]["index"] = ri
        add(f"X/b3/cover-first/record-film-index={ri}", src=s, prb=p)
    # the cover's codec, as the record says it and as the probe says it
    for rcodec in ("mjpeg", "png", "h264", "hevc", MISSING, None, 5):
        for mark in (True, MISSING):
            p, s = probe(), source()
            p["streams"].append(cover_probe())
            rc = {"index": 1, "type": "video", "codec": rcodec}
            if rcodec is MISSING:
                del rc["codec"]
            if mark is not MISSING:
                rc["dispositions"] = {"attachedPic": True}
            s["streams"].append(rc)
            add(f"X/b3/probe-cover/record-codec={rcodec if rcodec is not MISSING else 'missing'},marked={mark is not MISSING}",
                src=s, prb=p)

    # ---- b2: the record's offList, and what Go computes ----
    words = {"[]": [], "[codec:vc1]": ["codec:vc1"], "[container:flv]": ["container:flv"],
             "[stream:data:bin]": ["stream:data:bin"], "[video-streams:2]": ["video-streams:2"],
             "[picture:vbi-lines]": ["picture:vbi-lines"], "[x]": ["x"], "text": "x", "null": None, "missing": MISSING,
             "[1]": [1], "['']": [""], "object": {}, "true": True, "twice-empty": Twice([], [])}
    setups = {
        "plain": lambda p, s: None,
        "probe-vc1": lambda p, s: p["streams"][0].__setitem__("codec_name", "vc1"),
        "record-vc1": lambda p, s: s["streams"][0].__setitem__("codec", "vc1"),
        "both-vc1": lambda p, s: (p["streams"][0].__setitem__("codec_name", "vc1"), s["streams"][0].__setitem__("codec", "vc1")),
        "probe-flv": lambda p, s: p["format"].__setitem__("format_name", "flv"),
        "record-flv": lambda p, s: s["container"].__setitem__("format", "flv"),
        "both-486": lambda p, s: (p["streams"][0].__setitem__("height", 486), s["streams"][0].__setitem__("height", 486)),
        "record-486": lambda p, s: s["streams"][0].__setitem__("height", 486),
        "probe-486": lambda p, s: p["streams"][0].__setitem__("height", 486),
        "both-data-bin": lambda p, s: (p["streams"].append({"index": 1, "codec_type": "data", "codec_name": "bin_data"}),
                                       s["streams"].append({"index": 1, "type": "data", "codec": "bin_data"})),
        "both-tmcd": lambda p, s: (p["streams"].append({"index": 1, "codec_type": "data", "codec_tag_string": "tmcd"}),
                                   s["streams"].append({"index": 1, "type": "data"})),
        "both-tmcd-codec-none": lambda p, s: (p["streams"].append({"index": 1, "codec_type": "data", "codec_name": "none",
                                                                    "codec_tag_string": "tmcd"}),
                                              s["streams"].append({"index": 1, "type": "data", "codec": "none"})),
        "record-stream-not-object": lambda p, s: s["streams"].append("x"),
        "record-stream-null": lambda p, s: s["streams"].append(None),
        "record-streams-null": lambda p, s: s.__setitem__("streams", None),
        "record-streams-missing": lambda p, s: s.__delitem__("streams"),
        "record-streams-empty": lambda p, s: s.__setitem__("streams", []),
        "record-streams-object": lambda p, s: s.__setitem__("streams", {}),
        "record-container-null": lambda p, s: s.__setitem__("container", None),
        "record-container-text": lambda p, s: s.__setitem__("container", "matroska,webm"),
    }
    for sname, fn in setups.items():
        for wname, w in words.items():
            p, s = probe(), source()
            fn(p, s)
            if w is not MISSING:
                s["offList"] = w
            add(f"X/b2/{sname}/offList={wname}", src=s, prb=p)



def generate(path):
    add("base")
    gen_plan()
    gen_read()
    gen_whole()
    gen_cover()
    gen_lists()
    gen_record()
    gen_scan()
    gen_loader()
    gen_extra()
    ids = collections.Counter(c["id"] for c in CASES)
    dup = [k for k, v in ids.items() if v > 1]
    assert not dup, dup[:5]
    with open(path, "w", encoding="utf-8") as f:
        for c in CASES:
            f.write(json.dumps(c, ensure_ascii=False) + "\n")
    print(len(CASES), "cases", collections.Counter(c["id"].split("/")[0] for c in CASES))


def compare(cases_path, out_path, summary_path):
    """Each case's verdict of the record logic (truth) against the gate's (GOES, KEPT or ERR) and the port's (HOLD
    with the keys it holds, or GO): must lists the cases the gate lets go and the record logic holds (none may be),
    port_must those where the port lets go a key the record logic holds - closedCaptions and interlaced apart, a
    raise holding both - (none may be), lesser those the gate holds and the record logic lets go, by their family,
    and port_lesser and port_held those where the port holds a key the record logic doesn't."""
    go = {}
    for line in open(out_path, encoding="utf-8"):
        cid, verdict, note, why, port = (line.rstrip("\n").split("\t") + ["", "", "", ""])[:5]
        go[cid] = (verdict, note, why, port)
    must, port_must, lesser, port_lesser, port_held = [], [], collections.Counter(), collections.Counter(), []
    agree, n = collections.Counter(), 0
    for line in open(cases_path, encoding="utf-8"):
        c = json.loads(line)
        n += 1
        verdict, note, why, port = go[c["id"]]
        t, never, twhy = truth(c)
        family = "/".join(c["id"].split("/")[:2])
        if verdict == "GOES" and t == "HOLD":
            must.append([c["id"], ",".join(never), twhy[:200], note[:80]])
        elif verdict != "GOES" and t == "GO":
            lesser[family] += 1
        else:
            agree[f"{verdict}/{t}"] += 1
        keys = set(never) if never != ["raises"] else set(NEVER)
        ported = set(filter(None, port.partition(" ")[2].split(","))) if port.startswith("HOLD") else set()
        if keys - ported:
            port_must.append([c["id"], ",".join(sorted(keys - ported)), twhy[:200]])
        if ported - keys:
            port_lesser[family] += 1
            port_held.append(c["id"])
    summary = {"cases": n, "agree": dict(agree), "must": must, "lesser": dict(sorted(lesser.items())),
               "port_must": port_must, "port_lesser": dict(sorted(port_lesser.items())), "port_held": port_held}
    with open(summary_path, "w", encoding="utf-8") as f:
        json.dump(summary, f, indent=1)
    print(f"{n} cases; the gate lets go and the record logic holds: {len(must)}; the gate holds and the record logic "
          f"lets go: {sum(lesser.values())}; the port lets go and the record logic holds: {len(port_must)}; the port "
          f"holds and the record logic lets go: {sum(port_lesser.values())}")


if __name__ == "__main__":
    if sys.argv[1] == "gen":
        generate(sys.argv[2])
    else:
        compare(sys.argv[2], sys.argv[3], sys.argv[4])
