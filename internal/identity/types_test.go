package identity

import "testing"

func TestScopeRefConstructors(t *testing.T) {
	tests := []struct {
		name string
		ref  ScopeRef
		want Scope
	}{
		{"user", UserScope("u1"), ScopeUser},
		{"team", TeamScope("t1"), ScopeTeam},
		{"system", SystemScope(), ScopeSystem},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.ref.Scope != tt.want {
				t.Fatalf("got scope %q want %q", tt.ref.Scope, tt.want)
			}
		})
	}
}

func TestUserScopeSetsUserID(t *testing.T) {
	r := UserScope("u1")
	if r.UserID != "u1" || r.TeamID != "" {
		t.Fatalf("unexpected ref: %+v", r)
	}
}

func TestTeamScopeSetsTeamID(t *testing.T) {
	r := TeamScope("t1")
	if r.TeamID != "t1" || r.UserID != "" {
		t.Fatalf("unexpected ref: %+v", r)
	}
}

func TestSystemScopeHasNoOwner(t *testing.T) {
	r := SystemScope()
	if r.UserID != "" || r.TeamID != "" {
		t.Fatalf("system scope should have no owner: %+v", r)
	}
}
