package main

import "strings"

// Wiki authz is PROJECT-scoped, not user-scoped: a page belongs to a project, and the
// gatekeeper resource leads with the project namespace so a grant can name "project X's
// wiki". Shape: "{project}/wiki/pages" for the collection (create/list) and
// "{project}/wiki/pages/{id}" per page. A user needs only WIKI permissions on the
// project — never git-factory grants; the service reaches git as its own bot (model B).

func pagesResource(project string) string {
	return project + "/wiki/pages"
}

func pageResource(project, id string) string {
	return project + "/wiki/pages/" + id
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
