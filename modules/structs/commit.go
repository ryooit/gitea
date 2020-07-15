package structs

type CommitAction struct {
	Action       string `json:"action"`
	Content      string `json:"content"`
	SHA          string `json:"sha"`
	TreePath     string `json:"tree_path"`
	FromTreePath string `json:"from_tree_path"`
}

type CreateCommitOptions struct {
	Message string `json:"message"`
	// branch (optional) to base this file from. if not given, the default branch is used
	BranchName string `json:"branch" binding:"GitRefName;MaxSize(100)"`
	// new_branch (optional) will make a new branch from `branch` before creating the file
	NewBranchName string `json:"new_branch" binding:"GitRefName;MaxSize(100)"`

	Actions      []*CommitAction `json:"actions"`
	Author       Identity        `json:"author"`
	Committer    Identity        `json:"committer"`
	LastCommitID string          `json:"last_commit_id"`
}
