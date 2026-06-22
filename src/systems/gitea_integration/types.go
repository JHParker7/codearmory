package main

import "time"

const maxBodyBytes = 64 * 1024

// GiteaAccount links a CodeArmory user_id to a Gitea/Forgejo username.
type GiteaAccount struct {
	UserID        string    `json:"user_id"        gorm:"column:user_id;primaryKey"`
	GiteaUsername string    `json:"gitea_username" gorm:"column:gitea_username"`
	CreatedAt     time.Time `json:"created_at"     gorm:"column:created_at"`
	UpdatedAt     time.Time `json:"updated_at"     gorm:"column:updated_at"`
}

func (GiteaAccount) TableName() string { return "gitea_accounts" }

// RepoProject tags a repository (by its globally-unique owner/repo full name)
// with a free-text workspace project label. It is the only local persistence for
// repos, which are otherwise proxied live from Gitea. The project is a view
// filter, not a security boundary.
type RepoProject struct {
	FullName  string    `json:"full_name" gorm:"column:full_name;primaryKey"`
	Project   string    `json:"project"   gorm:"column:project"`
	UpdatedAt time.Time `json:"updated_at" gorm:"column:updated_at"`
}

func (RepoProject) TableName() string { return "gitea_repo_projects" }

// Repo mirrors the Gitea repository API response fields we expose. Project is not
// returned by Gitea — it is stamped locally from the RepoProject mapping table.
type Repo struct {
	ID            int64     `json:"id"`
	Name          string    `json:"name"`
	FullName      string    `json:"full_name"`
	Description   string    `json:"description"`
	Private       bool      `json:"private"`
	Fork          bool      `json:"fork"`
	HTMLURL       string    `json:"html_url"`
	CloneURL      string    `json:"clone_url"`
	SSHURL        string    `json:"ssh_url"`
	DefaultBranch string    `json:"default_branch"`
	Stars         int       `json:"stars_count"`
	Forks         int       `json:"forks_count"`
	Project       string    `json:"project,omitempty" gorm:"-"`
	CreatedAt     time.Time `json:"created"`
	UpdatedAt     time.Time `json:"updated"`
}

// Branch mirrors the Gitea branch API response.
type Branch struct {
	Name   string `json:"name"`
	Commit struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	} `json:"commit"`
	Protected bool `json:"protected"`
}

// Tag mirrors the Gitea tag API response.
type Tag struct {
	Name       string `json:"name"`
	Message    string `json:"message"`
	ID         string `json:"id"`
	ZipballURL string `json:"zipball_url"`
	TarballURL string `json:"tarball_url"`
}

// Release mirrors the Gitea release API response.
type Release struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	TagName     string    `json:"tag_name"`
	Body        string    `json:"body"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	HTMLURL     string    `json:"html_url"`
	TarballURL  string    `json:"tarball_url"`
	ZipballURL  string    `json:"zipball_url"`
	CreatedAt   time.Time `json:"created_at"`
	PublishedAt time.Time `json:"published_at"`
}

// PRBranch is a branch reference inside a PullRequest.
type PRBranch struct {
	Label string `json:"label"`
	Ref   string `json:"ref"`
	SHA   string `json:"sha"`
}

// PullRequest mirrors the Gitea pull request API response.
type PullRequest struct {
	ID        int64      `json:"id"`
	Number    int64      `json:"number"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	State     string     `json:"state"`
	HTMLURL   string     `json:"html_url"`
	Mergeable *bool      `json:"mergeable"`
	Merged    bool       `json:"merged"`
	MergedAt  *time.Time `json:"merged_at,omitempty"`
	Head      PRBranch   `json:"head"`
	Base      PRBranch   `json:"base"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// Commit mirrors the Gitea commit API response.
type Commit struct {
	SHA     string `json:"sha"`
	HTMLURL string `json:"html_url"`
	Created string `json:"created"`
	Commit  struct {
		Message string `json:"message"`
		Author  struct {
			Name  string `json:"name"`
			Email string `json:"email"`
			Date  string `json:"date"`
		} `json:"author"`
	} `json:"commit"`
}
