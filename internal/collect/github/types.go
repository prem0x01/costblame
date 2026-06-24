package github

import "time"

// WorkflowRunPayload is the GitHub webhook payload for the `workflow_run` event.
type WorkflowRunPayload struct {
	Action      string      `json:"action"`
	WorkflowRun WorkflowRun `json:"workflow_run"`
	Repository  Repository  `json:"repository"`
}

type WorkflowRun struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	Status       string    `json:"status"`
	Conclusion   string    `json:"conclusion"`
	HeadBranch   string    `json:"head_branch"`
	HeadSHA      string    `json:"head_sha"`
	UpdatedAt    time.Time `json:"updated_at"`
	HTMLURL      string    `json:"html_url"`
	PullRequests []PRRef   `json:"pull_requests"`
}

type PRRef struct {
	Number int `json:"number"`
}

type Repository struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
}

// PullRequest is the GitHub REST API response for a single PR.
type PullRequest struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	User   User   `json:"user"`
	Labels []Label `json:"labels"`
	Head   Ref    `json:"head"`
	Base   Ref    `json:"base"`
}

type User struct {
	Login string `json:"login"`
}

type Label struct {
	Name string `json:"name"`
}

type Ref struct {
	Ref  string     `json:"ref"`
	SHA  string     `json:"sha"`
	Repo Repository `json:"repo"`
}

// File is one entry in the PR files list from GitHub's REST API.
type File struct {
	Filename string `json:"filename"`
	Status   string `json:"status"` // added, removed, modified, renamed
}

// Team is a GitHub organization team entry.
type Team struct {
	Name string `json:"name"`
	Slug string `json:"slug"`
}
