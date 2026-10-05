package graph

import (
	"context"
	"encoding/json"
	"testing"
)

// fakeSources records what replaceSource asks of it, and answers res or err.
type fakeSources struct {
	got []ReplaceSourceRequest
	res ReplaceSourceResult
	err error
}

func (f *fakeSources) ReplaceSource(_ context.Context, in ReplaceSourceRequest) (ReplaceSourceResult, error) {
	f.got = append(f.got, in)
	return f.res, f.err
}

// The title travels as it was named, by its id or by its file, with the new
// file. Nothing leaves the disk unless asked: deleteOldFile omitted or null
// is false; the title is encoded again unless asked not to: reencode omitted
// or null is true. So too with variables, as a Job sends them.
func TestReplaceSourcePassesItsArguments(t *testing.T) {
	const path = "/media/Big Buck Bunny (2008).mov"
	for _, tc := range []struct {
		args string
		want ReplaceSourceRequest
	}{
		{`itemId: "m1", path: "` + path + `"`, ReplaceSourceRequest{ItemID: "m1", Path: path, Reencode: true}},
		{`itemPath: "/media/BigBuckBunny_320x180.mp4", path: "` + path + `", deleteOldFile: true`,
			ReplaceSourceRequest{ItemPath: "/media/BigBuckBunny_320x180.mp4", Path: path, DeleteOldFile: true, Reencode: true}},
		{`itemId: "m1", path: "` + path + `", reencode: false`, ReplaceSourceRequest{ItemID: "m1", Path: path}},
		{`itemId: "m1", path: "` + path + `", deleteOldFile: null, reencode: null`, ReplaceSourceRequest{ItemID: "m1", Path: path, Reencode: true}},
		{`itemId: "m1", path: "` + path + `", deleteOldFile: false, reencode: true`, ReplaceSourceRequest{ItemID: "m1", Path: path, Reencode: true}},
	} {
		f := &fakeSources{res: ReplaceSourceResult{ItemID: "m1"}}
		q := `mutation { replaceSource(` + tc.args + `) { itemId } }`
		if resp := MustSchema(NewResolver(nil, testConfig, Services{Sources: f})).Exec(as(admin), q, "", nil); len(resp.Errors) > 0 {
			t.Fatalf("%s: %v", q, resp.Errors)
		}
		if len(f.got) != 1 || f.got[0] != tc.want {
			t.Errorf("%s: asked %+v, want %+v", q, f.got, tc.want)
		}
	}
	f := &fakeSources{res: ReplaceSourceResult{ItemID: "m1"}}
	resp := MustSchema(NewResolver(nil, testConfig, Services{Sources: f})).Exec(as(admin),
		`mutation($old: String, $new: String!, $drop: Boolean) { replaceSource(itemPath: $old, path: $new, deleteOldFile: $drop) { itemId } }`,
		"", map[string]any{"old": "/media/BigBuckBunny_320x180.mp4", "new": path, "drop": true})
	if want := (ReplaceSourceRequest{ItemPath: "/media/BigBuckBunny_320x180.mp4", Path: path, DeleteOldFile: true, Reencode: true}); len(resp.Errors) > 0 ||
		len(f.got) != 1 || f.got[0] != want {
		t.Errorf("with variables: %v, asked %+v, want %+v", resp.Errors, f.got, want)
	}
}

// The answer says what the replace did, with what encoding the title again
// did; reencode is null when none was asked.
func TestReplaceSourceAnswers(t *testing.T) {
	const doc = `mutation { replaceSource(itemId: "m1", path: "/media/new.mkv") { itemId oldPath path replaced oldFileDeleted
		oldSidecars reencode { itemId titles reencoded busy notSent message } message } }`
	f := &fakeSources{res: ReplaceSourceResult{ItemID: "m1", OldPath: "/media/old.mp4", Path: "/media/new.mkv", Replaced: true,
		OldFileDeleted: true, OldSidecars: 2, Reencode: &ReencodeResult{ItemID: "m1", Titles: 1, Reencoded: 1, Message: "encoding it again"},
		Message: "replaced"}}
	schema := MustSchema(NewResolver(nil, testConfig, Services{Sources: f}))
	resp := schema.Exec(as(admin), doc, "", nil)
	if want := `{"replaceSource":{"itemId":"m1","oldPath":"/media/old.mp4","path":"/media/new.mkv","replaced":true,` +
		`"oldFileDeleted":true,"oldSidecars":2,"reencode":{"itemId":"m1","titles":1,"reencoded":1,"busy":0,"notSent":0,` +
		`"message":"encoding it again"},"message":"replaced"}}`; len(resp.Errors) > 0 || string(resp.Data) != want {
		t.Errorf("the answer: %s %v\nwant %s", resp.Data, resp.Errors, want)
	}
	f.res = ReplaceSourceResult{ItemID: "m1", OldPath: "/media/new.mkv", Path: "/media/new.mkv", Message: "nothing changed"}
	resp = schema.Exec(as(admin), doc, "", nil)
	if want := `{"replaceSource":{"itemId":"m1","oldPath":"/media/new.mkv","path":"/media/new.mkv","replaced":false,` +
		`"oldFileDeleted":false,"oldSidecars":0,"reencode":null,"message":"nothing changed"}}`; len(resp.Errors) > 0 || string(resp.Data) != want {
		t.Errorf("a replace that changed nothing: %s %v\nwant %s", resp.Data, resp.Errors, want)
	}
}

// A refused replace is a GraphQL error with its code, and the title or the
// extra in the way when there is one; without the service wired, it is not
// configured.
func TestARefusedReplaceIsAnErrorWithItsCode(t *testing.T) {
	for _, tc := range []struct {
		err  *SourceRefused
		want string
	}{
		{&SourceRefused{Code: "SOURCE_CONFLICT", Message: "/media/new.mkv is a file of item m2 already", ItemID: "m2"},
			`{"message":"/media/new.mkv is a file of item m2 already","path":["replaceSource"],"extensions":{"code":"SOURCE_CONFLICT","itemId":"m2"}}`},
		{&SourceRefused{Code: "SOURCE_CONFLICT", Message: "/media/new.mkv is extra x1 of item m2 already", ItemID: "m2", ExtraID: "x1"},
			`{"message":"/media/new.mkv is extra x1 of item m2 already","path":["replaceSource"],"extensions":{"code":"SOURCE_CONFLICT","extraId":"x1","itemId":"m2"}}`},
		{RefuseSource("NOT_FOUND", "unknown item: %s", "m9"),
			`{"message":"unknown item: m9","path":["replaceSource"],"extensions":{"code":"NOT_FOUND"}}`},
	} {
		f := &fakeSources{err: tc.err}
		resp := MustSchema(NewResolver(nil, testConfig, Services{Sources: f})).Exec(as(admin),
			`mutation { replaceSource(itemId: "m1", path: "/media/new.mkv") { replaced } }`, "", nil)
		if len(resp.Errors) != 1 || string(resp.Data) != "null" {
			t.Fatalf("data %s, errors %v; want no data and one error", resp.Data, resp.Errors)
		}
		if got, _ := json.Marshal(resp.Errors[0]); string(got) != tc.want {
			t.Errorf("the error\n got  %s\n want %s", got, tc.want)
		}
	}
	resp := MustSchema(NewResolver(nil, testConfig, Services{})).Exec(as(admin),
		`mutation { replaceSource(itemId: "m1", path: "/media/new.mkv") { replaced } }`, "", nil)
	if len(resp.Errors) != 1 || resp.Errors[0].Message != "feature not configured" {
		t.Errorf("replaceSource without the service wired: %v", resp.Errors)
	}
}
