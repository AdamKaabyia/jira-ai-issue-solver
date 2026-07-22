package services

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"jira-ai-issue-solver/models"
)

// GitLabServiceImpl is the concrete implementation for GitLab operations.
// It satisfies the same consumer-defined interfaces as GitHubServiceImpl,
// enabling per-project hosting selection via the hosting router.
type GitLabServiceImpl struct {
	config   *models.Config
	client   *http.Client
	executor models.CommandExecutor
	logger   *zap.Logger
	gitOps   *GitOps
}

// NewGitLabService creates a new GitLabServiceImpl.
func NewGitLabService(config *models.Config, logger *zap.Logger, executor ...models.CommandExecutor) *GitLabServiceImpl {
	commandExecutor := exec.Command
	if len(executor) > 0 {
		commandExecutor = executor[0]
	}

	return &GitLabServiceImpl{
		config:   config,
		client:   &http.Client{Timeout: 30 * time.Second},
		executor: commandExecutor,
		logger:   logger,
		gitOps:   NewGitOps(commandExecutor, logger),
	}
}

// --- Git operations (delegated to shared GitOps) ---

func (s *GitLabServiceImpl) CreateBranch(directory, branchName, baseBranch string) error {
	return s.gitOps.CreateBranch(directory, branchName, baseBranch)
}

func (s *GitLabServiceImpl) SwitchBranch(directory, branchName string) error {
	return s.gitOps.SwitchBranch(directory, branchName)
}

func (s *GitLabServiceImpl) HasChanges(directory, baseBranch string) (bool, error) {
	return s.gitOps.HasChanges(directory, baseBranch)
}

func (s *GitLabServiceImpl) StripRemoteAuth(directory string) error {
	return s.gitOps.StripRemoteAuth(directory)
}

func (s *GitLabServiceImpl) FetchRemote(directory string) error {
	return s.gitOps.FetchRemote(directory)
}

func (s *GitLabServiceImpl) SyncWithRemote(directory, branch string, importExcludes []string) error {
	return s.gitOps.SyncWithRemote(directory, branch, importExcludes)
}

func (s *GitLabServiceImpl) MergeBase(dir, branch, fetchURL string) ([]string, error) {
	return s.gitOps.MergeBase(dir, branch, fetchURL)
}

func (s *GitLabServiceImpl) CloneImport(url, destDir, ref string) error {
	return s.gitOps.CloneImport(url, destDir, ref)
}

// --- Provider-specific operations ---

func (s *GitLabServiceImpl) RestoreRemoteAuth(directory, owner, repo string) error {
	token := s.config.GitLab.AccessToken
	baseURL := strings.TrimSuffix(s.config.GitLab.BaseURL, "/")
	authURL := fmt.Sprintf("%s/oauth2:%s@%s/%s/%s.git",
		strings.Replace(baseURL, "https://", "https://", 1),
		token,
		strings.TrimPrefix(strings.TrimPrefix(baseURL, "https://"), "http://"),
		owner, repo)

	return s.gitOps.SetRemoteURL(directory, authURL)
}

func (s *GitLabServiceImpl) SyncFork(forkOwner, repo, branch string) error {
	// GitLab fork sync: PUT /projects/:id/fork (no direct equivalent to
	// GitHub's merge-upstream). For now, this is a no-op — the clone
	// fetches from upstream before branching which achieves the same.
	s.logger.Debug("SyncFork is a no-op for GitLab (handled by fetch)",
		zap.String("forkOwner", forkOwner),
		zap.String("repo", repo),
		zap.String("branch", branch))
	return nil
}

func (s *GitLabServiceImpl) RemoteBranchExists(owner, repo, branch string) (bool, error) {
	projectID := s.projectPath(owner, repo)
	url := fmt.Sprintf("%s/api/v4/projects/%s/repository/branches/%s",
		s.config.GitLab.BaseURL, projectID, branch)

	resp, err := s.doRequest("GET", url, nil)
	if err != nil {
		return false, fmt.Errorf("check branch existence: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("unexpected status %d checking branch %s", resp.StatusCode, branch)
	}
}

func (s *GitLabServiceImpl) DeleteRemoteBranch(owner, repo, branch string) error {
	projectID := s.projectPath(owner, repo)
	url := fmt.Sprintf("%s/api/v4/projects/%s/repository/branches/%s",
		s.config.GitLab.BaseURL, projectID, branch)

	resp, err := s.doRequest("DELETE", url, nil)
	if err != nil {
		return fmt.Errorf("delete branch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete branch %s: status %d, body: %s", branch, resp.StatusCode, string(body))
	}
	return nil
}

// CommitChanges for GitLab: stage, commit locally, and git push. Returns
// the commit SHA from the push. The upstreamOwner parameter is ignored
// (only relevant for GitHub's Git Data API fork resolution).
func (s *GitLabServiceImpl) CommitChanges(upstreamOwner, owner, repo, branch, message, dir, baseBranch string,
	coAuthor *models.Author, importExcludes []string, skipFileGuardrail ...bool) (string, error) {

	fn := zap.String("function", "CommitChanges")

	// Stage and commit locally.
	if err := s.gitOps.StageAndCommitLocal(dir, fn); err != nil {
		return "", err
	}

	// Check if there are commits to push.
	hasChanges, err := s.gitOps.HasChanges(dir, baseBranch)
	if err != nil {
		return "", fmt.Errorf("check changes: %w", err)
	}
	if !hasChanges {
		return "", ErrNoChanges
	}

	// Amend the last commit with the proper message and co-author.
	commitMsg := message
	if coAuthor != nil && coAuthor.Name != "" && coAuthor.Email != "" {
		commitMsg += fmt.Sprintf("\n\nCo-authored-by: %s <%s>", coAuthor.Name, coAuthor.Email)
	}

	debugEnabled := s.logger.Core().Enabled(zapcore.DebugLevel)

	amendCmd := newGitCommand(
		s.executor("git", "commit", "--amend", "-m", commitMsg),
		dir, debugEnabled, true)
	if err := amendCmd.run(); err != nil {
		return "", fmt.Errorf("git commit --amend: %w, stderr: %s", err, amendCmd.getStderr())
	}

	// Restore auth for push.
	if err := s.RestoreRemoteAuth(dir, owner, repo); err != nil {
		return "", fmt.Errorf("restore auth for push: %w", err)
	}

	// Push to remote (force push since we amend).
	pushCmd := newGitCommand(
		s.executor("git", "push", "--force", "origin", branch),
		dir, debugEnabled, true)
	if err := pushCmd.run(); err != nil {
		return "", fmt.Errorf("git push: %w, stderr: %s", err, pushCmd.getStderr())
	}

	// Get the SHA of HEAD after push.
	shaCmd := newGitCommand(
		s.executor("git", "rev-parse", "HEAD"),
		dir, true, true)
	if err := shaCmd.run(); err != nil {
		return "", fmt.Errorf("get commit SHA: %w", err)
	}

	return strings.TrimSpace(shaCmd.getStdout()), nil
}

func (s *GitLabServiceImpl) CloneRepository(repoURL, directory string) error {
	if err := os.MkdirAll(directory, 0750); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	debugEnabled := s.logger.Core().Enabled(zapcore.DebugLevel)
	fn := zap.String("function", "CloneRepository")

	if _, err := os.Stat(filepath.Join(directory, ".git")); err == nil {
		cmd := newGitCommand(s.executor("git", "fetch", "origin"), directory, debugEnabled, true)
		if err := cmd.run(); err != nil {
			return fmt.Errorf("failed to fetch repository: %w, stderr: %s", err, cmd.getStderr())
		}
		s.logger.Debug("git fetch origin", fn, zap.String("stdout", cmd.getStdout()), zap.String("stderr", cmd.getStderr()))

		cmd = newGitCommand(s.executor("git", "reset", "--hard", "origin/main"), directory, debugEnabled, true)
		if err := cmd.run(); err != nil {
			cmd = newGitCommand(s.executor("git", "reset", "--hard", "origin/master"), directory, debugEnabled, true)
			if err := cmd.run(); err != nil {
				return fmt.Errorf("failed to reset to origin/main or origin/master: %w, stderr: %s", err, cmd.getStderr())
			}
		}

		cmd = newGitCommand(s.executor("git", "clean", "-fdx"), directory, debugEnabled, true)
		if err := cmd.run(); err != nil {
			return fmt.Errorf("failed to clean repository: %w, stderr: %s", err, cmd.getStderr())
		}
	} else {
		// Inject token into clone URL for auth.
		authRepoURL := s.injectAuth(repoURL)
		cmd := newGitCommand(s.executor("git", "clone", authRepoURL, directory), directory, debugEnabled, true)
		if err := cmd.run(); err != nil {
			return fmt.Errorf("failed to clone repository: %w, stderr: %s", err, cmd.getStderr())
		}
		s.logger.Debug("git clone", fn)
	}

	if err := s.gitOps.ConfigureUser(directory, s.config.GitLab.BotUsername, s.config.GitLab.BotEmail); err != nil {
		return err
	}

	if err := s.gitOps.ConfigureSSHSigning(directory, s.config.GitLab.SSHKeyPath); err != nil {
		return err
	}

	// Set remote URL with auth token.
	owner, repo, err := extractGitLabRepoInfo(repoURL)
	if err != nil {
		return fmt.Errorf("failed to extract repo info: %w", err)
	}
	return s.RestoreRemoteAuth(directory, owner, repo)
}

// --- Merge Request operations ---

func (s *GitLabServiceImpl) CreatePR(params models.PRParams) (*models.PR, error) {
	projectID := s.projectPath(params.Owner, params.Repo)
	url := fmt.Sprintf("%s/api/v4/projects/%s/merge_requests", s.config.GitLab.BaseURL, projectID)

	body := map[string]interface{}{
		"source_branch": params.Head,
		"target_branch": params.Base,
		"title":         params.Title,
		"description":   params.Body,
	}
	if len(params.Labels) > 0 {
		body["labels"] = strings.Join(params.Labels, ",")
	}
	if len(params.Assignees) > 0 {
		// GitLab uses user IDs for assignees; for now pass usernames
		// and let the caller handle ID resolution upstream.
		body["assignee_ids"] = []int{}
	}

	resp, err := s.doJSONRequest("POST", url, body)
	if err != nil {
		return nil, fmt.Errorf("create merge request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("create MR failed: status %d, body: %s", resp.StatusCode, string(respBody))
	}

	var mr struct {
		IID    int    `json:"iid"`
		WebURL string `json:"web_url"`
		State  string `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&mr); err != nil {
		return nil, fmt.Errorf("decode MR response: %w", err)
	}

	return &models.PR{
		Number: mr.IID,
		URL:    mr.WebURL,
		State:  mr.State,
	}, nil
}

func (s *GitLabServiceImpl) GetPRForBranch(owner, repo, head string) (*models.PRDetails, error) {
	return s.findMRByBranch(owner, repo, head, "opened")
}

func (s *GitLabServiceImpl) GetClosedPRForBranch(owner, repo, head string) (*models.PRDetails, error) {
	return s.findMRByBranch(owner, repo, head, "closed")
}

func (s *GitLabServiceImpl) GetMergedPRForBranch(owner, repo, head string) (*models.PRDetails, error) {
	return s.findMRByBranch(owner, repo, head, "merged")
}

func (s *GitLabServiceImpl) findMRByBranch(owner, repo, head, state string) (*models.PRDetails, error) {
	projectID := s.projectPath(owner, repo)
	// Strip fork owner prefix if present (e.g., "forkuser:branch" → "branch").
	branch := head
	if idx := strings.Index(head, ":"); idx >= 0 {
		branch = head[idx+1:]
	}

	url := fmt.Sprintf("%s/api/v4/projects/%s/merge_requests?source_branch=%s&state=%s&per_page=1",
		s.config.GitLab.BaseURL, projectID, branch, state)

	resp, err := s.doRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("find MR by branch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("find MR: unexpected status %d", resp.StatusCode)
	}

	var mrs []struct {
		IID          int    `json:"iid"`
		Title        string `json:"title"`
		SourceBranch string `json:"source_branch"`
		TargetBranch string `json:"target_branch"`
		WebURL       string `json:"web_url"`
		SHA          string `json:"sha"`
		CreatedAt    string `json:"created_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&mrs); err != nil {
		return nil, fmt.Errorf("decode MR list: %w", err)
	}

	if len(mrs) == 0 {
		return nil, nil
	}

	mr := mrs[0]
	createdAt, _ := time.Parse(time.RFC3339, mr.CreatedAt)
	return &models.PRDetails{
		Number:     mr.IID,
		Title:      mr.Title,
		Branch:     mr.SourceBranch,
		BaseBranch: mr.TargetBranch,
		URL:        mr.WebURL,
		HeadSHA:    mr.SHA,
		CreatedAt:  createdAt,
	}, nil
}

func (s *GitLabServiceImpl) GetPRComments(owner, repo string, number int, since time.Time) ([]models.PRComment, error) {
	projectID := s.projectPath(owner, repo)
	var allComments []models.PRComment
	page := 1

	for {
		url := fmt.Sprintf("%s/api/v4/projects/%s/merge_requests/%d/notes?per_page=100&page=%d&sort=asc",
			s.config.GitLab.BaseURL, projectID, number, page)

		resp, err := s.doRequest("GET", url, nil)
		if err != nil {
			return nil, fmt.Errorf("get MR notes: %w", err)
		}

		var notes []struct {
			ID        int64  `json:"id"`
			Body      string `json:"body"`
			Author    struct {
				Username string `json:"username"`
			} `json:"author"`
			CreatedAt  string `json:"created_at"`
			System     bool   `json:"system"`
			Resolvable bool   `json:"resolvable"`
			Position   *struct {
				NewPath string `json:"new_path"`
				NewLine int    `json:"new_line"`
			} `json:"position"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&notes); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("decode MR notes: %w", err)
		}
		resp.Body.Close()

		for _, note := range notes {
			if note.System {
				continue
			}
			ts, _ := time.Parse(time.RFC3339, note.CreatedAt)
			if !since.IsZero() && ts.Before(since) {
				continue
			}

			comment := models.PRComment{
				ID:     note.ID,
				Author: models.Author{Name: note.Author.Username},
				Body:   note.Body,
				URL: fmt.Sprintf("%s/api/v4/projects/%s/merge_requests/%d#note_%d",
					s.config.GitLab.BaseURL, projectID, number, note.ID),
				Timestamp:       ts,
				IsReviewComment: note.Resolvable,
			}
			if note.Position != nil {
				comment.FilePath = note.Position.NewPath
				comment.Line = note.Position.NewLine
			}
			allComments = append(allComments, comment)
		}

		if len(notes) < 100 {
			break
		}
		page++
	}

	if allComments == nil {
		allComments = []models.PRComment{}
	}
	return allComments, nil
}

func (s *GitLabServiceImpl) ReplyToComment(owner, repo string, prNumber int, commentID int64, body string) error {
	projectID := s.projectPath(owner, repo)

	// Find the discussion containing this note to reply in thread.
	discussionID, err := s.findDiscussionForNote(projectID, prNumber, commentID)
	if err != nil || discussionID == "" {
		// Fall back to posting a new note referencing the original.
		return s.PostIssueComment(owner, repo, prNumber, body)
	}

	url := fmt.Sprintf("%s/api/v4/projects/%s/merge_requests/%d/discussions/%s/notes",
		s.config.GitLab.BaseURL, projectID, prNumber, discussionID)

	reqBody := map[string]interface{}{"body": body}
	resp, err := s.doJSONRequest("POST", url, reqBody)
	if err != nil {
		return fmt.Errorf("reply to comment: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("reply to comment: status %d, body: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

func (s *GitLabServiceImpl) PostIssueComment(owner, repo string, prNumber int, body string) error {
	projectID := s.projectPath(owner, repo)
	url := fmt.Sprintf("%s/api/v4/projects/%s/merge_requests/%d/notes",
		s.config.GitLab.BaseURL, projectID, prNumber)

	reqBody := map[string]interface{}{"body": body}
	resp, err := s.doJSONRequest("POST", url, reqBody)
	if err != nil {
		return fmt.Errorf("post MR note: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("post MR note: status %d, body: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

func (s *GitLabServiceImpl) ListIssueComments(owner, repo string, prNumber int) ([]models.IssueComment, error) {
	projectID := s.projectPath(owner, repo)
	url := fmt.Sprintf("%s/api/v4/projects/%s/merge_requests/%d/notes?per_page=100&sort=asc",
		s.config.GitLab.BaseURL, projectID, prNumber)

	resp, err := s.doRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("list MR notes: %w", err)
	}
	defer resp.Body.Close()

	var notes []struct {
		ID     int64  `json:"id"`
		Body   string `json:"body"`
		Author struct {
			Username string `json:"username"`
		} `json:"author"`
		System bool `json:"system"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&notes); err != nil {
		return nil, fmt.Errorf("decode MR notes: %w", err)
	}

	var comments []models.IssueComment
	for _, note := range notes {
		if note.System {
			continue
		}
		comments = append(comments, models.IssueComment{
			ID:   note.ID,
			Body: note.Body,
		})
	}
	if comments == nil {
		comments = []models.IssueComment{}
	}
	return comments, nil
}

func (s *GitLabServiceImpl) UpdateIssueComment(owner, repo string, commentID int64, body string) error {
	projectID := s.projectPath(owner, repo)
	// GitLab's note update requires the MR IID, which we don't have here.
	// Use the notes API which accepts note ID at project level.
	url := fmt.Sprintf("%s/api/v4/projects/%s/notes/%d",
		s.config.GitLab.BaseURL, projectID, commentID)

	reqBody := map[string]interface{}{"body": body}
	resp, err := s.doJSONRequest("PUT", url, reqBody)
	if err != nil {
		return fmt.Errorf("update note: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("update note: status %d, body: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

func (s *GitLabServiceImpl) AddCommentReaction(owner, repo string, comment models.PRComment, reaction string) error {
	projectID := s.projectPath(owner, repo)
	// GitLab award emoji API for MR notes.
	url := fmt.Sprintf("%s/api/v4/projects/%s/merge_requests/%d/notes/%d/award_emoji",
		s.config.GitLab.BaseURL, projectID, 0, comment.ID)
	// We don't have the MR IID from the comment alone; best-effort skip.
	s.logger.Debug("AddCommentReaction skipped (MR IID not available from comment)",
		zap.String("owner", owner), zap.String("repo", repo))
	_ = url
	return nil
}

// --- Label operations ---

func (s *GitLabServiceImpl) AddPRLabel(owner, repo string, number int, label string) error {
	return s.updateMRLabels(owner, repo, number, label, true)
}

func (s *GitLabServiceImpl) RemovePRLabel(owner, repo string, number int, label string) error {
	return s.updateMRLabels(owner, repo, number, label, false)
}

func (s *GitLabServiceImpl) HasPRLabel(owner, repo string, number int, label string) (bool, error) {
	projectID := s.projectPath(owner, repo)
	url := fmt.Sprintf("%s/api/v4/projects/%s/merge_requests/%d",
		s.config.GitLab.BaseURL, projectID, number)

	resp, err := s.doRequest("GET", url, nil)
	if err != nil {
		return false, fmt.Errorf("get MR: %w", err)
	}
	defer resp.Body.Close()

	var mr struct {
		Labels []string `json:"labels"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&mr); err != nil {
		return false, fmt.Errorf("decode MR: %w", err)
	}

	for _, l := range mr.Labels {
		if l == label {
			return true, nil
		}
	}
	return false, nil
}

func (s *GitLabServiceImpl) LastLabelRemoval(owner, repo string, number int, label string) (time.Time, error) {
	// GitLab doesn't have a direct "label events" API equivalent to
	// GitHub's timeline events. Return zero time (never removed).
	return time.Time{}, nil
}

// --- CI operations ---

func (s *GitLabServiceImpl) ListCheckRunsForRef(owner, repo, ref string) ([]models.CheckRunFailure, bool, error) {
	projectID := s.projectPath(owner, repo)
	url := fmt.Sprintf("%s/api/v4/projects/%s/pipelines?sha=%s&per_page=1",
		s.config.GitLab.BaseURL, projectID, ref)

	resp, err := s.doRequest("GET", url, nil)
	if err != nil {
		return nil, false, fmt.Errorf("list pipelines: %w", err)
	}
	defer resp.Body.Close()

	var pipelines []struct {
		ID     int    `json:"id"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pipelines); err != nil {
		return nil, false, fmt.Errorf("decode pipelines: %w", err)
	}

	if len(pipelines) == 0 {
		return []models.CheckRunFailure{}, true, nil
	}

	pipeline := pipelines[0]
	allComplete := pipeline.Status != "running" && pipeline.Status != "pending" && pipeline.Status != "created"

	if pipeline.Status == "success" {
		return []models.CheckRunFailure{}, allComplete, nil
	}

	// Get failed jobs from the pipeline.
	jobsURL := fmt.Sprintf("%s/api/v4/projects/%s/pipelines/%d/jobs?scope[]=failed&per_page=100",
		s.config.GitLab.BaseURL, projectID, pipeline.ID)

	jobsResp, err := s.doRequest("GET", jobsURL, nil)
	if err != nil {
		return nil, allComplete, fmt.Errorf("list pipeline jobs: %w", err)
	}
	defer jobsResp.Body.Close()

	var jobs []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(jobsResp.Body).Decode(&jobs); err != nil {
		return nil, allComplete, fmt.Errorf("decode pipeline jobs: %w", err)
	}

	var failures []models.CheckRunFailure
	for _, job := range jobs {
		failures = append(failures, models.CheckRunFailure{
			ID:   int64(job.ID),
			Name: job.Name,
		})
	}
	if failures == nil {
		failures = []models.CheckRunFailure{}
	}
	return failures, allComplete, nil
}

func (s *GitLabServiceImpl) ListCheckRunAnnotations(owner, repo string, checkRunID int64) ([]models.CheckAnnotation, error) {
	// GitLab doesn't have an equivalent to GitHub check run annotations.
	return []models.CheckAnnotation{}, nil
}

func (s *GitLabServiceImpl) GetFailedJobLogs(owner, repo, headSHA string, maxBytesPerStep int) (map[string][]models.FailedStep, error) {
	projectID := s.projectPath(owner, repo)

	// Find pipeline for SHA.
	url := fmt.Sprintf("%s/api/v4/projects/%s/pipelines?sha=%s&per_page=1",
		s.config.GitLab.BaseURL, projectID, headSHA)

	resp, err := s.doRequest("GET", url, nil)
	if err != nil {
		return map[string][]models.FailedStep{}, fmt.Errorf("list pipelines: %w", err)
	}
	defer resp.Body.Close()

	var pipelines []struct {
		ID int `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pipelines); err != nil {
		return map[string][]models.FailedStep{}, nil
	}

	if len(pipelines) == 0 {
		return map[string][]models.FailedStep{}, nil
	}

	// Get failed jobs.
	jobsURL := fmt.Sprintf("%s/api/v4/projects/%s/pipelines/%d/jobs?scope[]=failed&per_page=100",
		s.config.GitLab.BaseURL, projectID, pipelines[0].ID)

	jobsResp, err := s.doRequest("GET", jobsURL, nil)
	if err != nil {
		return map[string][]models.FailedStep{}, nil
	}
	defer jobsResp.Body.Close()

	var jobs []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(jobsResp.Body).Decode(&jobs); err != nil {
		return map[string][]models.FailedStep{}, nil
	}

	result := make(map[string][]models.FailedStep)
	for _, job := range jobs {
		logURL := fmt.Sprintf("%s/api/v4/projects/%s/jobs/%d/trace",
			s.config.GitLab.BaseURL, projectID, job.ID)

		logResp, err := s.doRequest("GET", logURL, nil)
		if err != nil {
			continue
		}
		logBytes, _ := io.ReadAll(io.LimitReader(logResp.Body, int64(maxBytesPerStep)))
		logResp.Body.Close()

		result[job.Name] = []models.FailedStep{{
			JobName:  job.Name,
			StepName: job.Name,
			Log:      string(logBytes),
		}}
	}
	return result, nil
}

// --- Recovery interface ---

func (s *GitLabServiceImpl) BranchHasCommits(owner, repo, branch, base string) (bool, error) {
	projectID := s.projectPath(owner, repo)
	url := fmt.Sprintf("%s/api/v4/projects/%s/repository/compare?from=%s&to=%s",
		s.config.GitLab.BaseURL, projectID, base, branch)

	resp, err := s.doRequest("GET", url, nil)
	if err != nil {
		return false, fmt.Errorf("compare branches: %w", err)
	}
	defer resp.Body.Close()

	var comparison struct {
		Commits []struct{} `json:"commits"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&comparison); err != nil {
		return false, fmt.Errorf("decode comparison: %w", err)
	}

	return len(comparison.Commits) > 0, nil
}

// --- Mergeability ---

func (s *GitLabServiceImpl) GetPRMergeability(owner, repo string, number int) (*models.PRMergeState, error) {
	projectID := s.projectPath(owner, repo)
	url := fmt.Sprintf("%s/api/v4/projects/%s/merge_requests/%d",
		s.config.GitLab.BaseURL, projectID, number)

	resp, err := s.doRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("get MR mergeability: %w", err)
	}
	defer resp.Body.Close()

	var mr struct {
		MergeStatus  string `json:"merge_status"`
		TargetBranch string `json:"target_branch"`
		HasConflicts bool   `json:"has_conflicts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&mr); err != nil {
		return nil, fmt.Errorf("decode MR: %w", err)
	}

	mergeable := !mr.HasConflicts && mr.MergeStatus == "can_be_merged"
	return &models.PRMergeState{
		Mergeable:  &mergeable,
		BaseBranch: mr.TargetBranch,
	}, nil
}

// --- Internal helpers ---

// projectPath returns the URL-encoded project path for GitLab API calls.
func (s *GitLabServiceImpl) projectPath(owner, repo string) string {
	return strings.ReplaceAll(owner+"/"+repo, "/", "%2F")
}

// injectAuth injects the PAT into a clone URL for authenticated operations.
func (s *GitLabServiceImpl) injectAuth(repoURL string) string {
	token := s.config.GitLab.AccessToken
	baseURL := s.config.GitLab.BaseURL

	// Transform https://gitlab.com/org/repo.git →
	//           https://oauth2:token@gitlab.com/org/repo.git
	host := strings.TrimPrefix(strings.TrimPrefix(baseURL, "https://"), "http://")
	if strings.Contains(repoURL, host) {
		return strings.Replace(repoURL, host, fmt.Sprintf("oauth2:%s@%s", token, host), 1)
	}
	return repoURL
}

func (s *GitLabServiceImpl) doRequest(method, url string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("PRIVATE-TOKEN", s.config.GitLab.AccessToken)
	return s.client.Do(req)
}

func (s *GitLabServiceImpl) doJSONRequest(method, url string, body interface{}) (*http.Response, error) {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request body: %w", err)
	}
	req, err := http.NewRequest(method, url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("PRIVATE-TOKEN", s.config.GitLab.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	return s.client.Do(req)
}

func (s *GitLabServiceImpl) updateMRLabels(owner, repo string, number int, label string, add bool) error {
	projectID := s.projectPath(owner, repo)

	// First get current labels.
	getURL := fmt.Sprintf("%s/api/v4/projects/%s/merge_requests/%d",
		s.config.GitLab.BaseURL, projectID, number)

	resp, err := s.doRequest("GET", getURL, nil)
	if err != nil {
		return fmt.Errorf("get MR labels: %w", err)
	}
	defer resp.Body.Close()

	var mr struct {
		Labels []string `json:"labels"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&mr); err != nil {
		return fmt.Errorf("decode MR: %w", err)
	}

	var newLabels []string
	if add {
		for _, l := range mr.Labels {
			if l == label {
				return nil
			}
		}
		newLabels = append(mr.Labels, label)
	} else {
		for _, l := range mr.Labels {
			if l != label {
				newLabels = append(newLabels, l)
			}
		}
		if len(newLabels) == len(mr.Labels) {
			return nil
		}
	}

	// Update labels.
	putURL := fmt.Sprintf("%s/api/v4/projects/%s/merge_requests/%d",
		s.config.GitLab.BaseURL, projectID, number)

	reqBody := map[string]interface{}{
		"labels": strings.Join(newLabels, ","),
	}
	putResp, err := s.doJSONRequest("PUT", putURL, reqBody)
	if err != nil {
		return fmt.Errorf("update MR labels: %w", err)
	}
	defer putResp.Body.Close()

	if putResp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(putResp.Body)
		return fmt.Errorf("update MR labels: status %d, body: %s", putResp.StatusCode, string(respBody))
	}
	return nil
}

func (s *GitLabServiceImpl) findDiscussionForNote(projectID string, mrIID int, noteID int64) (string, error) {
	url := fmt.Sprintf("%s/api/v4/projects/%s/merge_requests/%d/discussions?per_page=100",
		s.config.GitLab.BaseURL, projectID, mrIID)

	resp, err := s.doRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var discussions []struct {
		ID    string `json:"id"`
		Notes []struct {
			ID int64 `json:"id"`
		} `json:"notes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&discussions); err != nil {
		return "", err
	}

	for _, d := range discussions {
		for _, n := range d.Notes {
			if n.ID == noteID {
				return d.ID, nil
			}
		}
	}
	return "", nil
}

// extractGitLabRepoInfo extracts the owner/group and repo name from a GitLab URL.
func extractGitLabRepoInfo(repoURL string) (string, string, error) {
	repoURL = strings.TrimSuffix(repoURL, ".git")
	// Handle https://gitlab.com/org/subgroup/repo format.
	parts := strings.Split(repoURL, "/")
	if len(parts) < 5 {
		return "", "", fmt.Errorf("cannot extract owner/repo from GitLab URL: %s", repoURL)
	}
	repo := parts[len(parts)-1]
	// Owner is everything between the host and the repo name.
	hostIdx := 3 // after https://host/
	owner := strings.Join(parts[hostIdx:len(parts)-1], "/")
	return owner, repo, nil
}

