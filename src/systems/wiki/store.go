package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Store is the wiki's storage seam. The content lives in a git repo, not in this
// service, so the whole interface is "read/write files + read history" over a project's
// wiki repo. git-factory is the first backend; the git connector's other backends
// (GitHub for a migrated user, Forgejo) slot in behind this same interface later.
type Store interface {
	GetManifest(ctx context.Context, project string) (Manifest, error)
	GetPage(ctx context.Context, project, pageID string) (Page, error)
	// GetManifestOnBranch / GetPageOnBranch read the page index / a page as they stand on
	// a plan branch, so the portal editor can load the in-review version (branch=="" => main).
	GetManifestOnBranch(ctx context.Context, project, branch string) (Manifest, error)
	GetPageOnBranch(ctx context.Context, project, branch, pageID string) (Page, error)
	PutPage(ctx context.Context, project string, p Page, msg string) (Page, error)
	// PutPageOnBranch writes to a plan branch (created off main on first use) instead of
	// main, for the review-before-merge plan flow. branch=="" behaves like PutPage.
	PutPageOnBranch(ctx context.Context, project, branch string, p Page, msg string) (Page, error)
	// OpenPlanPR opens a PR from a plan branch to main on the project's wiki repo.
	OpenPlanPR(ctx context.Context, project, branch, title, body string) (PlanPR, error)
	DeletePage(ctx context.Context, project, pageID, msg string) error
	History(ctx context.Context, project, pageID string) ([]Commit, error)
}

var errNotFound = errors.New("not found")

// gitFactoryStore implements Store over git-factory. Model B: the wiki service OWNS the
// wiki repos through its own bot identity — a user never needs git-factory grants, only
// wiki permissions (checked in the handlers). The wiki simply IS that bot: it logs in as
// the bot and uses that session (cached) for git. No privileged impersonation — the bot
// only ever acts on repos in its OWN namespace, so this needs no scoped-role minting, it
// is just an ordinary login. Every wiki commit is authored by the bot.
type gitFactoryStore struct {
	gfURL     string // http://…-git-factory:9002
	gkURL     string // gatekeeper, for the bot login
	botEmail  string // the wiki bot's login email
	botPass   string // the wiki bot's password
	namespace string // the namespace the bot owns, where <project>-wiki repos live
	hc        *http.Client

	mu       sync.Mutex
	token    string
	tokenExp time.Time
	repoIDs  map[string]string // project -> git-factory repo id (cache)
}

func newGitFactoryStore(gfURL, gkURL, botEmail, botPass, namespace string, hc *http.Client) *gitFactoryStore {
	return &gitFactoryStore{gfURL: gfURL, gkURL: gkURL, botEmail: botEmail, botPass: botPass,
		namespace: namespace, hc: hc, repoIDs: map[string]string{}}
}

// botToken logs in as the wiki bot and caches the session. Refreshed well before expiry;
// the bot's session carries exactly the bot's own grants (its own namespace), which is
// all the wiki ever needs — so this is a plain login, not a privileged token mint.
func (s *gitFactoryStore) botToken(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && time.Now().Before(s.tokenExp) {
		return s.token, nil
	}
	body, _ := json.Marshal(map[string]string{"email": s.botEmail, "password": s.botPass})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.gkURL+"/login", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("wiki bot login: gatekeeper %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", fmt.Errorf("wiki bot login: empty token")
	}
	// Sessions last hours; cache for 30 min and re-login well before expiry.
	s.token, s.tokenExp = out.Token, time.Now().Add(30*time.Minute)
	return s.token, nil
}

// gf issues a git-factory request as the bot. out (if non-nil) is JSON-decoded from a 2xx
// body; a 404 becomes errNotFound so callers can branch on "page/repo absent".
func (s *gitFactoryStore) gf(ctx context.Context, method, path string, in, out any) error {
	tok, err := s.botToken(ctx)
	if err != nil {
		return err
	}
	var rdr io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.gfURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return errNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("git-factory %s %s: %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	return nil
}

// repoID resolves (creating on first use) the git-factory repo backing a project's wiki.
func (s *gitFactoryStore) repoID(ctx context.Context, project string) (string, error) {
	s.mu.Lock()
	if id := s.repoIDs[project]; id != "" {
		s.mu.Unlock()
		return id, nil
	}
	s.mu.Unlock()

	name := project + "-wiki"
	var re struct {
		ID string `json:"id"`
	}
	err := s.gf(ctx, http.MethodPost, "/repos/by-path", map[string]string{"namespace": s.namespace, "name": name}, &re)
	if errors.Is(err, errNotFound) {
		// first use for this project — create the wiki repo (owned by the bot).
		// Tag it with the project it serves: git-factory now rejects a project-less
		// repo, and the wiki repo is a resource of that project (nothing is projectless).
		if cerr := s.gf(ctx, http.MethodPost, "/repos", map[string]string{"name": name, "visibility": "private", "project": project}, &re); cerr != nil {
			return "", fmt.Errorf("create wiki repo %q: %w", name, cerr)
		}
	} else if err != nil {
		return "", err
	}
	if re.ID == "" {
		return "", fmt.Errorf("wiki repo %q has no id", name)
	}
	s.mu.Lock()
	s.repoIDs[project] = re.ID
	s.mu.Unlock()
	return re.ID, nil
}

func (s *gitFactoryStore) readBlob(ctx context.Context, repoID, ref, path string) (string, error) {
	if ref == "" {
		ref = "main"
	}
	var out struct {
		Content string `json:"content"`
	}
	q := url.Values{"ref": {ref}, "path": {path}}
	if err := s.gf(ctx, http.MethodGet, "/repos/"+repoID+"/blob?"+q.Encode(), nil, &out); err != nil {
		return "", err
	}
	return out.Content, nil
}

// ensureBranch creates branch off main if it does not already exist. git-factory returns
// 409 when the branch is already there, which is exactly the state we want, so it is not
// an error. main (or empty) is a no-op.
func (s *gitFactoryStore) ensureBranch(ctx context.Context, repoID, branch string) error {
	if branch == "" || branch == "main" {
		return nil
	}
	err := s.gf(ctx, http.MethodPost, "/repos/"+repoID+"/branches",
		map[string]string{"name": branch, "from": "main"}, nil)
	if err == nil || strings.Contains(err.Error(), ": 409:") {
		return nil
	}
	return err
}

func (s *gitFactoryStore) writeBlob(ctx context.Context, repoID, ref, path, content, msg string) error {
	if ref == "" {
		ref = "main"
	}
	if err := s.ensureBranch(ctx, repoID, ref); err != nil {
		return err
	}
	return s.gf(ctx, http.MethodPut, "/repos/"+repoID+"/blob",
		map[string]string{"ref": ref, "path": path, "content": content, "message": msg}, nil)
}

func (s *gitFactoryStore) GetManifest(ctx context.Context, project string) (Manifest, error) {
	return s.getManifest(ctx, project, "main")
}

// getManifest reads a project's page manifest from a specific ref (branch). "" → main.
func (s *gitFactoryStore) getManifest(ctx context.Context, project, ref string) (Manifest, error) {
	id, err := s.repoID(ctx, project)
	if err != nil {
		return Manifest{}, err
	}
	raw, err := s.readBlob(ctx, id, ref, manifestPath)
	if errors.Is(err, errNotFound) {
		return Manifest{Project: project}, nil // fresh wiki / branch: empty manifest
	}
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return Manifest{}, fmt.Errorf("corrupt manifest: %w", err)
	}
	m.Project = project
	return m, nil
}

func (s *gitFactoryStore) writeManifest(ctx context.Context, repoID, ref string, m Manifest) error {
	m.Updated = time.Now().UTC()
	b, _ := json.MarshalIndent(m, "", "  ")
	return s.writeBlob(ctx, repoID, ref, manifestPath, string(b), "chore(wiki): update manifest")
}

func (s *gitFactoryStore) GetPage(ctx context.Context, project, pageID string) (Page, error) {
	return s.getPage(ctx, project, "main", pageID)
}

// GetPageOnBranch reads a page as it stands on a plan branch, so a reviewer can see (and
// the portal editor can load) the in-review version before merge. Falls back to main
// when branch is empty or "main".
func (s *gitFactoryStore) GetPageOnBranch(ctx context.Context, project, branch, pageID string) (Page, error) {
	if branch == "" {
		branch = "main"
	}
	return s.getPage(ctx, project, branch, pageID)
}

// GetManifestOnBranch returns the page index as it stands on a branch (the plan branch
// lists the pages under review). Falls back to main when branch is empty.
func (s *gitFactoryStore) GetManifestOnBranch(ctx context.Context, project, branch string) (Manifest, error) {
	if branch == "" {
		branch = "main"
	}
	return s.getManifest(ctx, project, branch)
}

func (s *gitFactoryStore) getPage(ctx context.Context, project, ref, pageID string) (Page, error) {
	m, err := s.getManifest(ctx, project, ref)
	if err != nil {
		return Page{}, err
	}
	meta, ok := findPage(m, pageID)
	if !ok {
		return Page{}, errNotFound
	}
	id, err := s.repoID(ctx, project)
	if err != nil {
		return Page{}, err
	}
	content, err := s.readBlob(ctx, id, ref, meta.Path)
	if err != nil && !errors.Is(err, errNotFound) {
		return Page{}, err
	}
	return Page{PageMeta: meta, Content: content}, nil
}

func (s *gitFactoryStore) PutPage(ctx context.Context, project string, p Page, msg string) (Page, error) {
	return s.putPage(ctx, project, "main", p, msg)
}

// PutPageOnBranch writes a page to a (plan) branch instead of main, creating the branch
// off main on first write so it starts as a copy of the live wiki. This is how the
// architect's plan lands on a branch a human reviews as a PR before it reaches main and
// the build agents read it.
func (s *gitFactoryStore) PutPageOnBranch(ctx context.Context, project, branch string, p Page, msg string) (Page, error) {
	if branch == "" {
		branch = "main"
	}
	return s.putPage(ctx, project, branch, p, msg)
}

func (s *gitFactoryStore) putPage(ctx context.Context, project, ref string, p Page, msg string) (Page, error) {
	id, err := s.repoID(ctx, project)
	if err != nil {
		return Page{}, err
	}
	// For a non-main branch, create it (off main) BEFORE reading the manifest so version
	// bumping sees the live page set, not an empty branch.
	if ref != "" && ref != "main" {
		if err := s.ensureBranch(ctx, id, ref); err != nil {
			return Page{}, err
		}
	}
	m, err := s.getManifest(ctx, project, ref)
	if err != nil {
		return Page{}, err
	}
	// upsert the manifest entry, bumping the version
	prev, existed := findPage(m, p.ID)
	p.Version = 1
	if existed {
		p.Version = prev.Version + 1
		if p.Path == "" {
			p.Path = prev.Path
		}
	}
	p.Updated = time.Now().UTC()
	upsertPage(&m, p.PageMeta)
	// write the page body, then the refreshed manifest (two commits; git holds both)
	if err := s.writeBlob(ctx, id, ref, p.Path, p.Content, msg); err != nil {
		return Page{}, err
	}
	if err := s.writeManifest(ctx, id, ref, m); err != nil {
		return Page{}, err
	}
	return p, nil
}

// OpenPlanPR opens a git-factory PR on the project's wiki repo from a plan branch to main,
// so the architect's plan can be reviewed (and refined on the branch) before merge. If a PR
// for that branch already exists, git-factory returns it / a conflict; the caller treats a
// non-2xx as "already open" is NOT assumed here — callers should surface the error.
func (s *gitFactoryStore) OpenPlanPR(ctx context.Context, project, branch, title, body string) (PlanPR, error) {
	id, err := s.repoID(ctx, project)
	if err != nil {
		return PlanPR{}, err
	}
	var pr struct {
		Number int    `json:"number"`
		ID     string `json:"id"`
		State  string `json:"state"`
	}
	err = s.gf(ctx, http.MethodPost, "/repos/"+id+"/pulls",
		map[string]string{"title": title, "body": body, "source_ref": branch, "target_ref": "main"}, &pr)
	if err != nil {
		return PlanPR{}, err
	}
	return PlanPR{Number: pr.Number, RepoID: id, Repo: project + "-wiki", Namespace: s.namespace, Branch: branch, State: pr.State}, nil
}

func (s *gitFactoryStore) DeletePage(ctx context.Context, project, pageID, msg string) error {
	id, err := s.repoID(ctx, project)
	if err != nil {
		return err
	}
	m, err := s.GetManifest(ctx, project)
	if err != nil {
		return err
	}
	if _, ok := findPage(m, pageID); !ok {
		return errNotFound
	}
	removePage(&m, pageID)
	return s.writeManifest(ctx, id, "main", m)
}

func (s *gitFactoryStore) History(ctx context.Context, project, pageID string) ([]Commit, error) {
	m, err := s.GetManifest(ctx, project)
	if err != nil {
		return nil, err
	}
	meta, ok := findPage(m, pageID)
	if !ok {
		return nil, errNotFound
	}
	id, err := s.repoID(ctx, project)
	if err != nil {
		return nil, err
	}
	var out struct {
		Commits []Commit `json:"commits"`
	}
	q := url.Values{"ref": {"main"}, "path": {meta.Path}, "limit": {"50"}}
	if err := s.gf(ctx, http.MethodGet, "/repos/"+id+"/commits?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out.Commits, nil
}

func findPage(m Manifest, id string) (PageMeta, bool) {
	for _, p := range m.Pages {
		if p.ID == id {
			return p, true
		}
	}
	return PageMeta{}, false
}

func upsertPage(m *Manifest, meta PageMeta) {
	for i, p := range m.Pages {
		if p.ID == meta.ID {
			m.Pages[i] = meta
			return
		}
	}
	m.Pages = append(m.Pages, meta)
}

func removePage(m *Manifest, id string) {
	out := m.Pages[:0]
	for _, p := range m.Pages {
		if p.ID != id {
			out = append(out, p)
		}
	}
	m.Pages = out
}
