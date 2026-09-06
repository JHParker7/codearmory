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
	PutPage(ctx context.Context, project string, p Page, msg string) (Page, error)
	DeletePage(ctx context.Context, project, pageID, msg string) error
	History(ctx context.Context, project, pageID string) ([]Commit, error)
}

var errNotFound = errors.New("not found")

// gitFactoryStore implements Store over git-factory. Model B: the wiki service OWNS the
// wiki repos through its own bot identity — a user never needs git-factory grants, only
// wiki permissions (checked in the handlers). The service mints a run-token AS the bot
// via gatekeeper's internal endpoint using its service key (the same mechanism the git
// connector uses to mint git-factory tokens), so every wiki commit is authored by the
// bot and no standing credential is stored.
type gitFactoryStore struct {
	gfURL     string // http://…-git-factory:9002
	gkURL     string // gatekeeper, for minting the bot run-token
	svcKey    string // this service's GATEKEEPER_SERVICE_KEY
	botUser   string // the wiki bot's gatekeeper user id (owns the wiki repos)
	namespace string // the namespace the bot owns, where <project>-wiki repos live
	hc        *http.Client

	mu       sync.Mutex
	token    string
	tokenExp time.Time
	repoIDs  map[string]string // project -> git-factory repo id (cache)
}

func newGitFactoryStore(gfURL, gkURL, svcKey, botUser, namespace string, hc *http.Client) *gitFactoryStore {
	return &gitFactoryStore{gfURL: gfURL, gkURL: gkURL, svcKey: svcKey, botUser: botUser,
		namespace: namespace, hc: hc, repoIDs: map[string]string{}}
}

// botToken mints (and caches) a gatekeeper run-token acting as the wiki bot. Re-minted
// well before expiry; the bot identity is what authors every wiki commit.
func (s *gitFactoryStore) botToken(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && time.Now().Before(s.tokenExp) {
		return s.token, nil
	}
	body, _ := json.Marshal(map[string]string{"user_id": s.botUser})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.gkURL+"/internal/run-tokens", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Key", "wiki:"+s.svcKey)
	resp, err := s.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("mint bot token: gatekeeper %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	s.token, s.tokenExp = out.Token, time.Now().Add(20*time.Minute)
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
		// first use for this project — create the wiki repo (owned by the bot)
		if cerr := s.gf(ctx, http.MethodPost, "/repos", map[string]string{"name": name, "visibility": "private"}, &re); cerr != nil {
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

func (s *gitFactoryStore) readBlob(ctx context.Context, repoID, path string) (string, error) {
	var out struct {
		Content string `json:"content"`
	}
	q := url.Values{"ref": {"main"}, "path": {path}}
	if err := s.gf(ctx, http.MethodGet, "/repos/"+repoID+"/blob?"+q.Encode(), nil, &out); err != nil {
		return "", err
	}
	return out.Content, nil
}

func (s *gitFactoryStore) writeBlob(ctx context.Context, repoID, path, content, msg string) error {
	return s.gf(ctx, http.MethodPut, "/repos/"+repoID+"/blob",
		map[string]string{"ref": "main", "path": path, "content": content, "message": msg}, nil)
}

func (s *gitFactoryStore) GetManifest(ctx context.Context, project string) (Manifest, error) {
	id, err := s.repoID(ctx, project)
	if err != nil {
		return Manifest{}, err
	}
	raw, err := s.readBlob(ctx, id, manifestPath)
	if errors.Is(err, errNotFound) {
		return Manifest{Project: project}, nil // fresh wiki: empty manifest
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

func (s *gitFactoryStore) writeManifest(ctx context.Context, repoID string, m Manifest) error {
	m.Updated = time.Now().UTC()
	b, _ := json.MarshalIndent(m, "", "  ")
	return s.writeBlob(ctx, repoID, manifestPath, string(b), "chore(wiki): update manifest")
}

func (s *gitFactoryStore) GetPage(ctx context.Context, project, pageID string) (Page, error) {
	m, err := s.GetManifest(ctx, project)
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
	content, err := s.readBlob(ctx, id, meta.Path)
	if err != nil && !errors.Is(err, errNotFound) {
		return Page{}, err
	}
	return Page{PageMeta: meta, Content: content}, nil
}

func (s *gitFactoryStore) PutPage(ctx context.Context, project string, p Page, msg string) (Page, error) {
	id, err := s.repoID(ctx, project)
	if err != nil {
		return Page{}, err
	}
	m, err := s.GetManifest(ctx, project)
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
	if err := s.writeBlob(ctx, id, p.Path, p.Content, msg); err != nil {
		return Page{}, err
	}
	if err := s.writeManifest(ctx, id, m); err != nil {
		return Page{}, err
	}
	return p, nil
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
	return s.writeManifest(ctx, id, m)
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
