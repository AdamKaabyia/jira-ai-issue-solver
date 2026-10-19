package executor

import (
	"context"

	"go.uber.org/zap"

	"jira-ai-issue-solver/container"
	"jira-ai-issue-solver/models"
	"jira-ai-issue-solver/repoconfig"
)

// MergeImports exposes mergeImports for testing.
func MergeImports(
	settings *models.ProjectSettings,
	repoCfg *repoconfig.Config,
) []ImportEntry {
	return mergeImports(settings, repoCfg)
}

// ImportEntry is the exported version of importEntry for tests.
type ImportEntry = importEntry

// RunImportInstalls exposes runImportInstalls for testing.
func (p *Pipeline) RunImportInstalls(
	ctx context.Context,
	logger *zap.Logger,
	ctr *container.Container,
	imports []ImportEntry,
) error {
	return p.runImportInstalls(ctx, logger, ctr, imports)
}

// ReadPRDescription exposes readPRDescription for testing.
func ReadPRDescription(dir string) *PRDescription {
	return readPRDescription(dir)
}

// ParsePRContent exposes parsePRContent for testing.
func ParsePRContent(content string) (string, string) {
	return parsePRContent(content)
}

// BuildPRContent exposes buildPRContent for testing.
func BuildPRContent(workItem *models.WorkItem, ticketKey, titlePrefix string, aiPR *PRDescription) (string, string) {
	return buildPRContent(workItem, ticketKey, titlePrefix, aiPR)
}

// ContainerCredsMountTarget exposes the mount path constant for tests.
const ContainerCredsMountTarget = containerCredsMountTarget

// SetPipelineLabel exposes setPipelineLabel for testing.
func SetPipelineLabel(p *Pipeline, logger *zap.Logger, ticketKey string, allLabels []string, targetLabel string) {
	p.setPipelineLabel(logger, ticketKey, allLabels, targetLabel)
}

// ClearFailureLabels exposes clearFailureLabels for testing.
func ClearFailureLabels(p *Pipeline, logger *zap.Logger, ticketKey string, fl models.FailureLabels) {
	p.clearFailureLabels(logger, ticketKey, fl)
}

// ValidateForkMode exposes validateForkMode for testing.
func ValidateForkMode(p *Pipeline, logger *zap.Logger, ticketKey string, workItem *models.WorkItem, settings *models.ProjectSettings) error {
	return p.validateForkMode(logger, ticketKey, workItem, settings)
}

// ValidationLabel exposes validationLabel for testing.
func ValidationLabel(session SessionOutput, exitCode int, vl models.PRValidationLabels) string {
	return validationLabel(session, exitCode, vl)
}

// ValidationPassed exposes validationPassed for testing.
func ValidationPassed(session SessionOutput, exitCode int) bool {
	return validationPassed(session, exitCode)
}

// SetPRValidationLabel exposes setPRValidationLabel for testing.
func SetPRValidationLabel(p *Pipeline, logger *zap.Logger, owner, repo string, prNumber int, vl models.PRValidationLabels, targetLabel string) {
	p.setPRValidationLabel(logger, owner, repo, prNumber, vl, targetLabel)
}

// ClearPRValidationLabels exposes clearPRValidationLabels for testing.
func ClearPRValidationLabels(p *Pipeline, logger *zap.Logger, owner, repo string, prNumber int, vl models.PRValidationLabels) {
	p.clearPRValidationLabels(logger, owner, repo, prNumber, vl)
}

// ApplyCostCapPRLabel exposes applyCostCapPRLabel for testing.
func ApplyCostCapPRLabel(p *Pipeline, logger *zap.Logger, ticketKey string, settings *models.ProjectSettings, exceeded bool) {
	p.applyCostCapPRLabel(logger, ticketKey, settings, exceeded)
}

// TicketCostPath exposes the ticket cost file path constant for tests.
const TicketCostPath = ticketCostPath

// ErrTicketCostCapExceeded exposes the sentinel error for tests.
var ErrTicketCostCapExceeded = errTicketCostCapExceeded

// CheckTicketCostCap exposes checkTicketCostCap for testing.
func CheckTicketCostCap(p *Pipeline, logger *zap.Logger, wsPath string, maxCost float64) bool {
	return p.checkTicketCostCap(logger, wsPath, maxCost)
}

// RecordTicketCost exposes recordTicketCost for testing.
func RecordTicketCost(p *Pipeline, logger *zap.Logger, wsPath string, maxCost float64, cost float64) {
	p.recordTicketCost(logger, wsPath, maxCost, cost)
}
