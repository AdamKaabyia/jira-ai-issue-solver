package executor

import (
	"errors"
	"fmt"

	"go.uber.org/zap"

	"jira-ai-issue-solver/models"
)

// setPipelineLabel applies the given label to a ticket and removes all
// other configured pipeline labels (both failure and lifecycle groups).
// This enforces mutual exclusivity across both groups — a ticket should
// have exactly one pipeline label at any time. No-op when targetLabel
// is empty (not configured). All operations are best-effort: errors are
// logged but never propagated.
func (p *Pipeline) setPipelineLabel(
	logger *zap.Logger,
	ticketKey string,
	allLabels []string,
	targetLabel string,
) {
	if targetLabel == "" {
		return
	}
	for _, label := range allLabels {
		if label != "" && label != targetLabel {
			if err := p.tracker.RemoveLabel(ticketKey, label); err != nil {
				logger.Debug("Failed to remove pipeline label",
					zap.String("label", label), zap.Error(err))
			}
		}
	}

	if targetLabel != "" {
		if err := p.tracker.AddLabel(ticketKey, targetLabel); err != nil {
			logger.Warn("Failed to add pipeline label",
				zap.String("label", targetLabel), zap.Error(err))
		}
	}
}

// validateForkMode checks that fork-mode projects have a resolved
// GitHub username for the ticket assignee. Returns an error and
// applies the ForkUserMissing label when the mapping is missing.
// No-op for direct-mode projects.
func (p *Pipeline) validateForkMode(
	logger *zap.Logger,
	ticketKey string,
	workItem *models.WorkItem,
	settings *models.ProjectSettings,
) error {
	if !settings.ForkMode || settings.GitHubUsername != "" {
		return nil
	}

	assigneeDesc := "unassigned"
	if workItem.Assignee != nil {
		assigneeDesc = workItem.Assignee.Email
	}

	if settings.FailureLabels.ForkUserMissing != "" {
		allLabels := models.AllPipelineLabels(settings.FailureLabels, settings.LifecycleLabels)
		p.setPipelineLabel(logger, ticketKey, allLabels, settings.FailureLabels.ForkUserMissing)
	}

	if !settings.DisableErrorComments {
		detail := fmt.Sprintf(
			"fork mode requires assignee GitHub mapping: ticket assignee %s has no entry in jira.assignee_to_github_username",
			assigneeDesc,
		)
		comment := fmt.Sprintf(
			"%s Fork mode validation failed\n\nError: %s\n\nAdd the assignee's GitHub username to jira.assignee_to_github_username in the bot configuration.",
			statusCommentMarker, detail,
		)
		if err := p.tracker.AddComment(ticketKey, comment); err != nil {
			logger.Warn("Failed to post fork-mode error comment", zap.Error(err))
		}
	}

	return errors.New("fork mode requires assignee GitHub mapping: ticket assignee has no entry in jira.assignee_to_github_username")
}

// clearFailureLabels removes executor-managed failure labels from a
// ticket. Called on pipeline success paths to clean up labels from
// prior failed attempts. Only clears labels the executor owns
// (blocked, fork_user_missing); scanner-managed labels (ci_failing,
// rejected) are left for the feedback scanner to reconcile based on
// observed PR/CI state. All operations are best-effort.
func (p *Pipeline) clearFailureLabels(
	logger *zap.Logger,
	ticketKey string,
	fl models.FailureLabels,
) {
	for _, label := range fl.ExecutorOwned() {
		if label != "" {
			if err := p.tracker.RemoveLabel(ticketKey, label); err != nil {
				logger.Debug("Failed to remove failure label",
					zap.String("label", label), zap.Error(err))
			}
		}
	}
}

// setPRValidationLabel applies the given validation label to a GitHub
// PR and removes the other configured validation labels (mutual
// exclusivity). If targetLabel is empty, only clears the others. All
// operations are best-effort: errors are logged but never propagated.
func (p *Pipeline) setPRValidationLabel(
	logger *zap.Logger,
	owner, repo string,
	prNumber int,
	vl models.PRValidationLabels,
	targetLabel string,
) {
	for _, label := range vl.All() {
		if label != "" && label != targetLabel {
			if err := p.git.RemovePRLabel(owner, repo, prNumber, label); err != nil {
				logger.Debug("Failed to remove PR validation label",
					zap.String("owner", owner), zap.String("repo", repo),
					zap.Int("pr", prNumber), zap.String("label", label),
					zap.Error(err))
			}
		}
	}

	if targetLabel != "" {
		if err := p.git.AddPRLabel(owner, repo, prNumber, targetLabel); err != nil {
			logger.Warn("Failed to add PR validation label",
				zap.String("owner", owner), zap.String("repo", repo),
				zap.Int("pr", prNumber), zap.String("label", targetLabel),
				zap.Error(err))
		}
	}
}

// clearPRValidationLabels removes all configured validation labels
// from a GitHub PR. Called when validation passes after a prior
// failure. All operations are best-effort.
func (p *Pipeline) clearPRValidationLabels(
	logger *zap.Logger,
	owner, repo string,
	prNumber int,
	vl models.PRValidationLabels,
) {
	for _, label := range vl.All() {
		if label != "" {
			if err := p.git.RemovePRLabel(owner, repo, prNumber, label); err != nil {
				logger.Debug("Failed to remove PR validation label",
					zap.String("owner", owner), zap.String("repo", repo),
					zap.Int("pr", prNumber), zap.String("label", label),
					zap.Error(err))
			}
		}
	}
}

// applyCostCapPRLabel applies or removes the cost-cap-exceeded label
// on all open PRs for the given ticket across all configured repos.
// When exceeded is true, the label is set and other validation labels
// are removed (mutual exclusivity). When exceeded is false, only the
// cost cap label is removed. All operations are best-effort.
func (p *Pipeline) applyCostCapPRLabel(
	logger *zap.Logger,
	ticketKey string,
	settings *models.ProjectSettings,
	exceeded bool,
) {
	label := settings.PRValidationLabels.CostCapLabel()
	if label == "" {
		return
	}

	branchName := fmt.Sprintf("%s/%s", p.cfg.BotUsername, ticketKey)
	heads := settings.PRHeads(branchName)

	for _, repo := range settings.Repos {
		pr, err := p.findPRByHeadsOptional(repo.Owner, repo.Repo, heads)
		if err != nil {
			logger.Debug("Failed to find PR for cost cap label",
				zap.String("repo", repo.Owner+"/"+repo.Repo),
				zap.Error(err))
			continue
		}
		if pr == nil {
			continue
		}

		if exceeded {
			p.setPRValidationLabel(logger, repo.Owner, repo.Repo,
				pr.Number, settings.PRValidationLabels, label)
		} else {
			if err := p.git.RemovePRLabel(repo.Owner, repo.Repo, pr.Number, label); err != nil {
				logger.Debug("Failed to remove cost cap PR label",
					zap.String("repo", repo.Owner+"/"+repo.Repo),
					zap.Int("pr", pr.Number),
					zap.Error(err))
			}
		}
	}
}
