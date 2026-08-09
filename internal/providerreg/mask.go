package providerreg

// MaskKey renders a credential as a display fragment: the last four characters
// behind a fixed-width ellipsis. The width is fixed rather than proportional to
// the key, so the rendering leaks nothing about the secret's length. A key too
// short to have a distinguishable tail masks entirely. Moved from the deleted
// internal/routing package; the provider registry owns credentials now.
func MaskKey(raw string) string {
	const keep = 4
	if len(raw) <= keep {
		return "••••"
	}
	return "••••" + raw[len(raw)-keep:]
}
