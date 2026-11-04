package graph

import (
	"context"
	"fmt"
	"log"

	graphql "github.com/graph-gophers/graphql-go"

	"github.com/zaentrum/katalog-manager/internal/auth"
)

// SourceReplacer gives a title another file, its source, and keeps the title
// (implemented by itemactions).
type SourceReplacer interface {
	ReplaceSource(ctx context.Context, in ReplaceSourceRequest) (ReplaceSourceResult, error)
}

// ReplaceSourceRequest gives the title another file: the title named one
// way, by its id or by the path of the file replaced; Path the new file.
// DeleteOldFile deletes the old file once the title has the new one, and
// Reencode encodes the title again from it. IfIdle refuses the replace
// (SOURCE_BUSY) while the title's transcode or package runs or waits for its
// worker, which reads the old file.
type ReplaceSourceRequest struct {
	ItemID        string
	ItemPath      string
	Path          string
	DeleteOldFile bool
	Reencode      bool
	IfIdle        bool
	// Key tells the replace from another (an addon's id of what it hands
	// over): one asked with a key a replace of the title was asked with
	// already changes nothing, and answers what that one did.
	Key string
}

// ReplaceSourceResult says what a replaceSource call did.
type ReplaceSourceResult struct {
	ItemID         string
	OldPath        string // the file the title had
	Path           string // the file it has now
	Replaced       bool   // false: Path was its file already, and nothing changed
	OldFileDeleted bool
	// OldFileSuperseded says, with the library's v2 layout, that the old
	// original asked to go is superseded: kept where it lies until the retire
	// job retires it.
	OldFileSuperseded bool
	// Repeated says the replace was asked with a key a replace of the title
	// was asked with already: nothing changed, and the answer is what that
	// one did.
	Repeated    bool
	OldSidecars int32           // the subtitle files beside the old file named after it, which the title keeps
	Reencode    *ReencodeResult // what encoding it again did; nil when not asked, or nothing changed
	Message     string
}

// SourceRefused refuses a replaceSource, saying why: Code is its GraphQL code
// (NOT_FOUND a title there is not, SOURCE_CONFLICT a file that is another
// title's or an extra's already, SOURCE_BUSY a title whose encode runs, asked
// with ifIdle, SOURCE_REFUSED anything else), and ItemID and ExtraID name the
// title or the extra in the way.
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
// reencode omitted is true; ifIdle omitted is false.
func (r *Resolver) ReplaceSource(ctx context.Context, args struct {
	ItemID        *graphql.ID
	ItemPath      *string
	Path          string
	DeleteOldFile *bool
	Reencode      *bool
	IfIdle        *bool
	Key           *string
}) (*replaceSourceResultResolver, error) {
	if err := r.allow(ctx, "Mutation.replaceSource"); err != nil {
		r.refusedReplace(ctx, args.ItemID, args.ItemPath, args.Path, err)
		return nil, err
	}
	// The platform's service account may give a title another file, never
	// delete one: a seed Job deletes the old file itself, once the title has
	// the new one. An addon may, carrying out what an admin asked it: the old
	// file of a title it grabbed a better one for goes once the title has it.
	if derefBool(args.DeleteOldFile) {
		if err := r.access.Check(ctx, auth.Addon, "replaceSource with deleteOldFile"); err != nil {
			r.refusedReplace(ctx, args.ItemID, args.ItemPath, args.Path, err)
			return nil, err
		}
	}
	if r.svc.Sources == nil {
		return nil, errNotConfigured
	}
	req := ReplaceSourceRequest{ItemPath: strDeref(args.ItemPath), Path: args.Path,
		DeleteOldFile: derefBool(args.DeleteOldFile), Reencode: args.Reencode == nil || *args.Reencode,
		IfIdle: derefBool(args.IfIdle), Key: strDeref(args.Key)}
	if args.ItemID != nil {
		req.ItemID = string(*args.ItemID)
	}
	res, err := r.svc.Sources.ReplaceSource(ctx, req)
	if err != nil {
		return nil, err
	}
	return &replaceSourceResultResolver{m: res}, nil
}

// refusedReplace logs a replaceSource refused before it was asked of the
// catalog's sources (whoever may not ask it), as every replace is logged:
// who asked it, the title, the new file, and why.
func (r *Resolver) refusedReplace(ctx context.Context, itemID *graphql.ID, itemPath *string, path string, err error) {
	p, _ := auth.PrincipalFrom(ctx)
	item := strDeref(itemPath)
	if itemID != nil {
		item = string(*itemID)
	}
	if item == "" {
		item = "-"
	}
	log.Printf("replaceSource asked by %s: item %s, new file %s: refused (FORBIDDEN): %v", r.access.Describe(p), item, path, err)
}

type replaceSourceResultResolver struct{ m ReplaceSourceResult }

func (r *replaceSourceResultResolver) ItemID() graphql.ID      { return gid(r.m.ItemID) }
func (r *replaceSourceResultResolver) OldPath() string         { return r.m.OldPath }
func (r *replaceSourceResultResolver) Path() string            { return r.m.Path }
func (r *replaceSourceResultResolver) Replaced() bool          { return r.m.Replaced }
func (r *replaceSourceResultResolver) OldFileDeleted() bool    { return r.m.OldFileDeleted }
func (r *replaceSourceResultResolver) OldFileSuperseded() bool { return r.m.OldFileSuperseded }
func (r *replaceSourceResultResolver) Repeated() bool          { return r.m.Repeated }
func (r *replaceSourceResultResolver) OldSidecars() int32      { return r.m.OldSidecars }
func (r *replaceSourceResultResolver) Message() string         { return r.m.Message }
func (r *replaceSourceResultResolver) Reencode() *reencodeResultResolver {
	if r.m.Reencode == nil {
		return nil
	}
	return &reencodeResultResolver{m: *r.m.Reencode}
}
