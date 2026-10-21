package request

import (
	"errors"
	"strings"
)

// SpecSkeletonHeadings and TicketSkeletonHeadings are the required headings
// of a spec and of a ticket, in order: the units a scoped redraft replaces.
func SpecSkeletonHeadings() []string   { return append([]string(nil), requiredSpecHeadings...) }
func TicketSkeletonHeadings() []string { return append([]string(nil), requiredTicketHeadings...) }

// skeletonFor returns the skeleton the file at path (request-relative) is
// written to: spec.md's, a ticket's, or nil for any other file.
func skeletonFor(path string) []string {
	switch {
	case path == specFileName:
		return requiredSpecHeadings
	case strings.HasPrefix(path, "tickets/") && strings.HasSuffix(path, ".spec.md"):
		return requiredTicketHeadings
	}
	return nil
}

// RedraftScope is the part of a reviewed document set a redraft may change:
// the files and, within each, the skeleton sections the reviewer commented
// on. Everything else is copied from the reviewed files by the driver
// (SpliceSections), never taken from the model.
type RedraftScope struct {
	// Files maps a request-relative path to the headings to redraft in it;
	// a nil slice means the whole file.
	Files map[string][]string
}

// Whole reports whether the whole of path is redrafted.
func (s *RedraftScope) Whole(path string) bool {
	headings, ok := s.Files[path]
	return ok && headings == nil
}

// Sections returns the headings of path to redraft, in skeleton order, and
// whether path is in scope at all.
func (s *RedraftScope) Sections(path string) ([]string, bool) {
	headings, ok := s.Files[path]
	return headings, ok
}

// RedraftScope returns what the redraft that follows the newest review
// rejection of stage (StateSpecReview or StatePlanReview) may change, or nil
// when it is a full redraft. The newest rejection is the newest one a person
// made: the factory's own (a plan it refused and had re-planned) neither
// widens nor narrows the scope of the review it follows, and a send-back,
// whose feedback names no passage, makes a full redraft. A rejection scopes
// the redraft only when it carries anchors, no free note beside them (a note
// on the whole document asks for a whole-document redraft), and no anchor on
// the whole of spec.md or on a heading the skeleton lacks.
func (r *Request) RedraftScope(stage State) *RedraftScope {
	var newest *Rejection
	for i := range r.Rejections {
		rej := &r.Rejections[i]
		if rej.Stage() == stage && rej.By != FactoryActor {
			newest = rej
		}
	}
	if newest == nil || newest.ForStage != "" || len(newest.Anchors) == 0 || newest.Note != "" {
		return nil
	}
	want := map[string]map[string]bool{}
	for _, a := range newest.Anchors {
		skeleton := skeletonFor(a.Path)
		if skeleton == nil || (a.Path == specFileName && a.Section == "") {
			return nil
		}
		if want[a.Path] == nil {
			want[a.Path] = map[string]bool{}
		}
		if a.Section == "" {
			want[a.Path][""] = true
			continue
		}
		expanded := expandSection(skeleton, a.Section)
		if expanded == nil {
			return nil
		}
		for _, h := range expanded {
			want[a.Path][h] = true
		}
	}
	scope := &RedraftScope{Files: map[string][]string{}}
	for path, set := range want {
		if set[""] {
			scope.Files[path] = nil
			continue
		}
		headings := []string{}
		for _, h := range skeletonFor(path) {
			if set[h] {
				headings = append(headings, h)
			}
		}
		scope.Files[path] = headings
	}
	return scope
}

// expandSection returns the skeleton headings a comment on heading covers:
// the heading itself, plus the "###" subsections the skeleton gives a "##"
// heading (a comment on a ticket's "## Plan" is a comment on its four parts).
// It is nil when the skeleton lacks heading.
func expandSection(skeleton []string, heading string) []string {
	for i, h := range skeleton {
		if h != heading {
			continue
		}
		out := []string{h}
		if strings.HasPrefix(h, "## ") {
			for _, next := range skeleton[i+1:] {
				if !strings.HasPrefix(next, "### ") {
					break
				}
				out = append(out, next)
			}
		}
		return out
	}
	return nil
}

// ErrPassageMoved is the refusal for a comment whose passage is no longer in
// the file it was made on (the file was edited, redrafted or replaced since
// the reviewer read it).
var ErrPassageMoved = errors.New("a comment's passage is no longer in the document")
