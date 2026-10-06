package graph

import (
	"context"
	"fmt"
	"strings"

	graphql "github.com/graph-gophers/graphql-go"
	"github.com/zaentrum/katalog-manager/internal/model"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// isSecretSetting reports whether the setting key holds a credential
// (model.IsSecretSetting): its value is write-only.
func isSecretSetting(key string) bool { return model.IsSecretSetting(key) }

// errSecretSetting refuses a write of a secret setting by the general
// setting mutations.
func errSecretSetting(key string) error {
	return fmt.Errorf("%s is a secret setting: set it with setSecretSetting, clear it with clearSecretSetting", key)
}

// SecretChecker checks the value of a secret setting with the service it is
// for, before setSecretSetting stores it (implemented by tmdb, for the TMDB
// key). It answers the zero SecretCheck for a key it does not check. It never
// writes the value anywhere, and no message it gives holds it.
type SecretChecker interface {
	CheckSecret(ctx context.Context, key, value string) SecretCheck
}

// What a check of a secret's value found (SecretCheck.Status).
const (
	// SecretValid: the service took the value.
	SecretValid = "valid"
	// SecretUnchecked: the service could not be asked (no answer in time, or
	// an answer that is neither yes nor no); the value is stored all the same.
	SecretUnchecked = "unchecked"
	// SecretRefused: the service refused the value, a definite no; it is not
	// stored.
	SecretRefused = "refused"
)

// SecretCheck is what a check of a secret's value found: Status (empty when
// nothing checks the key) and what to tell the admin.
type SecretCheck struct {
	Status  string
	Message string
}

// secretRefused refuses a secret the service it is for refused: nothing is
// stored. As a GraphQL error it carries the code SECRET_REFUSED and the key.
type secretRefused struct{ key, message string }

func (e *secretRefused) Error() string { return e.key + ": " + e.message + "; nothing was saved" }

// Extensions are the GraphQL error's extensions.
func (e *secretRefused) Extensions() map[string]any {
	return map[string]any{"code": "SECRET_REFUSED", "key": e.key}
}

// layoutBusy refuses a change of the library's layout while titles or extras
// are between their transcode and their package. As a GraphQL error it
// carries the code LAYOUT_BUSY and how many of each.
type layoutBusy struct {
	from, to       string
	titles, extras int
}

func (e *layoutBusy) Error() string {
	return fmt.Sprintf("%s stays %s: %d titles and %d extras are between their transcode and their package, the "+
		"transcode's handoff in the %s layout's inbox, which the packager does not read once the layout is %s; "+
		"let the packager finish them (with the transcoder paused, nothing new starts) and set it again",
		store.LayoutSetting, e.from, e.titles, e.extras, e.from, e.to)
}

// Extensions are the GraphQL error's extensions.
func (e *layoutBusy) Extensions() map[string]any {
	return map[string]any{"code": "LAYOUT_BUSY", "titles": e.titles, "extras": e.extras}
}

// checkLayout refuses a write of the setting key that changes the library's
// layout while anything is between its transcode and its package
// (store.LayoutWaits). next is the rows of library.layout as the write leaves
// them, from those there are (by id); the layout is the first one's. A
// setting created beside one there is counts as the layout it says.
func (r *Resolver) checkLayout(ctx context.Context, key string, next func(rows []*model.Setting) string) error {
	if strings.TrimSpace(key) != store.LayoutSetting {
		return nil
	}
	rows, err := r.store.LayoutRows(ctx)
	if err != nil {
		return err
	}
	from := store.LayoutOf("")
	if len(rows) > 0 {
		from = store.LayoutOf(rows[0].ValueText)
	}
	to := store.LayoutOf(next(rows))
	if to == from {
		return nil
	}
	titles, extras, err := r.store.LayoutWaits(ctx)
	if err != nil || titles+extras == 0 {
		return err
	}
	return &layoutBusy{from: from, to: to, titles: titles, extras: extras}
}

// layoutWith is the layout's value once the row id holds value (nil: once it
// is deleted).
func layoutWith(id string, value *string) func([]*model.Setting) string {
	return func(rows []*model.Setting) string {
		for _, row := range rows {
			if row.ID != id {
				return row.ValueText
			}
			if value != nil {
				return *value
			}
		}
		return ""
	}
}

// ---- Setting ----

// settingResolver is a setting; check is what setSecretSetting found when it
// checked the value it set, nil anywhere else.
type settingResolver struct {
	m     *model.Setting
	check *SecretCheck
}

func (r *settingResolver) ID() graphql.ID       { return gid(r.m.ID) }
func (r *settingResolver) Key() string          { return r.m.Key }
func (r *settingResolver) ValueType() string    { return r.m.ValueType }
func (r *settingResolver) Description() *string { return r.m.Description }
func (r *settingResolver) IsSecret() bool       { return isSecretSetting(r.m.Key) }
func (r *settingResolver) IsSet() bool          { return strings.TrimSpace(r.m.ValueText) != "" }

// ValueText is the value, and null for a secret.
func (r *settingResolver) ValueText() *string {
	if r.IsSecret() {
		return nil
	}
	v := r.m.ValueText
	return &v
}

// UpdatedAt is when the setting was last written.
func (r *settingResolver) UpdatedAt() *graphql.Time {
	if r.m.ModifiedAt != nil {
		return gtime(r.m.ModifiedAt)
	}
	return gtime(r.m.CreatedAt)
}

// Check is what setSecretSetting found when it checked the value it set.
func (r *settingResolver) Check() *secretCheckResolver {
	if r.check == nil {
		return nil
	}
	return &secretCheckResolver{m: *r.check}
}

type secretCheckResolver struct{ m SecretCheck }

func (r *secretCheckResolver) Status() string  { return r.m.Status }
func (r *secretCheckResolver) Message() string { return r.m.Message }

// SetSecretSetting sets a secret setting, creating it when there is none. A
// value the service it is for refuses is not stored (SecretChecker).
func (r *Resolver) SetSecretSetting(ctx context.Context, args struct {
	Key   string
	Value string
}) (*settingResolver, error) {
	if err := r.allow(ctx, "Mutation.setSecretSetting"); err != nil {
		return nil, err
	}
	key, value := strings.TrimSpace(args.Key), strings.TrimSpace(args.Value)
	if !isSecretSetting(key) {
		return nil, fmt.Errorf("%q is not a secret setting: set it with createSetting or updateSetting", key)
	}
	if value == "" {
		return nil, fmt.Errorf("%s: a secret setting is cleared with clearSecretSetting, not set blank", key)
	}
	var check *SecretCheck
	if r.svc.Secrets != nil {
		if c := r.svc.Secrets.CheckSecret(ctx, key, value); c.Status != "" {
			check = &c
		}
	}
	if check != nil && check.Status == SecretRefused {
		return nil, &secretRefused{key: key, message: check.Message}
	}
	s, err := r.store.SetSettingByKey(ctx, key, value)
	if err != nil {
		return nil, err
	}
	return &settingResolver{m: s, check: check}, nil
}

// ClearSecretSetting deletes a secret setting, so the service falls back to
// its environment default.
func (r *Resolver) ClearSecretSetting(ctx context.Context, args struct{ Key string }) (bool, error) {
	if err := r.allow(ctx, "Mutation.clearSecretSetting"); err != nil {
		return false, err
	}
	key := strings.TrimSpace(args.Key)
	if !isSecretSetting(key) {
		return false, fmt.Errorf("%q is not a secret setting: delete it with deleteSetting", key)
	}
	return r.store.DeleteSettingsByKey(ctx, key)
}
