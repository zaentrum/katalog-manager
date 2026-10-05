package graph

import (
	"context"
	"fmt"

	graphql "github.com/graph-gophers/graphql-go"
)

// SourceReplacer gives a title another file, its source, and keeps the title
// (implemented by itemactions).
type SourceReplacer interface {
	ReplaceSource(ctx context.Context, in ReplaceSourceRequest) (ReplaceSourceResult, error)
}

// ReplaceSourceRequest gives the title another file: the title named one
// way, by its id or by the path of the file replaced; Path the new file.
// DeleteOldFile deletes the old file once the title has the new one, and
// Reencode encodes the title again from it.
type ReplaceSourceRequest struct {
	ItemID        string
	ItemPath      string
	Path          string
	DeleteOldFile bool
	Reencode      bool
}

// ReplaceSourceResult says what a replaceSource call did.
type ReplaceSourceResult struct {
	ItemID         string
	OldPath        string // the file the title had
	Path           string // the file it has now
	Replaced       bool   // false: Path was its file already, and nothing changed
	OldFileDeleted bool
	OldSidecars    int32           // the subtitle files beside the old file named after it, which the title keeps
	Reencode       *ReencodeResult // what encoding it again did; nil when not asked, or nothing changed
	Message        string
}

// SourceRefused refuses a replaceSource, saying why: Code is its GraphQL code
// (NOT_FOUND a title there is not, SOURCE_CONFLICT a file that is another
// title's or an extra's already, SOURCE_REFUSED anything else), and ItemID
// and ExtraID name the title or the extra in the way.
type SourceRefused struct {
	Code    string
	Message string
	ItemID  string
	ExtraID string
}

func (e *SourceRefused) Error() string { return e.Message }

// Extensions are the GraphQL error's extensions: its code, and the title and
// the extra in the way when there is one.
func (e *SourceRefused) Extensions() map[string]any {
	ext := map[string]any{"code": e.Code}
	if e.ItemID != "" {
		ext["itemId"] = e.ItemID
	}
	if e.ExtraID != "" {
		ext["extraId"] = e.ExtraID
	}
	return ext
}

// RefuseSource is a refusal with code, its message formatted.
func RefuseSource(code, format string, args ...any) *SourceRefused {
	return &SourceRefused{Code: code, Message: fmt.Sprintf(format, args...)}
}

// ReplaceSource gives a title another file and keeps the title (see the
// SDL). Nothing leaves the disk unless asked: deleteOldFile omitted is false;
// reencode omitted is true.
func (r *Resolver) ReplaceSource(ctx context.Context, args struct {
	ItemID        *graphql.ID
	ItemPath      *string
	Path          string
	DeleteOldFile *bool
	Reencode      *bool
}) (*replaceSourceResultResolver, error) {
	if err := r.allow(ctx, "Mutation.replaceSource"); err != nil {
		return nil, err
	}
	if r.svc.Sources == nil {
		return nil, errNotConfigured
	}
	req := ReplaceSourceRequest{ItemPath: strDeref(args.ItemPath), Path: args.Path,
		DeleteOldFile: derefBool(args.DeleteOldFile), Reencode: args.Reencode == nil || *args.Reencode}
	if args.ItemID != nil {
		req.ItemID = string(*args.ItemID)
	}
	res, err := r.svc.Sources.ReplaceSource(ctx, req)
	if err != nil {
		return nil, err
	}
	return &replaceSourceResultResolver{m: res}, nil
}

type replaceSourceResultResolver struct{ m ReplaceSourceResult }

func (r *replaceSourceResultResolver) ItemID() graphql.ID   { return gid(r.m.ItemID) }
func (r *replaceSourceResultResolver) OldPath() string      { return r.m.OldPath }
func (r *replaceSourceResultResolver) Path() string         { return r.m.Path }
func (r *replaceSourceResultResolver) Replaced() bool       { return r.m.Replaced }
func (r *replaceSourceResultResolver) OldFileDeleted() bool { return r.m.OldFileDeleted }
func (r *replaceSourceResultResolver) OldSidecars() int32   { return r.m.OldSidecars }
func (r *replaceSourceResultResolver) Message() string      { return r.m.Message }
func (r *replaceSourceResultResolver) Reencode() *reencodeResultResolver {
	if r.m.Reencode == nil {
		return nil
	}
	return &reencodeResultResolver{m: *r.m.Reencode}
}
