package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const manifestAccept = "application/vnd.oci.image.manifest.v1+json," +
	"application/vnd.docker.distribution.manifest.v2+json," +
	"application/vnd.docker.distribution.manifest.list.v2+json," +
	"application/vnd.oci.image.index.v1+json"

type registryClient struct {
	baseURL  string
	username string
	password string
	http     *http.Client
}

type registryError struct {
	Status int
	Body   string
}

func (e *registryError) Error() string {
	return fmt.Sprintf("registry %d: %s", e.Status, e.Body)
}

func isRegistryNotFound(err error) bool {
	if e, ok := err.(*registryError); ok {
		return e.Status == http.StatusNotFound
	}
	return false
}

func (c *registryClient) newRequest(ctx context.Context, method, path, accept string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	if c.username != "" {
		req.SetBasicAuth(c.username, c.password)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	return req, nil
}

func (c *registryClient) do(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return &registryError{Status: resp.StatusCode, Body: string(b[:min(len(b), 512)])}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *registryClient) listRepositories(ctx context.Context) ([]string, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/v2/_catalog", "application/json")
	if err != nil {
		return nil, err
	}
	var result struct {
		Repositories []string `json:"repositories"`
	}
	if err := c.do(req, &result); err != nil {
		return nil, err
	}
	if result.Repositories == nil {
		result.Repositories = []string{}
	}
	return result.Repositories, nil
}

func (c *registryClient) listTags(ctx context.Context, name string) (*TagList, error) {
	// name may contain slashes (e.g. "namespace/image") — path escape each segment.
	segments := strings.Split(name, "/")
	escaped := make([]string, len(segments))
	for i, s := range segments {
		escaped[i] = url.PathEscape(s)
	}
	req, err := c.newRequest(ctx, http.MethodGet,
		"/v2/"+strings.Join(escaped, "/")+"/tags/list", "application/json")
	if err != nil {
		return nil, err
	}
	var tl TagList
	if err := c.do(req, &tl); err != nil {
		return nil, err
	}
	if tl.Tags == nil {
		tl.Tags = []string{}
	}
	return &tl, nil
}

func (c *registryClient) getManifest(ctx context.Context, name, reference string) (*Manifest, error) {
	segments := strings.Split(name, "/")
	escaped := make([]string, len(segments))
	for i, s := range segments {
		escaped[i] = url.PathEscape(s)
	}
	path := "/v2/" + strings.Join(escaped, "/") + "/manifests/" + url.PathEscape(reference)
	req, err := c.newRequest(ctx, http.MethodGet, path, manifestAccept)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return nil, &registryError{Status: resp.StatusCode, Body: string(b)}
	}
	digest := resp.Header.Get("Docker-Content-Digest")
	var m Manifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, err
	}
	m.Digest = digest
	m.Repository = name
	m.Reference = reference
	m.FetchedAt = time.Now().UTC()
	return &m, nil
}

func (c *registryClient) deleteManifest(ctx context.Context, name, digest string) error {
	segments := strings.Split(name, "/")
	escaped := make([]string, len(segments))
	for i, s := range segments {
		escaped[i] = url.PathEscape(s)
	}
	path := "/v2/" + strings.Join(escaped, "/") + "/manifests/" + url.PathEscape(digest)
	req, err := c.newRequest(ctx, http.MethodDelete, path, "")
	if err != nil {
		return err
	}
	return c.do(req, nil)
}
