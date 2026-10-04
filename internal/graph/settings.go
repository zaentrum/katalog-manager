package graph

import (
	"context"
	"fmt"
	"strings"

	graphql "github.com/graph-gophers/graphql-go"
	"github.com/zaentrum/katalog-manager/internal/model"
)

// isSecretSetting reports whether the setting key holds a credential
// (model.IsSecretSetting): its value is write-only.
func isSecretSetting(key string) bool { return model.IsSecretSetting(key) }

// errSecretSetting refuses a write of a secret setting by the general
// setting mutations.
func errSecretSetting(key string) error {
	return fmt.Errorf("%s is a secret setting: set it with setSecretSetting, clear it with clearSecretSetting", key)
}

// ---- Setting ----

type settingResolver struct{ m *model.Setting }

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

// SetSecretSetting sets a secret setting, creating it when there is none.
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
	s, err := r.store.SetSettingByKey(ctx, key, value)
	if err != nil {
		return nil, err
	}
	return &settingResolver{m: s}, nil
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
