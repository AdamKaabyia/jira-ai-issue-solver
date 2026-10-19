package commentfilter

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"jira-ai-issue-solver/models"
)

var ciFixAttemptRe = regexp.MustCompile(`<!-- ci-fix-attempt: ([\d,]+) -->`)

// CIFixAttemptMarker returns a comment body recording that the bot
// attempted to fix CI failures in the given commit.
func CIFixAttemptMarker(failures []models.CheckRunFailure, commitSHA string) string {
	ids := make([]int64, len(failures))
	for i, f := range failures {
		ids[i] = f.ID
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}

	return fmt.Sprintf("CI failures addressed in %s.\n<!-- ci-fix-attempt: %s -->",
		commitSHA, strings.Join(parts, ","))
}

// CountCIFixAttempts counts the number of ci-fix-attempt markers in
// the given PR comments from the bot.
func CountCIFixAttempts(comments []models.PRComment, botUsername string) int {
	normBot := NormalizeUsername(botUsername)
	count := 0
	for _, c := range comments {
		if NormalizeUsername(c.Author.Username) != normBot {
			continue
		}
		if ciFixAttemptRe.MatchString(c.Body) {
			count++
		}
	}
	return count
}
