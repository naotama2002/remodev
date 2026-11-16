package session

import (
	"strings"

	"github.com/naotama2002/remodev/internal/store"
)

// Visibility of a session.
//
//	team    (default) every logged-in member can view it, send messages,
//	        interrupt and stop it. Only the creator can delete it or change
//	        its visibility.
//	private only the creator can see or touch it.
const (
	VisibilityTeam    = "team"
	VisibilityPrivate = "private"
)

// Access is what a user may do with a session.
type Access struct {
	View             bool `json:"view"`
	Write            bool `json:"write"` // send messages, interrupt, stop
	Delete           bool `json:"delete"`
	ChangeVisibility bool `json:"changeVisibility"`
}

func isOwner(s store.Session, login string) bool {
	return login != "" && strings.EqualFold(s.CreatedBy, login)
}

// AccessFor returns the permissions of login on s.
func AccessFor(s store.Session, login string) Access {
	owner := isOwner(s, login)
	view := owner || (login != "" && s.Visibility != VisibilityPrivate)
	return Access{View: view, Write: view, Delete: owner, ChangeVisibility: owner}
}

func validVisibility(v string) bool {
	return v == VisibilityTeam || v == VisibilityPrivate
}
