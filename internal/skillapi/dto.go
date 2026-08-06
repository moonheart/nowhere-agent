package skillapi

import (
	"net/http"
	"strings"
	"time"

	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/skill"
)

// skillDTO is a skill as the editor renders it: identity + scope + the current
// version's content + review state.
type skillDTO struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Scope            string            `json:"scope"`
	UserID           string            `json:"user_id,omitempty"`
	TeamID           string            `json:"team_id,omitempty"`
	CurrentVersion   int               `json:"current_version"`
	OverridesVersion int               `json:"overrides_version"`
	Description      string            `json:"description"`
	Body             string            `json:"body"`
	Resources        map[string]string `json:"resources"`
	Scripts          map[string]string `json:"scripts"`
	NeedsReview      bool              `json:"needs_review"`
	CreatedAt        time.Time         `json:"created_at"`
	UpdatedAt        time.Time         `json:"updated_at"`
}

func skillDTOOf(sk skill.Skill) skillDTO {
	return skillDTO{
		ID:               sk.ID,
		Name:             sk.Name,
		Scope:            string(sk.Scope.Scope),
		UserID:           sk.Scope.UserID,
		TeamID:           sk.Scope.TeamID,
		CurrentVersion:   sk.Version,
		OverridesVersion: sk.OverridesVersion,
		Description:      sk.Description,
		Body:             sk.Body,
		Resources:        orEmpty(sk.Resources),
		Scripts:          orEmpty(sk.Scripts),
		NeedsReview:      sk.NeedsReview,
		CreatedAt:        sk.CreatedAt,
		UpdatedAt:        sk.UpdatedAt,
	}
}

func skillDTOs(sks []skill.Skill) []skillDTO {
	out := make([]skillDTO, 0, len(sks))
	for _, sk := range sks {
		out = append(out, skillDTOOf(sk))
	}
	return out
}

// versionDTO is one revision in a skill's history (no content).
type versionDTO struct {
	Version   int       `json:"version"`
	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func versionDTOs(vs []skill.Version) []versionDTO {
	out := make([]versionDTO, 0, len(vs))
	for _, v := range vs {
		out = append(out, versionDTO{Version: v.Version, CreatedBy: v.CreatedBy, CreatedAt: v.CreatedAt})
	}
	return out
}

// skillRequest is the create/update payload. The editor sends the whole skill;
// the scope comes from the route, not the body, so a caller cannot write into a
// scope it does not own by lying in the payload.
type skillRequest struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Body        string            `json:"body"`
	Resources   map[string]string `json:"resources"`
	Scripts     map[string]string `json:"scripts"`
	// OverridesVersion records which upstream version an override is based on
	// (set when a higher-scope skill overrides a lower-scope one).
	OverridesVersion int `json:"overrides_version"`
}

// toSkill converts the request into a Skill for the given scope, validating the
// fields that must hold regardless of scope. It answers the response and
// reports false on invalid input.
func (req *skillRequest) toSkill(w http.ResponseWriter, scope identity.ScopeRef) (skill.Skill, bool) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "skill name required")
		return skill.Skill{}, false
	}
	if err := validateFiles(req.Resources); err != nil {
		writeError(w, http.StatusBadRequest, "invalid resource path: "+err.Error())
		return skill.Skill{}, false
	}
	if err := validateFiles(req.Scripts); err != nil {
		writeError(w, http.StatusBadRequest, "invalid script path: "+err.Error())
		return skill.Skill{}, false
	}
	return skill.Skill{
		Name:             name,
		Scope:            scope,
		Description:      strings.TrimSpace(req.Description),
		Body:             req.Body,
		Resources:        req.Resources,
		Scripts:          req.Scripts,
		OverridesVersion: req.OverridesVersion,
	}, true
}

// validateFiles rejects file paths that could escape the sandbox when a script
// is staged: absolute paths and parent traversal are refused at write time.
func validateFiles(files map[string]string) error {
	for p := range files {
		if p == "" {
			return errPath("empty path")
		}
		if strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) {
			return errPath(p + " is absolute")
		}
		// Reject any ".." segment (after normalizing separators).
		norm := strings.ReplaceAll(p, `\`, "/")
		for _, seg := range strings.Split(norm, "/") {
			if seg == ".." {
				return errPath(p + " traverses parent")
			}
		}
	}
	return nil
}

type pathError string

func (e pathError) Error() string { return string(e) }
func errPath(s string) error      { return pathError(s) }

// orEmpty normalizes a nil map to an empty one so JSON encodes {}, not null.
func orEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
