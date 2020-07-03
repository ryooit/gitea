// Copyright 2019 The Gitea Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package repofiles

import (
	"code.gitea.io/gitea/models"
	"code.gitea.io/gitea/modules/charset"
	"code.gitea.io/gitea/modules/git"
	"code.gitea.io/gitea/modules/lfs"
	"code.gitea.io/gitea/modules/log"
	repo_module "code.gitea.io/gitea/modules/repository"
	"code.gitea.io/gitea/modules/setting"
	"fmt"
	stdcharset "golang.org/x/net/html/charset"
	"golang.org/x/text/transform"
	"path"
	"strings"
)

// CountDivergingCommits determines how many commits a branch is ahead or behind the repository's base branch
func CountDivergingCommits(repo *models.Repository, branch string) (*git.DivergeObject, error) {
	divergence, err := git.GetDivergingCommits(repo.RepoPath(), repo.DefaultBranch, branch)
	if err != nil {
		return nil, err
	}
	return &divergence, nil
}

type CreateCommitOptions struct {
	LastCommitID string
	OldBranch    string
	NewBranch    string
	Message      string
	Actions      []*CreateCommitAction
	Author       *IdentityOptions
	Committer    *IdentityOptions
	Dates        *CommitDateOptions
}

type CommitActionType string

const CommitActionTypeCreate = CommitActionType("Create")
const CommitActionTypeUpdate = CommitActionType("Update")
const CommitActionTypeMove = CommitActionType("Move")
const CommitActionTypeDelete = CommitActionType("Delete")
const CommitActionTypeChmod = CommitActionType("Chmod")

type CreateCommitAction struct {
	Action       CommitActionType
	Content      string
	SHA          string
	TreePath     string
	FromTreePath string
}

// From repofiles/update.go: CreateOrUpdateRepoFile
func CreateCommit(repo *models.Repository, doer *models.User, opts *CreateCommitOptions) (*git.Commit, error) {
	// If no branch name is set, assume master
	if opts.OldBranch == "" {
		opts.OldBranch = repo.DefaultBranch
	}
	if opts.NewBranch == "" {
		opts.NewBranch = opts.OldBranch
	}

	// oldBranch must exist for this operation
	if _, err := repo_module.GetBranch(repo, opts.OldBranch); err != nil {
		return nil, err
	}

	// A NewBranch can be specified for the file to be created/updated in a new branch.
	// Check to make sure the branch does not already exist, otherwise we can't proceed.
	// If we aren't branching to a new branch, make sure user can commit to the given branch
	if opts.NewBranch != opts.OldBranch {
		existingBranch, err := repo_module.GetBranch(repo, opts.NewBranch)
		if existingBranch != nil {
			return nil, models.ErrBranchAlreadyExists{
				BranchName: opts.NewBranch,
			}
		}
		if err != nil && !git.IsErrBranchNotExist(err) {
			return nil, err
		}
	} else {
		protectedBranch, err := repo.GetBranchProtection(opts.OldBranch)
		if err != nil {
			return nil, err
		}
		if protectedBranch != nil {
			if !protectedBranch.CanUserPush(doer.ID) {
				return nil, models.ErrUserCannotCommit{
					UserName: doer.LowerName,
				}
			}
			if protectedBranch.RequireSignedCommits {
				_, _, err := repo.SignCRUDAction(doer, repo.RepoPath(), opts.OldBranch)
				if err != nil {
					if !models.IsErrWontSign(err) {
						return nil, err
					}
					return nil, models.ErrUserCannotCommit{
						UserName: doer.LowerName,
					}
				}
			}
			patterns := protectedBranch.GetProtectedFilePatterns()
			for _, action := range opts.Actions {
				for _, pat := range patterns {
					if pat.Match(strings.ToLower(action.TreePath)) {
						return nil, models.ErrFilePathProtected{
							Path: action.TreePath,
						}
					}
				}
			}
		}
	}

	message := strings.TrimSpace(opts.Message)

	author, committer := GetAuthorAndCommitterUsers(opts.Author, opts.Committer, doer)

	t, err := NewTemporaryUploadRepository(repo)
	if err != nil {
		log.Error("%v", err)
	}
	defer t.Close()
	if err := t.Clone(opts.OldBranch); err != nil {
		return nil, err
	}
	if err := t.SetDefaultIndex(); err != nil {
		return nil, err
	}

	// Get the commit of the original branch
	commit, err := t.GetBranchCommit(opts.OldBranch)
	if err != nil {
		return nil, err // Couldn't get a commit for the branch
	}

	// Assigned LastCommitID in opts if it hasn't been set
	if opts.LastCommitID == "" {
		opts.LastCommitID = commit.ID.String()
	} else {
		lastCommitID, err := t.gitRepo.ConvertToSHA1(opts.LastCommitID)
		if err != nil {
			return nil, fmt.Errorf("DeleteRepoFile: Invalid last commit ID: %v", err)
		}
		opts.LastCommitID = lastCommitID.String()

	}

	type LFSMetaWithAction struct {
		action *CreateCommitAction
		lfs    *models.LFSMetaObject
	}
	lfsMetaWithActionObjects := make([]*LFSMetaWithAction, 0)

	// If FromTreePath is not set, set it to the opts.TreePath
	for _, action := range opts.Actions {
		if action.TreePath != "" && action.FromTreePath == "" {
			action.FromTreePath = action.TreePath
		}

		// Check that the path given in opts.treePath is valid (not a git path)
		treePath := CleanUploadFileName(action.TreePath)
		if treePath == "" {
			return nil, models.ErrFilenameInvalid{
				Path: action.TreePath,
			}
		}
		// If there is a fromTreePath (we are copying it), also clean it up
		fromTreePath := CleanUploadFileName(action.FromTreePath)
		if fromTreePath == "" && action.FromTreePath != "" {
			return nil, models.ErrFilenameInvalid{
				Path: action.FromTreePath,
			}
		}

		encoding := "UTF-8"
		bom := false
		executable := false

		if !action.IsNewFile {
			fromEntry, err := commit.GetTreeEntryByPath(fromTreePath)
			if err != nil {
				return nil, err
			}
			if action.SHA != "" {
				// If a SHA was given and the SHA given doesn't match the SHA of the fromTreePath, throw error
				if action.SHA != fromEntry.ID.String() {
					return nil, models.ErrSHADoesNotMatch{
						Path:       treePath,
						GivenSHA:   action.SHA,
						CurrentSHA: fromEntry.ID.String(),
					}
				}
			} else if opts.LastCommitID != "" {
				// If a lastCommitID was given and it doesn't match the commitID of the head of the branch throw
				// an error, but only if we aren't creating a new branch.
				if commit.ID.String() != opts.LastCommitID && opts.OldBranch == opts.NewBranch {
					if changed, err := commit.FileChangedSinceCommit(treePath, opts.LastCommitID); err != nil {
						return nil, err
					} else if changed {
						return nil, models.ErrCommitIDDoesNotMatch{
							GivenCommitID:   opts.LastCommitID,
							CurrentCommitID: opts.LastCommitID,
						}
					}
					// The file wasn't modified, so we are good to delete it
				}
			} else {
				// When updating a file, a lastCommitID or SHA needs to be given to make sure other commits
				// haven't been made. We throw an error if one wasn't provided.
				return nil, models.ErrSHAOrCommitIDNotProvided{}
			}
			encoding, bom = detectEncodingAndBOM(fromEntry, repo)
			executable = fromEntry.IsExecutable()
		}

		// For the path where this file will be created/updated, we need to make
		// sure no parts of the path are existing files or links except for the last
		// item in the path which is the file name, and that shouldn't exist IF it is
		// a new file OR is being moved to a new path.
		treePathParts := strings.Split(treePath, "/")
		subTreePath := ""
		for index, part := range treePathParts {
			subTreePath = path.Join(subTreePath, part)
			entry, err := commit.GetTreeEntryByPath(subTreePath)
			if err != nil {
				if git.IsErrNotExist(err) {
					// Means there is no item with that name, so we're good
					break
				}
				return nil, err
			}
			if index < len(treePathParts)-1 {
				if !entry.IsDir() {
					return nil, models.ErrFilePathInvalid{
						Message: fmt.Sprintf("a file exists where you’re trying to create a subdirectory [path: %s]", subTreePath),
						Path:    subTreePath,
						Name:    part,
						Type:    git.EntryModeBlob,
					}
				}
			} else if entry.IsLink() {
				return nil, models.ErrFilePathInvalid{
					Message: fmt.Sprintf("a symbolic link exists where you’re trying to create a subdirectory [path: %s]", subTreePath),
					Path:    subTreePath,
					Name:    part,
					Type:    git.EntryModeSymlink,
				}
			} else if entry.IsDir() {
				return nil, models.ErrFilePathInvalid{
					Message: fmt.Sprintf("a directory exists where you’re trying to create a file [path: %s]", subTreePath),
					Path:    subTreePath,
					Name:    part,
					Type:    git.EntryModeTree,
				}
			} else if fromTreePath != treePath || action.IsNewFile {
				// The entry shouldn't exist if we are creating new file or moving to a new path
				return nil, models.ErrRepoFileAlreadyExists{
					Path: treePath,
				}
			}

		}

		// Get the two paths (might be the same if not moving) from the index if they exist
		filesInIndex, err := t.LsFiles(action.TreePath, action.FromTreePath)
		if err != nil {
			return nil, fmt.Errorf("UpdateRepoFile: %v", err)
		}
		// If is a new file (not updating) then the given path shouldn't exist
		if action.IsNewFile {
			for _, file := range filesInIndex {
				if file == action.TreePath {
					return nil, models.ErrRepoFileAlreadyExists{
						Path: action.TreePath,
					}
				}
			}
		}

		// Remove the old path from the tree
		if fromTreePath != treePath && len(filesInIndex) > 0 {
			for _, file := range filesInIndex {
				if file == fromTreePath {
					if err := t.RemoveFilesFromIndex(action.FromTreePath); err != nil {
						return nil, err
					}
				}
			}
		}

		content := action.Content
		if bom {
			content = string(charset.UTF8BOM) + content
		}
		if encoding != "UTF-8" {
			charsetEncoding, _ := stdcharset.Lookup(encoding)
			if charsetEncoding != nil {
				result, _, err := transform.String(charsetEncoding.NewEncoder(), content)
				if err != nil {
					// Look if we can't encode back in to the original we should just stick with utf-8
					log.Error("Error re-encoding %s (%s) as %s - will stay as UTF-8: %v", action.TreePath, action.FromTreePath, encoding, err)
					result = content
				}
				content = result
			} else {
				log.Error("Unknown encoding: %s", encoding)
			}
		}
		// Reset the opts.Content to our adjusted content to ensure that LFS gets the correct content
		action.Content = content

		if setting.LFS.StartServer {
			// Check there is no way this can return multiple infos
			filename2attribute2info, err := t.CheckAttribute("filter", treePath)
			if err != nil {
				return nil, err
			}

			if filename2attribute2info[treePath] != nil && filename2attribute2info[treePath]["filter"] == "lfs" {
				// OK so we are supposed to LFS this data!
				oid, err := models.GenerateLFSOid(strings.NewReader(action.Content))
				if err != nil {
					return nil, err
				}
				lfsMetaObject := &models.LFSMetaObject{Oid: oid, Size: int64(len(action.Content)), RepositoryID: repo.ID}
				content = lfsMetaObject.Pointer()
				lfsMetaWithActionObjects = append(lfsMetaWithActionObjects, &LFSMetaWithAction{
					action: action,
					lfs:    lfsMetaObject,
				})
			}
		}
		// Add the object to the database
		objectHash, err := t.HashObject(strings.NewReader(content))
		if err != nil {
			return nil, err
		}

		// Add the object to the index
		if executable {
			if err := t.AddObjectToIndex("100755", objectHash, treePath); err != nil {
				return nil, err
			}
		} else {
			if err := t.AddObjectToIndex("100644", objectHash, treePath); err != nil {
				return nil, err
			}
		}
	}

	// Now write the tree
	treeHash, err := t.WriteTree()
	if err != nil {
		return nil, err
	}

	// Now commit the tree
	var commitHash string
	if opts.Dates != nil {
		commitHash, err = t.CommitTreeWithDate(author, committer, treeHash, message, opts.Dates.Author, opts.Dates.Committer)
	} else {
		commitHash, err = t.CommitTree(author, committer, treeHash, message)
	}
	if err != nil {
		return nil, err
	}

	if len(lfsMetaWithActionObjects) > 0 {
		// We have an LFS object - create it
		for _, lfsMetaWithAction := range lfsMetaWithActionObjects {
			lfsMetaObject := lfsMetaWithAction.lfs
			action := lfsMetaWithAction.action

			lfsMetaObject, err = models.NewLFSMetaObject(lfsMetaObject)
			if err != nil {
				return nil, err
			}
			contentStore := &lfs.ContentStore{BasePath: setting.LFS.ContentPath}
			if !contentStore.Exists(lfsMetaObject) {
				if err := contentStore.Put(lfsMetaObject, strings.NewReader(action.Content)); err != nil {
					if _, err2 := repo.RemoveLFSMetaObjectByOid(lfsMetaObject.Oid); err2 != nil {
						return nil, fmt.Errorf("Error whilst removing failed inserted LFS object %s: %v (Prev Error: %v)", lfsMetaObject.Oid, err2, err)
					}
					return nil, err
				}
			}
		}
	}

	// Then push this tree to NewBranch
	if err := t.Push(doer, commitHash, opts.NewBranch); err != nil {
		log.Error("%T %v", err, err)
		return nil, err
	}

	commit, err = t.GetCommit(commitHash)
	if err != nil {
		return nil, err
	}

	return commit, nil
}
