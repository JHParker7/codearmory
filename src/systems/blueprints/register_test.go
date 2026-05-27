package main

import "testing"

func TestServiceConfigLoaded(t *testing.T) {
	if serviceConfig.Name == "" {
		t.Fatal("serviceConfig.Name must not be empty")
	}
	if len(serviceConfig.Endpoints) == 0 {
		t.Fatal("serviceConfig.Endpoints must not be empty")
	}
	if len(serviceConfig.Roles) == 0 {
		t.Fatal("serviceConfig.Roles must not be empty")
	}
}

func TestServiceConfigEndpoints(t *testing.T) {
	var hasUserScoped, hasOrgScoped bool
	for _, ep := range serviceConfig.Endpoints {
		if ep.Path == "/state/{username}/{workspace}" {
			hasUserScoped = true
		}
		if ep.Path == "/{org}/state/{team}/{workspace}" {
			hasOrgScoped = true
		}
	}
	if !hasUserScoped {
		t.Fatal("expected user-scoped endpoint /state/{username}/{workspace}")
	}
	if !hasOrgScoped {
		t.Fatal("expected org-scoped endpoint /{org}/state/{team}/{workspace}")
	}
}
