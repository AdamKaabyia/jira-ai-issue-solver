// Package commentfilter provides bot-loop prevention filtering for
// PR comments.
//
// The filter removes comments that would create bot-to-bot
// conversation loops or violate thread depth limits. It is used by
// both scanners (to decide whether to emit feedback events) and
// executors (to exclude non-actionable comments before processing).
//
// Filtering rules:
//   - Comments from ignored usernames are removed entirely
//   - Comments containing only slash commands (e.g. /lgtm) are removed
//   - Comments containing @<botUsername> ignore are removed
//   - Known bot comments replying to our bot are removed (prevents
//     bot-to-bot ping-pong)
//   - Comments in threads where our bot has replied at or beyond
//     [Config.MaxThreadDepth] times are removed
//
// Bot's own comments are preserved in the output. Callers that need
// to split or exclude them (e.g., [executor.CategorizeComments])
// handle that separately.
package commentfilter

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"jira-ai-issue-solver/models"
)

// addressedRe matches the HTML comment marker embedded in bot replies
// to conversation comments. The captured group is the comment ID.
var addressedRe = regexp.MustCompile(`<!-- addressed: (\d+) -->`)

// AddressedMarker returns the HTML comment marker that links a bot
// reply to the conversation comment it addresses.
func AddressedMarker(commentID int64) string {
	return fmt.Sprintf("<!-- addressed: %d -->", commentID)
}

// parseAddressedIDs extracts comment IDs from addressed markers in
// the given text. Returns nil if no markers are found.
func parseAddressedIDs(body string) []int64 {
	matches := addressedRe.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(matches))
	for _, m := range matches {
		id, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

// Config holds bot-loop prevention settings.
type Config struct {
	// BotUsername is the bot's GitHub username, used to identify
	// the bot's own comments and calculate thread depth.
	BotUsername string

	// IgnoredUsernames lists usernames whose comments are removed
	// entirely. Use for CI/build bots whose output is not code
	// review feedback (e.g., packit-as-a-service[bot]).
	IgnoredUsernames []string

	// KnownBotUsernames lists usernames of other bots. Their
	// top-level comments are kept, but replies to our bot's
	// comments are removed to prevent bot-to-bot loops.
	KnownBotUsernames []string

	// MaxThreadDepth limits how many times our bot can appear in
	// a comment thread's ancestry. Comments in threads at or
	// exceeding this depth are removed. Zero or negative disables
	// the limit.
	MaxThreadDepth int
}

// Filter removes comments that violate bot-loop prevention rules.
// Bot's own comments are preserved (callers need them for address
// detection). The returned slice is never nil.
func Filter(comments []models.PRComment, cfg Config) []models.PRComment {
	if len(comments) == 0 {
		return []models.PRComment{}
	}

	byID := buildLookup(comments)
	normBot := NormalizeUsername(cfg.BotUsername)
	ignoreRe := botIgnoreDirectiveRe(normBot)

	result := make([]models.PRComment, 0, len(comments))
	for _, c := range comments {
		norm := NormalizeUsername(c.Author.Username)

		// Keep bot's own comments (needed for address detection).
		if norm == normBot {
			result = append(result, c)
			continue
		}

		if isIgnored(norm, cfg.IgnoredUsernames) {
			continue
		}

		if isSlashCommandOnly(c.Body) {
			continue
		}

		if ignoreRe.MatchString(c.Body) {
			continue
		}

		if isKnownBot(norm, cfg.KnownBotUsernames) && c.InReplyTo != 0 {
			if shouldSkipBotReply(c, byID, normBot) {
				continue
			}
		}

		if cfg.MaxThreadDepth > 0 {
			if threadDepth(c.ID, normBot, byID) >= cfg.MaxThreadDepth {
				continue
			}
		}

		result = append(result, c)
	}

	return result
}

// HasNewActionable reports whether any comments remain actionable
// after bot-loop prevention AND excluding comments the bot has
// already replied to.
func HasNewActionable(comments []models.PRComment, cfg Config) bool {
	filtered := Filter(comments, cfg)
	normBot := NormalizeUsername(cfg.BotUsername)
	botRepliedTo := BotRepliedTo(filtered, normBot)

	for _, c := range filtered {
		if NormalizeUsername(c.Author.Username) == normBot {
			continue
		}
		if !botRepliedTo[c.ID] {
			return true
		}
	}

	return false
}

// BotRepliedTo builds the set of comment IDs that the bot has
// replied to. For review comments this is detected via InReplyTo;
// for conversation comments it is detected via addressed markers
// embedded in the bot's comment body.
//
// For flat review threads (where GitHub normalises every reply's
// InReplyTo to the thread root), a second pass marks non-bot
// comments as replied-to when a bot reply in the same thread has
// a timestamp at or after the comment's timestamp.
func BotRepliedTo(comments []models.PRComment, normBot string) map[int64]bool {
	replied := make(map[int64]bool)

	// Track the latest bot reply timestamp per review thread root.
	threadBotLatest := make(map[int64]time.Time)

	for _, c := range comments {
		if NormalizeUsername(c.Author.Username) != normBot {
			continue
		}
		// Review comment reply: threaded via InReplyTo.
		if c.InReplyTo != 0 {
			replied[c.InReplyTo] = true
			if c.IsReviewComment && c.Timestamp.After(threadBotLatest[c.InReplyTo]) {
				threadBotLatest[c.InReplyTo] = c.Timestamp
			}
		}
		// Conversation comment reply: marker in body.
		for _, id := range parseAddressedIDs(c.Body) {
			replied[id] = true
		}
	}

	// Second pass: in flat review threads, mark non-bot follow-up
	// comments as replied-to when the bot replied at or after them.
	for _, c := range comments {
		if NormalizeUsername(c.Author.Username) == normBot {
			continue
		}
		if c.IsReviewComment && c.InReplyTo != 0 {
			if botLatest, ok := threadBotLatest[c.InReplyTo]; ok && !c.Timestamp.After(botLatest) {
				replied[c.ID] = true
			}
		}
	}

	return replied
}

// NormalizeUsername strips the GitHub [bot] suffix and lowercases
// for case-insensitive comparison.
func NormalizeUsername(s string) string {
	return strings.ToLower(strings.TrimSuffix(s, "[bot]"))
}

func buildLookup(comments []models.PRComment) map[int64]models.PRComment {
	m := make(map[int64]models.PRComment, len(comments))
	for _, c := range comments {
		m[c.ID] = c
	}
	return m
}

func isIgnored(normUsername string, ignored []string) bool {
	for _, u := range ignored {
		if NormalizeUsername(u) == normUsername {
			return true
		}
	}
	return false
}

func isKnownBot(normUsername string, knownBots []string) bool {
	for _, b := range knownBots {
		if NormalizeUsername(b) == normUsername {
			return true
		}
	}
	return false
}

// isSlashCommandOnly returns true when every non-empty line in body
// starts with '/' (e.g. Prow commands like /lgtm, /approve).
func isSlashCommandOnly(body string) bool {
	hasCommand := false
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "/") {
			return false
		}
		hasCommand = true
	}
	return hasCommand
}

// botIgnoreDirectiveRe builds a regexp matching @<botUsername> ignore
// (case-insensitive), where the [bot] suffix is optional. The word
// boundary after "ignore" prevents false positives like "ignoring".
// Horizontal whitespace only ([ \t]+) prevents matching when the
// mention and "ignore" are on separate lines. A left boundary
// ensures the @ is not part of a larger token (e.g., an email).
func botIgnoreDirectiveRe(normBot string) *regexp.Regexp {
	pattern := `(?i)(^|[^[:alnum:]_])@` + regexp.QuoteMeta(normBot) + `(\[bot\])?[ \t]+ignore\b`
	return regexp.MustCompile(pattern)
}

// BotCommandRe builds a compiled regexp matching @<botUsername> <command>.
// The structure mirrors [botIgnoreDirectiveRe]: case-insensitive,
// optional [bot] suffix, horizontal whitespace only, word boundary.
// Callers that check multiple comments should compile once and reuse.
func BotCommandRe(botUsername, command string) *regexp.Regexp {
	normBot := NormalizeUsername(botUsername)
	pattern := `(?i)(^|[^[:alnum:]_])@` + regexp.QuoteMeta(normBot) + `(\[bot\])?[ \t]+` + regexp.QuoteMeta(command) + `\b`
	return regexp.MustCompile(pattern)
}

// IsSyncCommand reports whether body contains an @<botUsername> sync
// directive. The match is case-insensitive and accepts an optional
// [bot] suffix on the username. Recompiles the regex on each call;
// for checking multiple bodies, use [BotCommandRe] once and reuse.
func IsSyncCommand(body, botUsername string) bool {
	return BotCommandRe(botUsername, "sync").MatchString(body)
}

// shouldSkipBotReply returns true if a known bot is replying to our
// bot's comment, or if the parent comment is missing (defensive
// skip — the parent may have been our bot's comment).
func shouldSkipBotReply(c models.PRComment, byID map[int64]models.PRComment, normBot string) bool {
	parent, found := byID[c.InReplyTo]
	if !found {
		return true
	}
	return NormalizeUsername(parent.Author.Username) == normBot
}

// threadDepth counts how many times our bot appears in a comment's
// thread. For review comments (flat threading where all replies share
// the same InReplyTo root), it counts bot replies in the thread. For
// conversation comments (nested threading), it walks the parent chain.
func threadDepth(commentID int64, normBot string, byID map[int64]models.PRComment) int {
	c, found := byID[commentID]
	if !found {
		return 0
	}

	if c.IsReviewComment {
		return reviewThreadBotCount(c, normBot, byID)
	}

	return parentChainBotCount(commentID, normBot, byID)
}

// reviewThreadBotCount counts bot replies in a flat review thread.
// GitHub normalises all review replies to point at the thread root,
// so we count siblings rather than walking ancestors. Root comments
// (InReplyTo == 0) return 0 — they start the thread and should
// never be filtered by depth.
func reviewThreadBotCount(c models.PRComment, normBot string, byID map[int64]models.PRComment) int {
	if c.InReplyTo == 0 {
		return 0
	}

	count := 0
	for _, sibling := range byID {
		if NormalizeUsername(sibling.Author.Username) == normBot && sibling.InReplyTo == c.InReplyTo {
			count++
		}
	}
	return count
}

// parentChainBotCount counts bot appearances walking from commentID
// upward through InReplyTo parents. Used for conversation comments
// whose reply chains are properly nested.
func parentChainBotCount(commentID int64, normBot string, byID map[int64]models.PRComment) int {
	depth := 0
	currentID := commentID
	visited := make(map[int64]bool)

	for currentID != 0 {
		if visited[currentID] {
			break
		}
		visited[currentID] = true

		c, found := byID[currentID]
		if !found {
			break
		}

		if NormalizeUsername(c.Author.Username) == normBot {
			depth++
		}

		currentID = c.InReplyTo
	}

	return depth
}
