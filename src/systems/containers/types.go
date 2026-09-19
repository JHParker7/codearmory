package main

import "time"

const maxBodyBytes = 32 * 1024

// Repository is a named image repository in the registry. The portal drills into
// tags with (Namespace, Name), so the catalog path is split: Namespace is the first
// path component, Name is the rest, FullName is the whole path. Returning only the
// full path as Name left the portal's Namespace empty and its tags URL malformed.
type Repository struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	FullName  string `json:"full_name"`
}

// TagList is the list of tags for a repository.
type TagList struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

// ManifestDescriptor describes one layer, config, or sub-manifest.
type ManifestDescriptor struct {
	MediaType string `json:"mediaType"`
	Size      int64  `json:"size"`
	Digest    string `json:"digest"`
	Platform  *struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
		Variant      string `json:"variant,omitempty"`
	} `json:"platform,omitempty"`
}

// Manifest is a summarised view of an OCI/Docker image manifest.
// Push/pull are not proxied here — users interact with the registry
// directly for those; this service provides management visibility.
type Manifest struct {
	SchemaVersion int                  `json:"schemaVersion"`
	MediaType     string               `json:"mediaType"`
	Digest        string               `json:"digest"`
	Config        *ManifestDescriptor  `json:"config,omitempty"`
	Layers        []ManifestDescriptor `json:"layers,omitempty"`
	Manifests     []ManifestDescriptor `json:"manifests,omitempty"`
	Repository    string               `json:"repository"`
	Reference     string               `json:"reference"`
	FetchedAt     time.Time            `json:"fetched_at"`
}
