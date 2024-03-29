package ghrepos

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/google/go-github/v54/github"
	"golang.org/x/oauth2"
)

var (
	GitHubToken = os.Getenv("GITHUB_TOKEN")
	ProjectRoot = os.Getenv("KANVAS_WORKSPACE")
)

// Summarizer summarizes GitHub repositories
type Summarizer struct {
	GitHubToken string

	client *github.Client
	sync.Once
}

func (c *Summarizer) getGitHubClient() *github.Client {
	c.Once.Do(func() {
		if c.GitHubToken == "" {
			c.GitHubToken = GitHubToken
		}
		ts := oauth2.StaticTokenSource(
			&oauth2.Token{AccessToken: c.GitHubToken},
		)
		ctx := context.Background()
		tc := oauth2.NewClient(ctx, ts)

		c.client = github.NewClient(tc)
	})

	return c.client
}

// Summary is a summary of GitHub repositories possibly related to the target repository.
// It's used as a context to let the AI suggest a kanvas.yaml file content
// based on your environment.
type Summary struct {
	// Repos is a list of GitHub repositories possibly related to the target repository.
	Repos []string
	// Contents is a list of summaries of the repositories.
	// Each summary starts with the repository name followed by a list of paths to files.
	Contents []string
}

type Tree struct {
	Label string
	Files []string
	Trees map[string]*Tree
}

func (t *Tree) Add(path string) {
	if path == "" {
		return
	}

	if t.Trees == nil {
		t.Trees = map[string]*Tree{}
	}

	splits := strings.SplitN(path, string(os.PathSeparator), 2)
	if len(splits) == 1 {
		t.Files = append(t.Files, path)
		return
	}
	label := splits[0]
	nextPath := splits[1]

	sub, ok := t.Trees[label]
	if !ok {
		sub = &Tree{
			Label: label,
			Trees: map[string]*Tree{},
		}
		t.Trees[label] = sub
	}

	sub.Add(nextPath)
}

func (t *Tree) String() string {
	return t.getStringRepr(0)
}

// getStringRepr returns a string representation of the tree.
func (t *Tree) getStringRepr(depth int) string {
	var b strings.Builder

	b.WriteString(t.Label)
	b.WriteString("\n")

	var i int

	var treeNames []string

	for name := range t.Trees {
		treeNames = append(treeNames, name)
	}

	sort.Strings(treeNames)

	for _, name := range treeNames {
		sub := t.Trees[name]
		for i := 0; i < depth; i++ {
			b.WriteString("  ")
		}

		b.WriteString("+ ")
		b.WriteString(sub.getStringRepr(depth + 1))

		i++
	}

	for _, sub := range t.Files {
		for i := 0; i < depth; i++ {
			b.WriteString("  ")
		}

		b.WriteString("* ")
		b.WriteString(sub)
		b.WriteString("\n")
	}

	return b.String()
}

func (c *Summarizer) Summarize(workspace string) (*Summary, error) {
	if workspace == "" {
		workspace = ProjectRoot
	}

	contents, err := c.getPossiblyRelatedRepoContents(workspace)
	if err != nil {
		return nil, err
	}

	var (
		projectName = filepath.Base(workspace)
		summary     Summary
	)

	for _, content := range contents {
		repo := content.Repo.GetName()
		if repo == projectName {
			repo += " (this project)"
		}
		summary.Repos = append(summary.Repos, repo)

		contentStr := content.Root.String()

		summary.Contents = append(summary.Contents, contentStr)
	}

	return &summary, nil
}

func (c *Summarizer) getPossiblyRelatedRepoContents(workspace string) ([]RepoContent, error) {
	ctx := context.Background()

	client := c.getGitHubClient()

	r, err := git.PlainOpen(workspace)
	if err != nil {
		return nil, err
	}

	remotes, err := r.Remotes()
	if err != nil {
		return nil, err
	}

	var origin *config.RemoteConfig
	for _, remote := range remotes {
		if remote.Config().Name == "origin" {
			origin = remote.Config()
			break
		}
	}

	url := origin.URLs[0]
	// git@github.com:davinci-std/exampleapp.git"
	urlParts := strings.Split(url, ":")
	if len(urlParts) != 2 {
		return nil, fmt.Errorf("unexpected url: %s", url)
	}

	// davinci-std/exampleapp.git"
	userRepoStr := urlParts[1]
	userRepo := strings.Split(userRepoStr, "/")
	if len(urlParts) != 2 {
		return nil, fmt.Errorf("unexpected url: %s", url)
	}

	// davinci-std
	org := userRepo[0]
	// exampleapp.git
	repo := userRepo[1]
	// exampleapp
	repo = strings.TrimSuffix(repo, ".git")

	var (
		allRepos []*github.Repository
		nextPage *int
	)

	for {
		opts := github.RepositoryListByOrgOptions{}

		if nextPage != nil {
			opts.ListOptions.Page = *nextPage
		}

		// List all repositories by the organization of the target repository
		// to collect possibly related repositories.
		//
		// Note that List doesn't work as it's tied to the user endnpoint.
		// We need to use ListByOrg assuming everyone uses organization repositories...
		//
		// Do also note that GitHub classic tokens doesn't work probably when you aren&t an admin of
		// the organization.
		// GitHub fine-grained tokens work fine after an admin of the organization approved your token.
		// Note that the approval needs manual operation.
		//
		// See https://github.com/google/go-github/issues/2396#issuecomment-1181176636
		repos, res, err := client.Repositories.ListByOrg(ctx, org, &opts)
		if err != nil {
			return nil, err
		}

		allRepos = append(allRepos, repos...)

		if res.NextPage == 0 {
			break
		}

		if res.NextPage != 0 {
			nextPage = &res.NextPage
		}

		time.Sleep(1 * time.Second)
	}

	for _, r := range allRepos {
		fmt.Fprintf(os.Stderr, "Summarizing repository: %s\n", r.GetName())
	}

	var possibilyRelevantRepos []*github.Repository
	for _, repo := range allRepos {
		// Skip the repository if it's archived
		if repo.GetArchived() {
			continue
		}

		// Skip the repository if it's a fork
		if repo.GetFork() {
			continue
		}

		// Skip the repository if it's a template
		if repo.GetTemplateRepository() != nil {
			continue
		}

		// Skip the repository if it's a mirror
		if repo.GetMirrorURL() != "" {
			continue
		}

		// Skip the repository if it's a disabled
		if repo.GetDisabled() {
			continue
		}

		possibilyRelevantRepos = append(possibilyRelevantRepos, repo)
	}

	contents, err := c.getRepoContents(possibilyRelevantRepos, repo)
	if err != nil {
		return nil, err
	}

	return contents, nil
}

type RepoContent struct {
	Repo  *github.Repository
	Files []string
	Root  *Tree
}

func (c *Summarizer) getRepoContents(repos []*github.Repository, projectRepo string) ([]RepoContent, error) {
	client := c.getGitHubClient()

	numRepos := len(repos)

	var repoContents []RepoContent
	for i, repo := range repos {
		// Get the tree for the master branch
		tree, res, err := client.Git.GetTree(context.Background(), repo.GetOwner().GetLogin(), repo.GetName(), *repo.DefaultBranch, true)

		// Maybe "409 Repopsitory is empty" which means the repository is literally empty
		// and have no files yet.
		// This needs to be earlier than the error check because
		// it is also an error.
		if res != nil && res.StatusCode == 409 {
			fmt.Fprintf(os.Stderr, "  Skipping empty repository: %s\n", repo.GetName())
			continue
		}

		fmt.Fprintf(os.Stderr, "  Summarizing repository: %s (%d/%d)\n", repo.GetName(), i+1, numRepos)

		if err != nil {
			return nil, err
		}

		numEntries := len(tree.Entries)
		var (
			files []string
			root  = &Tree{
				Label: repo.GetName(),
			}
			numNodes int
		)
		for j, entry := range tree.Entries {
			fmt.Fprintf(os.Stderr, "    Processing tree entry: %s %s (%d/%d)\n", entry.GetPath(), entry.GetType(), j+1, numEntries)

			if entry.GetType() == "blob" {
				path := entry.GetPath()
				ext := filepath.Ext(path)
				repo := repo.GetName()
				if ext == ".tf" || (repo == projectRepo && strings.Contains(path, "Dockerfile")) {
					files = append(files, entry.GetPath())
					root.Add(entry.GetPath())
					numNodes++
				}
			}
		}

		if numNodes == 0 {
			fmt.Fprintf(os.Stderr, "  Skipping empty repository: %s\n", repo.GetName())
			continue
		}

		repoContents = append(repoContents, RepoContent{
			Repo:  repo,
			Files: files,
			Root:  root,
		})
	}

	return repoContents, nil
}
