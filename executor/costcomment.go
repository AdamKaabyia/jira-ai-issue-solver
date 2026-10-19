package executor

import (
	"fmt"
	"strings"

	"go.uber.org/zap"

	"jira-ai-issue-solver/models"
)

const costCommentMarker = "<!-- AI-BOT-COST -->"
const costCrossRefMarker = "<!-- AI-BOT-COST-XREF -->"

type costEntry struct {
	Label string
	Cost  float64
}

// formatCostComment renders a cost comment body from a list of entries.
func formatCostComment(entries []costEntry) string {
	var b strings.Builder
	b.WriteString(costCommentMarker)
	b.WriteString("\n**AI Session Costs**\n\n")
	b.WriteString("| Session | Cost |\n")
	b.WriteString("|---------|------|\n")

	var total float64
	for _, e := range entries {
		fmt.Fprintf(&b, "| %s | $%.2f |\n", e.Label, e.Cost)
		total += e.Cost
	}
	fmt.Fprintf(&b, "| **Total** | **$%.2f** |\n", total)

	return b.String()
}

// parseCostComment extracts cost entries from an existing cost comment.
// Returns nil if the body does not contain a parseable cost table.
func parseCostComment(body string) []costEntry {
	if !strings.Contains(body, costCommentMarker) {
		return nil
	}

	var entries []costEntry
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
			continue
		}

		// Skip header, separator, and total rows.
		if strings.Contains(line, "---") ||
			strings.Contains(line, "Session") ||
			strings.Contains(line, "**Total**") {
			continue
		}

		parts := strings.Split(line, "|")
		if len(parts) < 4 {
			continue
		}
		label := strings.TrimSpace(parts[1])
		costStr := strings.TrimSpace(parts[2])
		costStr = strings.TrimPrefix(costStr, "$")

		var cost float64
		if _, err := fmt.Sscanf(costStr, "%f", &cost); err != nil {
			continue
		}
		entries = append(entries, costEntry{Label: label, Cost: cost})
	}

	return entries
}

// findCostComment returns the existing cost comment from a list of
// issue comments, or nil if none exists.
func findCostComment(comments []models.IssueComment) *models.IssueComment {
	for i := range comments {
		if strings.Contains(comments[i].Body, costCommentMarker) {
			return &comments[i]
		}
	}
	return nil
}

// countFeedbackRounds returns the number of distinct feedback rounds
// in the existing entries. A round starts with a non-retry "Feedback"
// entry (first attempt of each round, regardless of outcome).
func countFeedbackRounds(entries []costEntry) int {
	count := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Label, "Feedback") &&
			!strings.Contains(e.Label, "retry") {
			count++
		}
	}
	return count
}

// feedbackLabel builds a descriptive label for a feedback cost entry.
// attemptNum distinguishes new rounds (1) from retries (2+). suffix
// describes the outcome (e.g., " (no changes)", " (unable)").
func feedbackLabel(entries []costEntry, attemptNum int, suffix string) string {
	if attemptNum <= 1 {
		round := countFeedbackRounds(entries) + 1
		return fmt.Sprintf("Feedback (%d)%s", round, suffix)
	}
	round := countFeedbackRounds(entries)
	if round == 0 {
		round = 1
	}
	retry := attemptNum - 1
	return fmt.Sprintf("Feedback (%d) retry %d%s", round, retry, suffix)
}

// postOrUpdateCostComment posts or updates a cost comment on a PR.
// Labels starting with "Feedback" are auto-sequenced into rounds and
// retries based on attemptNum. Errors are logged but not propagated.
func (p *Pipeline) postOrUpdateCostComment(
	logger *zap.Logger,
	owner, repo string,
	prNumber int,
	cost float64,
	label string,
	attemptNum int,
) {
	if cost <= 0 {
		return
	}

	comments, err := p.git.ListIssueComments(owner, repo, prNumber)
	if err != nil {
		logger.Warn("Failed to list PR comments for cost tracking",
			zap.String("owner", owner), zap.String("repo", repo),
			zap.Int("pr_number", prNumber), zap.Error(err))
		return
	}

	existing := findCostComment(comments)
	if existing != nil {
		entries := parseCostComment(existing.Body)
		if strings.HasPrefix(label, "Feedback") {
			suffix := strings.TrimPrefix(label, "Feedback")
			label = feedbackLabel(entries, attemptNum, suffix)
		}
		entries = append(entries, costEntry{Label: label, Cost: cost})
		body := formatCostComment(entries)

		if err := p.git.UpdateIssueComment(owner, repo, existing.ID, body); err != nil {
			logger.Warn("Failed to update cost comment",
				zap.String("owner", owner), zap.String("repo", repo),
				zap.Int("pr_number", prNumber), zap.Error(err))
		}
		return
	}

	if strings.HasPrefix(label, "Feedback") {
		suffix := strings.TrimPrefix(label, "Feedback")
		label = feedbackLabel(nil, attemptNum, suffix)
	}
	body := formatCostComment([]costEntry{{Label: label, Cost: cost}})

	if _, err := p.git.PostIssueComment(owner, repo, prNumber, body); err != nil {
		logger.Warn("Failed to post cost comment",
			zap.String("owner", owner), zap.String("repo", repo),
			zap.Int("pr_number", prNumber), zap.Error(err))
	}
}

// costCrossRef describes the primary and secondary PRs for a
// cross-reference comment.
type costCrossRef struct {
	primaryOwner  string
	primaryRepo   string
	primaryNumber int
	primaryURL    string
	secondaryPRs  []repoPR
}

// costCrossRefFromRepoPRs builds a costCrossRef from a slice of
// repoPR where the first element is the primary (cost-tracking) PR.
// Returns nil when there are fewer than 2 PRs.
func costCrossRefFromRepoPRs(prs []repoPR) *costCrossRef {
	if len(prs) < 2 {
		return nil
	}
	return &costCrossRef{
		primaryOwner:  prs[0].owner,
		primaryRepo:   prs[0].repo,
		primaryNumber: prs[0].number,
		primaryURL:    prs[0].url,
		secondaryPRs:  prs[1:],
	}
}

// costCrossRefFromRepoInfos builds a costCrossRef from a slice of
// repoPRInfo (feedback path) where the first element is the primary
// (cost-tracking) PR. Returns nil when there are fewer than 2 PRs.
func costCrossRefFromRepoInfos(infos []repoPRInfo) *costCrossRef {
	if len(infos) < 2 {
		return nil
	}
	secondary := make([]repoPR, len(infos)-1)
	for i, ri := range infos[1:] {
		secondary[i] = repoPR{
			owner:  ri.repo.Owner,
			repo:   ri.repo.Repo,
			url:    ri.pr.URL,
			number: ri.pr.Number,
		}
	}
	return &costCrossRef{
		primaryOwner:  infos[0].repo.Owner,
		primaryRepo:   infos[0].repo.Repo,
		primaryNumber: infos[0].pr.Number,
		primaryURL:    infos[0].pr.URL,
		secondaryPRs:  secondary,
	}
}

// findCostCrossRef returns the existing cross-reference comment from
// a list of issue comments, or nil if none exists.
func findCostCrossRef(comments []models.IssueComment) *models.IssueComment {
	for i := range comments {
		if strings.Contains(comments[i].Body, costCrossRefMarker) {
			return &comments[i]
		}
	}
	return nil
}

// postCostCrossReference posts a comment on secondary PRs (PRs 2..N
// in a multi-repo workspace) linking to the cost comment on the
// primary PR. Uses a dedicated marker distinct from the cost table
// marker so the two comment types are not confused.
func (p *Pipeline) postCostCrossReference(logger *zap.Logger, ref *costCrossRef) {
	if ref == nil {
		return
	}

	body := fmt.Sprintf("%s\nAI session costs are tracked on [%s/%s#%d](%s).",
		costCrossRefMarker, ref.primaryOwner, ref.primaryRepo,
		ref.primaryNumber, ref.primaryURL)

	for _, pr := range ref.secondaryPRs {
		comments, err := p.git.ListIssueComments(pr.owner, pr.repo, pr.number)
		if err != nil {
			logger.Warn("Failed to list comments for cost cross-reference",
				zap.String("repo", pr.repo), zap.Error(err))
			continue
		}
		if findCostCrossRef(comments) != nil {
			continue
		}
		if _, err := p.git.PostIssueComment(pr.owner, pr.repo, pr.number, body); err != nil {
			logger.Warn("Failed to post cost cross-reference",
				zap.String("repo", pr.repo), zap.Error(err))
		}
	}
}
