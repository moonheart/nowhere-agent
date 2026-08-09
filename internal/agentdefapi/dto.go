package agentdefapi

import (
	"time"

	"nowhere-agent/internal/agentdef"
)

// defDTO is an agent definition as the console renders it: identity + scope +
// parsed frontmatter fields + the raw source document for the editor.
type defDTO struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Scope           string    `json:"scope"`
	UserID          string    `json:"user_id,omitempty"`
	TeamID          string    `json:"team_id,omitempty"`
	CurrentVersion  int       `json:"current_version"`
	WhenToUse       string    `json:"description"`
	Tools           []string  `json:"tools"`
	DisallowedTools []string  `json:"disallowedTools"`
	Skills          []string  `json:"skills"`
	Model           string    `json:"model,omitempty"`
	MaxTurns        int       `json:"maxTurns,omitempty"`
	Document        string    `json:"document"`
	CreatedBy       string    `json:"created_by,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

func defDTOOf(sd agentdef.StoredDef) defDTO {
	return defDTO{
		ID:              sd.ID,
		Name:            sd.Name,
		Scope:           string(sd.Scope.Scope),
		UserID:          sd.Scope.UserID,
		TeamID:          sd.Scope.TeamID,
		CurrentVersion:  sd.Version,
		WhenToUse:       sd.WhenToUse,
		Tools:           listOrEmpty(sd.Tools),
		DisallowedTools: listOrEmpty(sd.DisallowedTools),
		Skills:          listOrEmpty(sd.Skills),
		Model:           sd.Model,
		MaxTurns:        sd.MaxTurns,
		Document:        sd.RawDocument,
		CreatedBy:       sd.CreatedBy,
		CreatedAt:       sd.CreatedAt,
		UpdatedAt:       sd.UpdatedAt,
	}
}

func defDTOs(defs []agentdef.StoredDef) []defDTO {
	out := make([]defDTO, 0, len(defs))
	for _, sd := range defs {
		out = append(out, defDTOOf(sd))
	}
	return out
}

// defRequest is the create/update payload: the whole markdown document. The
// scope comes from the route, not the body, so a caller cannot write into a
// scope it does not own by lying in the payload.
type defRequest struct {
	Document string `json:"document"`
}

// listOrEmpty normalizes a nil list so JSON encodes [], not null.
func listOrEmpty(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}
