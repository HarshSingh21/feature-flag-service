package core

// EvalContext is the caller-supplied description of who or what is being evaluated.
//
// It is passed by value and must not be retained or mutated by the engine.
type EvalContext struct {
	// UserID is the default bucketing subject and the usual targeting key.
	UserID string

	// TenantID identifies the owning organisation. Useful as a bucketing subject
	// when a whole company must flip together rather than individual users.
	TenantID string

	// Attributes are arbitrary request attributes such as country, plan, or
	// app_version. A nil map is valid and means "no attributes"; it must not panic.
	Attributes map[string]Value
}

// Attribute resolves a named attribute.
//
// UserID and TenantID are addressable as attributes so a targeting rule can match
// on them without a special case in the matcher. An explicit entry in Attributes
// wins, so a caller can override the shorthand.
//
// The second return distinguishes ABSENT from PRESENT-BUT-ZERO. That distinction
// is load bearing: an absent attribute makes a condition false BEFORE negation is
// applied, which is what stops `country != "IN"` from silently matching every user
// on the planet when an upstream geo lookup fails.
func (c EvalContext) Attribute(name string) (Value, bool) {
	if v, ok := c.Attributes[name]; ok {
		return v, true
	}
	switch name {
	case "user_id":
		if c.UserID == "" {
			return Value{}, false
		}
		return String(c.UserID), true
	case "tenant_id":
		if c.TenantID == "" {
			return Value{}, false
		}
		return String(c.TenantID), true
	}
	return Value{}, false
}
