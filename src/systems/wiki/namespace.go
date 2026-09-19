package main

import "strings"

// Wiki authz is PROJECT-scoped, not user-scoped: a page belongs to a gatekeeper
// Project, and the resource leads with the reserved "project/<slug>/" namespace —
// the same top-level scope every other service uses (git-factory, tickets,
// workflows, forge). Shape: "project/{slug}/wiki/pages" for the collection
// (create/list) and "project/{slug}/wiki/pages/{id}" per page. Because a project
// tier role grants "project/<slug>/*" over Service "*", a project MEMBER is
// authorized on the project's wiki with no per-user or per-service grant — and the
// service still reaches git as its own bot (model B), so no git-factory grants are
// needed either. (Before: "{project}/wiki/pages", which only matched by accident
// when the project name equalled the caller's username — see the project-scoping
// refactor.)

func pagesResource(project string) string {
	return "project/" + project + "/wiki/pages"
}

func pageResource(project, id string) string {
	return "project/" + project + "/wiki/pages/" + id
}

// validSlug guards the project and page id: both become path segments (URL, resource
// string, and a file path in the repo), so they are held to a safe charset.
func validSlug(s string) bool {
	if s == "" || len(s) > 128 || strings.Contains(s, "..") {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}
